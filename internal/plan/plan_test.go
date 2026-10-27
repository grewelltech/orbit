package plan

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/target"
)

const testKi = "00112233445566778899aabbccddeeff"

// labTarget is a target document the tests resolve against.
const labTarget = `version: 1
kind: target
name: lab1
core:
  amf: 192.0.2.10:38412
  plmn: {mcc: "208", mnc: "93"}
  slices: [{sst: 1, sd: "010203"}]
subscribers:
  - name: main
    first_supi: "208930100007500"
    count: 1000
    ki: ` + testKi + `
    opc: 000102030405060708090a0b0c0d0e0f
ran:
  n2_source_ips: [192.0.2.20-192.0.2.29]
app:
  responders:
    - {name: n6, control: 192.0.2.30:9551, token: tok-secret-1}
`

func noEnv(string) (string, bool) { return "", false }

// resolve resolves the lab target, a run file and any further inputs.
func resolve(t *testing.T, runFile string, more ...target.Input) *target.Resolved {
	t.Helper()
	files, err := target.Parse("lab1.yaml", []byte(labTarget), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	rf, err := target.ParseRunFile("run.yaml", []byte(runFile), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	r, err := target.Resolve(append([]target.Input{target.DefaultInput(), files[0].Input, rf.Input}, more...)...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A run file becomes the plan it describes: the target, the workloads in
// order with their kind from their fields, how the run stops, and gates
// whose metric and comparison may come from their name alone.
func TestBuild(t *testing.T) {
	tests := []struct {
		name    string
		run     string
		files   map[string]string
		check   func(*testing.T, *orbitv1.RunPlan)
		wantErr string
	}{
		{name: "a load with gates by name", run: `kind: run
workloads:
  - name: pop
    attach: {count: 50, rate: 10.5, ramp: "1:20:30", arrival: poisson, seed: 7}
    topology: {gnb_count: 2, gnb_ids: {assign: true}}
    pdu_session: true
until: {duration: 2h}
gates:
  - name: attach_success_ratio
    slo: {threshold: "0.99"}
  - name: pop/registration_p99_ms
    slo: {threshold: 500ms}
`, check: func(t *testing.T, p *orbitv1.RunPlan) {
			pop := p.GetWorkloads()[0].GetPopulation()
			a := pop.GetAttach()
			if a.GetCount() != 50 || a.GetRate() != 10.5 || a.GetRamp().GetEnd() != 20 || a.GetArrival() != orbitv1.Arrival_ARRIVAL_POISSON || a.GetSeed() != 7 {
				t.Errorf("attach = %v", a)
			}
			if !pop.GetTopology().GetGnbIds().GetAssign() || pop.GetTopology().GetGnbCount() != 2 || !pop.GetPduSession() {
				t.Errorf("population = %v", pop)
			}
			if p.GetUntil().GetDurationMs() != 7_200_000 {
				t.Errorf("until = %v", p.GetUntil())
			}
			g0, g1 := p.GetGates()[0].GetSlo(), p.GetGates()[1]
			if g0.GetMetric() != "attach.success_ratio" || g0.GetOp() != orbitv1.CompareOp_COMPARE_OP_GE || g0.GetThreshold() != 0.99 {
				t.Errorf("gate 0 = %v", g0)
			}
			if g1.GetWorkload() != "pop" || g1.GetSlo().GetMetric() != "registration.p99_ms" || g1.GetSlo().GetOp() != orbitv1.CompareOp_COMPARE_OP_LE || g1.GetSlo().GetThreshold() != 500 {
				t.Errorf("gate 1 = %v", g1)
			}
			if p.GetTarget().GetSubscribers()[0].GetKi() != testKi || p.GetTarget().GetCore().GetAmf() != "192.0.2.10:38412" {
				t.Errorf("target = %v", p.GetTarget())
			}
		}},
		{name: "conformance under load", run: `kind: run
workloads:
  - name: pop
    attach: {count: 10}
    behaviors: {latency: {target: 192.0.2.1, interval_ms: 1000}}
  - name: suite
    start: {after: pop, phase: attached}
    conformance:
      categories: [negative-ie]
      target_release: Rel-17
      gnb_ids: {first: 700}
      expected: suite.yaml
until: {duration: 10m}
gates:
  - name: suite
    workload: suite
    conformance: true
`, files: map[string]string{"suite.yaml": `core: lab A
version: "1.0"
entries:
  - {check: NGAP-UNKNOWN-UE-SURVIVES, observed: "silently dropped; association survived", finding: EX-1, added: 2026-10-27, reason: example}
`}, check: func(t *testing.T, p *orbitv1.RunPlan) {
			if b := p.GetWorkloads()[0].GetPopulation().GetBehaviors(); b.GetLatency().GetTarget() != "192.0.2.1" || b.GetLatency().GetIntervalMs() != 1000 {
				t.Errorf("behaviors = %v", b)
			}
			w := p.GetWorkloads()[1]
			cw := w.GetConformance()
			if w.GetStart().GetAfter() != "pop" || w.GetStart().GetPhase() != orbitv1.WorkloadPhase_WORKLOAD_PHASE_ATTACHED {
				t.Errorf("start = %v", w.GetStart())
			}
			if cw.GetCategories()[0] != "negative-ie" || cw.GetTargetRelease() != "Rel-17" || cw.GetGnbIds().GetFirst() != 700 ||
				cw.GetExpected().GetCore() != "lab A" || cw.GetExpected().GetEntries()[0].GetFinding() != "EX-1" {
				t.Errorf("conformance = %v", cw)
			}
			if p.GetGates()[0].GetConformance() == nil || p.GetGates()[0].GetWorkload() != "suite" {
				t.Errorf("gate = %v", p.GetGates()[0])
			}
		}},
		{name: "an empty behaviors keeps its UEs", run: "kind: run\nworkloads:\n  - name: f\n    behaviors: {}\n",
			check: func(t *testing.T, p *orbitv1.RunPlan) {
				if p.GetWorkloads()[0].GetPopulation().GetBehaviors() == nil {
					t.Error("behaviors dropped: the population would be a storm")
				}
			}},
		{name: "both kinds", run: "kind: run\nworkloads:\n  - name: w\n    attach: {count: 1}\n    conformance: {categories: [nas]}\n",
			wantErr: "one kind per workload"},
		{name: "two stops", run: "kind: run\nuntil: {duration: 1h, forever: true}\n", wantErr: "give one of duration, complete and forever"},
		{name: "a bad duration", run: "kind: run\nuntil: {duration: soon}\n", wantErr: "until.duration"},
		{name: "first and assign", run: "kind: run\nworkloads:\n  - name: w\n    topology: {gnb_ids: {first: 1, assign: true}}\n", wantErr: "first or assign"},
		{name: "a bad phase", run: "kind: run\nworkloads:\n  - name: s\n    start: {after: p, phase: soon}\n    conformance: {categories: [nas]}\n", wantErr: "start.phase"},
		{name: "a bad arrival", run: "kind: run\nworkloads:\n  - name: w\n    attach: {arrival: bursty}\n", wantErr: "attach.arrival"},
		{name: "a gate with no metric", run: "kind: run\ngates:\n  - name: mine\n    slo: {threshold: \"1\"}\n", wantErr: "give slo.metric"},
		{name: "a gate with neither check", run: "kind: run\ngates:\n  - name: mine\n    workload: x\n", wantErr: "give slo.threshold"},
		{name: "an expected list by path, unread", run: "kind: run\nworkloads:\n  - name: s\n    conformance: {expected: x.yaml}\n", wantErr: "does not read files"},
		{name: "an expected list for no check", run: `kind: run
workloads:
  - name: s
    conformance:
      expected: {core: c, entries: [{check: NOPE, observed: x, finding: EX-1, added: 2026-10-27, reason: r}]}
`, wantErr: `unknown check "NOPE"`},
		{name: "an expected list file with no version", run: "kind: run\nworkloads:\n  - name: s\n    conformance: {expected: x.yaml}\n",
			files: map[string]string{"x.yaml": "core: x\n"}, wantErr: "version is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var opts []Option
			if tc.files != nil {
				opts = append(opts, WithReadFile(func(p string) ([]byte, error) {
					if d, ok := tc.files[p]; ok {
						return []byte(d), nil
					}
					return nil, errors.New("no file " + p)
				}))
			}
			p, _, err := Build(resolve(t, tc.run), opts...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, p)
		})
	}
}

// samples is a value for every run-file path, as a run file writes it: the
// coverage test sets each and looks for it in the plan. A run-file path
// without one fails the test, so a new path cannot land unbuilt.
var samples = map[string]string{
	"target":                                      "lab1",
	"run.name":                                    "r",
	"run.event_verbosity":                         "verbose",
	"until.duration_ms":                           "60000",
	"until.duration":                              "1m",
	"until.complete":                              "true",
	"until.forever":                               "true",
	"workloads[].name":                            "w",
	"workloads[].start":                           "{after: x, phase: started}",
	"workloads[].attach.count":                    "5",
	"workloads[].attach.rate":                     "2.5",
	"workloads[].attach.ramp":                     "1:2:3",
	"workloads[].attach.arrival":                  "poisson",
	"workloads[].attach.concurrency":              "4",
	"workloads[].attach.distribution":             "uneven",
	"workloads[].attach.distribution_seed":        "9",
	"workloads[].attach.seed":                     "11",
	"workloads[].pdu_session":                     "true",
	"workloads[].topology.gnb_count":              "3",
	"workloads[].topology.layout":                 "grid",
	"workloads[].topology.spacing_m":              "50",
	"workloads[].topology.gnb_ids.first":          "100",
	"workloads[].topology.gnb_ids.assign":         "true",
	"workloads[].behaviors":                       "{latency: {target: 192.0.2.1}}",
	"workloads[].subscriber_pool":                 "main",
	"workloads[].sample_interval_ms":              "500",
	"workloads[].conformance.categories":          "nas",
	"workloads[].conformance.target_release":      "Rel-17",
	"workloads[].conformance.expected":            "{core: c, entries: []}",
	"workloads[].conformance.per_test_timeout_ms": "2000",
	"workloads[].conformance.nas.integrity":       "NIA2",
	"workloads[].conformance.nas.ciphering":       "NEA0",
	"workloads[].conformance.responder":           "n6",
	"workloads[].conformance.traffic":             "192.0.2.1:9",
	"workloads[].conformance.gnb_ids.first":       "700",
	"workloads[].conformance.gnb_ids.assign":      "true",
	"workloads[].conformance.subscriber_pool":     "main",
	"workloads[].conformance.n3_bind":             "192.0.2.40:2152",
	"gates[].name":                                "w", // a list element's name is its selector
	"gates[].workload":                            "w",
	"gates[].slo.metric":                          "attach.success_ratio",
	"gates[].slo.op":                              ">=",
	"gates[].slo.threshold":                       "0.5",
	"gates[].conformance":                         "true",
}

// notInPlan are run-file paths that do not land in the RunPlan: they say
// which target to use, or travel beside the plan in StartRunRequest.
var notInPlan = map[string]bool{"target": true, "run.name": true, "run.event_verbosity": true}

// notFromRunFile are RunPlan fields no run-file path sets: the target's
// (target paths), the client's provenance (Build fills it from the
// resolver), and what later steps add.
var notFromRunFile = []string{
	"target.",     // the target's own paths (internal/target)
	"provenance.", // Build, from the resolver
	"workloads.steps.",
}

// concrete is a path pattern with its selectors filled (workloads[w]).
func concrete(p string) string { return strings.ReplaceAll(p, "[]", "[w]") }

// Both ways: every run-file path reaches a RunPlan field through Build,
// and every RunPlan field below the target is reached by a run-file path
// or is listed in notFromRunFile. A whole-value subtree (behaviors, start,
// expected) counts as one field.
func TestEveryRunPathReachesThePlan(t *testing.T) {
	base, _, err := Build(resolve(t, "kind: run\n"))
	if err != nil {
		t.Fatal(err)
	}
	reached := map[string]bool{}
	for _, p := range target.RunPaths() {
		v, ok := samples[p]
		if !ok {
			t.Errorf("%s has no sample value: add one, and Build it", p)
			continue
		}
		in := target.Input{Layer: target.LayerFlag, Settings: []target.Setting{{Path: concrete(p), Value: v, Source: target.Source{Name: "test"}}}}
		// A gate needs a check, and a conformance field makes a suite;
		// give each the minimum beside the field under test.
		switch {
		case strings.HasPrefix(p, "gates[]") && p != "gates[].slo.threshold" && p != "gates[].conformance" && p != "gates[].slo.metric":
			in.Settings = append(in.Settings, target.Setting{Path: "gates[w].slo.metric", Value: "attach.success_ratio"},
				target.Setting{Path: "gates[w].slo.threshold", Value: "0.5"})
		case p == "gates[].slo.metric":
			in.Settings = append(in.Settings, target.Setting{Path: "gates[w].slo.threshold", Value: "0.5"})
		case p == "gates[].slo.threshold":
			in.Settings = append(in.Settings, target.Setting{Path: "gates[w].slo.metric", Value: "attach.success_ratio"})
		}
		got, _, err := Build(resolve(t, "kind: run\n", in))
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		set := setFields(got.ProtoReflect(), "")
		if notInPlan[p] {
			continue
		}
		if proto.Equal(got, base) {
			t.Errorf("%s = %s reached nothing in the plan", p, v)
			continue
		}
		for f := range set {
			reached[f] = true
		}
	}
	for _, f := range leaves((&orbitv1.RunPlan{}).ProtoReflect().Descriptor(), "") {
		excluded := false
		for _, x := range notFromRunFile {
			if strings.HasPrefix(f+".", x) || strings.HasPrefix(f, x) {
				excluded = true
			}
		}
		if !excluded && !reached[f] {
			t.Errorf("RunPlan field %s is not reached by any run-file path", f)
		}
	}
}

// wholeValue are the messages a run file gives whole: their fields are
// one field of the plan.
var wholeValue = map[protoreflect.FullName]bool{
	"orbit.v1.Behaviors": true, "orbit.v1.StartCondition": true, "orbit.v1.ExpectedFailures": true,
	"orbit.v1.ConformanceGate": true,
}

// leaves are a message's field paths down to scalars, lists and
// whole-value messages, oneof members named by their field.
func leaves(md protoreflect.MessageDescriptor, prefix string) []string {
	var out []string
	fs := md.Fields()
	for i := 0; i < fs.Len(); i++ {
		fd := fs.Get(i)
		p := prefix + string(fd.Name())
		if fd.Kind() == protoreflect.MessageKind && !fd.IsMap() && !wholeValue[fd.Message().FullName()] {
			out = append(out, leaves(fd.Message(), p+".")...)
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// setFields are the leaf paths set in m, as leaves names them.
func setFields(m protoreflect.Message, prefix string) map[string]bool {
	out := map[string]bool{}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		p := prefix + string(fd.Name())
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind && !wholeValue[fd.Message().FullName()]:
			for i := 0; i < v.List().Len(); i++ {
				for f := range setFields(v.List().Get(i).Message(), p+".") {
					out[f] = true
				}
			}
		case !fd.IsList() && !fd.IsMap() && fd.Kind() == protoreflect.MessageKind && !wholeValue[fd.Message().FullName()]:
			for f := range setFields(v.Message(), p+".") {
				out[f] = true
			}
		default:
			out[p] = true
		}
		return true
	})
	return out
}

// A whole-value subtree holding a secret has its provenance masked whole,
// its overridden values too, so a cohort's token never shows.
func TestSubtreeProvenanceMasked(t *testing.T) {
	run := `kind: run
workloads:
  - name: pop
    behaviors:
      traffic:
        mix: [{name: web, share: 1, app: {app: http, peer: 192.0.2.30:9551, token: run-file-token}}]
`
	flag := target.Input{Layer: target.LayerFlag, Settings: []target.Setting{{Path: "workloads[pop].behaviors",
		Value: "{traffic: {mix: [{name: web, share: 1, app: {app: http, token: flag-token}}]}}", Source: target.Source{Name: "--set"}}}}
	p, prov, err := Build(resolve(t, run, flag))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.GetWorkloads()[0].GetPopulation().GetBehaviors().GetTraffic().GetMix()[0].GetApp().GetToken(); got != "flag-token" {
		t.Fatalf("the plan's token = %q, want the flag's (keys travel in the plan)", got)
	}
	var found bool
	for _, fp := range prov {
		if fp.GetPath() != "workloads[pop].behaviors" {
			continue
		}
		found = true
		if fp.GetValue() != target.Masked || len(fp.GetOverridden()) != 1 || fp.GetOverridden()[0].GetValue() != target.Masked {
			t.Errorf("provenance = %v, want the value and the overridden one masked whole", fp)
		}
	}
	if !found {
		t.Fatal("no provenance for the behaviors subtree")
	}
	for _, fp := range prov {
		if s := fp.String(); strings.Contains(s, "flag-token") || strings.Contains(s, "run-file-token") || strings.Contains(s, testKi) || strings.Contains(s, "tok-secret-1") {
			t.Errorf("provenance holds a secret: %v", fp)
		}
	}
}

// --gate reads into settings on the gates list; malformed ones are
// refused naming the form.
func TestParseGate(t *testing.T) {
	tests := []struct {
		spec    string
		want    map[string]string
		wantErr string
	}{
		{spec: "registration.p99_ms<=500ms", want: map[string]string{
			"gates[registration_p99_ms].slo.metric": "registration.p99_ms", "gates[registration_p99_ms].slo.op": "<=",
			"gates[registration_p99_ms].slo.threshold": "500ms"}},
		{spec: "ok=attach.success_ratio>=0.99", want: map[string]string{"gates[ok].slo.op": ">=", "gates[ok].slo.threshold": "0.99"}},
		{spec: "conformance", want: map[string]string{"gates[conformance].conformance": "true"}},
		{spec: "suite=conformance", want: map[string]string{"gates[suite].conformance": "true"}},
		{spec: "attach.success_ratio", wantErr: "want [name=]metric<op>value"},
		{spec: "a.b=attach.success_ratio>=1", wantErr: "may not hold"},
		{spec: "a.b=conformance", wantErr: "may not hold"},
		{spec: "x[0]=conformance", wantErr: "may not hold"},
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			got, err := ParseGate(tc.spec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			m := map[string]string{}
			for _, s := range got {
				m[s.Path] = s.Value.(string)
			}
			for k, v := range tc.want {
				if m[k] != v {
					t.Errorf("%s = %q, want %q (all %v)", k, m[k], v, m)
				}
			}
		})
	}
	// And a --gate's settings build into the gate they say.
	set, _ := ParseGate("reg=registration.p99_ms<=0.5s")
	p, _, err := Build(resolve(t, "kind: run\n", target.Input{Layer: target.LayerFlag, Settings: set}))
	if err != nil {
		t.Fatal(err)
	}
	if g := p.GetGates()[0].GetSlo(); g.GetThreshold() != 500 || g.GetOp() != orbitv1.CompareOp_COMPARE_OP_LE {
		t.Errorf("gate = %v, want registration.p99_ms <= 500", g)
	}
}

// Every template is a run file that builds against a target, and the
// population templates gate on attach success at least 0.99 (ADR-0008
// resolved question 4).
func TestTemplatesBuild(t *testing.T) {
	if got := Templates(); strings.Join(got, ",") != "conformance,fleet,load" {
		t.Fatalf("templates = %v", got)
	}
	for _, name := range Templates() {
		t.Run(name, func(t *testing.T) {
			text, _ := TemplateText(name)
			p, _, err := Build(resolve(t, text))
			if err != nil {
				t.Fatal(err)
			}
			if len(p.GetWorkloads()) != 1 || len(p.GetGates()) != 1 {
				t.Fatalf("plan = %v", p)
			}
			g := p.GetGates()[0]
			switch name {
			case "conformance":
				if g.GetConformance() == nil || p.GetWorkloads()[0].GetConformance() == nil {
					t.Errorf("conformance template: %v", p)
				}
			default:
				if s := g.GetSlo(); s.GetMetric() != "attach.success_ratio" || s.GetOp() != orbitv1.CompareOp_COMPARE_OP_GE || s.GetThreshold() != 0.99 {
					t.Errorf("%s gate = %v", name, s)
				}
			}
		})
	}
	if _, err := Template("nope"); err == nil || !strings.Contains(err.Error(), "want one of conformance, fleet, load") {
		t.Errorf("unknown template: %v", err)
	}
}

// Gates builds the gates alone, from gate settings with no workloads (the
// start-load and start-fleet flags), and refuses a gate it cannot build.
func TestGates(t *testing.T) {
	gate := func(specs ...string) []target.Setting {
		var out []target.Setting
		for _, s := range specs {
			set, err := ParseGate(s)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, set...)
		}
		return out
	}
	tests := []struct {
		name     string
		settings []target.Setting
		want     []string // name op threshold
		wantErr  string
	}{
		{name: "none", want: nil},
		{name: "slo", settings: gate("registration.p99_ms<=500ms"), want: []string{"registration_p99_ms COMPARE_OP_LE 500"}},
		{name: "--slo-* alias: threshold only", settings: []target.Setting{
			{Path: "gates[" + target.GateName("", target.MetricSuccessRatio) + "].slo.threshold", Value: "0.95"}},
			want: []string{"attach_success_ratio COMPARE_OP_GE 0.95"}},
		{name: "conformance", settings: gate("conformance"), want: []string{"conformance conformance"}},
		{name: "a name that says no metric", settings: []target.Setting{{Path: "gates[x].slo.threshold", Value: "1"}},
			wantErr: "give slo.metric"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := target.Resolve(target.Input{Layer: target.LayerFlag, Settings: tc.settings})
			if err != nil {
				t.Fatal(err)
			}
			gates, err := Gates(res)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, g := range gates {
				if s := g.GetSlo(); s != nil {
					got = append(got, fmt.Sprintf("%s %s %g", g.GetName(), s.GetOp(), s.GetThreshold()))
					continue
				}
				got = append(got, g.GetName()+" conformance")
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("gates = %v, want %v", got, tc.want)
			}
		})
	}
}

// The fleet template's latency probe echoes the target's first responder's
// data_ip; a target with none runs the fleet idle, with a note, and never
// fails. Other templates take nothing more from the target.
func TestFillTemplate(t *testing.T) {
	withResponder := func(ips ...string) target.Target {
		tg := target.Target{Name: "lab"}
		for i, ip := range ips {
			tg.App.Responders = append(tg.App.Responders, target.Responder{Name: fmt.Sprintf("r%d", i), DataIP: ip})
		}
		return tg
	}
	const noResponder = "fleet template: no responder with a data_ip in target lab; running without the latency probe"
	tests := []struct {
		name        string
		template    string
		target      target.Target
		wantChanged bool
		wantNote    string
		wantLatency string // the built plan's latency target; "" none
	}{
		{name: "fleet, a responder", template: "fleet", target: withResponder("198.51.100.7", "198.51.100.8"),
			wantChanged: true, wantLatency: "198.51.100.7"},
		{name: "fleet, no responder", template: "fleet", target: withResponder(), wantNote: noResponder},
		{name: "fleet, a responder with no data_ip", template: "fleet", target: withResponder(""), wantNote: noResponder},
		{name: "load", template: "load", target: withResponder("198.51.100.7")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rf, err := Template(tc.template)
			if err != nil {
				t.Fatal(err)
			}
			changed, note := FillTemplate(tc.template, rf, tc.target)
			if changed != tc.wantChanged || note != tc.wantNote {
				t.Fatalf("FillTemplate = %t, %q; want %t, %q", changed, note, tc.wantChanged, tc.wantNote)
			}
			files, err := target.Parse("lab1.yaml", []byte(labTarget), noEnv)
			if err != nil {
				t.Fatal(err)
			}
			res, err := target.Resolve(target.DefaultInput(), files[0].Input, rf.Input)
			if err != nil {
				t.Fatal(err)
			}
			p, _, err := Build(res)
			if err != nil {
				t.Fatal(err)
			}
			pop := p.GetWorkloads()[0].GetPopulation()
			if got := pop.GetBehaviors().GetLatency().GetTarget(); got != tc.wantLatency {
				t.Errorf("latency target = %q, want %q", got, tc.wantLatency)
			}
			if tc.template == "fleet" && pop.GetBehaviors() == nil {
				t.Error("the fleet lost its behaviours: it would not keep its UEs")
			}
		})
	}
}

// conformance.nas is the two lists --nas-integrity and --nas-ciphering
// set (main's flags, bound in the mapping): from those flags' paths they
// reach the plan's ConformanceWorkload.nas.
func TestNASFlagsReachThePlan(t *testing.T) {
	ia, ok := target.FlagBinding("orbit conformance", "nas-integrity")
	if !ok {
		t.Fatal("--nas-integrity is not bound")
	}
	ea, ok := target.FlagBinding("orbit conformance", "nas-ciphering")
	if !ok {
		t.Fatal("--nas-ciphering is not bound")
	}
	src := target.Source{Name: "--nas-integrity"}
	tests := []struct {
		name     string
		settings []target.Setting
		wantIA   []string
		wantEA   []string
	}{
		{name: "integrity", settings: []target.Setting{{Path: ia.Path, Value: []string{"NIA1", "NIA3"}, Source: src}}, wantIA: []string{"NIA1", "NIA3"}},
		{name: "ciphering", settings: []target.Setting{{Path: ea.Path, Value: []string{"NEA2"}, Source: src}}, wantEA: []string{"NEA2"}},
		{name: "both", settings: []target.Setting{{Path: ia.Path, Value: []string{"NIA2"}, Source: src}, {Path: ea.Path, Value: []string{"NEA0"}, Source: src}},
			wantIA: []string{"NIA2"}, wantEA: []string{"NEA0"}},
		{name: "neither"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := resolve(t, "kind: run\nworkloads:\n  - name: suite\n    conformance: {categories: [nas]}\n",
				target.Input{Layer: target.LayerFlag, Settings: tc.settings})
			p, _, err := Build(res)
			if err != nil {
				t.Fatal(err)
			}
			nas := p.GetWorkloads()[0].GetConformance().GetNas()
			if (tc.wantIA == nil && tc.wantEA == nil) != (nas == nil) {
				t.Fatalf("nas = %v", nas)
			}
			if !slices.Equal(nas.GetIntegrity(), tc.wantIA) || !slices.Equal(nas.GetCiphering(), tc.wantEA) {
				t.Errorf("nas = %v, want %v / %v", nas, tc.wantIA, tc.wantEA)
			}
		})
	}
}
