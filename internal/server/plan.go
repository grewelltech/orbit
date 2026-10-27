package server

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/scenario"
	"github.com/bgrewell/orbit/internal/target"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// errR3 and errR2 name the roadmap step that lifts a plan restriction, so
// a client knows the refusal is "not yet", not "never" (ADR-0008 §2).
var (
	errR3 = errors.New("is not supported until R3")
	errR2 = errors.New("is not supported until R2")
)

// defaultGNBIDBits is the gNB ID length when the target gives none, as on
// the CLI (--gnb-id-bits).
const defaultGNBIDBits = 24

// planRun is a RunPlan translated into today's engine specs (S2): one
// population workload, run by the load engine when it has no behaviours
// and by the fleet engine when it has. Exactly one of load and fleet is
// set. assigned are the gNB IDs taken from the rotation, to hand back
// when the run releases its leases.
type planRun struct {
	load     engine.LoadRunFunc
	fleet    engine.FleetRunFunc
	needs    engine.Needs
	workload string
	amf      string // the gNB IDs' AMF and PLMN, for the rotation
	plmn     string
	assigned []uint32
}

// population checks a plan's shape for S2 and returns its one population
// workload.
func population(p *orbitv1.RunPlan) (*orbitv1.Workload, *orbitv1.PopulationWorkload, error) {
	if len(p.GetWorkloads()) != 1 {
		return nil, nil, fmt.Errorf("a plan with %d workloads %w: give exactly one population workload", len(p.GetWorkloads()), errR3)
	}
	w := p.GetWorkloads()[0]
	if w.GetStart() != nil {
		return nil, nil, fmt.Errorf("workload %q: a start condition %w", w.GetName(), errR3)
	}
	pop := w.GetPopulation()
	if pop == nil {
		return nil, nil, fmt.Errorf("workload %q: a conformance or steps workload %w; give a population", w.GetName(), errR3)
	}
	if len(p.GetGates()) > 0 {
		return nil, nil, fmt.Errorf("gates %w", errR2)
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

// gnbIDs are the population's gNB IDs: pinned (topology.gnb_ids.first, or
// the target's ran.gnb_ids.first, or 1), or assigned from the rotation.
func (s *runService) gnbIDs(t *orbitv1.ResolvedTarget, pop *orbitv1.PopulationWorkload, amf, plmn string, n int) (ids []uint32, assigned bool, err error) {
	sel := pop.GetTopology().GetGnbIds()
	if sel.GetAssign() {
		if s.rotation == nil {
			return nil, false, errors.New("topology.gnb_ids.assign: this server has no gNB ID rotation")
		}
		ids, err := s.rotation.Assign(amf, plmn, n, s.reg.GNBInUse(amf, plmn))
		return ids, true, err
	}
	first := sel.GetFirst()
	if first == 0 {
		first = t.GetRan().GetGnbIds().GetFirst()
	}
	if first == 0 {
		first = 1
	}
	for i := 0; i < n; i++ {
		ids = append(ids, first+uint32(i))
	}
	return ids, false, nil
}

// buildPlan translates a plan (S2). The caller hands assigned IDs back to
// the rotation if the run then fails to start.
func (s *runService) buildPlan(p *orbitv1.RunPlan, verbosity engine.EventVerbosity) (*planRun, error) {
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
	if until.GetForever() {
		return nil, fmt.Errorf("until.forever %w", errR2)
	}
	att := pop.GetAttach()
	if att.GetArrival() == orbitv1.Arrival_ARRIVAL_POISSON || att.GetSeed() != 0 {
		return nil, fmt.Errorf("attach.arrival poisson and attach.seed %w", errR2)
	}
	count, err := planCount(pop, sp, until.GetDurationMs() > 0)
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
	ids, assigned, err := s.gnbIDs(t, pop, core.GetAmf(), plmn, gnbCount)
	if err != nil {
		return nil, err
	}
	out := &planRun{workload: w.GetName(), amf: core.GetAmf(), plmn: plmn}
	if assigned {
		out.assigned = ids
	}
	fail := func(err error) (*planRun, error) {
		s.giveBack(out)
		return nil, err
	}
	creds := &orbitv1.Credentials{Ki: sp.GetKi(), Opc: sp.GetOpc()}
	if pop.GetBehaviors() == nil {
		spec, err := planLoadSpec(t, pop, sp, creds, count, ids, n2, n3, until)
		if err != nil {
			return fail(err)
		}
		fn, needs, err := loadRun(s.log, spec)
		if err != nil {
			return fail(err)
		}
		out.load, out.needs = fn, needs
		return out, nil
	}
	if until.GetComplete() {
		return fail(errors.New("until.complete with behaviours never finishes: behaviours run until the run's duration"))
	}
	if att.GetConcurrency() != 0 || att.GetRamp() != nil {
		return fail(fmt.Errorf("attach.concurrency and attach.ramp for a population with behaviours %w", errR2))
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
	for i := range spec.GNBs {
		spec.GNBs[i].Config.ID = ids[i] // assigned IDs need not be contiguous
	}
	fn, needs, err := fleetRunFrom(s.log, s.mgr, spec, beh, verbosity)
	if err != nil {
		return fail(err)
	}
	out.fleet, out.needs = fn, needs
	return out, nil
}

// giveBack returns a plan's assigned gNB IDs to the rotation, for a plan
// that never started (its run's OnRelease does it otherwise).
func (s *runService) giveBack(r *planRun) {
	if r == nil {
		return
	}
	s.releaseIDs(r.amf, r.plmn, r.assigned)
	r.assigned = nil
}

// releaseIDs returns gNB IDs to the rotation.
func (s *runService) releaseIDs(amf, plmn string, ids []uint32) {
	if len(ids) == 0 || s.rotation == nil {
		return
	}
	if err := s.rotation.Released(amf, plmn, ids); err != nil {
		s.log.Warn("could not record released gNB IDs", "err", err)
	}
}

// planLoadSpec is a population without behaviours as today's load spec.
func planLoadSpec(t *orbitv1.ResolvedTarget, pop *orbitv1.PopulationWorkload, sp *orbitv1.SubscriberPool,
	creds *orbitv1.Credentials, count uint32, ids []uint32, n2, n3 []string, until *orbitv1.Until) (*orbitv1.LoadRunSpec, error) {
	core := t.GetCore()
	if until.GetDurationMs() > math.MaxUint32 {
		return nil, fmt.Errorf("until.duration_ms %d is longer than a load run takes (%d)", until.GetDurationMs(), uint64(math.MaxUint32))
	}
	bits := t.GetRan().GetGnbIds().GetBits()
	if bits == 0 {
		bits = defaultGNBIDBits
	}
	spec := &orbitv1.LoadRunSpec{
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
		name := t.GetRan().GetGnbName()
		if name == "" {
			name = "orbit-gnb"
		}
		if len(ids) > 1 {
			name += "-" + strconv.Itoa(i+1)
		}
		spec.Gnbs = append(spec.Gnbs, &orbitv1.GnbConfig{
			Id: id, IdBits: bits, Name: name,
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
	if len(r.assigned) == 0 {
		return nil
	}
	ids := make([]string, len(r.assigned))
	for i, id := range r.assigned {
		ids[i] = strconv.FormatUint(uint64(id), 10)
	}
	return []*orbitv1.FieldProvenance{{
		Path:   "workloads[" + r.workload + "].population.topology.gnb_ids",
		Source: &orbitv1.ProvenanceSource{Layer: orbitv1.ProvenanceLayer_PROVENANCE_LAYER_LEASE, Detail: "gNB ID rotation (least recently used) at " + r.amf},
		Value:  "[" + strings.Join(ids, ",") + "]",
	}}
}
