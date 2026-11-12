package engine

import (
	"context"
	"errors"
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

// ErrReleaseWaitLost marks a RequestContextRelease whose wait for the UE
// CONTEXT RELEASE COMMAND ended because the UE's association could not be
// read, before the caller's context or the wait ran out.
var ErrReleaseWaitLost = errors.New("the association failed while the UE Context Release Command was awaited")

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
// error; an error means the request could not be sent or waited for. A
// wait that failed on the association wraps ErrReleaseWaitLost; a command
// whose complete could not be sent is reported Released, with an error
// wrapping ErrReleaseCompleteNotSent.
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

	// What arrived before the request cannot be its answer.
	if d, ok := sess.conn.(interface{ Drain() int }); ok {
		d.Drain()
	}
	sess.dropHeld()
	var res ContextReleaseResult
	sent := time.Now()
	active := sess.activeSessionIDs()
	if err := gnb.SendPDU(sess.conn, ueStream, gnb.BuildUEContextReleaseRequest(sess.amfID, sess.ranID, active...)); err != nil {
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
			return res, fmt.Errorf("%w: await UEContextReleaseCommand: %w", ErrReleaseWaitLost, err)
		}
		cmd, ok := gnb.ParseUEContextReleaseCommand(pdu)
		if !ok {
			code, _ := procedureCode(pdu)
			res.Skipped = append(res.Skipped, procedureName(code))
			continue
		}
		res.Released, res.After, res.Cause = true, time.Since(sent), cmd.Cause
		if err := sess.answerRelease(pdu, active...); err != nil {
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
