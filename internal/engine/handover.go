package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/free5gc/ngap/ngapType"

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
	// Retain the phase on the session so it survives the event. StateStream is
	// lossy (hub.publish drops on a full subscriber buffer) and has no replay,
	// so a polling client would otherwise have no way to observe mobility at
	// all. This is the single choke point for both N2 and Xn phases.
	m.mu.Lock()
	sess := m.sessions[ev.SUPI]
	m.mu.Unlock()
	if sess != nil {
		sess.setMobility(ev.State, ev.Time)
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

// HandoverResult is what a completed handover reports: how long it took and
// the target identity actually presented to the core, with the fields the
// caller left unset inherited from the serving gNB (inheritIdentity).
type HandoverResult struct {
	Elapsed time.Duration
	Target  gnb.Config
	// PathSwitchAck is the PDU Session Resource Switched List of the PATH
	// SWITCH REQUEST ACKNOWLEDGE that completed an Xn handover, as received;
	// nil for an N2 handover.
	PathSwitchAck *gnb.PathSwitchAck
	// PathSwitchCapabilities is the UE Security Capabilities IE of that
	// Acknowledge, as received; nil when it carried none, and for an N2
	// handover.
	PathSwitchCapabilities *ngapType.UESecurityCapabilities
	// Command is the PDU Session Resource Handover List of the HANDOVER
	// COMMAND of an N2 handover, as received; nil for an Xn handover. It is
	// also set on a handover that failed after the command arrived.
	Command *gnb.HandoverCommandInfo
}

// ErrInvalidTarget marks a handover target whose network identity, after
// inheriting from the serving gNB, is still invalid: a caller's input error
// rather than a procedure failure.
var ErrInvalidTarget = errors.New("invalid target gNB")

// inheritIdentity fills the target's unset network identity from the UE's
// serving gNB — the empty PLMN pair, a zero TAC, an empty slice list — and
// validates the result. A field the caller set explicitly is kept. This is
// what lets a same-network handover omit every identity flag, and it runs
// under the session's procedure lock so the serving cell it copies is the
// one the UE is actually on (a previous handover's target included).
func inheritIdentity(target gnb.Config, serving gnb.Config) (gnb.Config, error) {
	if target.MCC == "" && target.MNC == "" {
		target.MCC, target.MNC = serving.MCC, serving.MNC
	}
	if target.TAC == 0 {
		target.TAC = serving.TAC
	}
	if len(target.Slices) == 0 {
		target.Slices = append([]gnb.SNSSAI(nil), serving.Slices...)
	}
	if err := target.ValidateIdentity(); err != nil {
		return target, fmt.Errorf("%w: %v", ErrInvalidTarget, err)
	}
	return target, nil
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
// The elapsed time is returned so a single-UE handover is a measurement rather
// than a pass/fail: it is the same procedure the fleet's CP-latency series
// records, and an operator comparing N2 against Xn needs the number.
func (m *Manager) Handover(ctx context.Context, supi string, target GNBEndpoint) (HandoverResult, error) {
	start := time.Now()
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return HandoverResult{}, fmt.Errorf("UE %s is not registered", supi)
	}

	// Serialise against another handover or a deregistration on this UE: they
	// rewrite the association and the serving cell together.
	release, err := sess.beginProcedure(ctx)
	if err != nil {
		return HandoverResult{}, err
	}
	defer release()
	if sess.Idle() {
		return HandoverResult{}, fmt.Errorf("UE %s is CM-IDLE: a handover needs a connected UE", supi)
	}

	if target.Config, err = inheritIdentity(target.Config, sess.ServingGNB()); err != nil {
		return HandoverResult{}, err
	}

	m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverStarted,
		Detail: fmt.Sprintf("N2 handover %s → %s", sess.ServingGNB().Name, target.Config.Name)},
		"type", "n2", "source_gnb", sess.ServingGNB().Name, "target_gnb", target.Config.Name)

	cmd, err := m.runHandover(ctx, sess, target)
	if err != nil {
		m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverFailed, Detail: err.Error()},
			"type", "n2", "target_gnb", target.Config.Name)
		return HandoverResult{Command: cmd}, err
	}

	elapsed := time.Since(start)
	m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverComplete,
		Detail: fmt.Sprintf("UE on gNB %s (cell %#x) in %s", target.Config.Name, target.Config.ID,
			elapsed.Round(time.Millisecond))},
		"type", "n2", "target_gnb", target.Config.Name, "gnb_id", target.Config.ID,
		"elapsed_ms", elapsed.Milliseconds())
	return HandoverResult{Elapsed: elapsed, Target: target.Config, Command: cmd}, nil
}

// preparedHandover is an N2 handover whose preparation the AMF has
// answered with a HANDOVER COMMAND: the target association, what the target
// admitted, and the command's PDU Session Resource Handover List.
type preparedHandover struct {
	tgtConn  n2Conn
	tgt      *gnb.UETransport // the UE's lane on tgtConn
	hr       *gnb.HandoverRequestInfo
	admitted []gnb.AdmittedSession
	dlTEIDs  map[int64]uint32
	command  gnb.HandoverCommandInfo
}

// runHandover prepares and completes an N2 handover. It returns the
// HANDOVER COMMAND's PDU Session Resource Handover List once the command
// has arrived, also when a later step fails.
func (m *Manager) runHandover(ctx context.Context, sess *Session, target GNBEndpoint) (*gnb.HandoverCommandInfo, error) {
	prep, err := m.prepareHandover(ctx, sess, target)
	if err != nil {
		return nil, err
	}
	return &prep.command, m.completeHandover(ctx, sess, target, prep)
}

// prepareHandover runs Handover Preparation and Handover Resource
// Allocation (TS 38.413 8.4.1, 8.4.2) up to the HANDOVER COMMAND. On an
// error the target association is closed and the UE stays on its source.
func (m *Manager) prepareHandover(ctx context.Context, sess *Session, target GNBEndpoint) (*preparedHandover, error) {
	// Bring up the target association.
	tgtConn, err := sctp.Dial(ctx, target.BindAddr, target.AMFAddr)
	if err != nil {
		return nil, fmt.Errorf("dial target gNB: %w", err)
	}
	prep, err := m.prepareHandoverOn(ctx, sess, target, tgtConn)
	if err != nil {
		_ = tgtConn.Close()
		return nil, err
	}
	return prep, nil
}

// prepareHandoverOn is prepareHandover on a dialled target association,
// which the caller closes on an error.
func (m *Manager) prepareHandoverOn(ctx context.Context, sess *Session, target GNBEndpoint, tgtConn n2Conn) (*preparedHandover, error) {
	ng, err := gnb.NGSetup(ctx, tgtConn, target.Config)
	if err != nil || !ng.Accepted {
		return nil, fmt.Errorf("target NG Setup: %w", err)
	}
	// The target association's reads go through one reader too
	// (gnb.Dedicated); its lane is the UE's on the target.
	tgt := gnb.Dedicated(tgtConn, target.Config, targetHandoverRANID)

	pduIDs := sessionIDs(sess.Result)

	// 1. source → AMF: HandoverRequired.
	ho, err := gnb.BuildHandoverRequired(gnb.HandoverParams{
		Source: sess.ServingGNB(), Target: target.Config,
		AMFUENGAPID: sess.amfID, RANUENGAPID: sess.ranID, PDUSessionIDs: pduIDs,
	})
	if err != nil {
		return nil, err
	}
	if err := gnb.SendPDU(sess.conn, ueStream, ho); err != nil {
		return nil, fmt.Errorf("send HandoverRequired: %w", err)
	}

	// 2. AMF → target: HandoverRequest.
	reqPDU, err := gnb.ReadPDU(ctx, tgt)
	if err != nil {
		return nil, fmt.Errorf("await HandoverRequest: %w", err)
	}
	hr, err := gnb.ParseHandoverRequest(reqPDU)
	if err != nil {
		return nil, fmt.Errorf("HandoverRequest not delivered to target (AMF drove preparation): %w", err)
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
		return nil, err
	}
	if err := gnb.SendPDU(tgt, ueStream, ack); err != nil {
		return nil, fmt.Errorf("send HandoverRequestAcknowledge: %w", err)
	}

	// 4. AMF → source: HandoverCommand.
	cmdPDU, err := sess.readPDU(ctx)
	for err == nil && sess.absorbDownlinkNAS(ctx, m.log, cmdPDU) {
		cmdPDU, err = sess.readPDU(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("await HandoverCommand: %w", err)
	}
	if _, err := gnb.ParseHandoverCommand(cmdPDU); err != nil {
		return nil, fmt.Errorf("HandoverCommand: %w", err)
	}
	return &preparedHandover{tgtConn: tgtConn, tgt: tgt, hr: hr, admitted: admitted, dlTEIDs: dlTEIDs,
		command: gnb.ParseHandoverCommandItems(cmdPDU)}, nil
}

// completeHandover sends the HANDOVER NOTIFY of a prepared handover and
// moves the session onto the target gNB.
func (m *Manager) completeHandover(ctx context.Context, sess *Session, target GNBEndpoint, prep *preparedHandover) error {
	tgtConn, tgt, hr, admitted, dlTEIDs := prep.tgtConn, prep.tgt, prep.hr, prep.admitted, prep.dlTEIDs
	// 5. target → AMF: HandoverNotify (UE arrived → AMF drives N4 path switch).
	// N2 gives the RAN no path-switch confirmation (nothing corresponding to
	// Xn's PathSwitchRequestAcknowledge), so no PATH_SWITCH_COMPLETE here.
	notify, err := gnb.BuildHandoverNotify(target.Config, hr.AMFUENGAPID, targetHandoverRANID)
	if err != nil {
		_ = tgt.Close()
		return err
	}
	if err := gnb.SendPDU(tgt, ueStream, notify); err != nil {
		_ = tgt.Close()
		return fmt.Errorf("send HandoverNotify: %w", err)
	}

	// The AMF releases the source; drain it briefly (best effort) then close.
	relCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, _ = sess.readPDU(relCtx)
	cancel()
	oldConn := sess.conn

	// Move the session onto the target gNB.
	m.mu.Lock()
	sess.conn = tgt
	sess.setServingGNB(target.Config)
	sess.setN2Source(localAddrString(tgtConn))
	sess.ranID = targetHandoverRANID
	sess.amfID = hr.AMFUENGAPID
	if hr.UESecurityCapabilities != nil {
		sess.ueSecCaps = hr.UESecurityCapabilities
	}
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
	// The executor's contract is pass/fail; the duration is recorded by the
	// caller that wanted it.
	_, err := e.mgr.Handover(ctx, e.supi, ep)
	return err
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
