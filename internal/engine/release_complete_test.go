package engine

import (
	"context"
	"errors"
	"testing"
	"time"
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
