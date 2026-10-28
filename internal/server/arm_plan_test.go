package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/armplan"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/scenario"
	"github.com/bgrewell/orbit/internal/target"
)

// armGNB is gNB i of a homogeneous load arm.
func armGNB(id uint32, name string) *orbitv1.GnbConfig {
	return &orbitv1.GnbConfig{Id: id, Name: name, Mcc: "208", Mnc: "93", Tac: 1, Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}}}
}

// armLoad is a load arm with one gNB, changed by each mut.
func armLoad(mut ...func(*orbitv1.LoadRunSpec)) *orbitv1.LoadRunSpec { //nolint:staticcheck // the deprecated arm, kept for API clients
	p := &orbitv1.LoadRunSpec{ //nolint:staticcheck // the deprecated arm, kept for API clients
		AmfAddress: "127.0.0.1:38412", Gnbs: []*orbitv1.GnbConfig{armGNB(7, "orbit-gnb")},
		BaseImsi: "208930100007500", Count: 4, Credentials: &orbitv1.Credentials{Ki: archKi, Opc: archOpc},
		Rate: 5, Concurrency: 2,
	}
	for _, m := range mut {
		m(p)
	}
	return p
}

// threeGNBs gives an arm three gNBs, numbered from one base name.
func threeGNBs(p *orbitv1.LoadRunSpec) { //nolint:staticcheck // the deprecated arm, kept for API clients
	p.Gnbs = []*orbitv1.GnbConfig{armGNB(7, "lab-1"), armGNB(8, "lab-2"), armGNB(9, "lab-3")}
}

// normGNBBits writes a gNB ID length of 0 as the 24 it means (gnb.Config),
// the length a plan's gNBs carry.
func normGNBBits(gnbs []engine.GNBSpec) {
	for i := range gnbs {
		if gnbs[i].Config.IDBits == 0 {
			gnbs[i].Config.IDBits = 24
		}
	}
}

// Golden: an old load arm a plan can express runs as that plan, and the
// plan builds the arm's own engine spec field by field. One a plan cannot
// express (gNBs differing in more than ID, name number and address) runs
// from its own spec, exactly today's.
func TestLoadArmGolden(t *testing.T) {
	tests := []struct {
		name    string
		arm     *orbitv1.LoadRunSpec //nolint:staticcheck // the deprecated arm, kept for API clients
		viaPlan bool
	}{
		{name: "one gNB", arm: armLoad(), viaPlan: true},
		{name: "three gNBs, one bind address each", arm: armLoad(threeGNBs, func(p *orbitv1.LoadRunSpec) { //nolint:staticcheck // the deprecated arm, kept for API clients
			p.BindAddresses = []string{"127.0.0.10", "127.0.0.11", "127.0.0.12"}
		}), viaPlan: true},
		{name: "three gNBs, a shared bind address", arm: armLoad(threeGNBs, func(p *orbitv1.LoadRunSpec) { //nolint:staticcheck // the deprecated arm, kept for API clients
			p.BindAddresses = []string{"127.0.0.10"}
		}), viaPlan: true},
		{name: "a ramped soak with resource samples", arm: armLoad(func(p *orbitv1.LoadRunSpec) { //nolint:staticcheck // the deprecated arm, kept for API clients
			p.RampStart, p.RampEnd, p.RampSeconds = 1, 10, 30
			p.DurationMs, p.SampleIntervalMs = 60000, 500
		}), viaPlan: true},
		{name: "a PDU session", arm: armLoad(func(p *orbitv1.LoadRunSpec) { //nolint:staticcheck // the deprecated arm, kept for API clients
			p.PduSession = &orbitv1.PDUSession{PduSessionId: 1, Sst: 1, Sd: "010203", Dnn: "internet"}
			p.GnbN3Addr = "127.0.0.1"
		}), viaPlan: true},
		{name: "a 22-bit gNB ID", arm: armLoad(func(p *orbitv1.LoadRunSpec) { p.Gnbs[0].IdBits = 22 }), viaPlan: true}, //nolint:staticcheck // the deprecated arm, kept for API clients
		// Heterogeneous: today's spec, from the arm itself.
		{name: "gNBs in different TACs", arm: armLoad(threeGNBs, func(p *orbitv1.LoadRunSpec) { p.Gnbs[1].Tac = 2 })}, //nolint:staticcheck // the deprecated arm, kept for API clients
		{name: "gNBs with different slices", arm: armLoad(threeGNBs, func(p *orbitv1.LoadRunSpec) { //nolint:staticcheck // the deprecated arm, kept for API clients
			p.Gnbs[2].Slices = []*orbitv1.Snssai{{Sst: 1}}
		})},
		{name: "gNB IDs not consecutive", arm: armLoad(threeGNBs, func(p *orbitv1.LoadRunSpec) { p.Gnbs[2].Id = 20 })},         //nolint:staticcheck // the deprecated arm, kept for API clients
		{name: "gNB names not one base", arm: armLoad(threeGNBs, func(p *orbitv1.LoadRunSpec) { p.Gnbs[1].Name = "other-2" })}, //nolint:staticcheck // the deprecated arm, kept for API clients
		{name: "a second PDU session ID", arm: armLoad(func(p *orbitv1.LoadRunSpec) { //nolint:staticcheck // the deprecated arm, kept for API clients
			p.PduSession = &orbitv1.PDUSession{PduSessionId: 2, Sst: 1, Sd: "010203", Dnn: "internet"}
			p.GnbN3Addr = "127.0.0.1"
		})},
		{name: "a bind address range", arm: armLoad(threeGNBs, func(p *orbitv1.LoadRunSpec) { //nolint:staticcheck // the deprecated arm, kept for API clients
			p.BindAddresses = []string{"127.0.0.10", "127.0.0.11", "127.0.0.12-127.0.0.13"}
		})},
	}
	s := planService(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want, err := loadSpecFrom(tc.arm)
			if err != nil {
				t.Fatalf("the arm itself: %v", err)
			}
			got, err := s.loadArm(tc.arm, engine.EventsNormal)
			if err != nil {
				t.Fatal(err)
			}
			if got.viaPlan != tc.viaPlan {
				t.Fatalf("viaPlan = %t, want %t", got.viaPlan, tc.viaPlan)
			}
			normGNBBits(want.GNBs)
			normGNBBits(got.storm.GNBs)
			if !reflect.DeepEqual(got.storm, want) {
				t.Errorf("engine spec =\n%+v\nwant today's\n%+v", got.storm, want)
			}
			if _, err := armplan.Load(tc.arm); (err == nil) != tc.viaPlan || err != nil && !errors.Is(err, armplan.ErrNoPlan) {
				t.Errorf("loadArmPlan err = %v, want armplan.ErrNoPlan only off the plan", err)
			}
		})
	}
}

// Golden, the other way: a plan's storm population, as today's load arm
// (planLoadSpec), translates back to a plan that builds the same spec.
func TestLoadArmPlanRoundTrip(t *testing.T) {
	p := loadPlan(func(p *orbitv1.RunPlan) {
		pop(p).PduSession = true
		pop(p).SampleIntervalMs = 500
		pop(p).Topology = &orbitv1.Topology{GnbCount: 2, GnbIds: &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: 7}}}
		pop(p).Attach.Ramp = &orbitv1.Ramp{Start: 1, End: 10, Seconds: 30}
		p.Target.Ran.N2SourceIps = []string{"127.0.0.10-127.0.0.20"}
		p.Until = &orbitv1.Until{Stop: &orbitv1.Until_DurationMs{DurationMs: 60000}}
	})
	s := planService(t)
	direct, err := s.buildPlan(p, engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	arm, err := planLoadSpec(p.GetTarget(), pop(p), p.GetTarget().GetSubscribers()[0],
		&orbitv1.Credentials{Ki: archKi, Opc: archOpc}, 2, []uint32{7, 8}, []string{"127.0.0.10", "127.0.0.11"}, []string{"127.0.0.1"}, p.GetUntil())
	if err != nil {
		t.Fatal(err)
	}
	back, err := s.loadArm(arm, engine.EventsNormal)
	if err != nil || !back.viaPlan {
		t.Fatalf("the plan's own load arm: viaPlan %t, %v", back.viaPlan, err)
	}
	if !reflect.DeepEqual(back.storm, direct.storm) {
		t.Errorf("round trip spec =\n%+v\nwant\n%+v", back.storm, direct.storm)
	}
	again, err := armplan.Load(arm)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(again.GetWorkloads()[0].GetPopulation().GetAttach(), pop(p).GetAttach()) {
		t.Errorf("attach = %v, want %v", again.GetWorkloads()[0].GetPopulation().GetAttach(), pop(p).GetAttach())
	}
}

// fleetTargetYAML is a target the shipped fleet examples resolve against.
const fleetTargetYAML = `version: 1
kind: target
name: fixture
core:
  amf: 192.0.2.10:38412
  plmn: { mcc: "001", mnc: "01" }
  tac: 1
  slices:
    - { sst: 1, sd: "010203" }
  dnn: internet
subscribers:
  - name: main
    first_supi: "001010100007500"
    count: 1000
    ki: 00112233445566778899aabbccddeeff
    opc: 000102030405060708090a0b0c0d0e0f
ran:
  n2_source_ips: [192.0.2.20-192.0.2.29]
  n3_ips: [198.51.100.20-198.51.100.29]
  gnb_ids: { first: 100 }
app:
  responders:
    - { name: n6, control: 203.0.113.30:9551, token: ` + archToken + `, data_ip: 203.0.113.30 }
`

func fleetFixture(t *testing.T) target.Target {
	t.Helper()
	files, err := target.Parse("fixture.yaml", []byte(fleetTargetYAML), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	r, err := target.Resolve(target.DefaultInput(), files[0].Input)
	if err != nil {
		t.Fatal(err)
	}
	return r.Target
}

func normFleetBits(spec *engine.FleetRunSpec) {
	for i := range spec.GNBs {
		if spec.GNBs[i].Config.IDBits == 0 {
			spec.GNBs[i].Config.IDBits = 24
		}
	}
}

// normParams writes a cohort's empty params as none: the engine reads
// them by key, and a scenario's text round trip turns none into {}.
func normParams(b *engine.FleetBehaviors) {
	for i := range b.Apps {
		if len(b.Apps[i].Params) == 0 {
			b.Apps[i].Params = nil
		}
	}
}

// Golden: every shipped fleet example, resolved against a target, runs as
// its plan, and the plan builds the scenario's own engine spec and
// behaviours field by field. A fleet without behaviours stays persistent.
func TestFleetArmGolden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	type doc struct {
		name string
		data []byte
	}
	var docs []doc
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if kind, err := scenario.PeekKind(data); err == nil && kind == "fleet" {
			docs = append(docs, doc{filepath.Base(path), data})
		}
	}
	if len(docs) < 5 {
		t.Fatalf("found %d fleet examples; the path is wrong", len(docs))
	}
	docs = append(docs, doc{"no behaviours", []byte("kind: fleet\nname: bare\ntopology:\n  gnbs: { count: 2 }\nfleet: { count: 4, attach_rate: 2/s }\n")})
	tg := fleetFixture(t)
	creds := &orbitv1.Credentials{Ki: archKi, Opc: archOpc}
	s := planService(t)
	for _, d := range docs {
		t.Run(d.name, func(t *testing.T) {
			f, err := scenario.ParseFleetWithTarget(d.data, &tg)
			if err != nil {
				t.Fatal(err)
			}
			wantSpec, wantBeh, err := fleetSpecFrom(f, creds)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := armplan.Fleet(f, creds.GetKi(), creds.GetOpc())
			if err != nil {
				t.Fatalf("fleetArmPlan: %v", err)
			}
			got, err := s.buildPlanAs(plan, engine.EventsNormal, true)
			if err != nil {
				t.Fatalf("the plan does not build: %v", err)
			}
			normFleetBits(&wantSpec)
			normFleetBits(&got.built)
			if f.Fleet.Distribution == scenario.DistributionUneven && f.Fleet.DistributionSeed == 0 {
				// Unseeded, the layout is fresh every build: compare its size.
				if len(got.built.GNBAssign) != len(wantSpec.GNBAssign) {
					t.Errorf("%d UEs placed, want %d", len(got.built.GNBAssign), len(wantSpec.GNBAssign))
				}
				got.built.GNBAssign, wantSpec.GNBAssign = nil, nil
			}
			normParams(&got.beh)
			normParams(&wantBeh)
			if !reflect.DeepEqual(got.built, wantSpec) {
				t.Errorf("engine spec =\n%+v\nwant today's\n%+v", got.built, wantSpec)
			}
			if !reflect.DeepEqual(got.beh, wantBeh) {
				t.Errorf("behaviours =\n%+v\nwant today's\n%+v", got.beh, wantBeh)
			}
			// And StartRun's fleet arm, given the resolved scenario as a
			// client sends it, takes the plan's path.
			f.Credentials = scenario.Credentials{}
			doc, err := yaml.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			arm, err := s.fleetArm(&orbitv1.FleetRunSpec{ScenarioYaml: string(doc), Credentials: creds}, engine.EventsNormal) //nolint:staticcheck // the deprecated arm, kept for API clients
			if err != nil || !arm.viaPlan {
				t.Errorf("fleetArm: viaPlan %t, %v", arm.viaPlan, err)
			}
		})
	}
}

// A fleet whose text the typed behaviours would rewrite runs from its
// scenario, as today.
func TestFleetArmOffThePlan(t *testing.T) {
	tg := fleetFixture(t)
	base := "kind: fleet\nname: f\ntopology:\n  gnbs: { count: 1 }\nfleet: { count: 2, pdu_session: true }\n"
	tests := []struct{ name, doc string }{
		{"a bulk rate the plan writes another way", base + "behaviors:\n  traffic:\n    bulk: { ues: 1, uplink_rate: 1000Kbps }\n"},
		{"a sub-millisecond duration", base + "run: { duration: 1500us }\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := scenario.ParseFleetWithTarget([]byte(tc.doc), &tg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := armplan.Fleet(f, archKi, archOpc); !errors.Is(err, armplan.ErrNoPlan) {
				t.Errorf("fleetArmPlan err = %v, want armplan.ErrNoPlan", err)
			}
		})
	}
}

// An old fleet arm without behaviours runs as a plan but stays a FLEET run
// everywhere a client sees its kind: StartRun, GetRun, ListRuns' filter and
// the archive (a plan's own population without behaviours is LOAD).
func TestFleetArmWithoutBehavioursIsFleet(t *testing.T) {
	dir := t.TempDir()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", prometheus.NewRegistry(), Options{StateDir: dir, Responders: openResponders(t, dir)})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL)

	if arm, err := planService(t).fleetArm(fleetSpec(), engine.EventsNormal); err != nil || !arm.viaPlan {
		t.Fatalf("the behaviour-less fleet arm: viaPlan %t, %v; want the plan's path", arm.viaPlan, err)
	}
	resp, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{
		Spec: &orbitv1.StartRunRequest_Fleet{Fleet: fleetSpec()}, //nolint:staticcheck // the deprecated arm, kept for API clients
	}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetRun().GetRunId()
	fleet := orbitv1.RunKind_RUN_KIND_FLEET
	if k := resp.Msg.GetRun().GetKind(); k != fleet {
		t.Errorf("StartRun kind = %v, want FLEET", k)
	}
	final := waitRunState(t, c, id, orbitv1.RunState_RUN_STATE_FAILED)
	if final.GetKind() != fleet {
		t.Errorf("GetRun kind = %v, want FLEET", final.GetKind())
	}
	listed := func(k orbitv1.RunKind) bool {
		l, err := c.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{Kind: k}))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range l.Msg.GetRuns() {
			if r.GetRunId() == id {
				return true
			}
		}
		return false
	}
	if !listed(fleet) || listed(orbitv1.RunKind_RUN_KIND_LOAD) {
		t.Errorf("ListRuns: under FLEET %t, under LOAD %t; want FLEET only", listed(fleet), listed(orbitv1.RunKind_RUN_KIND_LOAD))
	}
	path := filepath.Join(dir, id+archiveExt)
	var a *orbitv1.RunArchive
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && a == nil; time.Sleep(25 * time.Millisecond) {
		if got, err := readArchive(path); err == nil {
			a = got
		}
	}
	if a == nil {
		t.Fatal("the run was never archived")
	}
	if k := a.GetRun().GetKind(); k != fleet {
		t.Errorf("archived kind = %v, want FLEET", k)
	}
}
