package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
)

// OtherRegistration reports a switch-off deregistration of one UE followed
// by an initial registration of another UE on an association of its own
// (Manager.RegisterAfterOtherRelease).
type OtherRegistration struct {
	// Deregistration is the first UE's switch-off deregistration: its
	// Release is the UE CONTEXT RELEASE COMMAND the first gNB answered.
	Deregistration *DeregisterResult
	// CompletedAt is when the first gNB had written the UE CONTEXT RELEASE
	// COMPLETE, and RequestedAt when the second had written the INITIAL UE
	// MESSAGE with the other UE's REGISTRATION REQUEST. RequestedAt is zero
	// when it was not sent.
	CompletedAt, RequestedAt time.Time
	// N2Source is the local address of the second association.
	N2Source string
	// Attach is the other UE's registration result. It is nil, with
	// AttachErr saying why, when the registration did not complete: its
	// association is then closed and the UE is not registered with the
	// Manager.
	Attach    *AttachResult
	AttachErr error
}

// RegisterAfterOtherRelease deregisters the registered UE supi (switch-off)
// and registers another UE, ueCfg, through the gNB g as soon as the core
// has released the first: g's association is opened and set up before the
// deregistration, and at the first UE's UE CONTEXT RELEASE COMMAND its gNB
// sends the UE CONTEXT RELEASE COMPLETE and, gap later (zero: at once), g
// sends an INITIAL UE MESSAGE with ueCfg's REGISTRATION REQUEST. The first
// UE's association stays open until the other UE's registration has ended,
// as a gNB keeps its NG association when one of its UEs leaves, and is then
// closed. Both associations are set to send each PDU as it is written
// (gnb.UETransport.SetNoDelay), the first's for good. Of opts only WithReleaseWait applies: the
// deregistration is always a switch-off.
//
// An error wrapping ErrNGSetup means g's association or its NG Setup did
// not complete; nothing else was sent and the first UE is still registered.
// An error wrapping ErrNoReregistration means the other UE's registration
// was never sent, and both associations are closed with the first UE
// deregistered as far as the request went. Otherwise the result says how
// the other UE's registration ended: with Attach set that UE is registered
// with the Manager on g's association.
func (m *Manager) RegisterAfterOtherRelease(ctx context.Context, supi string, g GNBSpec, ueCfg UEConfig, gap time.Duration, opts ...DeregisterOption) (*OtherRegistration, error) {
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
	_, taken := m.sessions[ueCfg.Sub.SUPI]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("UE %s is not registered", supi)
	}
	if taken && ueCfg.Sub.SUPI != supi {
		return nil, fmt.Errorf("UE %s is already registered", ueCfg.Sub.SUPI)
	}
	release, err := sess.beginProcedure(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	// The first UE's UE CONTEXT RELEASE COMPLETE must leave when written,
	// not when the kernel has the previous write acknowledged, or the other
	// UE's request could reach the AMF ahead of it.
	if nd, ok := sess.conn.(interface{ SetNoDelay(bool) error }); ok {
		if err := nd.SetNoDelay(true); err != nil {
			return nil, fmt.Errorf("UE %s: %w", supi, err)
		}
	}

	dial := m.dial
	if dial == nil {
		dial = dialN2
	}
	conn, err := dial(ctx, g.BindAddr, g.AMFAddr)
	if err != nil {
		return nil, beforeAttach(fmt.Errorf("gNB %#x: SCTP dial: %w", g.Config.ID, err))
	}
	ng, err := gnb.NGSetup(ctx, conn, g.Config)
	switch {
	case err != nil:
		conn.Close()
		return nil, beforeAttach(fmt.Errorf("gNB %#x: NG Setup: %w", g.Config.ID, err))
	case !ng.Accepted:
		conn.Close()
		return nil, beforeAttach(fmt.Errorf("gNB %#x: NG Setup rejected: %s", g.Config.ID, ng.Cause))
	}
	ranID := ueCfg.RANUENGAPID
	if ranID == 0 {
		ranID = 1
	}
	ueCfg.RANUENGAPID = ranID
	lane := gnb.Dedicated(conn, g.Config, ranID)
	if err := lane.SetNoDelay(true); err != nil {
		_ = lane.Close()
		return nil, fmt.Errorf("gNB %#x: %w", g.Config.ID, err)
	}

	dres, completedAt, err := m.switchOffForRegistration(ctx, sess, linger)
	res := &OtherRegistration{Deregistration: dres, CompletedAt: completedAt, N2Source: localAddrString(conn)}
	if err != nil {
		_ = sess.conn.Close()
		_ = lane.Close()
		if dres == nil {
			return nil, err
		}
		return res, err
	}
	defer sess.conn.Close()

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
	emit := func(ev StateEvent) {
		if ev.State == StateRegistering && res.RequestedAt.IsZero() {
			res.RequestedAt = time.Now()
		}
		m.hub.publish(ev)
	}
	other, err := Attach(ctx, lane, g.Config, ueCfg, m.log, emit)
	if err != nil {
		_ = lane.Close()
		res.AttachErr = err
		return res, nil
	}
	m.adopt(other, res.N2Source, ueCfg.GNBN3Addr)
	res.Attach = other.Result
	return res, nil
}
