package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// writeRunFile writes a run file next to the target and returns its path.
func writeRunFile(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// gateNames are a plan's gates as name=metric op threshold.
func gateNames(p *orbitv1.RunPlan) []string {
	var out []string
	for _, g := range p.GetGates() {
		if s := g.GetSlo(); s != nil {
			out = append(out, g.GetName()+"="+s.GetMetric())
			continue
		}
		out = append(out, g.GetName())
	}
	return out
}

// `orbit test <template>` builds the template over the target and the
// flags, starts it and returns at start, saying how to follow the run.
func TestOrbitTestStartsTemplate(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	tests := []struct {
		name      string
		args      []string
		check     func(t *testing.T, p *orbitv1.RunPlan)
		wantCode  int
		wantInErr string
	}{
		{name: "load as written", args: []string{"test", "load", "-t", "lab1"},
			check: func(t *testing.T, p *orbitv1.RunPlan) {
				if got := p.GetTarget().GetCore().GetAmf(); got != "192.0.2.10:38412" {
					t.Errorf("amf = %q, want the target's", got)
				}
				if a := p.GetWorkloads()[0].GetPopulation().GetAttach(); a.GetCount() != 100 || a.GetRate() != 20 {
					t.Errorf("attach = %v, want the template's", a)
				}
				if !p.GetUntil().GetComplete() {
					t.Errorf("until = %v, want complete", p.GetUntil())
				}
				if g := gateNames(p); len(g) != 1 || g[0] != "attach_success_ratio=attach.success_ratio" {
					t.Errorf("gates = %v, want the template's default gate", g)
				}
			}},
		{name: "--for replaces the template's stop", args: []string{"test", "load", "-t", "lab1", "--for", "10m"},
			check: func(t *testing.T, p *orbitv1.RunPlan) {
				if u := p.GetUntil(); u.GetDurationMs() != 600000 || u.GetComplete() {
					t.Errorf("until = %v, want 10m only", u)
				}
			}},
		{name: "--gate adds, --set overrides", args: []string{"test", "load", "-t", "lab1",
			"--gate", "registration.p99_ms<=500ms", "--set", "workloads[load].attach.count=7"},
			check: func(t *testing.T, p *orbitv1.RunPlan) {
				if g := gateNames(p); len(g) != 2 {
					t.Errorf("gates = %v, want the default and --gate's", g)
				}
				if c := p.GetWorkloads()[0].GetPopulation().GetAttach().GetCount(); c != 7 {
					t.Errorf("count = %d, want --set's 7", c)
				}
			}},
		{name: "no template", args: []string{"test", "-t", "lab1"}, wantCode: ExitCouldNotStart, wantInErr: "name a template"},
		{name: "unknown template", args: []string{"test", "soak", "-t", "lab1"}, wantCode: ExitCouldNotStart, wantInErr: `no template "soak"`},
		{name: "no target", args: []string{"test", "load"}, wantCode: ExitCouldNotStart, wantInErr: "a run needs a target"},
		{name: "bad gate", args: []string{"test", "load", "-t", "lab1", "--gate", "p99"}, wantCode: ExitCouldNotStart},
		{name: "bad --set", args: []string{"test", "load", "-t", "lab1", "--set", "nopath"}, wantCode: ExitCouldNotStart, wantInErr: "want path=value"},
		{name: "unknown --set path", args: []string{"test", "load", "-t", "lab1", "--set", "workloads[load].nope=1"}, wantCode: ExitCouldNotStart},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubRunService{startRunID: "run-t1"}
			url := stubRunServer(t, stub)
			r := runOrbit(t, loc, append(tc.args, "--server", *url)...)
			if tc.check == nil {
				if got := ExitCode(r.execErr); got != tc.wantCode || !strings.Contains(fmt.Sprint(r.execErr), tc.wantInErr) {
					t.Fatalf("exit %d (%v), want %d with %q", got, r.execErr, tc.wantCode, tc.wantInErr)
				}
				if stub.startReq != nil {
					t.Error("a refused test started a run")
				}
				return
			}
			if r.execErr != nil {
				t.Fatalf("%v\n%s", r.execErr, r.err)
			}
			if r.out != "run-t1\n" {
				t.Errorf("stdout = %q, want the run ID alone", r.out)
			}
			if want := "started run-t1; follow with `orbit runs watch run-t1` or pass --wait"; !strings.Contains(r.err, want) {
				t.Errorf("stderr lacks %q:\n%s", want, r.err)
			}
			tc.check(t, stub.startReq.GetPlan())
		})
	}
}

// --list names the templates; with one named, prints it.
func TestOrbitTestList(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	r := runOrbit(t, loc, "test", "--list")
	if r.execErr != nil || r.out != "conformance\nfleet\nload\n" {
		t.Fatalf("--list = %q, %v", r.out, r.execErr)
	}
	r = runOrbit(t, loc, "test", "--list", "load")
	if r.execErr != nil || !strings.Contains(r.out, "kind: run") {
		t.Fatalf("--list load = %q, %v", r.out, r.execErr)
	}
}

// --explain prints the plan with its keys masked and starts nothing.
func TestOrbitTestExplain(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	stub := &stubRunService{startRunID: "run-x"}
	url := stubRunServer(t, stub)
	r := runOrbit(t, loc, "test", "load", "-t", "lab1", "--explain", "--server", *url)
	if r.execErr != nil {
		t.Fatalf("%v\n%s", r.execErr, r.err)
	}
	if stub.startReq != nil {
		t.Error("--explain started a run")
	}
	for _, s := range []string{testKi, testOPc} {
		if strings.Contains(r.out+r.err, s) {
			t.Errorf("--explain shows a key")
		}
	}
	for _, w := range []string{"attach_success_ratio", "192.0.2.10:38412"} {
		if !strings.Contains(r.out, w) {
			t.Errorf("plan lacks %q:\n%s", w, r.out)
		}
	}
	if !strings.Contains(r.err, "template load") {
		t.Errorf("provenance does not name the template:\n%s", r.err)
	}
}

// `orbit run` on a kind: run file waits for the run's end by default and
// exits with its result; --detach returns at start.
func TestOrbitRunPlanFile(t *testing.T) {
	clearOrbitEnv(t)
	old := waitPoll
	waitPoll = 5 * time.Millisecond
	defer func() { waitPoll = old }()
	loc := targetDir(t, lab1)
	const doc = `kind: run
target: lab1
workloads:
  - name: pop
    attach: {count: 5}
until: {complete: true}
gates:
  - name: reg
    slo: {metric: registration.p99_ms, threshold: 500ms}
`
	path := writeRunFile(t, doc)

	t.Run("waits by default", func(t *testing.T) {
		stub := startingWaitStub{&waitStub{end: orbitv1.RunState_RUN_STATE_COMPLETE,
			report: &orbitv1.GetRunReportResponse{Gates: []*orbitv1.GateResult{gateResult("reg", false)}}}}
		mux := http.NewServeMux()
		mux.Handle(orbitv1connect.NewRunServiceHandler(stub))
		srv := httptest.NewServer(mux)
		defer srv.Close()
		r := runOrbit(t, loc, "run", path, "--server", srv.URL)
		if got := ExitCode(r.execErr); got != ExitGateFailed {
			t.Fatalf("exit %d (%v), want %d (the gate failed)\n%s", got, r.execErr, ExitGateFailed, r.err)
		}
	})
	t.Run("--detach returns at start", func(t *testing.T) {
		stub := &stubRunService{startRunID: "run-d"}
		url := stubRunServer(t, stub)
		r := runOrbit(t, loc, "run", path, "--detach", "--server", *url, "--gate", "attach.success_ratio>=0.95")
		if r.execErr != nil {
			t.Fatalf("%v\n%s", r.execErr, r.err)
		}
		if r.out != "run-d\n" {
			t.Errorf("stdout = %q", r.out)
		}
		p := stub.startReq.GetPlan()
		if g := gateNames(p); len(g) != 2 {
			t.Errorf("gates = %v, want the file's and --gate's", g)
		}
		if p.GetTarget().GetCore().GetAmf() != "192.0.2.10:38412" {
			t.Errorf("target = %v, want the file's target:", p.GetTarget())
		}
	})
	t.Run("a run file restating its target is refused", func(t *testing.T) {
		bad := writeRunFile(t, doc+"core: {amf: 192.0.2.99:38412}\n")
		r := runOrbit(t, loc, "run", bad, "--detach", "--server", "http://127.0.0.1:1")
		if got := ExitCode(r.execErr); got != ExitCouldNotStart {
			t.Fatalf("exit %d (%v), want %d", got, r.execErr, ExitCouldNotStart)
		}
	})
	t.Run("run-file flags on a step scenario", func(t *testing.T) {
		step := writeRunFile(t, "target: lab1\nsteps: []\n")
		r := runOrbit(t, loc, "run", step, "--detach")
		if r.execErr == nil || !strings.Contains(r.execErr.Error(), "--detach applies to a run file") {
			t.Fatalf("err = %v", r.execErr)
		}
	})
}

// startingWaitStub is a waitStub that starts the run it then reports on.
type startingWaitStub struct{ *waitStub }

func (startingWaitStub) StartRun(context.Context, *connect.Request[orbitv1.StartRunRequest]) (*connect.Response[orbitv1.StartRunResponse], error) {
	return connect.NewResponse(&orbitv1.StartRunResponse{Run: &orbitv1.Run{RunId: "run-w"}}), nil
}

// start-load and start-fleet take --gate and the --slo-* aliases, which
// put gates in the plan they send.
func TestStartCommandsGateFlags(t *testing.T) {
	clearOrbitEnv(t)
	load := []string{"runs", "start-load", "--amf", "10.0.0.1:38412", "--base-imsi", "208930100007500",
		"--ki", testKi, "--opc", testOPc, "--mcc", "208", "--mnc", "93"}
	tests := []struct {
		name      string
		args      []string
		wantGates []string
		wantCode  int
	}{
		{name: "start-load, no gates", args: load},
		{name: "start-load --gate", args: append(load[:len(load):len(load)], "--gate", "registration.p99_ms<=500ms"),
			wantGates: []string{"registration_p99_ms=registration.p99_ms"}},
		{name: "start-load --slo-* aliases", args: append(load[:len(load):len(load)], "--slo-min-success", "0.95", "--slo-reg-p99", "400ms"),
			wantGates: []string{"attach_success_ratio=attach.success_ratio", "registration_p99_ms=registration.p99_ms"}},
		{name: "start-load --gate and its --slo-* alias are one gate", args: append(load[:len(load):len(load)],
			"--gate", "registration.p99_ms<=500ms", "--slo-reg-p99", "400ms"),
			wantGates: []string{"registration_p99_ms=registration.p99_ms"}},
		{name: "start-load bad --gate", args: append(load[:len(load):len(load)], "--gate", "nonsense"), wantCode: ExitCouldNotStart},
		{name: "start-fleet --gate", args: []string{"runs", "start-fleet", "FLEET", "-t", "lab1", "--gate", "attach.success_ratio>=0.9"},
			wantGates: []string{"attach_success_ratio=attach.success_ratio"}},
		{name: "start-fleet gates the arm cannot carry", args: []string{"runs", "start-fleet", "SELF", "--ki", testKi, "--opc", testOPc,
			"--slo-min-success", "0.9"}, wantCode: ExitCouldNotStart},
	}
	fleet := writeScenario(t, fleetFile)
	self := writeScenario(t, "kind: fleet\nname: f\ncore:\n  amf: 192.0.2.1:38412\n")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubRunService{startRunID: "run-g"}
			url := stubRunServer(t, s)
			args := append([]string{"--server", *url}, tc.args...)
			for i, a := range args {
				switch a {
				case "FLEET":
					args[i] = fleet
				case "SELF":
					args[i] = self
				}
			}
			r := runOrbit(t, targetDir(t, lab1), args...)
			if got := ExitCode(r.execErr); got != tc.wantCode {
				t.Fatalf("exit %d (%v), want %d\n%s", got, r.execErr, tc.wantCode, r.err)
			}
			if tc.wantCode != 0 {
				if s.startReq != nil {
					t.Error("a refused start started a run")
				}
				return
			}
			got := gateNames(s.startReq.GetPlan())
			if strings.Join(got, ",") != strings.Join(tc.wantGates, ",") {
				t.Errorf("gates = %v, want %v", got, tc.wantGates)
			}
		})
	}
}

// start-load names gNBs as a plan does: one by --gnb-name itself, several
// <name>-1 .. <name>-n; and it always sends a plan.
func TestStartLoadGNBNaming(t *testing.T) {
	clearOrbitEnv(t)
	for _, n := range []string{"1", "3"} {
		s := &stubRunService{startRunID: "run-n"}
		runRunsCmd(t, stubRunServer(t, s), "start-load", "--amf", "10.0.0.1:38412", "--base-imsi", "208930100007500",
			"--ki", testKi, "--opc", testOPc, "--mcc", "208", "--mnc", "93", "--gnb-name", "edge", "--gnb-count", n)
		p := s.startReq.GetPlan()
		if p == nil {
			t.Fatalf("--gnb-count %s sent no plan: %v", n, s.startReq)
		}
		if got := p.GetTarget().GetRan().GetGnbName(); got != "edge" {
			t.Errorf("--gnb-count %s: gnb_name = %q, want edge (the server numbers several from -1)", n, got)
		}
	}
}
