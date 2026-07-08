package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/loomgtp"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// FleetGNB is one generated gNB in a fleet run: its NGAP config, the local
// source address to bind (distinct per gNB, for handover), and its N3 address
// for the data path (usually the same IP).
type FleetGNB struct {
	Config   gnb.Config
	BindAddr string
	N3Addr   string
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
// Duration (ADR-0004). Mobility hands a subset of UEs between gNBs; Traffic runs
// one loom flow per gNB (a shared-N3 demux is needed for concurrent per-UE
// traffic — deferred, so one representative flow per gNB for now).
type FleetBehaviors struct {
	Duration      time.Duration
	MobileUEs     int
	HandoverEvery time.Duration
	Traffic       bool
	TrafficRate   string
	TrafficTarget string
}

// FleetReport summarises a fleet run.
type FleetReport struct {
	Attached, AttachFailed int
	AttachElapsed          time.Duration
	Handovers, HandoverErr int
	TrafficFlows           int
	TrafficBytes           uint64
	Deregistered           int
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
	specs := make([]GNBSpec, len(spec.GNBs))
	for i, g := range spec.GNBs {
		specs[i] = GNBSpec{AMFAddr: spec.AMFAddr, Config: g.Config, BindAddr: g.BindAddr}
	}
	f, err := NewFleet(ctx, specs, log)
	if err != nil {
		return rep, err
	}
	defer f.Close()

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
			fu, err := attachFleetUE(ctx, f, spec, i)
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
		runFleetBehaviors(ctx, f, spec, ues, beh, &rep, log)
	}

	// Deregister.
	for _, fu := range ues {
		if err := fu.sess.deregister(context.Background()); err == nil {
			rep.Deregistered++
		}
	}
	return rep, nil
}

// attachFleetUE attaches one UE muxed on its serving gNB's association and
// returns a persistent handle.
func attachFleetUE(ctx context.Context, f *Fleet, spec FleetRunSpec, i int) (*fleetUE, error) {
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
	return &fleetUE{sess: sess, gnbIdx: gi}, nil
}

// runFleetBehaviors runs mobility + traffic concurrently until the duration
// elapses (or the context is cancelled).
func runFleetBehaviors(ctx context.Context, f *Fleet, spec FleetRunSpec, ues []*fleetUE,
	beh FleetBehaviors, rep *FleetReport, log *slog.Logger) {
	dctx, cancel := context.WithTimeout(ctx, beh.Duration)
	defer cancel()
	var wg sync.WaitGroup

	// Traffic: one loom flow per gNB, using a representative (non-mobile) UE.
	var trafficBytes atomic.Uint64
	if beh.Traffic && spec.PDUSession != nil {
		for gi := range f.sessions {
			fu := representativeUE(ues, gi, beh.MobileUEs)
			if fu == nil {
				continue
			}
			wg.Add(1)
			rep.TrafficFlows++
			go func(fu *fleetUE) {
				defer wg.Done()
				n := runFleetFlow(dctx, spec, fu, beh)
				trafficBytes.Add(n)
			}(fu)
		}
	}

	// Mobility: cycle each mobile UE through the gNBs every HandoverEvery.
	if beh.MobileUEs > 0 && len(f.sessions) > 1 {
		every := beh.HandoverEvery
		if every <= 0 {
			every = 15 * time.Second
		}
		var hmu sync.Mutex
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(every)
			defer t.Stop()
			mobile := ues[:min(beh.MobileUEs, len(ues))]
			for {
				select {
				case <-dctx.Done():
					return
				case <-t.C:
					for _, fu := range mobile {
						target := (fu.gnbIdx + 1) % len(f.sessions)
						err := fleetHandover(dctx, f, spec, fu, target)
						hmu.Lock()
						if err != nil {
							rep.HandoverErr++
						} else {
							rep.Handovers++
						}
						hmu.Unlock()
					}
				}
			}
		}()
	}

	wg.Wait()
	rep.TrafficBytes = trafficBytes.Load()
}

// fleetHandover moves fu to the target gNB via an Xn PathSwitch on the target's
// muxed association, updating the UE handle on success.
func fleetHandover(ctx context.Context, f *Fleet, spec FleetRunSpec, fu *fleetUE, target int) error {
	uetT, ranIDT := f.sessions[target].NewUE()
	newTEID := uint32(0x1000) + uint32(ranIDT)
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
	fu.sess.conn = uetT
	if newAMFID != 0 {
		fu.sess.amfID = newAMFID
	}
	fu.sess.Result.DLTEID = newTEID
	fu.gnbIdx = target
	return nil
}

// runFleetFlow runs one loom UDP flow over fu's tunnel for the duration and
// returns the bytes sent.
func runFleetFlow(ctx context.Context, spec FleetRunSpec, fu *fleetUE, beh FleetBehaviors) uint64 {
	r := fu.sess.Result
	tun, err := datapath.NewTunnel(datapath.Config{
		LocalN3: net.JoinHostPort(spec.GNBs[fu.gnbIdx].N3Addr, "2152"),
		UPFN3:   net.JoinHostPort(r.UPFAddress, "2152"),
		ULTEID:  r.UPFTEID, DLTEID: r.DLTEID, QFI: r.QFI,
	})
	if err != nil {
		return 0
	}
	defer tun.Close()
	res, err := loomgtp.RunFlow(ctx, loomgtp.Config{
		Uplink: tun, UEIP: net.ParseIP(r.PDUAddress),
		Target: beh.TrafficTarget, Rate: beh.TrafficRate, Duration: beh.Duration,
	})
	if err != nil {
		return 0
	}
	return res.Bytes
}

// representativeUE returns a non-mobile UE served by gNB gi (the first UE beyond
// the mobile set that lands on gi), or nil.
func representativeUE(ues []*fleetUE, gi, mobile int) *fleetUE {
	for i := mobile; i < len(ues); i++ {
		if ues[i].gnbIdx == gi {
			return ues[i]
		}
	}
	return nil
}

func orDefaultInt(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}
