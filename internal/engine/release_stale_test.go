package engine

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
)

// staleReleaseCommand is a UE CONTEXT RELEASE COMMAND for the UE with the
// given NGAP IDs.
func staleReleaseCommand(amfID, ranID int64) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeUEContextRelease
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentUEContextReleaseCommand
	cmd := new(ngapType.UEContextReleaseCommand)
	pdu.InitiatingMessage.Value.UEContextReleaseCommand = cmd
	ie := ngapType.UEContextReleaseCommandIEs{}
	ie.Id.Value = ngapType.ProtocolIEIDUENGAPIDs
	ie.Criticality.Value = ngapType.CriticalityPresentReject
	ie.Value.Present = ngapType.UEContextReleaseCommandIEsPresentUENGAPIDs
	ie.Value.UENGAPIDs = &ngapType.UENGAPIDs{Present: ngapType.UENGAPIDsPresentUENGAPIDPair,
		UENGAPIDPair: &ngapType.UENGAPIDPair{AMFUENGAPID: ngapType.AMFUENGAPID{Value: amfID}, RANUENGAPID: ngapType.RANUENGAPID{Value: ranID}}}
	cause := ngapType.UEContextReleaseCommandIEs{}
	cause.Id.Value = ngapType.ProtocolIEIDCause
	cause.Criticality.Value = ngapType.CriticalityPresentIgnore
	cause.Value.Present = ngapType.UEContextReleaseCommandIEsPresentCause
	cause.Value.Cause = &ngapType.Cause{Present: ngapType.CausePresentNas, Nas: &ngapType.CauseNas{Value: ngapType.CauseNasPresentNormalRelease}}
	cmd.ProtocolIEs.List = append(cmd.ProtocolIEs.List, ie, cause)
	return pdu
}

// A UE CONTEXT RELEASE COMMAND that arrived before the request, kept by an
// earlier read or queued on the UE's lane, is not the answer: a core that
// does not answer the request leaves the UE released = false and CM-CONNECTED.
func TestRequestContextReleaseIgnoresAStaleCommand(t *testing.T) {
	const supi, amfID, ranID = "208930100007541", 5, 1
	tests := []struct {
		name string
		// stale puts the command on the UE's association before the request.
		stale func(t *testing.T, sess *Session, conn *scriptedConn)
	}{
		{name: "kept by an earlier read", stale: func(t *testing.T, sess *Session, conn *scriptedConn) {
			sess.conn = conn
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			_, _ = sess.readPDU(ctx)
			cancel()
			conn.feed(t, staleReleaseCommand(amfID, ranID))
			deadline := time.Now().Add(2 * time.Second)
			for len(sess.held) == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if len(sess.held) == 0 {
				t.Fatal("the stale command was not kept")
			}
		}},
		{name: "queued on the lane", stale: func(t *testing.T, sess *Session, conn *scriptedConn) {
			sess.conn = gnb.Dedicated(conn, gnb.Config{}, ranID)
			conn.feed(t, staleReleaseCommand(amfID, ranID))
			// The lane's read loop queues it.
			time.Sleep(100 * time.Millisecond)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newScriptedConn()
			sess := &Session{SUPI: supi, Result: &AttachResult{}, amfID: amfID, ranID: ranID}
			tc.stale(t, sess, conn)
			defer func() { _ = sess.conn.Close() }()
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.sessions[supi] = sess
			res, err := m.RequestContextRelease(context.Background(), supi, 150*time.Millisecond)
			if err != nil {
				t.Fatalf("RequestContextRelease: %v", err)
			}
			if res.Released || sess.Idle() {
				t.Errorf("result %+v, idle %v; want the stale command ignored and the UE CM-CONNECTED", res, sess.Idle())
			}
		})
	}
}
