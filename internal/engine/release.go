package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
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
// A released UE is CM-IDLE, as after Manager.Idle: registered, with no NG
// connection and no data path, its PDU sessions' user plane gone with the
// context. Deregister then sends the Deregistration Request as an initial
// NAS message on a new NG connection.
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
	if sess.Idle() {
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
		if err := sess.answerRelease(pdu); err != nil {
			return res, err
		}
		m.releaseAppSessions(supi)
		sess.closeDataPath()
		sess.setConnection(true, true)
		sess.setState(StateIdle)
		m.hub.publish(StateEvent{SUPI: supi, State: StateIdle, Detail: "UE context released on the gNB's request; UE is CM-IDLE"})
		m.log.Info("UE context released on the gNB's request", "supi", supi, "cause", cmd.Cause)
		return res, nil
	}
}
