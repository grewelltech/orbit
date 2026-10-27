package server

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/scenario"
	"github.com/bgrewell/orbit/internal/target"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// errNotYet is a plan restriction a later step may lift, so a client knows
// the refusal is "not yet", not "never" (ADR-0008 §2).
var errNotYet = errors.New("is not supported yet")

// defaultGNBIDBits is the gNB ID length when the target gives none, as on
// the CLI (--gnb-id-bits).
const defaultGNBIDBits = 24

// planRun is a RunPlan translated into today's engine specs (S2): one
// population workload, run by the load engine when it has no behaviours
// and by the fleet engine when it has. Exactly one of load and fleet is
// set. assigned are the gNB IDs taken from the rotation, to hand back
// when the run releases its leases.
type planRun struct {
	load      engine.LoadRunFunc
	fleet     engine.FleetRunFunc
	storm     engine.LoadSpec     // the storm path's engine spec, as run
	built     engine.FleetRunSpec // the persistent path's engine spec, as run
	beh       engine.FleetBehaviors
	suite     engine.ConformanceRunFunc // a conformance workload's launcher
	suiteLive *suiteLive
	mixed     []engine.MixedWorkload // a mixed plan's workloads
	kinds     []engine.RunKind       // and their kinds
	needs     engine.Needs
	workload  string
	amf       string   // the gNB IDs' AMF, as given
	amfKeys   []string // its identity set, which the rotation keys on
	plmn      string
	assigned  []uint32
	// registered is what the registry filled in that a run's provenance
	// records (a latency target from a registered responder).
	registered []*orbitv1.FieldProvenance
}

// population checks a plan's shape for S2 and returns its one population
// workload.
func population(p *orbitv1.RunPlan) (*orbitv1.Workload, *orbitv1.PopulationWorkload, error) {
	switch n := len(p.GetWorkloads()); {
	case n == 0:
		return nil, nil, errors.New("a plan needs at least one workload")
	case n > 1:
		return nil, nil, fmt.Errorf("a plan with %d workloads %w: give one workload, or one population and one conformance workload", n, errNotYet)
	}
	w := p.GetWorkloads()[0]
	if w.GetStart() != nil {
		return nil, nil, fmt.Errorf("workload %q: a start condition needs another workload to wait on", w.GetName())
	}
	pop := w.GetPopulation()
	if pop == nil {
		return nil, nil, fmt.Errorf("workload %q: a steps workload %w; give a population or conformance workload", w.GetName(), errNotYet)
	}
	if err := validateGates(p.GetGates(), w.GetName(), false); err != nil {
		return nil, nil, err
	}
	return w, pop, nil
}

// subscriberPool picks the population's pool: by name, else the first.
func subscriberPool(t *orbitv1.ResolvedTarget, name string) (*orbitv1.SubscriberPool, error) {
	pools := t.GetSubscribers()
	if len(pools) == 0 {
		return nil, errors.New("the target has no subscriber pool")
	}
	if name == "" {
		return pools[0], nil
	}
	for _, sp := range pools {
		if sp.GetName() == name {
			return sp, nil
		}
	}
	return nil, fmt.Errorf("the target has no subscriber pool %q", name)
}

// planCount is how many UEs the population uses: attach.count, else (a
// timed run) the whole pool, so even an open-ended soak leases a bounded
// SUPI block.
func planCount(pop *orbitv1.PopulationWorkload, sp *orbitv1.SubscriberPool, timed bool) (uint32, error) {
	n := pop.GetAttach().GetCount()
	switch {
	case n == 0 && timed && sp.GetCount() > 0:
		n = sp.GetCount()
	case n == 0:
		return 0, errors.New("attach.count is required (or a timed run on a pool with a count)")
	}
	if sp.GetCount() > 0 && n > sp.GetCount() {
		return 0, fmt.Errorf("attach.count %d is more than subscriber pool %q holds (%d)", n, sp.GetName(), sp.GetCount())
	}
	return n, nil
}

// gnbBits is the target's gNB ID length, or the default.
func gnbBits(t *orbitv1.ResolvedTarget) uint32 {
	if b := t.GetRan().GetGnbIds().GetBits(); b != 0 {
		return b
	}
	return defaultGNBIDBits
}

// gnbRange is the range a population's gNB IDs must come from (ADR-0008
// §5): the target's ran.gnb_ids block when it has a count, else (for
// assignment) the server default, or from ran.gnb_ids.first upward, either
// way within the ID length. ranged reports whether the target gave a block.
func gnbRange(t *orbitv1.ResolvedTarget) (lo, hi uint32, ranged bool, err error) {
	ids := t.GetRan().GetGnbIds()
	bits := gnbBits(t)
	if bits < 22 || bits > 32 {
		return 0, 0, false, fmt.Errorf("ran.gnb_ids.bits %d: want 22 to 32", bits)
	}
	maxID := uint64(1)<<bits - 1
	if n := ids.GetCount(); n > 0 {
		end := uint64(ids.GetFirst()) + uint64(n) - 1
		if end > maxID {
			return 0, 0, false, fmt.Errorf("ran.gnb_ids %d..%d does not fit %d bits", ids.GetFirst(), end, bits)
		}
		return ids.GetFirst(), uint32(end), true, nil
	}
	lo, hi = engine.DefaultGNBIDFirst, engine.DefaultGNBIDLast
	if ids.GetFirst() != 0 {
		lo = ids.GetFirst()
		hi = uint32(maxID)
	}
	if uint64(hi) > maxID {
		hi = uint32(maxID)
	}
	return lo, hi, false, nil
}

// gnbIDs are the population's gNB IDs: pinned (topology.gnb_ids.first, or
// the target's ran.gnb_ids.first, or 1), which must lie in the target's
// block when it gives one; or assigned from the rotation within the range
// (gnbRange). amfKeys is the AMF's identity set (RunRegistry.AMFIdentity),
// which the rotation keys on; amf is the AMF as given, for the in-use
// check.
func (s *runService) gnbIDs(t *orbitv1.ResolvedTarget, pop *orbitv1.PopulationWorkload, amf string, amfKeys []string, plmn string, n int) (ids []uint32, assigned bool, err error) {
	lo, hi, ranged, err := gnbRange(t)
	if err != nil {
		return nil, false, err
	}
	sel := pop.GetTopology().GetGnbIds()
	if sel.GetAssign() {
		if s.rotation == nil {
			return nil, false, errors.New("topology.gnb_ids.assign: this server has no gNB ID rotation")
		}
		ids, err := s.rotation.Assign(amfKeys, plmn, lo, hi, n, s.reg.GNBInUse(amf, plmn))
		return ids, true, err
	}
	first := sel.GetFirst()
	if first == 0 {
		first = t.GetRan().GetGnbIds().GetFirst()
	}
	if first == 0 && !ranged {
		first = 1
	}
	last := uint64(first) + uint64(n) - 1
	if ranged && (first < lo || last > uint64(hi)) {
		return nil, false, fmt.Errorf("gNB IDs %d..%d fall outside the target's ran.gnb_ids %d..%d; %d gNBs need a block of %d there",
			first, last, lo, hi, n, n)
	}
	if last > uint64(1)<<gnbBits(t)-1 {
		return nil, false, fmt.Errorf("gNB IDs %d..%d do not fit %d bits", first, last, gnbBits(t))
	}
	for i := 0; i < n; i++ {
		ids = append(ids, first+uint32(i))
	}
	return ids, false, nil
}

// gnbName is gNB i of n's RANNodeName, the same for the load and fleet
// paths: the target's ran.gnb_name (default "orbit-gnb"), numbered when
// there are several.
func gnbName(t *orbitv1.ResolvedTarget, i, n int) string {
	name := t.GetRan().GetGnbName()
	if name == "" {
		name = "orbit-gnb"
	}
	if n > 1 {
		name += "-" + strconv.Itoa(i+1)
	}
	return name
}

// buildPlan translates a plan (S2). The caller hands assigned IDs back to
// the rotation if the run then fails to start.
func (s *runService) buildPlan(p *orbitv1.RunPlan, verbosity engine.EventVerbosity) (*planRun, error) {
	return s.buildPlanAs(p, verbosity, false)
}

// buildPlanAs is buildPlan, with persistent choosing the persistent
// strategy even without behaviours: an old fleet arm's plan, since a fleet
// always kept its UEs.
func (s *runService) buildPlanAs(p *orbitv1.RunPlan, verbosity engine.EventVerbosity, persistent bool) (*planRun, error) {
	p, registered, err := s.withRegistered(p)
	if err != nil {
		return nil, err
	}
	switch ws := p.GetWorkloads(); {
	case len(ws) == 1 && ws[0].GetConformance() != nil && ws[0].GetStart() == nil:
		return s.buildSuite(p, ws[0])
	case len(ws) == 2:
		return s.buildMixed(p, verbosity)
	}
	w, pop, err := population(p)
	if err != nil {
		return nil, err
	}
	t := p.GetTarget()
	core := t.GetCore()
	if core.GetAmf() == "" || core.GetPlmn().GetMcc() == "" || core.GetPlmn().GetMnc() == "" {
		return nil, errors.New("the target needs core.amf and core.plmn")
	}
	sp, err := subscriberPool(t, pop.GetSubscriberPool())
	if err != nil {
		return nil, err
	}
	if sp.GetFirstSupi() == "" {
		return nil, fmt.Errorf("subscriber pool %q has no first_supi", sp.GetName())
	}
	if _, err := auth.ParseHexKey("Ki", sp.GetKi()); err != nil {
		return nil, fmt.Errorf("subscriber pool %q: %w", sp.GetName(), err)
	}
	if _, err := auth.ParseHexKey("OPc", sp.GetOpc()); err != nil {
		return nil, fmt.Errorf("subscriber pool %q: %w", sp.GetName(), err)
	}
	until := p.GetUntil()
	att := pop.GetAttach()
	curve, err := planRate(att)
	if err != nil {
		return nil, err
	}
	count, err := planCount(pop, sp, until.GetDurationMs() > 0 || until.GetForever())
	if err != nil {
		return nil, err
	}
	gnbCount := int(pop.GetTopology().GetGnbCount())
	if gnbCount == 0 {
		gnbCount = 1
	}
	n2, err := target.ExpandPool(t.GetRan().GetN2SourceIps(), gnbCount)
	if err != nil {
		return nil, fmt.Errorf("ran.n2_source_ips: %w", err)
	}
	n3, err := target.ExpandPool(t.GetRan().GetN3Ips(), gnbCount)
	if err != nil {
		return nil, fmt.Errorf("ran.n3_ips: %w", err)
	}
	plmn := core.GetPlmn().GetMcc() + "/" + core.GetPlmn().GetMnc()
	var amfKeys []string
	if pop.GetTopology().GetGnbIds().GetAssign() {
		amfKeys = s.reg.AMFIdentity(core.GetAmf()) // resolved here, outside any lock
	}
	ids, assigned, err := s.gnbIDs(t, pop, core.GetAmf(), amfKeys, plmn, gnbCount)
	if err != nil {
		return nil, err
	}
	out := &planRun{workload: w.GetName(), amf: core.GetAmf(), amfKeys: amfKeys, plmn: plmn, registered: registered}
	if assigned {
		out.assigned = ids
	}
	fail := func(err error) (*planRun, error) {
		s.giveBack(out)
		return nil, err
	}
	creds := &orbitv1.Credentials{Ki: sp.GetKi(), Opc: sp.GetOpc()}
	if pop.GetBehaviors() == nil && !persistent {
		// Storm: today's load run, through the load arm's own translation.
		lspec, err := planLoadSpec(t, pop, sp, creds, count, ids, n2, n3, until)
		if err != nil {
			return fail(err)
		}
		spec, err := loadSpecFrom(lspec)
		if err != nil {
			return fail(err)
		}
		if _, ok := curve.(*load.Poisson); ok {
			spec.Rate = curve // Poisson arrival, which a load arm cannot carry
		}
		if until.GetForever() {
			spec.Duration = forever
		}
		fn, needs, err := loadRunFrom(s.log, spec)
		if err != nil {
			return fail(err)
		}
		out.load, out.needs, out.storm = fn, needs, spec
		return out, nil
	}
	if until.GetComplete() {
		return fail(errors.New("until.complete with behaviours never finishes: behaviours run until the run's duration"))
	}
	fs, err := planFleetScenario(w, t, pop, sp, count, ids[0], n2, n3, until)
	if err != nil {
		return fail(err)
	}
	doc, err := yaml.Marshal(fs)
	if err != nil {
		return fail(err)
	}
	// Through today's parser, so the plan gets the same validation as a
	// scenario file (the SUPI range in the PLMN included).
	parsed, err := scenario.ParseFleetNoEnv(doc)
	if err != nil {
		return fail(err)
	}
	ki, _ := auth.ParseHexKey("Ki", creds.GetKi())
	opc, _ := auth.ParseHexKey("OPc", creds.GetOpc())
	spec, beh, err := scenario.BuildFleetRun(parsed, ki, opc)
	if err != nil {
		return fail(err)
	}
	// As the load path builds them: assigned IDs need not be contiguous,
	// and the target's ID length and gNB name apply to fleets too.
	for i := range spec.GNBs {
		spec.GNBs[i].Config.ID = ids[i]
		spec.GNBs[i].Config.IDBits = int(gnbBits(t))
		spec.GNBs[i].Config.Name = gnbName(t, i, len(spec.GNBs))
	}
	// The attach controller's knobs a scenario file has no words for. A
	// constant rate is the scenario's own attach_rate already.
	if _, constant := curve.(load.Constant); curve != nil && !constant {
		spec.Rate = curve
	}
	if c := att.GetConcurrency(); c > 0 {
		spec.Concurrency = int(c)
	}
	spec.SampleEvery = time.Duration(pop.GetSampleIntervalMs()) * time.Millisecond
	if until.GetForever() {
		beh.Duration = forever
	}
	// A plan's until.duration is the run's deadline: the population keeps
	// its UEs until then, even with no behaviours or ones that end sooner.
	// An old fleet arm's plan (persistent) keeps the arm's meaning: its
	// run.duration only bounds its behaviours.
	beh.Hold = until.GetDurationMs() > 0 && !persistent
	fn, needs, err := fleetRunFrom(s.log, s.mgr, spec, beh, verbosity)
	if err != nil {
		return fail(err)
	}
	out.fleet, out.needs, out.built, out.beh = fn, needs, spec, beh
	return out, nil
}

// giveBack returns a plan's assigned gNB IDs to the rotation, for a plan
// that never started (its run's OnRelease does it otherwise).
func (s *runService) giveBack(r *planRun) {
	if r == nil {
		return
	}
	s.releaseIDs(r.amfKeys, r.plmn, r.assigned)
	r.assigned = nil
}

// releaseIDs returns gNB IDs to the rotation, under the AMF's identity set.
func (s *runService) releaseIDs(amfKeys []string, plmn string, ids []uint32) {
	if len(ids) == 0 || s.rotation == nil {
		return
	}
	if err := s.rotation.Released(amfKeys, plmn, ids); err != nil {
		s.log.Warn("could not record released gNB IDs", "err", err)
	}
}

// planLoadSpec is a population without behaviours as today's load spec.
func planLoadSpec(t *orbitv1.ResolvedTarget, pop *orbitv1.PopulationWorkload, sp *orbitv1.SubscriberPool,
	creds *orbitv1.Credentials, count uint32, ids []uint32, n2, n3 []string, until *orbitv1.Until) (*orbitv1.LoadRunSpec, error) { //nolint:staticcheck // the deprecated arm, kept for API clients
	core := t.GetCore()
	if until.GetDurationMs() > math.MaxUint32 {
		return nil, fmt.Errorf("until.duration_ms %d is longer than a load run takes (%d)", until.GetDurationMs(), uint64(math.MaxUint32))
	}
	bits := gnbBits(t)
	spec := &orbitv1.LoadRunSpec{ //nolint:staticcheck // the deprecated arm, kept for API clients
		AmfAddress:       core.GetAmf(),
		BaseImsi:         sp.GetFirstSupi(),
		Count:            count,
		Credentials:      creds,
		Rate:             pop.GetAttach().GetRate(),
		Concurrency:      pop.GetAttach().GetConcurrency(),
		DurationMs:       uint32(until.GetDurationMs()),
		SampleIntervalMs: pop.GetSampleIntervalMs(),
		BindAddresses:    n2,
	}
	if r := pop.GetAttach().GetRamp(); r != nil {
		spec.RampStart, spec.RampEnd, spec.RampSeconds = r.GetStart(), r.GetEnd(), r.GetSeconds()
	}
	for i, id := range ids {
		spec.Gnbs = append(spec.Gnbs, &orbitv1.GnbConfig{
			Id: id, IdBits: bits, Name: gnbName(t, i, len(ids)),
			Mcc: core.GetPlmn().GetMcc(), Mnc: core.GetPlmn().GetMnc(), Tac: core.GetTac(),
			Slices: core.GetSlices(),
		})
	}
	if pop.GetPduSession() {
		var sst uint32 = 1
		var sd string
		if len(core.GetSlices()) > 0 {
			sst, sd = core.GetSlices()[0].GetSst(), core.GetSlices()[0].GetSd()
		}
		spec.PduSession = &orbitv1.PDUSession{PduSessionId: 1, Sst: sst, Sd: sd, Dnn: core.GetDnn()}
		if len(n3) == 0 {
			return nil, errors.New("pdu_session needs the target's ran.n3_ips")
		}
		spec.GnbN3Addr = n3[0]
	}
	return spec, nil
}

// planFleetScenario is a population with behaviours as today's fleet
// scenario. Responders named by a cohort or bulk traffic resolve from the
// target.
func planFleetScenario(w *orbitv1.Workload, t *orbitv1.ResolvedTarget, pop *orbitv1.PopulationWorkload, sp *orbitv1.SubscriberPool,
	count uint32, firstID uint32, n2, n3 []string, until *orbitv1.Until) (*scenario.FleetScenario, error) {
	core := t.GetCore()
	f := &scenario.FleetScenario{Kind: "fleet", Name: w.GetName()}
	f.Core = scenario.Core{AMF: core.GetAmf(), PLMN: scenario.PLMN{MCC: core.GetPlmn().GetMcc(), MNC: core.GetPlmn().GetMnc()},
		TAC: core.GetTac(), DNN: core.GetDnn()}
	if len(core.GetSlices()) > 0 {
		f.Core.Slice = scenario.Slice{SST: core.GetSlices()[0].GetSst(), SD: core.GetSlices()[0].GetSd()}
	}
	topo := pop.GetTopology()
	f.Topology.GNBs = scenario.GNBGen{Count: len(n2), IDBase: firstID, SourceIPs: n2, N3IPs: n3,
		Layout: topo.GetLayout(), Spacing: topo.GetSpacingM()}
	if gc := int(topo.GetGnbCount()); gc > 0 {
		f.Topology.GNBs.Count = gc
	} else {
		f.Topology.GNBs.Count = 1
	}
	att := pop.GetAttach()
	f.Fleet = scenario.FleetSpec{Count: int(count), SUPIBase: sp.GetFirstSupi(), Distribution: att.GetDistribution(),
		DistributionSeed: att.GetDistributionSeed(), PDUSession: pop.GetPduSession()}
	if att.GetRate() > 0 {
		f.Fleet.AttachRate = strconv.FormatFloat(att.GetRate(), 'f', -1, 64) + "/s"
	}
	if d := until.GetDurationMs(); d > 0 {
		f.Run.Duration = strconv.FormatUint(d, 10) + "ms"
	}
	beh, err := planBehaviors(t, pop.GetBehaviors())
	if err != nil {
		return nil, err
	}
	f.Behaviors = beh
	return f, nil
}

// responderFor is a named target responder's control address, token and
// data IP.
func responderFor(t *orbitv1.ResolvedTarget, name string) (*orbitv1.Responder, error) {
	for _, r := range t.GetApp().GetResponders() {
		if r.GetName() == name {
			return r, nil
		}
	}
	return nil, fmt.Errorf("the target has no responder %q", name)
}

// planBehaviors is typed behaviours as the fleet scenario's.
func planBehaviors(t *orbitv1.ResolvedTarget, b *orbitv1.Behaviors) (scenario.Behaviors, error) {
	var out scenario.Behaviors
	if m := b.GetMobility(); m != nil {
		mb := &scenario.MobilityBehavior{Model: m.GetModel()}
		if m.GetSpeedMps() > 0 {
			mb.Speed = strconv.FormatFloat(m.GetSpeedMps(), 'f', -1, 64) + "m/s"
		}
		switch m.GetHandover() {
		case orbitv1.HandoverKind_HANDOVER_KIND_N2:
			mb.Handover = "n2" // BuildFleetRun refuses it, as for a file
		case orbitv1.HandoverKind_HANDOVER_KIND_XN:
			mb.Handover = "xn"
		}
		out.Mobility = mb
	}
	if tr := b.GetTraffic(); tr != nil {
		tb := &scenario.TrafficBehavior{}
		for _, share := range tr.GetMix() {
			ts := scenario.TrafficShare{Name: share.GetName(), Share: share.GetShare()}
			switch k := share.GetKind().(type) {
			case *orbitv1.TrafficShare_Synthetic:
				ts.Profile, ts.Target = k.Synthetic.GetProfile(), k.Synthetic.GetTarget()
				if r := k.Synthetic.GetRateBps(); r > 0 {
					ts.Rate = bitRate(r)
				}
			case *orbitv1.TrafficShare_App:
				a := k.App
				ts.App, ts.Peer, ts.Token, ts.PeerDataIP, ts.Params = a.GetApp(), a.GetPeer(), a.GetToken(), a.GetPeerDataIp(), a.GetParams()
				if a.GetStartAfterMs() > 0 {
					ts.StartAfter = strconv.FormatUint(a.GetStartAfterMs(), 10) + "ms"
				}
				if a.GetResponder() != "" {
					r, err := responderFor(t, a.GetResponder())
					if err != nil {
						return out, fmt.Errorf("traffic cohort %q: %w", share.GetName(), err)
					}
					ts.Peer, ts.Token, ts.PeerDataIP = orDefault(ts.Peer, r.GetControl()), orDefault(ts.Token, r.GetToken()), orDefault(ts.PeerDataIP, r.GetDataIp())
				}
			default:
				return out, fmt.Errorf("traffic share %q needs a synthetic profile or an app", share.GetName())
			}
			tb.Mix = append(tb.Mix, ts)
		}
		if bk := tr.GetBulk(); bk != nil {
			bt := &scenario.BulkTraffic{UEs: int(bk.GetUes()), Peer: bk.GetPeer(), Token: bk.GetToken(),
				PeerDataIP: bk.GetPeerDataIp(), PacketSize: int(bk.GetPacketSize())}
			if bk.GetUplinkBps() > 0 {
				bt.UplinkRate = bitRate(bk.GetUplinkBps())
			}
			if bk.GetDownlinkBps() > 0 {
				bt.DownlinkRate = bitRate(bk.GetDownlinkBps())
			}
			if bk.GetResponder() != "" {
				r, err := responderFor(t, bk.GetResponder())
				if err != nil {
					return out, fmt.Errorf("bulk traffic: %w", err)
				}
				bt.Peer, bt.Token, bt.PeerDataIP = orDefault(bt.Peer, r.GetControl()), orDefault(bt.Token, r.GetToken()), orDefault(bt.PeerDataIP, r.GetDataIp())
			}
			tb.Bulk = bt
		}
		out.Traffic = tb
	}
	if l := b.GetLatency(); l != nil {
		lb := &scenario.LatencyBehavior{Target: l.GetTarget(), UEs: int(l.GetUes())}
		if l.GetIntervalMs() > 0 {
			lb.Interval = strconv.FormatUint(l.GetIntervalMs(), 10) + "ms"
		}
		if l.GetTimeoutMs() > 0 {
			lb.Timeout = strconv.FormatUint(l.GetTimeoutMs(), 10) + "ms"
		}
		out.Latency = lb
	}
	return out, nil
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

// bitRate writes bits per second as the fleet scenario does ("8Mbps").
func bitRate(bps uint64) string {
	switch {
	case bps%1_000_000_000 == 0:
		return strconv.FormatUint(bps/1_000_000_000, 10) + "Gbps"
	case bps%1_000_000 == 0:
		return strconv.FormatUint(bps/1_000_000, 10) + "Mbps"
	case bps%1_000 == 0:
		return strconv.FormatUint(bps/1_000, 10) + "Kbps"
	}
	return strconv.FormatUint(bps, 10) + "bps"
}

// provenance is the server's own provenance for a plan: the gNB IDs it
// assigned from the rotation (layer LEASE).
func (r *planRun) provenance() []*orbitv1.FieldProvenance {
	out := append([]*orbitv1.FieldProvenance(nil), r.registered...)
	if len(r.assigned) == 0 {
		return out
	}
	ids := make([]string, len(r.assigned))
	for i, id := range r.assigned {
		ids[i] = strconv.FormatUint(uint64(id), 10)
	}
	return append(out, &orbitv1.FieldProvenance{
		Path:   "workloads[" + r.workload + "].topology.gnb_ids",
		Source: &orbitv1.ProvenanceSource{Layer: orbitv1.ProvenanceLayer_PROVENANCE_LAYER_LEASE, Detail: "gNB ID rotation (least recently used) at " + r.amf},
		Value:  "[" + strings.Join(ids, ",") + "]",
	})
}

// forever is how long an until.forever run's attaching (storm) or
// behaviours (persistent) last: until StopRun, in practice.
const forever = 100 * 365 * 24 * time.Hour

// planRate is the attach-rate curve a population asks for: a constant rate
// or a linear ramp, with Poisson arrival around it when asked (seeded by
// attach.seed for a reproducible run). nil: concurrency-bound.
func planRate(a *orbitv1.AttachProfile) (load.Rate, error) {
	var base load.Rate
	switch {
	case a.GetRamp() != nil:
		r := a.GetRamp()
		base = load.LinearRamp{Start: r.GetStart(), End: r.GetEnd(), Over: time.Duration(r.GetSeconds()) * time.Second}
	case a.GetRate() > 0:
		base = load.Constant{RPS: a.GetRate()}
	}
	switch a.GetArrival() {
	case orbitv1.Arrival_ARRIVAL_POISSON:
		if base == nil {
			return nil, errors.New("attach.arrival poisson needs attach.rate or attach.ramp")
		}
		return load.NewPoisson(base, a.GetSeed()), nil
	case orbitv1.Arrival_ARRIVAL_UNSPECIFIED, orbitv1.Arrival_ARRIVAL_UNIFORM:
		if a.GetSeed() != 0 {
			return nil, errors.New("attach.seed seeds Poisson arrival; set attach.arrival poisson")
		}
		return base, nil
	}
	return nil, fmt.Errorf("attach.arrival %v is not supported", a.GetArrival())
}

// planReport is a completed plan run's report for its one workload, with
// the plan's gates applied: from the engine's report while the run is in
// the registry. nil when the run was not a plan.
func planReport(plan *orbitv1.RunPlan, load *orbitv1.LoadReport, fleet *orbitv1.FleetReport, suite *orbitv1.ConformanceSummary) *orbitv1.RunReport {
	if plan == nil || len(plan.GetWorkloads()) != 1 {
		return nil
	}
	r := &orbitv1.RunReport{Workload: plan.GetWorkloads()[0].GetName()}
	switch {
	case load != nil:
		r.Kind, r.Report = orbitv1.RunKind_RUN_KIND_LOAD, &orbitv1.RunReport_Load{Load: load}
	case fleet != nil:
		r.Kind, r.Report = orbitv1.RunKind_RUN_KIND_FLEET, &orbitv1.RunReport_Fleet{Fleet: fleet}
	case suite != nil:
		r.Kind, r.Report = orbitv1.RunKind_RUN_KIND_CONFORMANCE, &orbitv1.RunReport_Conformance{Conformance: suite}
	default:
		return nil
	}
	applyGates(r, plan.GetGates())
	return r
}

// planJudge judges a plan run's report as it completes (RunOptions.Judge):
// each workload's RunReport with the plan's gates applied, in plan order.
// It keeps only the workload names and the gates, none of the plan's
// secrets.
func planJudge(plan *orbitv1.RunPlan) func(any) any {
	var gates []*orbitv1.Gate
	for _, g := range plan.GetGates() {
		gates = append(gates, proto.Clone(g).(*orbitv1.Gate))
	}
	held := &orbitv1.RunPlan{Gates: gates}
	for _, w := range plan.GetWorkloads() {
		held.Workloads = append(held.Workloads, &orbitv1.Workload{Name: w.GetName()})
	}
	return func(report any) any {
		var out []*orbitv1.RunReport
		add := func(rr *orbitv1.RunReport) {
			if rr != nil {
				out = append(out, rr)
			}
		}
		switch r := report.(type) {
		case *load.Report:
			add(planReport(held, loadReportProto(*r), nil, nil))
		case *engine.FleetReport:
			add(planReport(held, nil, fleetReportProto(*r), nil))
		case *suiteLive:
			add(planReport(held, nil, nil, r.summary()))
		case *engine.MixedReport:
			for _, w := range r.Workloads {
				add(mixedRunReport(w, gates))
			}
		}
		if len(out) == 0 {
			return nil // not a typed nil, which would read as a judgement
		}
		return out
	}
}

// mixedRunReport is one workload of a mixed run as a RunReport, with the
// gates that judge it applied. A workload that failed or was skipped has
// no report, and its gates fail saying why.
func mixedRunReport(w engine.MixedResult, gates []*orbitv1.Gate) *orbitv1.RunReport {
	rr := &orbitv1.RunReport{Workload: w.Name, Kind: runKindProto(w.Kind)}
	switch rep := w.Report.(type) {
	case *load.Report:
		rr.Report = &orbitv1.RunReport_Load{Load: loadReportProto(*rep)}
	case *engine.FleetReport:
		rr.Report = &orbitv1.RunReport_Fleet{Fleet: fleetReportProto(*rep)}
	case *suiteLive:
		rr.Report = &orbitv1.RunReport_Conformance{Conformance: rep.summary()}
	}
	applyGates(rr, gates)
	if w.State != engine.WorkloadDone {
		// A workload that did not complete (failed, skipped, stopped)
		// measured nothing a gate can pass: any partial report is kept for
		// the record, and every gate on it fails, as an incomplete suite's
		// do.
		for _, g := range rr.Gates {
			g.Passed = false
			g.Detail = fmt.Sprintf("workload %s did not complete (%s)", w.Name, w.State)
			if w.Detail != "" {
				g.Detail += ": " + w.Detail
			}
		}
		rr.GatesPassed = len(rr.Gates) == 0
	}
	return rr
}

// judgedReports is a copy of a completed plan run's judged reports, while
// the registry keeps the run; nil otherwise. A copy, since callers marshal
// them or redact them in place while the registry's stay shared.
func judgedReports(reg *engine.RunRegistry, id string) []*orbitv1.RunReport {
	v, ok := reg.Judgement(id)
	if !ok {
		return nil
	}
	rs, _ := v.([]*orbitv1.RunReport)
	out := make([]*orbitv1.RunReport, 0, len(rs))
	for _, rr := range rs {
		out = append(out, proto.Clone(rr).(*orbitv1.RunReport))
	}
	return out
}

// planVerdict is a plan run's judged reports: from the registry, else from
// the run's archive.
func (s *runService) planVerdict(id string) []*orbitv1.RunReport {
	if rs := judgedReports(s.reg, id); len(rs) > 0 {
		return rs
	}
	if a, ok := s.archive.get(id); ok {
		return a.GetReports()
	}
	return nil
}

// markStoppedEarly marks reports judged on a run finished early
// (StopRun{finish}), so a reader knows the results were cut short.
func markStoppedEarly(rs []*orbitv1.RunReport, early bool) {
	for _, rr := range rs {
		rr.StoppedEarly = early
	}
}

// gatesOf is reports' gate results together, and whether all passed.
func gatesOf(rs []*orbitv1.RunReport) ([]*orbitv1.GateResult, bool) {
	var out []*orbitv1.GateResult
	passed := true
	for _, rr := range rs {
		out = append(out, rr.GetGates()...)
		passed = passed && rr.GetGatesPassed()
	}
	return out, passed
}
