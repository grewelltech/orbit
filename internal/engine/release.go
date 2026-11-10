package engine

import (
	"context"
	"fmt"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
)

// ContextReleaseResult is what a RAN-initiated UE context release reports.
type ContextReleaseResult struct {
	// Released reports whether the AMF's UE CONTEXT RELEASE COMMAND arrived
	// within the wait (it is then answered with UE CONTEXT RELEASE
	// COMPLETE), After how long after the request was sent, and Cause the
	// command's cause.
	Released bool
	After    time.Duration
	Cause    string
	// Skipped names, in order, the other downlink PDUs read while waiting.
	Skipped []string
}

// RequestContextRelease sends a UE CONTEXT RELEASE REQUEST for a registered
// UE from its serving gNB (TS 38.413 8.3.2; cause radio network "release
// due to NG-RAN generated reason") and waits up to wait for the AMF's UE
// CONTEXT RELEASE COMMAND, which it answers with UE CONTEXT RELEASE
// COMPLETE.
//
// A released UE stays registered, with no NG connection and no data path:
// its PDU sessions' user plane is gone with the context. The only
// procedure it supports from then on is Deregister, which sends the
// Deregistration Request as an initial NAS message on a new NG connection.
// A UE the AMF did not release is as it was. A missing command is not an
// error; an error means the request could not be sent or waited for.
func (m *Manager) RequestContextRelease(ctx context.Context, supi string, wait time.Duration) (ContextReleaseResult, error) {
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return ContextReleaseResult{}, fmt.Errorf("UE %s is not registered", supi)
	}
	release, err := sess.beginProcedure(ctx)
	if err != nil {
		return ContextReleaseResult{}, err
	}
	defer release()
	if sess.idle.Load() {
		return ContextReleaseResult{}, fmt.Errorf("UE %s has no NG connection to release", supi)
	}

	var res ContextReleaseResult
	sent := time.Now()
	if err := gnb.SendPDU(sess.conn, ueStream, gnb.BuildUEContextReleaseRequest(sess.amfID, sess.ranID)); err != nil {
		return res, fmt.Errorf("send UEContextReleaseRequest: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		pdu, err := sess.readPDU(wctx)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return res, fmt.Errorf("await UEContextReleaseCommand: %w", ctx.Err())
			case wctx.Err() != nil:
				return res, nil // no command within the wait
			}
			return res, fmt.Errorf("await UEContextReleaseCommand: %w", err)
		}
		cmd, ok := gnb.ParseUEContextReleaseCommand(pdu)
		if !ok {
			code, _ := procedureCode(pdu)
			res.Skipped = append(res.Skipped, procedureName(code))
			continue
		}
		res.Released, res.After, res.Cause = true, time.Since(sent), cmd.Cause
		amfID, ranID := sess.amfID, sess.ranID
		if cmd.HasPair {
			amfID, ranID = cmd.AMFUENGAPID, cmd.RANUENGAPID
		}
		if err := gnb.SendPDU(sess.conn, ueStream, gnb.BuildUEContextReleaseComplete(amfID, ranID)); err != nil {
			return res, fmt.Errorf("UE Context Release Complete not sent: %w", err)
		}
		sess.idle.Store(true)
		sess.closeDataPath()
		m.log.Info("UE context released on the gNB's request", "supi", supi, "cause", cmd.Cause)
		return res, nil
	}
}

// deregisterFromIdle sends the Deregistration Request of a UE with no NG
// connection: as an initial NAS message, integrity protected and not
// ciphered (TS 24.501 4.4.6: all of its IEs are cleartext IEs), in an
// Initial UE Message under a new RAN-UE-NGAP-ID that carries the UE's
// 5G-S-TMSI.
func (s *Session) deregisterFromIdle(msg *f5nas.Message) error {
	wrapped, err := s.sec.EncodeSecure(msg, nas.SecHdrIntegrity)
	if err != nil {
		return fmt.Errorf("wrap Deregistration Request: %w", err)
	}
	s.ranID++
	pdu, err := gnb.BuildInitialUEMessageFromIdle(s.ServingGNB(), s.ranID, wrapped, s.guti)
	if err != nil {
		return err
	}
	return gnb.SendPDU(s.conn, ueStream, pdu)
}
