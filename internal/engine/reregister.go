package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
)

// ErrNoReregistration is the error ReregisterAfterRelease returns when the
// new registration was never sent: the deregistration could not be sent,
// or the core sent no UE CONTEXT RELEASE COMMAND to answer.
var ErrNoReregistration = errors.New("the registration after the release was not sent")

// Reregistration reports a switch-off deregistration followed, on the same
// association, by a new initial registration of the same UE
// (Manager.ReregisterAfterRelease).
type Reregistration struct {
	// Deregistration is the switch-off deregistration: its Release is the
	// UE CONTEXT RELEASE COMMAND the gNB answered.
	Deregistration *DeregisterResult
	// CompletedAt is when the UE CONTEXT RELEASE COMPLETE had been written
	// and RequestedAt when the INITIAL UE MESSAGE with the new REGISTRATION
	// REQUEST had been. RequestedAt is zero when it was not sent.
	CompletedAt, RequestedAt time.Time
	// RANUENGAPID is the RAN UE NGAP ID of the new registration.
	RANUENGAPID int64
	// Attach is the new registration's result. It is nil, with AttachErr
	// saying why, when the registration did not complete: the association
	// is then closed and the UE is not registered with the Manager.
	Attach    *AttachResult
	AttachErr error
}

// ReregisterAfterRelease deregisters a registered UE (switch-off) and
// registers it again over the same association as soon as the core has
// released it: at the UE CONTEXT RELEASE COMMAND the gNB sends the UE
// CONTEXT RELEASE COMPLETE and, gap later (zero: at once), an INITIAL UE
// MESSAGE with the REGISTRATION REQUEST of ueCfg under the next RAN UE
// NGAP ID. The association is set to send each PDU as it is written
// (gnb.UETransport.SetNoDelay), and stays so, so that the two leave gap
// apart. ueCfg carries the new registration's probe, if any; its
// RANUENGAPID is set here. Of opts only WithReleaseWait applies: the
// deregistration is always a switch-off.
//
// The UE's association must be one the Manager opened. An error wrapping
// ErrNoReregistration means the new registration was never sent, and the
// association is closed with the UE deregistered as far as the request
// went. Otherwise the result says how the new registration ended: with
// Attach set the UE is registered again, under the same SUPI, and stays
// with the Manager.
func (m *Manager) ReregisterAfterRelease(ctx context.Context, supi string, ueCfg UEConfig, gap time.Duration, opts ...DeregisterOption) (*Reregistration, error) {
	var o deregisterOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.normal {
		return nil, errors.New("a registration after the release follows a switch-off deregistration only")
	}
	linger := m.releaseLinger
	if o.releaseWait > 0 {
		linger = o.releaseWait
	}
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("UE %s is not registered", supi)
	}
	release, err := sess.beginProcedure(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	// The association is read under the procedure lock: a handover replaces
	// it under the same lock.
	lane, ok := sess.conn.(*gnb.UETransport)
	if !ok {
		return nil, fmt.Errorf("UE %s: its association cannot carry a second UE", supi)
	}
	// The UE CONTEXT RELEASE COMPLETE and the INITIAL UE MESSAGE are two
	// small writes: without this the kernel holds the second until the
	// first is acknowledged, about 200 ms, and gap would mean nothing.
	if err := lane.SetNoDelay(true); err != nil {
		return nil, fmt.Errorf("UE %s: %w", supi, err)
	}

	dres, completedAt, err := m.switchOffForRegistration(ctx, sess, linger)
	if dres == nil {
		_ = lane.Close()
		return nil, err
	}
	res := &Reregistration{Deregistration: dres, CompletedAt: completedAt}
	if err != nil {
		_ = lane.Close()
		return res, err
	}

	if gap > 0 {
		t := time.NewTimer(gap)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			_ = lane.Close()
			return res, fmt.Errorf("%w: %v", ErrNoReregistration, ctx.Err())
		}
	}
	// The new UE takes the association over; the old lane closes without
	// closing it.
	res.RANUENGAPID = sess.ranID + 1
	next := lane.Successor(res.RANUENGAPID)
	_ = lane.Close()
	ueCfg.RANUENGAPID = res.RANUENGAPID
	emit := func(ev StateEvent) {
		if ev.State == StateRegistering && res.RequestedAt.IsZero() {
			res.RequestedAt = time.Now()
		}
		m.hub.publish(ev)
	}
	again, err := Attach(ctx, next, sess.ServingGNB(), ueCfg, m.log, emit)
	if err != nil {
		_ = next.Close()
		res.AttachErr = err
		return res, nil
	}
	m.adopt(again, sess.N2Source(), ueCfg.GNBN3Addr)
	res.Attach = again.Result
	return res, nil
}

// switchOffForRegistration ends sess's registration as DeregisterWith does
// with a switch-off, and waits up to linger for the core's UE CONTEXT
// RELEASE COMMAND, which the gNB answers with the UE CONTEXT RELEASE
// COMPLETE. It returns the deregistration and when the complete had been
// written. The deregistration is nil when the request could not be sent;
// the error, wrapping ErrNoReregistration, is set then and when no release
// came. It closes no association, and the caller holds sess's procedure
// lock.
func (m *Manager) switchOffForRegistration(ctx context.Context, sess *Session, linger time.Duration) (*DeregisterResult, time.Time, error) {
	supi := sess.SUPI
	// The registration ends here whatever follows, as in DeregisterWith.
	m.mu.Lock()
	delete(m.sessions, supi)
	m.mu.Unlock()
	sess.markReleased()
	m.releaseAppSessions(supi)
	sess.closeDataPath()
	m.appMetrics.forgetUE(supi)

	if err := sess.deregister(ctx, true); err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %v", ErrNoReregistration, err)
	}
	res := &DeregisterResult{SwitchOff: true, AMFUENGAPID: sess.amfID, RANUENGAPID: sess.ranID, SentAt: time.Now()}
	res.Release, res.ReleaseErr = sess.awaitRelease(ctx, linger, m.log)
	completedAt := time.Now()
	if err := res.ReleaseErr; err != nil {
		m.hub.publish(StateEvent{SUPI: supi, State: StateDeregistered, Detail: "UE deregistered (switch-off); " + err.Error()})
		return res, completedAt, fmt.Errorf("%w: %v", ErrNoReregistration, err)
	}
	m.hub.publish(StateEvent{SUPI: supi, State: StateDeregistered, Detail: "UE deregistered (switch-off)"})
	return res, completedAt, nil
}
