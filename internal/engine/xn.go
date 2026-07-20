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

	m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverStarted,
		Detail: fmt.Sprintf("Xn handover %s → %s", sess.gnbCfg.Name, target.Config.Name)},
		"type", "xn", "source_gnb", sess.gnbCfg.Name, "target_gnb", target.Config.Name)

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

	// Per-session PathSwitch with the target's new downlink tunnels.
	admitted := make([]gnb.AdmittedSession, 0)
	teid := targetHandoverTEID
	for _, id := range sessionIDs(sess.Result) {
		admitted = append(admitted, gnb.AdmittedSession{
			PDUSessionID: id, GNBTunnel: gnb.GNBTunnel{Address: target.N3Addr, TEID: teid}, QFIs: []int64{1},
		})
		teid++
	}
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

	// The core has switched the downlink to the target's N3 tunnel — the
	// user-plane cutover point of an Xn handover.
	m.publishMobility(StateEvent{SUPI: sess.SUPI, State: StatePathSwitchComplete,
		Detail: fmt.Sprintf("PathSwitchRequestAcknowledge; downlink → %s (TEID %#x)", target.N3Addr, targetHandoverTEID)},
		"type", "xn", "target_gnb", target.Config.Name, "gnb_id", target.Config.ID, "n3", target.N3Addr)

	// Move the session onto the target gNB.
	oldConn := sess.conn
	m.mu.Lock()
	sess.conn = tgtConn
	sess.gnbCfg = target.Config
	sess.ranID = targetHandoverRANID
	if newAMFID != 0 {
		sess.amfID = newAMFID
	}
	sess.gnbN3 = target.N3Addr
	sess.Result.DLTEID = targetHandoverTEID
	// Move an open data path onto the target (keeping live media lanes — a
	// running call sees a gap, then recovers); a closed one re-opens lazily.
	if err := sess.rebindDataPath(); err != nil {
		m.log.Warn("data path rebind after Xn handover failed; downlink consumers closed",
			"supi", sess.SUPI, "err", err)
	}
	m.mu.Unlock()

	oldConn.Close() // AMF releases the source after the switch
	return nil
}
