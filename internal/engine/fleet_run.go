package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/loomgtp"
	"github.com/bgrewell/orbit/internal/meas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// FleetGNB is one generated gNB in a fleet run: its NGAP config, the local
// source address to bind (distinct per gNB, for handover), the N3 address for
// the data path (usually the same IP), and its grid position (metres) for the
// mobility model.
type FleetGNB struct {
	Config   gnb.Config
	BindAddr string
	N3Addr   string
	X, Y     float64
}

// FleetRunSpec parameterises a fleet run (ADR-0004). UEs spread round-robin
// across the gNBs (even distribution): UE i is served by GNBs[i % len(GNBs)].
type FleetRunSpec struct {
	AMFAddr     string
	GNBs        []FleetGNB
	BaseIMSI    string
	Count       int
	MCC, MNC    string
	Ki, OPc     []byte
	RateRPS     float64 // offered attach rate (attaches/sec; 0 = concurrency-bound)
	Concurrency int
	PDUSession  *ue.PDUSessionParams // per-UE session using the serving gNB's N3
}

// FleetBehaviors are the continuous behaviours run on the attached fleet for
// Duration (ADR-0004). Mobility moves a subset of UEs along tracks across the
// gNB grid; the meas RSRP/A3 model turns each UE's position into handover
// triggers, executed on schedule. Traffic runs one loom flow per non-mobile UE
// over its gNB's shared N3 socket.
type FleetBehaviors struct {
	Duration      time.Duration
	MobileUEs     int
	Traffic       bool
	TrafficRate   string
	TrafficTarget string
	// Latency, when its Target is set, samples user-plane RTT over the UEs'
	// own N3 data paths for the duration of the run.
	Latency FleetLatencyProbe
	// Apps are the application-traffic cohorts (design §8, fleet_app.go):
	// carved subsets of the non-mobile fleet each running a real loom app
	// engine (voip/http/video) against an N6 loomd, with the far-end
	// control plumbing shared per loomd and results reported as per-cohort
	// distributions.
	Apps []FleetAppCohort
	// AppMetricsReg, when set, registers the per-cohort distribution gauges
	// (orbit_fleet_app_*, labeled by cohort name — bounded cardinality) and
	// keeps them live during the run; nil disables them.
	AppMetricsReg prometheus.Registerer
}

// FleetReport summarises a fleet run.
type FleetReport struct {
	Attached, AttachFailed int
	AttachElapsed          time.Duration
	Handovers, HandoverErr int
	TrafficFlows           int
	TrafficBytes           uint64
	Deregistered           int
	// AppCohorts are the app-traffic cohort outcomes, index-aligned with
	// FleetBehaviors.Apps.
	AppCohorts []FleetAppCohortReport
}

// fleetUE is a persistent attached UE and the gNB index currently serving it.
type fleetUE struct {
	sess   *Session
	gnbIdx int
}

// RunFleet attaches the fleet (persistent, muxed over one association per gNB),
// runs the behaviours concurrently for the duration, then deregisters — the
// direct-drive population run. Attach numbers are integration-capacity (bounded
// by the core under test).
// FleetDeps are the seams a fleet run borrows from its host rather than
// creating for itself. Every field is optional; the zero value runs the fleet
// standalone, which is what the local `orbit run <fleet>` path wants.
type FleetDeps struct {
	// Stats and Events receive live reporting; nil disables it.
	Stats  *FleetLiveStats
	Events *RunEvents
	// Manager, when set, lends its N3 socket pool.
	//
	// This matters more than it looks. A gNB's N3 address is ONE UDP bind for
	// the whole process, so a fleet run with its own pool cannot open a data
	// path on an address an ad-hoc `orbit ue` session already holds — and the
	// failure is per-UE and quiet, so the run reports a healthy population
	// carrying zero traffic. Sharing the pool makes the second acquirer reuse
	// the socket, which is what the refcounting was always for.
	Manager *Manager
}

// n3 returns the pool this run should use: the host Manager's, so ad-hoc UE
// sessions and fleet UEs share one socket per gNB address, or a private one
// when running standalone.
func (d FleetDeps) n3() *n3Pool {
	if d.Manager != nil && d.Manager.n3 != nil {
		return d.Manager.n3
	}
	return newN3Pool()
}

// deps may be the zero value, which disables live reporting and runs the
// fleet on its own socket pool.
func RunFleet(ctx context.Context, log *slog.Logger, spec FleetRunSpec, beh FleetBehaviors,
	deps FleetDeps) (FleetReport, error) {
	live, ev := deps.Stats, deps.Events
	var rep FleetReport
	if len(spec.GNBs) == 0 {
		return rep, fmt.Errorf("fleet run needs at least one gNB")
	}
	// App-cohort specs that cannot run (unknown app, missing peer, a voip
	// cohort exceeding its far-end port range) are refused BEFORE any gNB
	// dials or attaches — an actionable error now beats per-UE failures
	// minutes into a soak.
	if err := validateFleetAppCohorts(beh.Apps); err != nil {
		return rep, err
	}
	specs := make([]GNBSpec, len(spec.GNBs))
	for i, g := range spec.GNBs {
		specs[i] = GNBSpec{AMFAddr: spec.AMFAddr, Config: g.Config, BindAddr: g.BindAddr}
	}
	f, err := NewFleet(ctx, specs, log)
	if err != nil {
		return rep, err
	}
	defer f.Close()

	// One shared N3 tunnel pool: sessions' lazy data paths (app cohorts) and
	// the synthetic traffic flows all acquire the SAME per-gNB socket through
	// it — one 2152 bind per gNB N3 address, never two behaviours colliding on
	// the port. Borrowed from the host Manager when there is one, so ad-hoc
	// `orbit ue` sessions count against the same refcount rather than holding
	// a bind this run then cannot get.
	pool := deps.n3()

	// Attach phase — persistent, keeping a handle per UE.
	ev.Milestone(EventKindAttach, fmt.Sprintf("attach phase started: %d UEs across %d gNBs%s",
		spec.Count, len(spec.GNBs), attachRateNote(spec.RateRPS)))
	start := time.Now()
	ues := make([]*fleetUE, 0, spec.Count)
	var amu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, orDefaultInt(spec.Concurrency, 64))
	var lim *rate.Limiter
	if spec.RateRPS > 0 {
		lim = rate.NewLimiter(rate.Limit(spec.RateRPS), 1)
	}
	for i := 0; i < spec.Count; i++ {
		if err := ctx.Err(); err != nil {
			break
		}
		if lim != nil {
			_ = lim.Wait(ctx)
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fu, err := attachFleetUE(ctx, f, spec, pool, i, ev, live)
			amu.Lock()
			defer amu.Unlock()
			if err != nil {
				rep.AttachFailed++
				live.AttachFailed()
				// Per UE, at every verbosity: attach failures are rare and are
				// the reason to be watching. supi is derived rather than taken
				// from the (nil) handle.
				ev.Failure(EventKindAttach, fleetSUPI(spec, i), err.Error())
				return
			}
			ues = append(ues, fu)
			live.AttachOK(gnbAttributionLabel(f.gnbConfigFor(fu.gnbIdx)), fu.sess.SUPI, fu.sess)
		}(i)
	}
	wg.Wait()
	rep.Attached = len(ues)
	rep.AttachElapsed = time.Since(start)
	// The aggregated stand-in for the per-UE successes normal verbosity does
	// not emit: one line that still says whether the phase went well.
	attachMsg := fmt.Sprintf("attach complete: %d/%d in %s",
		rep.Attached, rep.Attached+rep.AttachFailed, rep.AttachElapsed.Round(time.Millisecond))
	if rep.AttachFailed > 0 {
		ev.send("warn", EventKindAttach, "", attachMsg)
	} else {
		ev.Milestone(EventKindAttach, attachMsg)
	}
	if len(ues) == 0 {
		return rep, fmt.Errorf("no UEs attached")
	}

	// Behaviour phase.
	if beh.Duration > 0 {
		runFleetBehaviors(ctx, f, spec, pool, ues, beh, &rep, log, live, ev)
	}

	// Deregister. App-cohort members opened lazy data paths on the shared
	// pool — close them first so the per-gNB sockets (and any netstack
	// bridge) release with the last UE.
	for _, fu := range ues {
		// Retire the UE's counters BEFORE closing its data path: a closed
		// session reports zeros, which would empty the run's totals at exactly
		// the moment the final report is taken.
		live.Detached(gnbAttributionLabel(f.gnbConfigFor(fu.gnbIdx)), fu.sess)
		fu.sess.closeDataPath()
		if err := fu.sess.deregister(context.Background()); err == nil {
			rep.Deregistered++
		}
	}
	ev.Milestone(EventKindAttach, fmt.Sprintf("deregistered %d/%d UEs", rep.Deregistered, len(ues)))
	return rep, nil
}

// attachRateNote renders the offered attach rate for a milestone, or "" when
// the run is concurrency-bound.
func attachRateNote(rps float64) string {
	if rps <= 0 {
		return " (concurrency-bound)"
	}
	return fmt.Sprintf(" at %.3g/s", rps)
}

// fleetSUPI recovers UE i's SUPI for an event about an attach that failed
// before a handle existed. A malformed base yields the index alone rather than
// an error: this is a label on an event, never a control decision.
func fleetSUPI(spec FleetRunSpec, i int) string {
	supi, err := incIMSI(spec.BaseIMSI, i)
	if err != nil {
		return fmt.Sprintf("ue-%d", i)
	}
	return supi
}

// attachFleetUE attaches one UE muxed on its serving gNB's association and
// returns a persistent handle. The session's data path (opened lazily only
// when an app cohort uses the UE) rides the run's shared per-gNB pool.
func attachFleetUE(ctx context.Context, f *Fleet, spec FleetRunSpec, pool *n3Pool, i int,
	ev *RunEvents, live *FleetLiveStats) (*fleetUE, error) {
	gi := i % len(f.sessions)
	supi, err := incIMSI(spec.BaseIMSI, i)
	if err != nil {
		return nil, err
	}
	id, err := ue.ParseIdentity(supi, spec.MCC, spec.MNC, "0")
	if err != nil {
		return nil, err
	}
	cfg := UEConfig{Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: spec.Ki, OPc: spec.OPc}}
	uet, ranID := f.sessions[gi].NewUE()
	cfg.RANUENGAPID = ranID
	if spec.PDUSession != nil {
		p := *spec.PDUSession
		cfg.PDUSession = &p
		cfg.GNBN3Addr = spec.GNBs[gi].N3Addr
	}
	// The lifecycle transitions the Manager publishes to its hub reach the run
	// stream here instead (there is no Manager behind a fleet run). The policy
	// decides what survives: at normal verbosity these are suppressed in favour
	// of the phase milestones above.
	//
	// The same emitter splits registration from the whole attach, mirroring the
	// load driver: REGISTERED marks the control-plane half, and whatever time
	// remains is the PDU session being established.
	start := time.Now()
	var regDur time.Duration
	sink := ev.stateEventSink()
	emit := func(sev StateEvent) {
		if sev.State == StateRegistered && regDur == 0 {
			regDur = time.Since(start)
		}
		if sink != nil {
			sink(sev)
		}
	}
	sess, err := Attach(ctx, uet, f.gnbConfigFor(gi), cfg, f.log, emit)
	if err != nil {
		return nil, err
	}
	if !sess.Result.Registered {
		return nil, fmt.Errorf("UE %s not registered", supi)
	}
	sess.gnbN3 = spec.GNBs[gi].N3Addr
	sess.n3 = pool
	live.RecordProcedure("attach", time.Since(start))
	if regDur > 0 {
		live.RecordProcedure("registration", regDur)
	}
	if sess.Result.SessionActive {
		live.RecordProcedure("pdu_session", time.Since(start))
	}
	return &fleetUE{sess: sess, gnbIdx: gi}, nil
}

// runFleetBehaviors runs mobility + traffic + app cohorts concurrently until
// the duration elapses (or the context is cancelled).
func runFleetBehaviors(ctx context.Context, f *Fleet, spec FleetRunSpec, pool *n3Pool, ues []*fleetUE,
	beh FleetBehaviors, rep *FleetReport, log *slog.Logger, live *FleetLiveStats, ev *RunEvents) {
	dctx, cancel := context.WithTimeout(ctx, beh.Duration)
	defer cancel()
	var wg sync.WaitGroup

	// App cohorts take their members from the TAIL of the fleet; mobility
	// owns the head and the synthetic traffic below skips both populations.
	appMembers, appTotal := carveAppCohorts(ues, min(beh.MobileUEs, len(ues)), beh.Apps, log)
	trafficUEs := ues[:len(ues)-appTotal]

	// Traffic: one shared N3 socket per gNB N3 ADDRESS (several FleetGNBs may
	// share one N3 address — the single-node walkthrough shape — and binding
	// it twice would EADDRINUSE); every non-mobile UE on it runs a loom flow
	// concurrently (the shared-N3 demux — a whole population carries traffic
	// without colliding on port 2152). The sockets come from the run's shared
	// pool, so app-cohort data paths on the same gNB reuse them instead of
	// fighting over the bind. A gNB whose socket cannot bind is logged and
	// skipped — its flows are absent from TrafficFlows, never silently
	// counted as run.
	var trafficBytes atomic.Uint64
	if beh.Traffic && spec.PDUSession != nil && len(trafficUEs) > 0 {
		ev.Traffic(fmt.Sprintf("synthetic traffic started: %d UEs → %s%s",
			len(trafficUEs), beh.TrafficTarget, trafficRateNote(beh.TrafficRate)))
		upfN3 := fleetUPFN3(ues[0].sess.Result.UPFAddress)
		tuns := map[string]*sharedN3{}
		for gi := range f.sessions {
			flowUEs := uesOnGNB(trafficUEs, gi, beh.MobileUEs)
			if len(flowUEs) == 0 {
				continue
			}
			localN3 := net.JoinHostPort(spec.GNBs[gi].N3Addr, "2152")
			ref, ok := tuns[localN3]
			if !ok {
				var err error
				ref, err = pool.acquire(localN3, upfN3)
				if err != nil {
					log.Warn("fleet traffic: cannot bind gNB N3 socket; this gNB's flows are skipped",
						"gnb_n3", localN3, "ues", len(flowUEs), "err", err)
					continue
				}
				tuns[localN3] = ref
				defer pool.release(ref)
			}
			st := ref.st
			for _, fu := range flowUEs {
				r := fu.sess.Result
				// Uplink goes to the UE's OWN UPF N3 endpoint (sessions may
				// anchor on different UPFs), not UE 0's.
				flow, err := st.UEFlowTo(r.UPFTEID, r.QFI, fleetUPFN3(r.UPFAddress))
				if err != nil {
					log.Warn("fleet traffic: bad UPF N3 endpoint; UE flow skipped",
						"supi", fu.sess.SUPI, "upf", r.UPFAddress, "err", err)
					continue
				}
				wg.Add(1)
				rep.TrafficFlows++
				// The synthetic traffic path writes through this UEFlow, NOT
				// through the session's lazily-opened UETunnel — a UE carrying
				// only synthetic traffic never opens one. Register the flow so
				// its bytes reach the live totals.
				live.TrafficFlowStarted()
				live.AddFlow(FleetFlow{
					SUPI: fu.sess.SUPI, App: "udp", Peer: beh.TrafficTarget,
					GNB: gnbAttributionLabel(f.gnbConfigFor(fu.gnbIdx)),
				}, flow)
				go func(fu *fleetUE, flow *datapath.UEFlow) {
					defer wg.Done()
					r := fu.sess.Result
					res, err := loomgtp.RunFlow(dctx, loomgtp.Config{
						Uplink: flow, UEIP: net.ParseIP(r.PDUAddress),
						Target: beh.TrafficTarget, Rate: beh.TrafficRate, Duration: beh.Duration,
					})
					if err == nil {
						trafficBytes.Add(res.Bytes)
					}
				}(fu, flow)
			}
		}
	}

	// App cohorts (fleet_app.go): real voip/http/video engines on the carved
	// members, far-end plumbing shared per loomd (one control connection +
	// one TimeSync loop), per-cohort distributions into the report. RTCP's
	// randomized reporting interval (RFC 3550 §6.3/A.7, implemented by loom)
	// keeps thousands of concurrent calls from synchronising their reports.
	var appReports []FleetAppCohortReport
	if appTotal > 0 {
		live.AppSessions(appTotal)
		// One event per cohort, not per member: a 500-UE cohort costs one line.
		for i, c := range beh.Apps {
			if i < len(appMembers) {
				ev.Traffic(fmt.Sprintf("cohort %s (%s) started: %d UEs → %s",
					cohortName(c, i), c.App, len(appMembers[i]), c.Peer))
			}
		}
		agents := newFleetAgentPool(appTuning{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			appReports = runFleetApps(dctx, log, agents, beh.Apps, appMembers, beh.Duration, beh.AppMetricsReg, live)
		}()
	}

	// User-plane latency: ICMP echoes over sampled UEs' own data paths, so the
	// dashboard's UP-latency panel reports the tunnel's round trip rather than
	// the management network's. Sampled, not swept — see FleetLatencyProbe.
	if beh.Latency.Target != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runFleetLatencyProbe(dctx, ues, beh.Latency, live, log)
		}()
	}

	// Mobility: each mobile UE moves along a track across the gNB grid; the meas
	// RSRP/A3 model turns its position into handover triggers (fire when a
	// neighbour cell becomes the strongest by the a3-offset for the time-to-
	// trigger), which we execute on schedule — geometry-driven, not a timer.
	if beh.MobileUEs > 0 && len(f.sessions) > 1 {
		cells := make([]meas.Cell, len(spec.GNBs))
		for i, g := range spec.GNBs {
			cells[i] = meas.Cell{ID: int64(i), X: g.X, Y: g.Y}
		}
		base := time.Now()
		var hmu sync.Mutex
		mobile := ues[:min(beh.MobileUEs, len(ues))]
		for _, fu := range mobile {
			triggers := fleetUETriggers(cells, fu.gnbIdx, beh.Duration, base)
			wg.Add(1)
			go func(fu *fleetUE, triggers []meas.Trigger) {
				defer wg.Done()
				for _, tr := range triggers {
					select {
					case <-dctx.Done():
						return
					case <-time.After(time.Until(tr.At)):
					}
					from := fu.gnbIdx
					hoStart := time.Now()
					err := fleetHandover(dctx, f, spec, fu, int(tr.TargetCellID))
					if err == nil {
						// Labelled by the procedure actually run, not by what
						// the scenario asked for: fleet mobility is Xn today.
						live.RecordProcedure(ProcedureHandoverXn, time.Since(hoStart))
					}
					hmu.Lock()
					if err != nil {
						rep.HandoverErr++
					} else {
						rep.Handovers++
						// fleetHandover updates fu.gnbIdx on success, so the
						// population moves with the UE instead of the spread
						// freezing at its attach-time shape.
						live.MovedGNB(gnbAttributionLabel(f.gnbConfigFor(from)),
							gnbAttributionLabel(f.gnbConfigFor(fu.gnbIdx)))
					}
					live.Handover(err)
					hmu.Unlock()
					if err != nil {
						ev.Mobility(fu.sess.SUPI, StateHandoverFailed, err.Error())
					} else {
						ev.Mobility(fu.sess.SUPI, StateHandoverComplete,
							fmt.Sprintf("%s → %s", gnbAttributionLabel(f.gnbConfigFor(from)),
								gnbAttributionLabel(f.gnbConfigFor(fu.gnbIdx))))
					}
				}
			}(fu, triggers)
		}
	}

	wg.Wait()
	rep.TrafficBytes = trafficBytes.Load()
	rep.AppCohorts = appReports
	if beh.Traffic && spec.PDUSession != nil && len(trafficUEs) > 0 {
		ev.Traffic(fmt.Sprintf("synthetic traffic stopped: %d flows, %s",
			rep.TrafficFlows, byteSize(rep.TrafficBytes)))
	}
	for _, r := range appReports {
		msg := fmt.Sprintf("cohort %s (%s) stopped: %d UEs", r.Name, r.App, r.UEs)
		if r.Failed > 0 {
			msg += fmt.Sprintf(", %d failed", r.Failed)
		}
		if r.Err != "" {
			ev.Failure(EventKindTraffic, "", fmt.Sprintf("cohort %s (%s): %s", r.Name, r.App, r.Err))
			continue
		}
		ev.Traffic(msg)
	}
}

// trafficRateNote renders a synthetic flow's rate, or "" when unlimited.
func trafficRateNote(rate string) string {
	if rate == "" {
		return ""
	}
	return " at " + rate
}

// byteSize renders a byte total in the unit that keeps an event line readable.
func byteSize(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// fleetHandover moves fu to the target gNB via an Xn PathSwitch on the target's
// muxed association, updating the UE handle on success.
func fleetHandover(ctx context.Context, f *Fleet, spec FleetRunSpec, fu *fleetUE, target int) error {
	// Same exclusion as the Manager's handover paths. Fleet sessions live
	// outside Manager.sessions today, so nothing else drives them concurrently
	// — but this keeps the invariant true for when ADR-0005 moves fleet runs
	// onto the server's Manager.
	release, err := fu.sess.beginProcedure(ctx)
	if err != nil {
		return err
	}
	defer release()

	uetT, ranIDT := f.sessions[target].NewUE()
	// The target handle registers a demux inbox on the target gNB session.
	// Release it unless the handover commits to it, or every failed handover
	// leaks an inbox for the life of the run.
	committed := false
	defer func() {
		if !committed {
			_ = uetT.Close()
		}
	}()
	// DL TEIDs come from the same process-wide allocator as attach — a
	// derived scheme (0x1000+ranID) could collide with a TEID a fleet attach
	// already handed out on the target gNB.
	newTEID := allocDLTEID()
	ps, err := gnb.BuildPathSwitchRequest(f.gnbConfigFor(target), fu.sess.amfID, ranIDT,
		[]gnb.AdmittedSession{{PDUSessionID: 1, GNBTunnel: gnb.GNBTunnel{Address: spec.GNBs[target].N3Addr, TEID: newTEID}, QFIs: []int64{1}}})
	if err != nil {
		return err
	}
	if err := gnb.SendPDU(uetT, ueStream, ps); err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := gnb.ReadPDU(rctx, uetT)
	if err != nil {
		return err
	}
	if gnb.ClassifyPathSwitch(resp) != gnb.PathSwitchAcknowledged {
		return fmt.Errorf("path switch not acknowledged")
	}
	newAMFID, _ := gnb.ParsePathSwitchAcknowledge(resp)
	switched := gnb.ParsePathSwitchAcknowledgeSwitched(resp)
	// Commit to the target handle and release the source one: the old handle's
	// demux inbox on the source gNB session is no longer used, and dropping it
	// without Close would leak it.
	oldConn := fu.sess.conn
	fu.sess.conn = uetT
	committed = true
	if oldConn != nil {
		_ = oldConn.Close()
	}
	if newAMFID != 0 {
		fu.sess.amfID = newAMFID
	}
	// The serving cell moves with the UE. Without this the session keeps
	// reporting the cell it attached to, so ServingGNB would be wrong for
	// every fleet UE that ever hands over.
	//
	// The mobility phase stays unset here: a fleet run has no event emitter
	// (Attach is called with emit=nil), so its UEs publish nothing. That is
	// the wider gap ADR-0005 addresses, not something to paper over locally.
	fu.sess.setServingGNB(f.gnbConfigFor(target))
	// Data-path identity moves under the session's data-path lock (fleet
	// sessions have no open path today — mobile UEs carry no traffic — but
	// the locked path keeps that invariant local, not load-bearing).
	mv := dataPathMove{gnbN3: spec.GNBs[target].N3Addr, dlTEID: newTEID}
	if s := switchedFor(switched, 1); s != nil {
		mv.upfTEID, mv.upfN3 = s.UPFTEID, s.UPFAddress
	}
	if err := fu.sess.retargetDataPath(mv); err != nil {
		return err
	}
	fu.gnbIdx = target
	return nil
}

// fleetUPFN3 normalises a UPF address to host:port (bare IPs get GTP-U 2152).
func fleetUPFN3(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, "2152")
}

// fleetUETriggers computes a mobile UE's handover triggers: it moves from its
// serving cell across the grid to the roughly-opposite cell over the run, and
// the meas RSRP/A3 model returns when (and to which cell) it should hand over.
func fleetUETriggers(cells []meas.Cell, servingIdx int, duration time.Duration, base time.Time) []meas.Trigger {
	start := cells[servingIdx]
	dest := cells[(servingIdx+len(cells)/2)%len(cells)]
	return meas.Scenario{
		Cells:   cells,
		Serving: int64(servingIdx),
		Track: meas.Track{
			StartX: start.X, StartY: start.Y, EndX: dest.X, EndY: dest.Y,
			Duration: duration, Step: 500 * time.Millisecond,
		},
		Event: meas.EventA3{Offset: 3, Hysteresis: 1, TTT: 200 * time.Millisecond},
	}.Run(base)
}

// uesOnGNB returns the non-mobile UEs (index >= mobile) currently served by
// gNB gi — the flows that share that gNB's N3 socket.
func uesOnGNB(ues []*fleetUE, gi, mobile int) []*fleetUE {
	var out []*fleetUE
	for i := mobile; i < len(ues); i++ {
		if ues[i].gnbIdx == gi {
			out = append(out, ues[i])
		}
	}
	return out
}

func orDefaultInt(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}
