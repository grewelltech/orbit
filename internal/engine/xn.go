package engine

import (
	"context"
	"fmt"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/sctp"
)

// XnHandover performs an Xn handover of a registered UE to target. In an Xn
// handover the source and target gNBs coordinate directly over Xn (stubbed
// in-process here); the only core-facing message is the target gNB's NGAP
// PathSwitchRequest, which asks the AMF/SMF to switch the downlink to the
// target's new N3 tunnel. On Acknowledge the session moves to the target gNB.
//
// Unlike N2 (which SD-Core cannot complete on the user plane — Finding 2), the
// Xn PathSwitch transfer decodes cleanly, so the UPF downlink switch takes
// effect and a flow survives the handover. Mobility events reuse the
// HANDOVER_* states with an "Xn" detail.
func (m *Manager) XnHandover(ctx context.Context, supi string, target GNBEndpoint) error {
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("UE %s is not registered", supi)
	}

	// Serialise against another handover or a deregistration on this UE: they
	// rewrite the association and the serving cell together.
	release, err := sess.beginProcedure(ctx)
	if err != nil {
		return err
	}
	defer release()

	m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverStarted,
		Detail: fmt.Sprintf("Xn handover %s → %s", sess.ServingGNB().Name, target.Config.Name)},
		"type", "xn", "source_gnb", sess.ServingGNB().Name, "target_gnb", target.Config.Name)

	if err := m.runXnHandover(ctx, sess, target); err != nil {
		m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverFailed, Detail: "Xn: " + err.Error()},
			"type", "xn", "target_gnb", target.Config.Name)
		return err
	}

	m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverComplete,
		Detail: fmt.Sprintf("UE on gNB %s (cell %#x) via Xn", target.Config.Name, target.Config.ID)},
		"type", "xn", "target_gnb", target.Config.Name, "gnb_id", target.Config.ID)
	return nil
}

func (m *Manager) runXnHandover(ctx context.Context, sess *Session, target GNBEndpoint) error {
	tgtConn, err := sctp.Dial(target.BindAddr, target.AMFAddr)
	if err != nil {
		return fmt.Errorf("dial target gNB: %w", err)
	}
	if ng, err := gnb.NGSetup(ctx, tgtConn, target.Config); err != nil || !ng.Accepted {
		tgtConn.Close()
		return fmt.Errorf("target NG Setup: %w", err)
	}

	// Per-session PathSwitch with the target's new downlink tunnels. Each
	// session's DL TEID comes from the process-wide allocator: a constant
	// per-target TEID would collide on the target's shared Demux (and at the
	// UPF) as soon as a second UE handed over to the same gNB.
	admitted := make([]gnb.AdmittedSession, 0)
	dlTEIDs := map[int64]uint32{}
	for _, id := range sessionIDs(sess.Result) {
		teid := allocDLTEID()
		dlTEIDs[id] = teid
		admitted = append(admitted, gnb.AdmittedSession{
			PDUSessionID: id, GNBTunnel: gnb.GNBTunnel{Address: target.N3Addr, TEID: teid}, QFIs: []int64{1},
		})
	}
	firstTEID := admitted[0].GNBTunnel.TEID
	ps, err := gnb.BuildPathSwitchRequest(target.Config, sess.amfID, targetHandoverRANID, admitted)
	if err != nil {
		tgtConn.Close()
		return err
	}
	if err := gnb.SendPDU(tgtConn, ueStream, ps); err != nil {
		tgtConn.Close()
		return fmt.Errorf("send PathSwitchRequest: %w", err)
	}

	pdu, err := gnb.ReadPDU(ctx, tgtConn)
	if err != nil {
		tgtConn.Close()
		return fmt.Errorf("await PathSwitch response: %w", err)
	}
	if gnb.ClassifyPathSwitch(pdu) != gnb.PathSwitchAcknowledged {
		tgtConn.Close()
		return fmt.Errorf("path switch not acknowledged")
	}
	newAMFID, _ := gnb.ParsePathSwitchAcknowledge(pdu)
	// The ack's switched list may carry NEW UPF UL F-TEIDs (re-anchoring at
	// path switch, TS 38.413) — subsequent uplink must go there, or the UPF
	// drops it as unknown-TEID.
	switched := gnb.ParsePathSwitchAcknowledgeSwitched(pdu)

	// The core has switched the downlink to the target's N3 tunnel — the
	// user-plane cutover point of an Xn handover.
	m.publishMobility(StateEvent{SUPI: sess.SUPI, State: StatePathSwitchComplete,
		Detail: fmt.Sprintf("PathSwitchRequestAcknowledge; downlink → %s (TEID %#x)", target.N3Addr, firstTEID)},
		"type", "xn", "target_gnb", target.Config.Name, "gnb_id", target.Config.ID, "n3", target.N3Addr)

	// Move the session onto the target gNB.
	oldConn := sess.conn
	m.mu.Lock()
	sess.conn = tgtConn
	sess.setServingGNB(target.Config)
	sess.ranID = targetHandoverRANID
	if newAMFID != 0 {
		sess.amfID = newAMFID
	}
	// Record per-session endpoints (multi-session bookkeeping).
	applySwitchedSessions(sess.Result, dlTEIDs, switched)
	// Move an open data path onto the target (keeping live media lanes — a
	// running call sees a gap, then recovers); a closed one re-opens lazily.
	// The gnbN3/DLTEID/UL updates happen inside retargetDataPath under the
	// data-path lock, so a concurrent dataplane() never sees a torn pair.
	mv := dataPathMove{gnbN3: target.N3Addr, dlTEID: firstTEID}
	if s := switchedFor(switched, firstSessionID(sess.Result)); s != nil {
		mv.upfTEID, mv.upfN3 = s.UPFTEID, s.UPFAddress
	}
	if err := sess.retargetDataPath(mv); err != nil {
		m.log.Warn("data path rebind after Xn handover failed; downlink consumers closed",
			"supi", sess.SUPI, "err", err)
	}
	m.mu.Unlock()

	oldConn.Close() // AMF releases the source after the switch
	return nil
}

// firstSessionID is the PDU session mirrored into the AttachResult's
// single-session fields.
func firstSessionID(r *AttachResult) int64 {
	if len(r.Sessions) == 0 {
		return 1
	}
	return int64(r.Sessions[0].PDUSessionID)
}

// switchedFor finds the switched-list entry for one PDU session (nil = the
// core kept that session's UL tunnel).
func switchedFor(switched []gnb.SwitchedSession, id int64) *gnb.SwitchedSession {
	for i := range switched {
		if switched[i].PDUSessionID == id {
			return &switched[i]
		}
	}
	return nil
}

// applySwitchedSessions updates the per-session result entries with the
// handover's new DL TEIDs and any UL F-TEIDs the core reallocated. The
// single-session mirror fields are updated by retargetDataPath (under the
// data-path lock) — this covers the Sessions slice the status surfaces show.
func applySwitchedSessions(r *AttachResult, dlTEIDs map[int64]uint32, switched []gnb.SwitchedSession) {
	for i := range r.Sessions {
		sr := &r.Sessions[i]
		if teid, ok := dlTEIDs[int64(sr.PDUSessionID)]; ok {
			sr.DLTEID = teid
		}
		if s := switchedFor(switched, int64(sr.PDUSessionID)); s != nil {
			sr.UPFTEID = s.UPFTEID
			if s.UPFAddress != "" {
				sr.UPFAddress = s.UPFAddress
			}
		}
	}
}
