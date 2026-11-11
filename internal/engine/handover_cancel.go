package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
)

// targetReleaseGrace is how long a cancelled handover's target association
// stays open, after the wait for the acknowledge, for the AMF's release of
// the UE context it had prepared there.
const targetReleaseGrace = 500 * time.Millisecond

// ErrHandoverNotPrepared marks a CancelHandover whose handover never
// reached the HANDOVER COMMAND, so there was nothing to cancel.
var ErrHandoverNotPrepared = errors.New("handover not prepared")

// HandoverCancelResult is what a prepared and then cancelled N2 handover
// reports.
type HandoverCancelResult struct {
	// Target is the identity the target gNB presented (inheritIdentity).
	Target gnb.Config
	// Acknowledged reports whether a HANDOVER CANCEL ACKNOWLEDGE arrived on
	// the source association within the wait, AckAfter how long after the
	// HANDOVER CANCEL was sent.
	Acknowledged bool
	AckAfter     time.Duration
	// Skipped names, in order, the other downlink PDUs read on the source
	// association while waiting.
	Skipped []string
	// SourceLost is the read error that ended the wait when the source
	// association was lost after the HANDOVER CANCEL; nil otherwise.
	SourceLost error
	// TargetReleased reports whether the AMF released the UE context
	// prepared at the target gNB (a UE CONTEXT RELEASE COMMAND there,
	// answered with UE CONTEXT RELEASE COMPLETE) before its association
	// was closed.
	TargetReleased bool
}

// CancelHandover prepares an N2 handover of a registered UE to target, up
// to the HANDOVER COMMAND, and then cancels it from the source gNB with a
// HANDOVER CANCEL (TS 38.413 8.4.5), waiting up to wait for the AMF's
// HANDOVER CANCEL ACKNOWLEDGE. The target's association is closed
// afterwards and the UE stays on its source gNB, registered, with its
// session and data path as they were. No mobility event is published: the
// UE never leaves its serving gNB.
//
// An error means the handover was not prepared (ErrHandoverNotPrepared) or
// the cancel could not be sent or waited for (the caller's context ended).
// A missing acknowledge is not an error (TS 38.413 8.4.5.4: the source then
// takes the cancellation as done), and neither is a source association lost
// during the wait, which the result reports (SourceLost).
func (m *Manager) CancelHandover(ctx context.Context, supi string, target GNBEndpoint, wait time.Duration) (HandoverCancelResult, error) {
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return HandoverCancelResult{}, fmt.Errorf("UE %s is not registered", supi)
	}
	release, err := sess.beginProcedure(ctx)
	if err != nil {
		return HandoverCancelResult{}, err
	}
	defer release()
	if sess.Idle() {
		return HandoverCancelResult{}, fmt.Errorf("UE %s is CM-IDLE: a handover needs a connected UE", supi)
	}
	if target.Config, err = inheritIdentity(target.Config, sess.ServingGNB()); err != nil {
		return HandoverCancelResult{}, err
	}
	res := HandoverCancelResult{Target: target.Config}

	prep, err := m.prepareHandover(ctx, sess, target)
	if err != nil {
		return res, fmt.Errorf("%w: %w", ErrHandoverNotPrepared, err)
	}
	// The AMF releases what it prepared at the target; the target's lane
	// answers until the target is closed.
	var tgt sync.WaitGroup
	released := make(chan struct{})
	tctx, stopTarget := context.WithCancel(context.Background())
	tgt.Add(1)
	go func() {
		defer tgt.Done()
		for {
			pdu, err := gnb.ReadPDU(tctx, prep.tgt)
			if err != nil {
				return
			}
			if !gnb.IsUEContextReleaseCommand(pdu) {
				continue
			}
			if err := gnb.SendPDU(prep.tgt, ueStream, gnb.BuildUEContextReleaseComplete(prep.hr.AMFUENGAPID, targetHandoverRANID)); err == nil {
				close(released)
			}
			return
		}
	}()
	closeTarget := func() {
		select {
		case <-released:
		case <-time.After(targetReleaseGrace):
		}
		stopTarget()
		tgt.Wait()
		_ = prep.tgt.Close()
		select {
		case <-released:
			res.TargetReleased = true
		default:
		}
	}

	sent := time.Now()
	if err := gnb.SendPDU(sess.conn, ueStream, gnb.BuildHandoverCancel(sess.amfID, sess.ranID)); err != nil {
		closeTarget()
		return res, fmt.Errorf("send HandoverCancel: %w", err)
	}
	actx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for !res.Acknowledged {
		pdu, err := sess.readPDU(actx)
		if err != nil {
			// What ended the read is settled before the target's grace,
			// which can outlast the wait.
			ctxErr, waited := ctx.Err(), actx.Err() != nil
			closeTarget()
			switch {
			case ctxErr != nil:
				return res, fmt.Errorf("await HandoverCancelAcknowledge: %w", ctxErr)
			case waited:
				return res, nil // no acknowledge within the wait
			}
			res.SourceLost = err
			return res, nil
		}
		if gnb.IsHandoverCancelAcknowledge(pdu) {
			res.Acknowledged, res.AckAfter = true, time.Since(sent)
			continue
		}
		if sess.absorbDownlinkNAS(ctx, m.log, pdu) {
			continue
		}
		code, _ := procedureCode(pdu)
		res.Skipped = append(res.Skipped, procedureName(code))
	}
	closeTarget()
	m.log.Info("N2 handover prepared and cancelled", "supi", supi, "target_gnb", target.Config.Name,
		"acknowledged", res.Acknowledged, "target_released", res.TargetReleased)
	return res, nil
}
