package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
)

// unwritableConn is a scripted association whose writes fail.
type unwritableConn struct{ *scriptedConn }

func (c unwritableConn) WriteNGAP(uint16, []byte) error {
	return errors.New("association write failed")
}

// A UE CONTEXT RELEASE COMMAND the core sent is recorded as received also
// when its UE CONTEXT RELEASE COMPLETE could not be sent, with an error
// wrapping ErrReleaseCompleteNotSent rather than one that says no release
// came.
func TestReleaseCompleteNotSent(t *testing.T) {
	const amfID, ranID = 7, 3
	newSession := func(t *testing.T) *Session {
		conn := newScriptedConn()
		t.Cleanup(func() { _ = conn.Close() })
		conn.feed(t, staleReleaseCommand(amfID, ranID))
		return &Session{SUPI: "208930100007544", amfID: amfID, ranID: ranID, conn: unwritableConn{conn}}
	}
	check := func(t *testing.T, rel *UEContextRelease, err error) {
		t.Helper()
		if rel == nil || rel.AMFUENGAPID != amfID || rel.RANUENGAPID != ranID {
			t.Errorf("release %+v, want the command recorded", rel)
		}
		if !errors.Is(err, ErrReleaseCompleteNotSent) || errors.Is(err, errNoUEContextRelease) {
			t.Errorf("err = %v, want ErrReleaseCompleteNotSent and not errNoUEContextRelease", err)
		}
	}
	t.Run("switch-off", func(t *testing.T) {
		rel, err := newSession(t).awaitRelease(context.Background(), time.Second, nil)
		check(t, rel, err)
	})
	t.Run("after a Deregistration Accept", func(t *testing.T) {
		res := &DeregisterResult{Accepted: true}
		err := newSession(t).awaitAnswer(context.Background(), time.Second, false, nil, res)
		check(t, res.Release, err)
		check(t, res.Release, res.ReleaseErr)
	})
	t.Run("in place of the Accept", func(t *testing.T) {
		res := &DeregisterResult{}
		err := newSession(t).awaitAnswer(context.Background(), time.Second, true, nil, res)
		if !errors.Is(err, errNoDeregistrationAccept) {
			t.Errorf("err = %v, want errNoDeregistrationAccept", err)
		}
		check(t, res.Release, res.ReleaseErr)
	})
}

// firstWriteConn is a scripted association whose first write succeeds and
// whose later ones fail.
type firstWriteConn struct {
	*scriptedConn
	writes *int
}

func (c firstWriteConn) WriteNGAP(uint16, []byte) error {
	*c.writes++
	if *c.writes > 1 {
		return errors.New("association write failed")
	}
	return nil
}

// RequestContextRelease says which side ended it: an association that
// fails while the command is awaited wraps ErrReleaseWaitLost; a command
// whose complete could not be sent is reported released, with an error
// wrapping ErrReleaseCompleteNotSent.
func TestRequestContextReleaseErrors(t *testing.T) {
	const supi, amfID, ranID = "208930100007545", 7, 3
	tests := []struct {
		name         string
		setup        func(t *testing.T, conn *scriptedConn) gnb.Transport
		want         error
		wantReleased bool
	}{
		{name: "association lost", want: ErrReleaseWaitLost, setup: func(t *testing.T, conn *scriptedConn) gnb.Transport {
			go func() {
				time.Sleep(20 * time.Millisecond)
				_ = conn.Close()
			}()
			return conn
		}},
		{name: "the complete not sent", want: ErrReleaseCompleteNotSent, wantReleased: true, setup: func(t *testing.T, conn *scriptedConn) gnb.Transport {
			conn.feed(t, staleReleaseCommand(amfID, ranID))
			return firstWriteConn{conn, new(int)}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newScriptedConn()
			sess := &Session{SUPI: supi, Result: &AttachResult{}, amfID: amfID, ranID: ranID}
			sess.conn = tc.setup(t, conn)
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.sessions[supi] = sess
			res, err := m.RequestContextRelease(context.Background(), supi, 2*time.Second)
			if !errors.Is(err, tc.want) || res.Released != tc.wantReleased {
				t.Errorf("= %+v, %v; want released %v and an error wrapping %v", res, err, tc.wantReleased, tc.want)
			}
		})
	}
}
