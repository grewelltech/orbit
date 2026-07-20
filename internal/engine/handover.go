package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/bgrewell/orbit/internal/coreprofile"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/meas"
	"github.com/bgrewell/orbit/internal/sctp"
)

// Mobility lifecycle states, reported on StateStream alongside the UE states.
const (
	StateHandoverStarted  = "HANDOVER_STARTED"
	StateHandoverComplete = "HANDED_OVER"
	StateHandoverFailed   = "HANDOVER_FAILED"
	// StatePathSwitchComplete marks the AMF's PathSwitchRequestAcknowledge
	// during an Xn handover — the moment the core has switched the downlink
	// to the target gNB's N3 tunnel. N2 has no RAN-visible equivalent.
	StatePathSwitchComplete = "PATH_SWITCH_COMPLETE"
)

// publishMobility stamps a mobility phase event, logs it at info (so handover
// phases are visible in serve logs with wall-clock timestamps and gNB/UE
// identifiers — the correlation timeline's raw material), and fans it out to
// StateStream subscribers.
func (m *Manager) publishMobility(ev StateEvent, attrs ...any) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	m.log.Info("mobility phase",
		append([]any{"supi", ev.SUPI, "state", ev.State, "detail", ev.Detail}, attrs...)...)
	if ev.State == StateHandoverStarted {
		// orbit_ue_handover_timestamp_seconds — the Grafana annotation
		// anchor for every handover, app session running or not.
		m.appMetrics.recordHandover(ev.SUPI, ev.Time)
	}
	m.hub.publish(ev)
}

// GNBEndpoint describes a gNB the mobility layer can hand a UE over to: its
// config (cell), the AMF N2 address, the SCTP bind address (a distinct routed
// source IP per gNB — see docs/interop/sdcore.md), and the gNB's N3 address
// for the downlink tunnel.
type GNBEndpoint struct {
	Config   gnb.Config
	AMFAddr  string
	BindAddr string
	N3Addr   string
}

// UseProfile selects the named core-compatibility profile (default
// strict-3gpp). Unknown names are rejected.
func (m *Manager) UseProfile(name string) error {
	p, ok := coreprofile.Get(name)
	if !ok {
		return fmt.Errorf("unknown core profile %q (have %v)", name, coreprofile.Names())
	}
	m.profile = p
	return nil
}

// targetHandoverRANID is the RAN-UE-NGAP-ID on the target side. Each
// handover dials its own SCTP association to the target, so a constant is
// unambiguous per association. Downlink TEIDs, by contrast, key the target
// gNB's shared Demux across ALL its UEs and come from the process-wide
// allocator (allocDLTEID) — a constant there collided as soon as two UEs
// handed over to the same target gNB.
const targetHandoverRANID int64 = 1

// Handover performs an N2 (AMF-mediated) handover of a registered UE from its
// current gNB to target, driving both NGAP associations (TS 38.413 §8.4):
// HandoverRequired → HandoverRequest → HandoverRequestAcknowledge →
// HandoverCommand → HandoverNotify. On success the session moves to the target
// gNB (association, cell, RAN-UE-NGAP-ID, N3), and the next Ping re-opens the
// data path there. Mobility events are streamed to StateStream subscribers.
//
// The control plane completes against a conformant core (and, with the sdcore
// profile, against SD-Core). User-plane continuity is bounded by the core's
// downlink path-switch behaviour — see docs/interop/sdcore.md.
func (m *Manager) Handover(ctx context.Context, supi string, target GNBEndpoint) error {
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("UE %s is not registered", supi)
	}

	m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverStarted,
		Detail: fmt.Sprintf("N2 handover %s → %s", sess.gnbCfg.Name, target.Config.Name)},
		"type", "n2", "source_gnb", sess.gnbCfg.Name, "target_gnb", target.Config.Name)

	if err := m.runHandover(ctx, sess, target); err != nil {
		m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverFailed, Detail: err.Error()},
			"type", "n2", "target_gnb", target.Config.Name)
		return err
	}

	m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverComplete,
		Detail: fmt.Sprintf("UE on gNB %s (cell %#x)", target.Config.Name, target.Config.ID)},
		"type", "n2", "target_gnb", target.Config.Name, "gnb_id", target.Config.ID)
	return nil
}

func (m *Manager) runHandover(ctx context.Context, sess *Session, target GNBEndpoint) error {
	// Bring up the target association.
	tgtConn, err := sctp.Dial(target.BindAddr, target.AMFAddr)
	if err != nil {
		return fmt.Errorf("dial target gNB: %w", err)
	}
	ng, err := gnb.NGSetup(ctx, tgtConn, target.Config)
	if err != nil || !ng.Accepted {
		tgtConn.Close()
		return fmt.Errorf("target NG Setup: %w", err)
	}

	pduIDs := sessionIDs(sess.Result)

	// 1. source → AMF: HandoverRequired.
	ho, err := gnb.BuildHandoverRequired(gnb.HandoverParams{
		Source: sess.gnbCfg, Target: target.Config,
		AMFUENGAPID: sess.amfID, RANUENGAPID: sess.ranID, PDUSessionIDs: pduIDs,
	})
	if err != nil {
		tgtConn.Close()
		return err
	}
	if err := gnb.SendPDU(sess.conn, ueStream, ho); err != nil {
		tgtConn.Close()
		return fmt.Errorf("send HandoverRequired: %w", err)
	}

	// 2. AMF → target: HandoverRequest.
	reqPDU, err := gnb.ReadPDU(ctx, tgtConn)
	if err != nil {
		tgtConn.Close()
		return fmt.Errorf("await HandoverRequest: %w", err)
	}
	hr, err := gnb.ParseHandoverRequest(reqPDU)
	if err != nil {
		tgtConn.Close()
		return fmt.Errorf("HandoverRequest not delivered to target (AMF drove preparation): %w", err)
	}

	// 3. target → AMF: HandoverRequestAcknowledge (target DL N3 tunnel).
	// DL TEIDs come from the process-wide allocator so two UEs handing over
	// to the same target gNB never collide on its shared Demux.
	admitted := make([]gnb.AdmittedSession, 0, len(hr.PDUSessionIDs))
	dlTEIDs := map[int64]uint32{}
	for _, sid := range hr.PDUSessionIDs {
		teid := allocDLTEID()
		dlTEIDs[sid] = teid
		admitted = append(admitted, gnb.AdmittedSession{
			PDUSessionID: sid,
			GNBTunnel:    gnb.GNBTunnel{Address: target.N3Addr, TEID: teid},
			QFIs:         []int64{1},
		})
	}
	ack, err := gnb.BuildHandoverRequestAcknowledge(hr.AMFUENGAPID, targetHandoverRANID, admitted, m.profile.Quirks)
	if err != nil {
		tgtConn.Close()
		return err
	}
	if err := gnb.SendPDU(tgtConn, ueStream, ack); err != nil {
		tgtConn.Close()
		return fmt.Errorf("send HandoverRequestAcknowledge: %w", err)
	}

	// 4. AMF → source: HandoverCommand.
	cmdPDU, err := gnb.ReadPDU(ctx, sess.conn)
	if err != nil {
		tgtConn.Close()
		return fmt.Errorf("await HandoverCommand: %w", err)
	}
	if _, err := gnb.ParseHandoverCommand(cmdPDU); err != nil {
		tgtConn.Close()
		return fmt.Errorf("HandoverCommand: %w", err)
	}

	// 5. target → AMF: HandoverNotify (UE arrived → AMF drives N4 path switch).
	// N2 gives the RAN no path-switch confirmation (nothing corresponding to
	// Xn's PathSwitchRequestAcknowledge), so no PATH_SWITCH_COMPLETE here.
	notify, err := gnb.BuildHandoverNotify(target.Config, hr.AMFUENGAPID, targetHandoverRANID)
	if err != nil {
		tgtConn.Close()
		return err
	}
	if err := gnb.SendPDU(tgtConn, ueStream, notify); err != nil {
		tgtConn.Close()
		return fmt.Errorf("send HandoverNotify: %w", err)
	}

	// The AMF releases the source; drain it briefly (best effort) then close.
	relCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, _ = gnb.ReadPDU(relCtx, sess.conn)
	cancel()
	oldConn := sess.conn

	// Move the session onto the target gNB.
	m.mu.Lock()
	sess.conn = tgtConn
	sess.gnbCfg = target.Config
	sess.ranID = targetHandoverRANID
	sess.amfID = hr.AMFUENGAPID
	// Per-session bookkeeping: new DL TEIDs, plus any UL F-TEID the core
	// re-anchored (carried in the HandoverRequest's per-session transfer).
	for i := range sess.Result.Sessions {
		sr := &sess.Result.Sessions[i]
		if teid, ok := dlTEIDs[int64(sr.PDUSessionID)]; ok {
			sr.DLTEID = teid
		}
		if ul, ok := hr.ULTunnels[int64(sr.PDUSessionID)]; ok {
			sr.UPFTEID, sr.UPFAddress = ul.TEID, ul.Address
		}
	}
	// Move an open data path onto the target (keeping live media lanes — a
	// running call sees a gap, then recovers); a closed one re-opens lazily.
	// gnbN3/DLTEID/UL updates happen inside retargetDataPath under the
	// data-path lock, so a concurrent dataplane() never sees a torn pair.
	firstID := firstSessionID(sess.Result)
	mv := dataPathMove{gnbN3: target.N3Addr, dlTEID: dlTEIDs[firstID]}
	if mv.dlTEID == 0 && len(admitted) > 0 {
		mv.dlTEID = admitted[0].GNBTunnel.TEID
	}
	if ul, ok := hr.ULTunnels[firstID]; ok {
		mv.upfTEID, mv.upfN3 = ul.TEID, ul.Address
	}
	if err := sess.retargetDataPath(mv); err != nil {
		m.log.Warn("data path rebind after N2 handover failed; downlink consumers closed",
			"supi", sess.SUPI, "err", err)
	}
	m.mu.Unlock()

	oldConn.Close()
	return nil
}

// managerExecutor drives real N2 handovers for one UE, mapping a trigger's
// target cell to a gNB endpoint. It implements HandoverExecutor so a
// MobilityController can turn synthesized mobility into live handovers.
type managerExecutor struct {
	mgr   *Manager
	supi  string
	cells map[int64]GNBEndpoint
}

func (e managerExecutor) Handover(ctx context.Context, t meas.Trigger) error {
	ep, ok := e.cells[t.TargetCellID]
	if !ok {
		return fmt.Errorf("no gNB endpoint for target cell %#x", t.TargetCellID)
	}
	return e.mgr.Handover(ctx, e.supi, ep)
}

// MobilityExecutor returns a HandoverExecutor that performs live N2 handovers
// for supi across the cell→gNB map, for use with a MobilityController. Cell
// IDs are the gNB IDs (one cell per gNB in the sim).
func (m *Manager) MobilityExecutor(supi string, cells map[int64]GNBEndpoint) HandoverExecutor {
	return managerExecutor{mgr: m, supi: supi, cells: cells}
}

// sessionIDs returns the PDU session IDs to hand over (multi-session aware,
// falling back to the default session).
func sessionIDs(r *AttachResult) []int64 {
	if len(r.Sessions) == 0 {
		return []int64{1}
	}
	ids := make([]int64, 0, len(r.Sessions))
	for _, s := range r.Sessions {
		ids = append(ids, int64(s.PDUSessionID))
	}
	return ids
}
