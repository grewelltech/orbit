// Package armplan translates the old run arms (StartRunRequest's load and
// fleet specs) into one-workload RunPlans (ADR-0008 §2), exactly or not at
// all: Load and Fleet return a plan whose build is the arm's own engine
// spec, or ErrNoPlan when a plan cannot express the arm. The server runs
// an arm through it, and a client sends the plan instead of the arm.
package armplan

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/scenario"
	"github.com/bgrewell/orbit/internal/target"
)

// ErrNoPlan is an arm a plan cannot express exactly.
var ErrNoPlan = errors.New("not expressible as a plan")

// Workload is the workload name an old arm's plan gives its population.
const Workload = "arm"

// Subscribers is the subscriber pool an old arm's plan defines.
const Subscribers = "arm"

// Load is a load arm as a plan whose storm build is the arm's own
// engine spec: one gNB, or gNBs that share everything but their IDs
// (consecutive), names (numbered from one base) and addresses.
func Load(p *orbitv1.LoadRunSpec) (*orbitv1.RunPlan, error) {
	gnbs := p.GetGnbs()
	if len(gnbs) == 0 || p.GetCount() == 0 {
		return nil, ErrNoPlan
	}
	g0 := gnbs[0]
	name, err := armGNBName(gnbs)
	if err != nil {
		return nil, err
	}
	for i, g := range gnbs {
		if g.GetMcc() != g0.GetMcc() || g.GetMnc() != g0.GetMnc() || g.GetTac() != g0.GetTac() ||
			g.GetIdBits() != g0.GetIdBits() || !slices.EqualFunc(g.GetSlices(), g0.GetSlices(), snssaiEqual) {
			return nil, fmt.Errorf("gnb %d differs from gnb 0 in its PLMN, TAC, slices or ID length: %w", i, ErrNoPlan)
		}
		if g.GetId() != g0.GetId()+uint32(i) || g.GetId() < g0.GetId() {
			return nil, fmt.Errorf("gnb IDs are not consecutive: %w", ErrNoPlan)
		}
	}
	if g0.GetId() == 0 {
		return nil, fmt.Errorf("gnb ID 0: %w", ErrNoPlan) // a plan's first ID 0 means unset
	}
	n := len(gnbs)
	if !samePool(p.GetBindAddresses(), n) {
		return nil, fmt.Errorf("bind_addresses: %w", ErrNoPlan)
	}
	core := &orbitv1.TargetCore{Amf: p.GetAmfAddress(), Plmn: &orbitv1.Plmn{Mcc: g0.GetMcc(), Mnc: g0.GetMnc()},
		Tac: g0.GetTac(), Slices: g0.GetSlices()}
	ran := &orbitv1.TargetRAN{GnbName: name, N2SourceIps: p.GetBindAddresses(), GnbIds: &orbitv1.GNBIDRange{Bits: g0.GetIdBits()}}
	pop := &orbitv1.PopulationWorkload{
		Attach: &orbitv1.AttachProfile{Count: p.GetCount(), Rate: p.GetRate(), Concurrency: p.GetConcurrency()},
		Topology: &orbitv1.Topology{GnbCount: uint32(n),
			GnbIds: &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: g0.GetId()}}},
		SubscriberPool:   Subscribers,
		SampleIntervalMs: p.GetSampleIntervalMs(),
	}
	if p.GetRampStart() != 0 || p.GetRampEnd() != 0 || p.GetRampSeconds() != 0 {
		pop.Attach.Ramp = &orbitv1.Ramp{Start: p.GetRampStart(), End: p.GetRampEnd(), Seconds: p.GetRampSeconds()}
	}
	switch pdu := p.GetPduSession(); {
	case pdu != nil:
		// The plan's PDU session is session 1 on the first slice (SST 1
		// when there is none) and the target's DNN, from one N3 address.
		var sst uint32 = 1
		var sd string
		if len(core.GetSlices()) > 0 {
			sst, sd = core.GetSlices()[0].GetSst(), core.GetSlices()[0].GetSd()
		}
		if pdu.GetPduSessionId() != 1 || pdu.GetSst() != sst || pdu.GetSd() != sd || p.GetGnbN3Addr() == "" || !samePool([]string{p.GetGnbN3Addr()}, 1) {
			return nil, fmt.Errorf("pdu_session: %w", ErrNoPlan)
		}
		pop.PduSession = true
		core.Dnn = pdu.GetDnn()
		ran.N3Ips = []string{p.GetGnbN3Addr()}
	case p.GetGnbN3Addr() != "":
		return nil, fmt.Errorf("gnb_n3_addr without pdu_session: %w", ErrNoPlan)
	}
	plan := &orbitv1.RunPlan{
		Target: &orbitv1.ResolvedTarget{Core: core, Ran: ran, Subscribers: []*orbitv1.SubscriberPool{{
			Name: Subscribers, FirstSupi: p.GetBaseImsi(),
			Ki: p.GetCredentials().GetKi(), Opc: p.GetCredentials().GetOpc()}}},
		Workloads: []*orbitv1.Workload{{Name: Workload, Kind: &orbitv1.Workload_Population{Population: pop}}},
	}
	if d := p.GetDurationMs(); d > 0 {
		plan.Until = &orbitv1.Until{Stop: &orbitv1.Until_DurationMs{DurationMs: uint64(d)}}
	}
	return plan, nil
}

// armGNBName is the target gNB name that gives a load arm's gNBs their
// names (gnbName): the one gNB's own, or the base of "<base>-1" .. "-n".
func armGNBName(gnbs []*orbitv1.GnbConfig) (string, error) {
	if len(gnbs) == 1 {
		if gnbs[0].GetName() == "" {
			return "", fmt.Errorf("an unnamed gnb: %w", ErrNoPlan)
		}
		return gnbs[0].GetName(), nil
	}
	base, ok := strings.CutSuffix(gnbs[0].GetName(), "-1")
	if !ok || base == "" {
		return "", fmt.Errorf("gnb names are not <name>-1 .. -%d: %w", len(gnbs), ErrNoPlan)
	}
	for i, g := range gnbs {
		if g.GetName() != base+"-"+strconv.Itoa(i+1) {
			return "", fmt.Errorf("gnb names are not %s-1 .. -%d: %w", base, len(gnbs), ErrNoPlan)
		}
	}
	return base, nil
}

func snssaiEqual(a, b *orbitv1.Snssai) bool { return proto.Equal(a, b) }

// samePool reports whether addresses, as a target pool for n gNBs, expand
// to themselves: single addresses only, no more of them than n.
func samePool(addrs []string, n int) bool {
	if len(addrs) > n {
		return false
	}
	got, err := target.ExpandPool(addrs, n)
	return err == nil && slices.Equal(got, addrs)
}

// Fleet is a fleet scenario as a plan whose persistent build is the
// scenario's own engine spec. ki and opc are the request's credentials.
func Fleet(f *scenario.FleetScenario, ki, opc string) (*orbitv1.RunPlan, error) {
	g := f.Topology.GNBs
	if g.Count < 1 || g.IDBase == 0 || f.Fleet.Count < 1 {
		return nil, ErrNoPlan
	}
	if !samePool(g.SourceIPs, g.Count) || !samePool(g.N3IPs, g.Count) {
		return nil, fmt.Errorf("topology.gnbs addresses: %w", ErrNoPlan)
	}
	rate, err := scenario.ParseAttachRate(f.Fleet.AttachRate)
	if err != nil {
		return nil, err
	}
	beh, err := f.Behaviors.Proto()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoPlan, err)
	}
	dur, err := scenarioMs(f.Run.Duration)
	if err != nil {
		return nil, err
	}
	// A fleet names its gNBs gnb-1 .. gnb-n, numbered even when alone.
	name := "gnb"
	if g.Count == 1 {
		name = "gnb-1"
	}
	pop := &orbitv1.PopulationWorkload{
		Attach: &orbitv1.AttachProfile{Count: uint32(f.Fleet.Count), Rate: rate,
			Distribution: f.Fleet.Distribution, DistributionSeed: f.Fleet.DistributionSeed},
		Topology: &orbitv1.Topology{GnbCount: uint32(g.Count), Layout: g.Layout, SpacingM: g.Spacing,
			GnbIds: &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: g.IDBase}}},
		PduSession:     f.Fleet.PDUSession,
		Behaviors:      beh,
		SubscriberPool: Subscribers,
	}
	plan := &orbitv1.RunPlan{
		Target: &orbitv1.ResolvedTarget{
			Core: &orbitv1.TargetCore{Amf: f.Core.AMF, Plmn: &orbitv1.Plmn{Mcc: f.Core.PLMN.MCC, Mnc: f.Core.PLMN.MNC},
				Tac: f.Core.TAC, Dnn: f.Core.DNN, Slices: []*orbitv1.Snssai{{Sst: f.Core.Slice.SST, Sd: f.Core.Slice.SD}}},
			Ran:         &orbitv1.TargetRAN{GnbName: name, N2SourceIps: g.SourceIPs, N3Ips: g.N3IPs},
			Subscribers: []*orbitv1.SubscriberPool{{Name: Subscribers, FirstSupi: f.Fleet.SUPIBase, Ki: ki, Opc: opc}},
		},
		Workloads: []*orbitv1.Workload{{Name: f.Name, Kind: &orbitv1.Workload_Population{Population: pop}}},
	}
	if dur > 0 {
		plan.Until = &orbitv1.Until{Stop: &orbitv1.Until_DurationMs{DurationMs: dur}}
	}
	return plan, nil
}

// scenarioMs is a scenario duration as whole milliseconds ("" is 0).
func scenarioMs(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d%time.Millisecond != 0 {
		return 0, fmt.Errorf("run.duration %q: %w", s, ErrNoPlan)
	}
	return uint64(d / time.Millisecond), nil
}
