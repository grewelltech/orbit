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
func RunFleet(ctx context.Context, log *slog.Logger, spec FleetRunSpec, beh FleetBehaviors) (FleetReport, error) {
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

	// One shared N3 tunnel pool for the whole run: sessions' lazy data
	// paths (app cohorts) and the synthetic traffic flows all acquire the
	// SAME per-gNB socket through it — one 2152 bind per gNB N3 address,
	// never two behaviours colliding on the port.
	pool := newN3Pool()

	// Attach phase — persistent, keeping a handle per UE.
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
			fu, err := attachFleetUE(ctx, f, spec, pool, i)
			amu.Lock()
			defer amu.Unlock()
			if err != nil {
				rep.AttachFailed++
				return
			}
			ues = append(ues, fu)
		}(i)
	}
	wg.Wait()
	rep.Attached = len(ues)
	rep.AttachElapsed = time.Since(start)
	if len(ues) == 0 {
		return rep, fmt.Errorf("no UEs attached")
	}

	// Behaviour phase.
	if beh.Duration > 0 {
		runFleetBehaviors(ctx, f, spec, pool, ues, beh, &rep, log)
	}

	// Deregister. App-cohort members opened lazy data paths on the shared
	// pool — close them first so the per-gNB sockets (and any netstack
	// bridge) release with the last UE.
	for _, fu := range ues {
		fu.sess.closeDataPath()
		if err := fu.sess.deregister(context.Background()); err == nil {
			rep.Deregistered++
		}
	}
	return rep, nil
}

// attachFleetUE attaches one UE muxed on its serving gNB's association and
// returns a persistent handle. The session's data path (opened lazily only
// when an app cohort uses the UE) rides the run's shared per-gNB pool.
func attachFleetUE(ctx context.Context, f *Fleet, spec FleetRunSpec, pool *n3Pool, i int) (*fleetUE, error) {
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
	sess, err := Attach(ctx, uet, f.gnbConfigFor(gi), cfg, f.log, nil)
	if err != nil {
		return nil, err
	}
	if !sess.Result.Registered {
		return nil, fmt.Errorf("UE %s not registered", supi)
	}
	sess.gnbN3 = spec.GNBs[gi].N3Addr
	sess.n3 = pool
	return &fleetUE{sess: sess, gnbIdx: gi}, nil
}

// runFleetBehaviors runs mobility + traffic + app cohorts concurrently until
// the duration elapses (or the context is cancelled).
func runFleetBehaviors(ctx context.Context, f *Fleet, spec FleetRunSpec, pool *n3Pool, ues []*fleetUE,
	beh FleetBehaviors, rep *FleetReport, log *slog.Logger) {
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
		agents := newFleetAgentPool(appTuning{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			appReports = runFleetApps(dctx, log, agents, beh.Apps, appMembers, beh.Duration, beh.AppMetricsReg)
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
					err := fleetHandover(dctx, f, spec, fu, int(tr.TargetCellID))
					hmu.Lock()
					if err != nil {
						rep.HandoverErr++
					} else {
						rep.Handovers++
					}
					hmu.Unlock()
				}
			}(fu, triggers)
		}
	}

	wg.Wait()
	rep.TrafficBytes = trafficBytes.Load()
	rep.AppCohorts = appReports
}

// fleetHandover moves fu to the target gNB via an Xn PathSwitch on the target's
// muxed association, updating the UE handle on success.
func fleetHandover(ctx context.Context, f *Fleet, spec FleetRunSpec, fu *fleetUE, target int) error {
	uetT, ranIDT := f.sessions[target].NewUE()
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
	fu.sess.conn = uetT
	if newAMFID != 0 {
		fu.sess.amfID = newAMFID
	}
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
