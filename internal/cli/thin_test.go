package cli

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/server"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// thinLab is an in-process orbit server and a mock AMF behind real SCTP,
// with lab1 as a target for it.
type thinLab struct {
	url, amf string
	doc      string
	mock     *mockamf.AMF
}

func newThinLab(t *testing.T) thinLab {
	t.Helper()
	ki, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	srv := httptest.NewServer(server.New(observability.NewLogger(io.Discard, slog.LevelError), "test",
		observability.NewRegistry(), server.Options{StateDir: ""}))
	t.Cleanup(srv.Close)
	return thinLab{url: srv.URL, amf: front.Addr, doc: loadTarget(front.Addr, "127.0.0.1-127.0.0.9"), mock: amf}
}

// `orbit load` runs on the server by default and waits: the same storm,
// its KPIs and gates printed, exit by the result. --detach returns the run
// ID; --gate and --slo-* gate it.
func TestThinLoad(t *testing.T) {
	clearOrbitEnv(t)
	lab := newThinLab(t)
	loc := targetDir(t, lab.doc)
	long := []string{"load", "--amf", lab.amf, "--mcc", "208", "--mnc", "93", "--sst", "1", "--sd", "010203",
		"--base-imsi", "208930000000001", "--ki", testKi, "--opc", testOPc, "--count", "3", "--rate", "50"}
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
		wantErr  []string
	}{
		{name: "the long command line", args: long, wantOut: []string{"load: 3/3 attached", "registration", "COMPLETE"},
			wantErr: []string{"network: plmn=208/93 tac=1 snssai=1/010203 dnn=internet", "exit 0"}},
		{name: "from a target", args: []string{"load", "-t", "lab1", "--count", "3"}, wantOut: []string{"load: 3/3 attached"}},
		{name: "--slo-min-success passes", args: append(append([]string{}, long...), "--slo-min-success", "0.9"),
			wantOut: []string{"gate attach_success_ratio: PASS"}},
		{name: "--gate fails", args: append(append([]string{}, long...), "--gate", "attach.achieved_rate>=1000000"),
			wantCode: ExitGateFailed, wantOut: []string{"gate attach_achieved_rate: FAIL"}},
		{name: "--detach", args: append(append([]string{}, long...), "--detach")},
		{name: "--detach with --local", args: append(append([]string{}, long...), "--detach", "--local"), wantCode: ExitCouldNotStart},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := runOrbit(t, loc, append([]string{"--server", lab.url}, tc.args...)...)
			if got := ExitCode(r.execErr); got != tc.wantCode {
				t.Fatalf("exit %d (%v), want %d\nout:\n%s\nerr:\n%s", got, r.execErr, tc.wantCode, r.out, r.err)
			}
			if tc.name == "--detach" && !regexp.MustCompile(`^run-[0-9a-f]+\n$`).MatchString(r.out) {
				t.Errorf("--detach stdout = %q, want the run ID alone", r.out)
			}
			for _, w := range tc.wantOut {
				if !strings.Contains(r.out, w) {
					t.Errorf("stdout lacks %q:\n%s", w, r.out)
				}
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(r.err, w) {
					t.Errorf("stderr lacks %q:\n%s", w, r.err)
				}
			}
		})
	}
}

// The plan thin load sends: the flags' fields, the command's own flag
// defaults where nothing else says, and the target's fields from the
// target (its provenance names it, not the flag the target filled).
func TestThinLoadPlan(t *testing.T) {
	clearOrbitEnv(t)
	stub := &stubRunService{startRunID: "run-l"}
	url := stubRunServer(t, stub)
	r := runOrbit(t, targetDir(t, lab1), "--server", *url, "load", "-t", "lab1", "--count", "7", "--gate", "registration.p99_ms<=500ms", "--detach")
	if r.execErr != nil {
		t.Fatalf("%v\n%s", r.execErr, r.err)
	}
	p := stub.startReq.GetPlan()
	if len(p.GetWorkloads()) != 1 || p.GetWorkloads()[0].GetName() != "load" {
		t.Fatalf("workloads = %v", p.GetWorkloads())
	}
	pop := p.GetWorkloads()[0].GetPopulation()
	if a := pop.GetAttach(); a.GetCount() != 7 || a.GetConcurrency() != 64 {
		t.Errorf("attach = %v, want --count's 7 and --concurrency's default 64", a)
	}
	if got := p.GetTarget().GetCore().GetAmf(); got != "192.0.2.10:38412" {
		t.Errorf("amf = %q, want the target's", got)
	}
	if g := gateNames(p); len(g) != 1 || g[0] != "registration_p99_ms=registration.p99_ms" {
		t.Errorf("gates = %v", g)
	}
	src := map[string]string{}
	for _, pv := range p.GetProvenance() {
		src[pv.GetPath()] = pv.GetSource().GetLayer().String() + " " + pv.GetSource().GetDetail()
	}
	if s := src["core.amf"]; !strings.Contains(s, "TARGET") {
		t.Errorf("core.amf provenance = %q, want the target", s)
	}
	if s := src["workloads[load].attach.concurrency"]; s != "PROVENANCE_LAYER_DEFAULT default (orbit load)" {
		t.Errorf("concurrency provenance = %q, want orbit load's own default", s)
	}
	if s := src["core.profile"]; s != "PROVENANCE_LAYER_DEFAULT default built-in" {
		t.Errorf("core.profile provenance = %q, want the built-in default, apart from the command's", s)
	}
	if s := src["workloads[load].attach.count"]; !strings.Contains(s, "--count") {
		t.Errorf("count provenance = %q, want --count", s)
	}
}

// `orbit conformance` on the server gives what --local gives for the same
// suite: the same checks and verdicts, and the same exit. Each side has its
// own mock AMF, which keeps state between runs.
func TestThinConformanceMatchesLocal(t *testing.T) {
	clearOrbitEnv(t)
	run := func(local, jsonOut bool) orbitRun {
		lab := newThinLab(t)
		// No subscriber: the mobility checks SKIP either way.
		loc := targetDir(t, "version: 1\nkind: target\nname: lab1\ncore:\n  amf: "+lab.amf+"\n  plmn: { mcc: \"208\", mnc: \"93\" }\n"+
			"  slices:\n    - { sst: 1, sd: \"010203\" }\nran:\n  n2_source_ips: [127.0.0.1-127.0.0.9]\n  gnb_ids: { first: 100 }\n")
		args := []string{"--server", lab.url, "conformance", "-t", "lab1", "--category", "procedural,negative-ie,nas", "--per-test-timeout", "2s"}
		if jsonOut {
			args = append(args, "--json")
		}
		if local {
			args = append(args, "--local")
		}
		return runOrbit(t, loc, args...)
	}
	type result struct{ ID, Verdict string }
	parse := func(name string, r orbitRun) []result {
		var sum struct{ Results []result }
		if err := json.Unmarshal([]byte(r.out), &sum); err != nil {
			t.Fatalf("%s: JSON: %v (err %v)\n%s\n%s", name, err, r.execErr, r.out, r.err)
		}
		return sum.Results
	}
	local, thin := run(true, true), run(false, true)
	lr, tr := parse("--local", local), parse("thin", thin)
	if len(lr) < 3 {
		t.Fatalf("only %d results: %+v", len(lr), lr)
	}
	if !reflect.DeepEqual(lr, tr) {
		t.Errorf("the server's suite differs from --local's:\nlocal %+v\nthin  %+v", lr, tr)
	}
	if ExitCode(local.execErr) != ExitCode(thin.execErr) {
		t.Errorf("exit differs: --local %v, thin %v", local.execErr, thin.execErr)
	}
	if !strings.Contains(thin.err, "gate conformance:") {
		t.Errorf("thin stderr lacks the gate's verdict:\n%s", thin.err)
	}
	if !strings.Contains(local.err, localDeprecated) || strings.Contains(thin.err, localDeprecated) {
		t.Errorf("the deprecation notice belongs to --local only:\nlocal %q\nthin %q", local.err, thin.err)
	}

	// Text output: the same network line and per-check lines, verdict and
	// ID (the observed text can carry timings).
	lines := func(r orbitRun) []string {
		var out []string
		for _, l := range strings.Split(r.out, "\n") {
			if f := strings.Fields(l); len(f) > 0 && regexp.MustCompile(`^(PASS|FAIL|ERROR|SKIP|DEVIATION)$`).MatchString(f[0]) {
				out = append(out, strings.Join(f[:min(4, len(f))], " "))
			} else if strings.HasPrefix(l, "network:") || strings.HasPrefix(l, "target-release:") {
				out = append(out, l)
			}
		}
		return out
	}
	if l, th := lines(run(true, false)), lines(run(false, false)); len(l) == 0 || !reflect.DeepEqual(l, th) {
		t.Errorf("text differs:\nlocal %q\nthin  %q", l, th)
	}
}

// A check failing (an AMF that is not there: every check ERRORs) fails the
// conformance gate, exit 1, as --local exits.
func TestThinConformanceGateFails(t *testing.T) {
	clearOrbitEnv(t)
	lab := newThinLab(t)
	r := runOrbit(t, targetDir(t, lab1), "--server", lab.url, "conformance", "--amf", "127.0.0.1:1", "--mcc", "001", "--mnc", "01",
		"--category", "procedural", "--per-test-timeout", "1s")
	if got := ExitCode(r.execErr); got != ExitGateFailed {
		t.Fatalf("exit %d (%v), want %d\n%s\n%s", got, r.execErr, ExitGateFailed, r.out, r.err)
	}
	if !strings.Contains(r.out, "gate conformance: FAIL") {
		t.Errorf("stdout lacks the failed gate:\n%s", r.out)
	}
}

// The conformance plan: --n3-bind is the GTP-U checks' source
// (conformance.n3_bind) and --gnb-n3 the UE's gNB N3 (ran.n3_ips), as the
// request's n3_bind and gnb_n3 were; the conformance gate is always there.
func TestThinConformancePlan(t *testing.T) {
	clearOrbitEnv(t)
	stub := &stubRunService{startRunID: "run-c"}
	url := stubRunServer(t, stub)
	r := runOrbit(t, targetDir(t, lab1), "--server", *url, "conformance", "--amf", "192.0.2.1:38412", "--mcc", "001", "--mnc", "01",
		"--n3-bind", "198.51.100.9:2152", "--supi", "001010000000001", "--ki", testKi, "--opc", testOPc, "--gnb-n3", "198.51.100.7",
		"--category", "gtpu", "--detach")
	if r.execErr != nil {
		t.Fatalf("%v\n%s", r.execErr, r.err)
	}
	p := stub.startReq.GetPlan()
	cw := p.GetWorkloads()[0].GetConformance()
	if cw.GetN3Bind() != "198.51.100.9:2152" || strings.Join(p.GetTarget().GetRan().GetN3Ips(), ",") != "198.51.100.7" {
		t.Errorf("n3_bind %q, n3_ips %v; want --n3-bind's and --gnb-n3's", cw.GetN3Bind(), p.GetTarget().GetRan().GetN3Ips())
	}
	if g := p.GetGates(); len(g) != 1 || g[0].GetConformance() == nil {
		t.Errorf("gates = %v, want the conformance gate", g)
	}
	if pt := cw.GetPerTestTimeoutMs(); pt != 15000 {
		t.Errorf("per-test timeout = %d ms, want --per-test-timeout's default 15s", pt)
	}
}

// With no server there, a thin command says how to get one, and exits 3.
func TestThinNoServer(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	fleet := writeScenario(t, "kind: fleet\nname: f\ntarget: lab1\ntopology:\n  gnbs: { count: 1 }\nfleet: { count: 2 }\n")
	for _, args := range [][]string{
		{"load", "-t", "lab1"},
		{"conformance", "-t", "lab1"},
		{"run", fleet},
	} {
		t.Run(args[0], func(t *testing.T) {
			r := runOrbit(t, loc, append([]string{"--server", "http://127.0.0.1:1"}, args...)...)
			want := "no orbit server at http://127.0.0.1:1: start one with `orbit serve`, or pass --local"
			if ExitCode(r.execErr) != ExitCouldNotStart || r.execErr == nil || r.execErr.Error() != want {
				t.Errorf("err = %v, want exit 3 with %q", r.execErr, want)
			}
		})
	}
}

// `orbit run <fleet.yaml>` runs the fleet on the server and waits: its
// report, gates and exit; --local (deprecated) still runs it here.
func TestThinFleet(t *testing.T) {
	clearOrbitEnv(t)
	lab := newThinLab(t)
	loc := targetDir(t, lab.doc)
	fleet := writeScenario(t, "kind: fleet\nname: f\ntarget: lab1\ntopology:\n  gnbs: { count: 1 }\nfleet: { count: 2, attach_rate: 50/s }\n")
	r := runOrbit(t, loc, "--server", lab.url, "run", fleet, "--slo-min-success", "0.5")
	if r.execErr != nil {
		t.Fatalf("%v\n%s\n%s", r.execErr, r.out, r.err)
	}
	for _, w := range []string{"COMPLETE", "gate attach_success_ratio: PASS"} {
		if !strings.Contains(r.out, w) {
			t.Errorf("stdout lacks %q:\n%s", w, r.out)
		}
	}
	r = runOrbit(t, loc, "--server", lab.url, "run", fleet, "--detach", "--local")
	if ExitCode(r.execErr) == ExitPass {
		t.Error("--detach with --local ran")
	}
}

// The thin commands' plans go through plan.Build like a run file's, so a
// key a target holds travels only in the plan's target, never its
// provenance.
func TestThinPlanKeysMasked(t *testing.T) {
	clearOrbitEnv(t)
	stub := &stubRunService{startRunID: "run-k"}
	url := stubRunServer(t, stub)
	r := runOrbit(t, targetDir(t, lab1), "--server", *url, "load", "-t", "lab1", "--detach")
	if r.execErr != nil {
		t.Fatalf("%v\n%s", r.execErr, r.err)
	}
	for _, pv := range stub.startReq.GetPlan().GetProvenance() {
		b, _ := json.Marshal(pv)
		if strings.Contains(string(b), testKi) || strings.Contains(string(b), testOPc) {
			t.Errorf("provenance holds a key: %s", b)
		}
	}
	if sp := stub.startReq.GetPlan().GetTarget().GetSubscribers(); len(sp) == 0 || sp[0].GetKi() != testKi {
		t.Errorf("the plan's target lost its key: %v", sp)
	}
}

// A bare thin `orbit load` presents the same gNB IDs and names as the
// in-process one (--local): the command's defaults (--gnb-id 1, --gnb-name
// orbit-gnb) reach the plan, and both name one gNB by the name itself and
// several <name>-1 .. <name>-n.
func TestThinLoadGNBsMatchLocal(t *testing.T) {
	clearOrbitEnv(t)
	for _, gnbs := range []string{"1", "3"} {
		t.Run(gnbs+" gNBs", func(t *testing.T) {
			setups := func(local bool) []mockamf.Setup {
				lab := newThinLab(t)
				args := []string{"--server", lab.url, "load", "--amf", lab.amf, "--mcc", "208", "--mnc", "93", "--sst", "1", "--sd", "010203",
					"--base-imsi", "208930000000001", "--ki", testKi, "--opc", testOPc, "--count", "3", "--gnb-count", gnbs}
				if local {
					args = append(args, "--local")
				}
				if r := runOrbit(t, targetDir(t, lab1), args...); r.execErr != nil {
					t.Fatalf("local %t: %v\n%s", local, r.execErr, r.err)
				}
				got := lab.mock.Setups()
				sort.Slice(got, func(i, j int) bool { return got[i].GNBID < got[j].GNBID })
				return got
			}
			local, thin := setups(true), setups(false)
			if len(local) == 0 || !reflect.DeepEqual(local, thin) {
				t.Errorf("NG Setups differ:\nlocal %+v\nthin  %+v", local, thin)
			}
			if local[0].GNBID != 1 {
				t.Errorf("first gNB ID %d, want --gnb-id's default 1", local[0].GNBID)
			}
		})
	}
}
