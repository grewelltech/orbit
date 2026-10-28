package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/prototext"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/join"
	"github.com/bgrewell/orbit/internal/plan"
)

// planTargetYAML is a target as the Targets page holds it: its keys and
// AMF as ${VAR}s, their values in the request's vars.
const planTargetYAML = `version: 1
kind: target
name: lab1
core:
  amf: "${LAB_AMF}"
  plmn: {mcc: "001", mnc: "01"}
subscribers:
  - name: main
    first_supi: "001010000000001"
    count: 10
    ki: "${ORBIT_KI}"
    opc: "${ORBIT_OPC}"
app:
  responders:
    - name: app1
`

func planVars() []*orbitv1.Variable {
	return []*orbitv1.Variable{{Name: "LAB_AMF", Value: "192.0.2.10:38412"}, {Name: "ORBIT_KI", Value: probeKi}, {Name: "ORBIT_OPC", Value: probeOPc}}
}

func templateYAML(t *testing.T, name string) string {
	t.Helper()
	s, err := plan.TemplateText(name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// PlanRun resolves what it is sent as orbit run resolves files: vars fill
// the ${VAR}s, the server's environment never does, a file is never read,
// and a refusal says where. The plan's keys are the request's own.
func TestPlanRun(t *testing.T) {
	t.Setenv("ORBIT_KI", "ffffffffffffffffffffffffffffffff")
	t.Setenv("LAB_AMF", "203.0.113.99:38412")
	load := templateYAML(t, "load")
	tests := []struct {
		name    string
		req     *orbitv1.ResolveRequest
		wantErr string
		check   func(t *testing.T, r *orbitv1.PlanRunResponse)
	}{
		{name: "a template over the target", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, RunYaml: load, Template: "load", Vars: planVars()},
			check: func(t *testing.T, r *orbitv1.PlanRunResponse) {
				p := r.GetPlan()
				if p.GetTarget().GetCore().GetAmf() != "192.0.2.10:38412" || p.GetTarget().GetSubscribers()[0].GetKi() != probeKi {
					t.Errorf("target = %v, want the vars' AMF and key", p.GetTarget())
				}
				if a := p.GetWorkloads()[0].GetPopulation().GetAttach(); a.GetCount() != 100 || !p.GetUntil().GetComplete() || len(p.GetGates()) != 1 {
					t.Errorf("plan = %v, want the load template", p)
				}
				if len(r.GetProvenance()) == 0 || len(p.GetProvenance()) != len(r.GetProvenance()) {
					t.Errorf("provenance: response %d, plan %d", len(r.GetProvenance()), len(p.GetProvenance()))
				}
				for _, pv := range r.GetProvenance() {
					if strings.Contains(pv.GetValue(), probeKi) || strings.Contains(pv.GetValue(), probeOPc) {
						t.Errorf("a key in provenance %v", pv)
					}
				}
			}},
		{name: "the flag layer over the run file", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, RunYaml: load, Vars: planVars(),
			Sets: []string{"workloads[load].attach.count=7"}, Gates: []string{"registration.p99_ms<=500ms"}},
			check: func(t *testing.T, r *orbitv1.PlanRunResponse) {
				if n := r.GetPlan().GetWorkloads()[0].GetPopulation().GetAttach().GetCount(); n != 7 || len(r.GetPlan().GetGates()) != 2 {
					t.Errorf("count %d, gates %v; want 7 and two gates", n, r.GetPlan().GetGates())
				}
			}},
		{name: "the run's name and verbosity", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, Vars: planVars(),
			RunYaml: "kind: run\nrun: {name: nightly, event_verbosity: verbose}\n" + strings.TrimPrefix(load[strings.Index(load, "workloads:"):], "")},
			check: func(t *testing.T, r *orbitv1.PlanRunResponse) {
				if r.GetRunName() != "nightly" || r.GetEventVerbosity() != orbitv1.EventVerbosity_EVENT_VERBOSITY_VERBOSE {
					t.Errorf("name %q, verbosity %v", r.GetRunName(), r.GetEventVerbosity())
				}
			}},
		// The fleet's latency probe comes from the target, as orbit test
		// fills it, while the run file leaves behaviors as the template has
		// them; the registered responder app1 (named only) takes it from
		// the server's registry at StartRun.
		{name: "the fleet template filled", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, RunYaml: templateYAML(t, "fleet"), Template: "fleet", Vars: planVars()},
			check: func(t *testing.T, r *orbitv1.PlanRunResponse) {
				b := r.GetPlan().GetWorkloads()[0].GetPopulation().GetBehaviors()
				if b.GetLatency() == nil || b.GetLatency().GetTarget() != "" {
					t.Errorf("behaviors = %v, want the latency probe, its target from the registry", b)
				}
			}},
		{name: "edited behaviors are not filled", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, Template: "fleet", Vars: planVars(),
			RunYaml: strings.Replace(templateYAML(t, "fleet"), "behaviors: {}", "behaviors: {mobility: {model: random_walk, speed_mps: 3}}", 1)},
			check: func(t *testing.T, r *orbitv1.PlanRunResponse) {
				if b := r.GetPlan().GetWorkloads()[0].GetPopulation().GetBehaviors(); b.GetLatency() != nil {
					t.Errorf("behaviors = %v, want the run file's alone", b)
				}
			}},
		// Refused, saying where.
		{name: "a ${VAR} with no value, though the server has it", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, RunYaml: load,
			Vars: []*orbitv1.Variable{{Name: "LAB_AMF", Value: "192.0.2.10:38412"}, {Name: "ORBIT_OPC", Value: probeOPc}}},
			wantErr: "${ORBIT_KI} is not set"},
		{name: "an expected-failures path", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, Vars: planVars(),
			RunYaml: "kind: run\nworkloads:\n  - name: s\n    conformance: {expected: /etc/passwd}\n"},
			wantErr: "give the list inline"},
		{name: "another target named", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, RunYaml: "target: lab2\n" + load, Vars: planVars()},
			wantErr: `target: is "lab2"`},
		{name: "a target field in the run file", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, RunYaml: load + "core: {amf: x}\n", Vars: planVars()},
			wantErr: "unknown key core"},
		{name: "a bad field", req: &orbitv1.ResolveRequest{TargetYaml: strings.Replace(planTargetYAML, `mcc: "001"`, `mcc: "1"`, 1), RunYaml: load, Vars: planVars()},
			wantErr: "core.plmn.mcc"},
		{name: "a bad key, masked", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, RunYaml: load,
			Vars: []*orbitv1.Variable{{Name: "LAB_AMF", Value: "192.0.2.10:38412"}, {Name: "ORBIT_KI", Value: "abc-not-a-key"}, {Name: "ORBIT_OPC", Value: probeOPc}}},
			wantErr: "subscribers[main].ki"},
		{name: "two targets", req: &orbitv1.ResolveRequest{TargetYaml: "- {kind: target, version: 1, name: a}\n- {kind: target, version: 1, name: b}\n", RunYaml: load},
			wantErr: "one target document"},
		{name: "an unknown template", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, RunYaml: load, Template: "nope", Vars: planVars()},
			wantErr: `no template "nope"`},
		{name: "no run file", req: &orbitv1.ResolveRequest{TargetYaml: planTargetYAML, Vars: planVars()}, wantErr: "no run file"},
	}
	s := registeredRunService(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.PlanRun(context.Background(), connect.NewRequest(tc.req))
			if tc.wantErr != "" {
				if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.HasPrefix(connectMsg(err), "invalid run: ") || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want InvalidArgument invalid run: …%q", err, tc.wantErr)
				}
				for _, secret := range []string{probeKi, probeOPc, "abc-not-a-key", "ffffffff"} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("a key in %v", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, res.Msg)
			// The server's environment never reaches the plan.
			text := prototext.Format(res.Msg)
			for _, env := range []string{"ffffffffffffffffffffffffffffffff", "203.0.113.99"} {
				if strings.Contains(text, env) {
					t.Errorf("the server's environment (%s) in the response", env)
				}
			}
			// The registry's responder token never leaves the server.
			if strings.Contains(text, registeredLoomToken) {
				t.Error("the registered responder's token in the response")
			}
		})
	}
}

// registeredLoomToken is registeredRunService's responder's token, which
// no PlanRun response may hold.
var registeredLoomToken string

// registeredRunService is a runService whose registry holds app1, online,
// with a loom token.
func registeredRunService(t *testing.T) *runService {
	t.Helper()
	clk := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	reg, err := join.OpenRegistry("", join.WithRegistryClock(func() time.Time { return clk }))
	if err != nil {
		t.Fatal(err)
	}
	store := join.NewStore(join.WithClock(func() time.Time { return clk }))
	_, secret, err := store.Create("", 0, join.Spec{UEPools: []string{"10.250.0.0/16"}, UPFN6: "198.51.100.1", N6Subnet: "198.51.100.0/24", Management: "192.0.2.10:8412"})
	if err != nil {
		t.Fatal(err)
	}
	cred := newCred(t)
	rs, err := reg.Register(store, join.RegisterRequest{JoinToken: secret, Credential: cred, Name: "app1", Control: "198.51.100.20:9551",
		DataIP: "198.51.100.20", PortLow: 32768, PortHigh: 60999})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Heartbeat(cred, "198.51.100.20:1", "", true, false); err != nil {
		t.Fatal(err)
	}
	registeredLoomToken = rs.LoomToken
	if registeredLoomToken == "" {
		t.Fatal("the registered responder has no token to leak")
	}
	return &runService{responders: reg}
}

// RenderRun is keyless both ways: a secret written out in the run file is
// refused by path, never echoed; what it gives holds none.
func TestRenderRunRPC(t *testing.T) {
	s := &runService{}
	load := templateYAML(t, "load")
	res, err := s.RenderRun(context.Background(), connect.NewRequest(&orbitv1.RenderRunRequest{RunYaml: load, Template: "load", TargetName: "lab1",
		Sets: []string{"workloads[load].attach.count=9"}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetCommand() != `orbit test load '--target=lab1' '--set=workloads[load].attach.count=9'` || res.Msg.GetFileName() != "" || !strings.Contains(res.Msg.GetRunYaml(), "kind: run") {
		t.Errorf("got %v", res.Msg)
	}
	_, err = s.RenderRun(context.Background(), connect.NewRequest(&orbitv1.RenderRunRequest{TargetName: "lab1",
		RunYaml: "kind: run\nworkloads:\n  - name: f\n    behaviors: {traffic: {apps: [{token: t-0123456789}]}}\n"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "t-0123456789") {
		t.Errorf("a literal token: err = %v, want InvalidArgument naming the path only", err)
	}
}
