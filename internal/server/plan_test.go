package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	isctp "github.com/ishidawataru/sctp"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/secrets"
)

// planTarget is a target whose AMF fails fast (nothing listens), carrying
// the archive test's keys and responder token.
func planTarget() *orbitv1.ResolvedTarget {
	return &orbitv1.ResolvedTarget{
		Name: "lab1",
		Core: &orbitv1.TargetCore{Amf: "127.0.0.1:1", Plmn: &orbitv1.Plmn{Mcc: "208", Mnc: "93"}, Tac: 1,
			Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}}, Dnn: "internet"},
		Subscribers: []*orbitv1.SubscriberPool{{Name: "main", FirstSupi: "208930100007500", Count: 10, Ki: archKi, Opc: archOpc}},
		Ran:         &orbitv1.TargetRAN{N2SourceIps: []string{"127.0.0.1"}, N3Ips: []string{"127.0.0.1"}, GnbIds: &orbitv1.GNBIDRange{First: 7, Bits: 24}},
		App:         &orbitv1.TargetApp{Responders: []*orbitv1.Responder{{Name: "n6", Control: "127.0.0.1:9", Token: archToken, DataIp: "127.0.0.1"}}},
	}
}

// loadPlan is a one-population plan without behaviours.
func loadPlan(mut ...func(*orbitv1.RunPlan)) *orbitv1.RunPlan {
	p := &orbitv1.RunPlan{
		Target: planTarget(),
		Workloads: []*orbitv1.Workload{{Name: "pop", Kind: &orbitv1.Workload_Population{Population: &orbitv1.PopulationWorkload{
			Attach: &orbitv1.AttachProfile{Count: 2, Rate: 5, Concurrency: 2},
		}}}},
		Provenance: []*orbitv1.FieldProvenance{
			// The overridden value is a key that appears nowhere else in the
			// request: only the path guard can mask it, not the scrub.
			{Path: "subscribers[main].ki", Source: &orbitv1.ProvenanceSource{Layer: orbitv1.ProvenanceLayer_PROVENANCE_LAYER_TARGET, Detail: "lab1.yaml:9"}, Value: archKi,
				Overridden: []*orbitv1.ProvenanceOverride{{Value: oldKi}}},
			{Path: "core.plmn.mcc", Source: &orbitv1.ProvenanceSource{Layer: orbitv1.ProvenanceLayer_PROVENANCE_LAYER_TARGET}, Value: "208"},
		},
	}
	for _, m := range mut {
		m(p)
	}
	return p
}

// oldKi is a key only a provenance entry's overridden value carries.
const oldKi = "0f0e0d0c0b0a09080706050403020100"

func pop(p *orbitv1.RunPlan) *orbitv1.PopulationWorkload { return p.GetWorkloads()[0].GetPopulation() }

func planService(t *testing.T) *runService {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rot, err := engine.OpenGNBRotation("", log)
	if err != nil {
		t.Fatal(err)
	}
	return &runService{log: log, reg: engine.NewRunRegistry(log, 0), rotation: rot, specs: newSpecStore()}
}

// What S2 runs is one population workload; anything later steps add is
// refused, naming the step.
func TestPlanShapeRefusals(t *testing.T) {
	tests := []struct {
		name    string
		mut     func(*orbitv1.RunPlan)
		wantErr string
	}{
		{name: "no workloads", mut: func(p *orbitv1.RunPlan) { p.Workloads = nil }, wantErr: "a plan needs at least one workload"},
		{name: "two workloads", mut: func(p *orbitv1.RunPlan) { p.Workloads = append(p.Workloads, p.Workloads[0]) }, wantErr: "until R3"},
		{name: "a conformance workload", mut: func(p *orbitv1.RunPlan) {
			p.Workloads[0].Kind = &orbitv1.Workload_Conformance{Conformance: &orbitv1.ConformanceWorkload{}}
		}, wantErr: "until R3"},
		{name: "a start condition", mut: func(p *orbitv1.RunPlan) { p.Workloads[0].Start = &orbitv1.StartCondition{After: "x"} }, wantErr: "until R3"},
		{name: "gates", mut: func(p *orbitv1.RunPlan) { p.Gates = []*orbitv1.Gate{{Name: "g"}} }, wantErr: "until R2"},
		{name: "until forever", mut: func(p *orbitv1.RunPlan) { p.Until = &orbitv1.Until{Stop: &orbitv1.Until_Forever{Forever: true}} }, wantErr: "until R2"},
		{name: "poisson arrival", mut: func(p *orbitv1.RunPlan) { pop(p).Attach.Arrival = orbitv1.Arrival_ARRIVAL_POISSON }, wantErr: "until R2"},
		{name: "a seed", mut: func(p *orbitv1.RunPlan) { pop(p).Attach.Seed = 7 }, wantErr: "until R2"},
		{name: "behaviours with a ramp", mut: func(p *orbitv1.RunPlan) {
			pop(p).Behaviors = &orbitv1.Behaviors{Latency: &orbitv1.LatencyProbe{Target: "127.0.0.1"}}
			pop(p).Attach.Concurrency = 0
			pop(p).Attach.Ramp = &orbitv1.Ramp{Start: 1, End: 2, Seconds: 3}
		}, wantErr: "until R2"},
		{name: "behaviours until complete", mut: func(p *orbitv1.RunPlan) {
			pop(p).Behaviors = &orbitv1.Behaviors{Latency: &orbitv1.LatencyProbe{Target: "127.0.0.1"}}
			p.Until = &orbitv1.Until{Stop: &orbitv1.Until_Complete{Complete: true}}
		}, wantErr: "never finishes"},
		{name: "more UEs than the pool", mut: func(p *orbitv1.RunPlan) { pop(p).Attach.Count = 11 }, wantErr: "more than subscriber pool"},
		{name: "an unknown pool", mut: func(p *orbitv1.RunPlan) { pop(p).SubscriberPool = "other" }, wantErr: `no subscriber pool "other"`},
		{name: "no AMF", mut: func(p *orbitv1.RunPlan) { p.Target.Core.Amf = "" }, wantErr: "core.amf"},
		{name: "a bad key", mut: func(p *orbitv1.RunPlan) { p.Target.Subscribers[0].Ki = "zz" }, wantErr: "Ki"},
		{name: "an unknown responder", mut: func(p *orbitv1.RunPlan) {
			pop(p).Attach.Concurrency = 0
			pop(p).Behaviors = &orbitv1.Behaviors{Traffic: &orbitv1.Traffic{Mix: []*orbitv1.TrafficShare{{Name: "web", Share: 1,
				Kind: &orbitv1.TrafficShare_App{App: &orbitv1.AppCohort{App: "http", Responder: "nope"}}}}}}
		}, wantErr: `no responder "nope"`},
		{name: "a SUPI pool outside the PLMN", mut: func(p *orbitv1.RunPlan) {
			pop(p).Attach.Concurrency = 0
			pop(p).Behaviors = &orbitv1.Behaviors{Latency: &orbitv1.LatencyProbe{Target: "127.0.0.1"}}
			p.Target.Subscribers[0].FirstSupi = "001010000000001"
		}, wantErr: "supi_base"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planService(t).buildPlan(loadPlan(tc.mut), engine.EventsNormal)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// A population without behaviours becomes today's load spec, field by field.
func TestPlanLoadTranslation(t *testing.T) {
	p := loadPlan(func(p *orbitv1.RunPlan) {
		pop(p).PduSession = true
		pop(p).SampleIntervalMs = 500
		pop(p).Topology = &orbitv1.Topology{GnbCount: 2}
		pop(p).Attach.Ramp = &orbitv1.Ramp{Start: 1, End: 10, Seconds: 30}
		p.Target.Ran.N2SourceIps = []string{"127.0.0.10-127.0.0.20"}
		p.Until = &orbitv1.Until{Stop: &orbitv1.Until_DurationMs{DurationMs: 60000}}
	})
	got, err := planLoadSpec(p.GetTarget(), pop(p), p.GetTarget().GetSubscribers()[0],
		&orbitv1.Credentials{Ki: archKi, Opc: archOpc}, 2, []uint32{7, 8}, []string{"127.0.0.10", "127.0.0.11"}, []string{"127.0.0.1"}, p.GetUntil())
	if err != nil {
		t.Fatal(err)
	}
	gnb := func(id uint32, name string) *orbitv1.GnbConfig {
		return &orbitv1.GnbConfig{Id: id, IdBits: 24, Name: name, Mcc: "208", Mnc: "93", Tac: 1, Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}}}
	}
	want := &orbitv1.LoadRunSpec{
		AmfAddress: "127.0.0.1:1", Gnbs: []*orbitv1.GnbConfig{gnb(7, "orbit-gnb-1"), gnb(8, "orbit-gnb-2")},
		BaseImsi: "208930100007500", Count: 2, Credentials: &orbitv1.Credentials{Ki: archKi, Opc: archOpc},
		Rate: 5, RampStart: 1, RampEnd: 10, RampSeconds: 30, Concurrency: 2, DurationMs: 60000, SampleIntervalMs: 500,
		PduSession: &orbitv1.PDUSession{PduSessionId: 1, Sst: 1, Sd: "010203", Dnn: "internet"},
		GnbN3Addr:  "127.0.0.1", BindAddresses: []string{"127.0.0.10", "127.0.0.11"},
	}
	if !proto.Equal(got, want) {
		t.Errorf("load spec =\n%v\nwant\n%v", got, want)
	}
}

// A population with behaviours becomes today's fleet scenario; a named
// responder fills the cohort's and bulk traffic's peer, token and data IP.
func TestPlanFleetTranslation(t *testing.T) {
	p := loadPlan(func(p *orbitv1.RunPlan) {
		pop(p).Attach.Concurrency = 0
		pop(p).PduSession = true
		pop(p).Topology = &orbitv1.Topology{GnbCount: 1, Layout: "grid"}
		pop(p).Behaviors = &orbitv1.Behaviors{
			Traffic: &orbitv1.Traffic{
				Mix: []*orbitv1.TrafficShare{{Name: "web", Share: 1, Kind: &orbitv1.TrafficShare_App{App: &orbitv1.AppCohort{
					App: "http", Responder: "n6", StartAfterMs: 2000, Params: map[string]string{"object_size": "256KB"}}}}},
				Bulk: &orbitv1.BulkTraffic{Ues: 1, UplinkBps: 1_000_000, Responder: "n6"},
			},
			Latency: &orbitv1.LatencyProbe{Target: "127.0.0.1", IntervalMs: 1000},
		}
		p.Until = &orbitv1.Until{Stop: &orbitv1.Until_DurationMs{DurationMs: 30000}}
	})
	w, popw, err := population(p)
	if err != nil {
		t.Fatal(err)
	}
	f, err := planFleetScenario(w, p.GetTarget(), popw, p.GetTarget().GetSubscribers()[0], 2, 7, []string{"127.0.0.1"}, []string{"127.0.0.1"}, p.GetUntil())
	if err != nil {
		t.Fatal(err)
	}
	cohort := f.Behaviors.Traffic.Mix[0]
	if cohort.Peer != "127.0.0.9" && cohort.Peer != "127.0.0.1:9" || cohort.Token != archToken || cohort.PeerDataIP != "127.0.0.1" || cohort.StartAfter != "2000ms" {
		t.Errorf("cohort = %+v", cohort)
	}
	if b := f.Behaviors.Traffic.Bulk; b.Peer != "127.0.0.1:9" || b.Token != archToken || b.UplinkRate != "1Mbps" {
		t.Errorf("bulk = %+v", b)
	}
	if f.Fleet.Count != 2 || f.Fleet.SUPIBase != "208930100007500" || f.Topology.GNBs.IDBase != 7 || f.Run.Duration != "30000ms" || !f.Fleet.PDUSession {
		t.Errorf("fleet scenario = %+v", f)
	}
	// And it builds: the scenario parser and BuildFleetRun accept it.
	pr, err := planService(t).buildPlan(p, engine.EventsNormal)
	if err != nil || pr.fleet == nil {
		t.Fatalf("buildPlan = %+v, %v", pr, err)
	}
}

// A timed run without attach.count uses the whole pool, so its SUPI lease
// is bounded by the pool (S4's open-ended soak span, closed).
func TestPlanSoakLeaseBoundedByPool(t *testing.T) {
	p := loadPlan(func(p *orbitv1.RunPlan) {
		pop(p).Attach.Count = 0
		p.Until = &orbitv1.Until{Stop: &orbitv1.Until_DurationMs{DurationMs: 60000}}
	})
	pr, err := planService(t).buildPlan(p, engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.needs.SUPIs) != 1 || pr.needs.SUPIs[0].Last-pr.needs.SUPIs[0].First != 9 {
		t.Errorf("SUPI lease = %+v, want the pool's 10", pr.needs.SUPIs)
	}
}

// gnb_ids.assign takes IDs from the rotation: two plans get different IDs,
// and a run's IDs return to the rotation when it ends.
func TestPlanAssignsGNBIDs(t *testing.T) {
	s := planService(t)
	assign := func(p *orbitv1.RunPlan) {
		pop(p).Topology = &orbitv1.Topology{GnbCount: 2, GnbIds: &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_Assign{Assign: true}}}
	}
	a, err := s.buildPlan(loadPlan(assign), engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.buildPlan(loadPlan(assign), engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.assigned) != 2 || len(b.assigned) != 2 || a.assigned[0] == b.assigned[0] || a.assigned[1] == b.assigned[1] {
		t.Fatalf("assigned %v and %v, want two distinct pairs", a.assigned, b.assigned)
	}
	if got := a.provenance(); len(got) != 1 || got[0].GetSource().GetLayer() != orbitv1.ProvenanceLayer_PROVENANCE_LAYER_LEASE {
		t.Errorf("provenance = %v", got)
	}
	s.giveBack(a)
	s.giveBack(b)
	if a.assigned != nil {
		t.Error("giveBack left the IDs assigned")
	}
}

// S2's done-when: a plan carrying keys and a responder token, run through
// the server, leaves no key in the archive's bytes or in any reply; the
// archive holds the masked plan, its provenance (the server's lease entry
// included) and the leases.
func TestPlanArchiveHoldsNoSecrets(t *testing.T) {
	dir := t.TempDir()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", prometheus.NewRegistry(), Options{StateDir: dir})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL)

	// A one-ID block: a second plan can only get the ID if the first run
	// released it.
	p := loadPlan(func(p *orbitv1.RunPlan) {
		p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{First: 7, Count: 1, Bits: 24}
		pop(p).Topology = &orbitv1.Topology{GnbIds: &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_Assign{Assign: true}}}
	})
	resp, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Name: "plan-run", Spec: &orbitv1.StartRunRequest_Plan{Plan: p}}))
	if err != nil {
		t.Fatalf("StartRun(plan): %v", err)
	}
	id := resp.Msg.GetRun().GetRunId()
	keys := []string{archKi, archOpc, archToken, oldKi}
	noKeys := func(what string, b []byte) {
		t.Helper()
		for _, k := range keys {
			if bytes.Contains(b, []byte(k)) {
				t.Errorf("%s holds %q", what, k)
			}
		}
	}
	path := filepath.Join(dir, id+archiveExt)
	var raw []byte
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			raw = b
			break
		}
	}
	if raw == nil {
		t.Fatal("the plan run was never archived")
	}
	noKeys(".orbitrun", raw)
	for _, call := range []func() (proto.Message, error){
		func() (proto.Message, error) {
			r, err := c.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
			return r.Msg, err
		},
		func() (proto.Message, error) {
			r, err := c.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{}))
			return r.Msg, err
		},
	} {
		m, err := call()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := proto.Marshal(m)
		noKeys("a reply", b)
		j, _ := json.Marshal(m)
		noKeys("a reply as JSON", j)
	}

	a, err := readArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.GetPlan() == nil || a.GetPlan().GetTarget().GetSubscribers()[0].GetKi() != secrets.Masked {
		t.Errorf("archived plan = %v, want it with the key masked", a.GetPlan())
	}
	var sawKiProv, sawLease bool
	for _, pv := range a.GetProvenance() {
		switch {
		case pv.GetPath() == "subscribers[main].ki":
			sawKiProv = pv.GetValue() == secrets.Masked
		case pv.GetSource().GetLayer() == orbitv1.ProvenanceLayer_PROVENANCE_LAYER_LEASE:
			sawLease = true
		}
	}
	if !sawKiProv || !sawLease {
		t.Errorf("archived provenance = %v; want the masked ki entry and the server's lease entry", a.GetProvenance())
	}
	var resources []orbitv1.LeaseResource
	for _, l := range a.GetLeases() {
		resources = append(resources, l.GetResource())
	}
	if len(resources) < 2 {
		t.Errorf("archived leases = %v, want the SUPI block and the gNB ID at least", a.GetLeases())
	}
	// The assigned gNB ID went back to the rotation when the run ended:
	// the one-ID block can be assigned again.
	if _, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: p}})); err != nil {
		t.Errorf("the block's only ID after the first run ended: %v", err)
	}
}

// Assigned and pinned gNB IDs both come from the target's ran.gnb_ids
// block when it has one; an exhausted block is FAILED_PRECONDITION naming
// it, and a pinned block outside it is refused.
func TestPlanGNBIDRange(t *testing.T) {
	ranged := func(first, count uint32) func(*orbitv1.RunPlan) {
		return func(p *orbitv1.RunPlan) {
			p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{First: first, Count: count, Bits: 24}
		}
	}
	assign := func(n uint32) func(*orbitv1.RunPlan) {
		return func(p *orbitv1.RunPlan) {
			pop(p).Topology = &orbitv1.Topology{GnbCount: n, GnbIds: &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_Assign{Assign: true}}}
			p.Target.Ran.N2SourceIps = []string{"127.0.0.10-127.0.0.40"}
		}
	}
	s := planService(t)
	a, err := s.buildPlan(loadPlan(ranged(5000, 20), assign(6)), engine.EventsNormal)
	if err != nil || a.assigned[0] != 5000 || a.assigned[5] != 5005 {
		t.Fatalf("assign in 5000..5019: %+v, %v", a, err)
	}
	// A later, narrower block on the same AMF draws only from it.
	b, err := s.buildPlan(loadPlan(ranged(5004, 4), assign(2)), engine.EventsNormal)
	if err != nil || b.assigned[0] != 5006 || b.assigned[1] != 5007 {
		t.Fatalf("assign in 5004..5007: %+v, %v", b, err)
	}
	if _, err := s.buildPlan(loadPlan(ranged(5004, 4), assign(1)), engine.EventsNormal); !errors.Is(err, engine.ErrGNBRangeExhausted) {
		t.Errorf("an exhausted block: err = %v, want ErrGNBRangeExhausted", err)
	}
	// Pinned: inside the block is fine, outside it is refused.
	pinned := func(first, n uint32) func(*orbitv1.RunPlan) {
		return func(p *orbitv1.RunPlan) {
			pop(p).Topology = &orbitv1.Topology{GnbCount: n, GnbIds: &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: first}}}
			p.Target.Ran.N2SourceIps = []string{"127.0.0.10-127.0.0.40"}
		}
	}
	if _, err := s.buildPlan(loadPlan(ranged(100, 4), pinned(101, 3)), engine.EventsNormal); err != nil {
		t.Errorf("pinned 101..103 in 100..103: %v", err)
	}
	if _, err := s.buildPlan(loadPlan(ranged(100, 1), pinned(100, 3)), engine.EventsNormal); err == nil || !strings.Contains(err.Error(), "outside the target's ran.gnb_ids 100..100") {
		t.Errorf("pinned 100..102 in 100..100: err = %v", err)
	}
	// No block: only the ID length bounds a pinned block.
	if _, err := s.buildPlan(loadPlan(func(p *orbitv1.RunPlan) { p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{Bits: 22} }, pinned(1<<22-1, 2)), engine.EventsNormal); err == nil || !strings.Contains(err.Error(), "do not fit 22 bits") {
		t.Errorf("pinned past 22 bits: err = %v", err)
	}
}

// The fleet path builds its gNBs as the load path does for the same
// target: the target's ID length and gNB name, numbered.
func TestPlanFleetGNBsMatchLoad(t *testing.T) {
	target := func(p *orbitv1.RunPlan) {
		p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{First: 40, Count: 10, Bits: 28}
		p.Target.Ran.GnbName = "lab-gnb"
		p.Target.Ran.N2SourceIps = []string{"127.0.0.10", "127.0.0.11"}
		pop(p).Topology = &orbitv1.Topology{GnbCount: 2}
		pop(p).Attach.Concurrency = 0
	}
	lp := loadPlan(target)
	ls, err := planLoadSpec(lp.GetTarget(), pop(lp), lp.GetTarget().GetSubscribers()[0],
		&orbitv1.Credentials{}, 2, []uint32{40, 41}, []string{"127.0.0.10", "127.0.0.11"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fp := loadPlan(target, func(p *orbitv1.RunPlan) {
		pop(p).Behaviors = &orbitv1.Behaviors{Latency: &orbitv1.LatencyProbe{Target: "127.0.0.1"}}
	})
	pr, err := planService(t).buildPlan(fp, engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	got := pr.built.GNBs
	if len(got) != 2 {
		t.Fatalf("fleet gNBs = %v, want 2", got)
	}
	for i, g := range got {
		want := ls.GetGnbs()[i]
		if g.Config.ID != want.GetId() || uint32(g.Config.IDBits) != want.GetIdBits() || g.Config.Name != want.GetName() {
			t.Errorf("fleet gNB %d = id %d bits %d name %q; load gives id %d bits %d name %q",
				i, g.Config.ID, g.Config.IDBits, g.Config.Name, want.GetId(), want.GetIdBits(), want.GetName())
		}
	}
}

// idleAMF is an SCTP listener that accepts associations and never answers,
// so a run against it stays up until it is stopped.
func idleAMF(t *testing.T) string {
	t.Helper()
	ln, err := isctp.ListenSCTPExt("sctp",
		&isctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}},
		isctp.InitMsg{NumOstreams: sctp.DefaultNGAPStreams, MaxInstreams: sctp.DefaultNGAPStreams})
	if err != nil {
		t.Skipf("no kernel SCTP on this host: %v", err)
	}
	var conns []*isctp.SCTPConn
	var mu sync.Mutex
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.AcceptSCTP()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	return ln.Addr().String()
}

// A load plan without assign that runs until StopRun (the tester's case)
// archives the client's provenance, masked by path, with every other
// entry as sent.
func TestPlanStoppedRunArchivesProvenance(t *testing.T) {
	amf := idleAMF(t)
	dir := t.TempDir()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", prometheus.NewRegistry(), Options{StateDir: dir})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL)
	p := loadPlan(func(p *orbitv1.RunPlan) {
		p.Target.Core.Amf = amf
		p.Until = &orbitv1.Until{Stop: &orbitv1.Until_DurationMs{DurationMs: 60000}}
		p.Provenance = append(p.Provenance,
			&orbitv1.FieldProvenance{Path: "app.responders[n6].token", Source: &orbitv1.ProvenanceSource{Layer: orbitv1.ProvenanceLayer_PROVENANCE_LAYER_ENV, Detail: "$ORBIT_TOKEN"}, Value: archToken},
			&orbitv1.FieldProvenance{Path: "workloads[pop].attach.rate", Source: &orbitv1.ProvenanceSource{Layer: orbitv1.ProvenanceLayer_PROVENANCE_LAYER_FLAG, Detail: "--rate"}, Value: "5"})
	})
	resp, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: p}}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetRun().GetRunId()
	waitRunState(t, c, id, orbitv1.RunState_RUN_STATE_RUNNING)
	g, err := c.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
	if err != nil || len(g.Msg.GetLeases()) == 0 {
		t.Fatalf("GetRun while active: leases %v, %v", g.Msg.GetLeases(), err)
	}
	if _, err := c.StopRun(context.Background(), connect.NewRequest(&orbitv1.StopRunRequest{RunId: id})); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+archiveExt)
	var raw []byte
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			raw = b
			break
		}
	}
	if raw == nil {
		t.Fatal("the stopped run was never archived")
	}
	for _, k := range []string{archKi, archOpc, archToken, oldKi} {
		if bytes.Contains(raw, []byte(k)) {
			t.Errorf(".orbitrun holds %q", k)
		}
	}
	a, err := readArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, pv := range a.GetProvenance() {
		got[pv.GetPath()] = pv.GetValue()
	}
	want := map[string]string{
		"subscribers[main].ki":       secrets.Masked,
		"core.plmn.mcc":              "208",
		"app.responders[n6].token":   secrets.Masked,
		"workloads[pop].attach.rate": "5",
	}
	for path, v := range want {
		if got[path] != v {
			t.Errorf("archived provenance %s = %q, want %q (all: %v)", path, got[path], v, got)
		}
	}
	if a.GetRun().GetState() != orbitv1.RunState_RUN_STATE_CANCELLED || len(a.GetLeases()) == 0 {
		t.Errorf("archive run %v, leases %v", a.GetRun(), a.GetLeases())
	}
}
