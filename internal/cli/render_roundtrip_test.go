package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/plan"
)

const (
	renderKi  = "465b5ce8b199b49faa5f0a2ee238a6bc"
	renderOPc = "981d464c7c52eb6e5036234984ad0bcf"
)

// shellArgv runs command through sh, as a user's shell runs what they
// paste, with orbit a stub that prints its argv; it returns that argv
// (orbit's arguments) and fails on any file left in dir, which a value
// the quoting let the shell act on would create.
func shellArgv(t *testing.T, dir, command string) []string {
	t.Helper()
	bin := t.TempDir()
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done\n"
	if err := os.WriteFile(filepath.Join(bin, "orbit"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("sh -c %s: %v\n%s", command, err, errOut.String())
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("the shell made files in %s running %s", dir, command)
	}
	if errOut.Len() > 0 {
		t.Errorf("the shell said: %s", errOut.String())
	}
	argv := strings.Split(out.String(), "\x00")
	return argv[:len(argv)-1]
}

// A value reaches orbit as one argument, exactly as written, whatever it
// holds: copy-as-CLI is pasted into a shell.
func TestRenderedCommandQuoting(t *testing.T) {
	load, err := plan.TemplateText("load")
	if err != nil {
		t.Fatal(err)
	}
	// A YAML double-quoted string: strconv's escapes (\t, \x7f, \u202e, …)
	// are YAML's too, so every control reaches the run file escaped.
	withPool := func(v string) []byte {
		return []byte(strings.Replace(load, "  - name: load\n", "  - name: load\n    subscriber_pool: "+strconv.Quote(v)+"\n", 1))
	}
	hostile := []string{
		"$(touch pwned1)", "`touch pwned2`", "; touch pwned3", "&& touch pwned4", "| touch pwned5",
		"it's", `say "hi"`, `back\slash`, "-x", "--server=evil", "*", "?", "[ab]", "~", "$HOME", "${HOME}",
		"ü€", "  spaced  ", "",
	}
	for _, v := range hostile {
		t.Run(v, func(t *testing.T) {
			r, err := plan.Render(plan.RenderInput{RunYAML: withPool(v), Template: "load", Target: "lab1",
				Sets: []string{"workloads[load].attach.distribution=" + v}})
			if err != nil {
				t.Fatal(err)
			}
			pasteSafe(t, r.Command)
			argv := shellArgv(t, t.TempDir(), r.Command)
			want := []string{"test", "load", "--target=lab1", "--set=workloads[load].subscriber_pool=" + v, "--set=workloads[load].attach.distribution=" + v}
			if strings.Contains(v, "${") {
				// A ${VAR} in the run file stays in the file, for the
				// command's shell to fill: orbit run.
				want = []string{"run", "./lab1.run.yaml", "--target=lab1", "--set=workloads[load].attach.distribution=" + v}
			}
			if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
				t.Errorf("argv\n got %q\nwant %q\nfrom %s", argv, want, r.Command)
			}
		})
	}
	// What a terminal acts on when pasted (^C, ESC, CR, a bracketed-paste
	// end), or what shows text other than it runs (bidi, zero-width), or an
	// argument over 4 KiB, never goes in the command: from the run file it
	// goes in the file (orbit run <file>), which keeps it; from --set it is
	// refused, naming the path.
	unsafe := map[string]string{
		"a newline": "a\nb", "a tab": "a\tb", "^C then CR": "x\x03\rrm -rf ~", "an ESC": "\x1b[201~", "NUL": "a\x00b",
		"DEL": "a\x7fb", "a C1 control": "a\u009bb", "RLO": "ü€\u202e", "an isolate": "a\u2066b", "LRM": "a\u200eb",
		"ALM": "a\u061cb", "a line separator": "a\u2028b", "a paragraph separator": "a\u2029b", "a zero-width space": "a\u200bb",
		"5 KiB": strings.Repeat("x", 5000),
	}
	for name, v := range unsafe {
		t.Run("unsafe/"+name, func(t *testing.T) {
			r, err := plan.Render(plan.RenderInput{RunYAML: withPool(v), Template: "load", Target: "lab1"})
			if err != nil {
				t.Fatal(err)
			}
			pasteSafe(t, r.Command)
			if r.Command != "orbit run './lab1.run.yaml' '--target=lab1'" || r.FileName != "lab1.run.yaml" {
				t.Errorf("command %q, file %q; want orbit run with the file", r.Command, r.FileName)
			}
			var back struct {
				Workloads []struct {
					SubscriberPool string `yaml:"subscriber_pool"`
				} `yaml:"workloads"`
			}
			if err := yaml.Unmarshal([]byte(r.RunYAML), &back); err != nil || len(back.Workloads) != 1 || back.Workloads[0].SubscriberPool != v {
				t.Errorf("the file does not keep the value: %v %+v", err, back)
			}
			_, err = plan.Render(plan.RenderInput{RunYAML: []byte(load), Template: "load", Target: "lab1",
				Sets: []string{"workloads[load].attach.distribution=" + v}})
			if err == nil || !strings.Contains(err.Error(), "--set workloads[load].attach.distribution") {
				t.Errorf("a --set holding it: err = %v, want refused naming the path", err)
			}
			_, err = plan.Render(plan.RenderInput{RunYAML: []byte(load), Template: "load", Target: "lab1", Gates: []string{"x=attach.p99_ms<=" + v}})
			if err == nil {
				t.Error("a --gate holding it: not refused")
			}
		})
	}
}

// pasteSafe fails when command holds a code point a pasted terminal acts
// on, or that shows other text than it runs.
func pasteSafe(t *testing.T, command string) {
	t.Helper()
	for _, r := range command {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) ||
			r == 0x200e || r == 0x200f || r == 0x061c || r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r) {
			t.Fatalf("the command holds U+%04X: %q", r, command)
		}
	}
}

// The command copy-as-CLI gives, split by the shell and run through the
// real command tree, starts the plan PlanRun builds from the same
// documents; so does the exported run file through orbit run.
func TestRenderedCommandRoundTrip(t *testing.T) {
	clearOrbitEnv(t)
	t.Setenv("ORBIT_TEST_KI", renderKi)
	t.Setenv("ORBIT_TEST_OPC", renderOPc)
	tmpl := func(name string) string {
		s, err := plan.TemplateText(name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	edit := func(name string, pairs ...string) string {
		s := tmpl(name)
		for i := 0; i+1 < len(pairs); i += 2 {
			if !strings.Contains(s, pairs[i]) {
				t.Fatalf("%q not in %s", pairs[i], name)
			}
			s = strings.Replace(s, pairs[i], pairs[i+1], 1)
		}
		return s
	}
	tests := []struct {
		name      string
		in        plan.RenderInput
		wantWords string // the command's first words
		target    string // the target document; lab1 when empty
		probe     bool   // the plan keeps the fleet's latency probe
	}{
		// A target whose first responder has a data_ip fills the fleet's
		// latency probe: orbit test fills it itself; on the orbit run
		// fallback the exported file carries it (FleetProbe), which the
		// server fills at the start, as PlanRun's plan does.
		{name: "fleet with a responder", in: plan.RenderInput{RunYAML: []byte(tmpl("fleet")), Template: "fleet"}, wantWords: "orbit test fleet",
			target: lab1Responder, probe: true},
		{name: "fleet with a responder, its gate removed", in: plan.RenderInput{Template: "fleet", RunYAML: []byte(tmpl("fleet")[:strings.Index(tmpl("fleet"), "gates:")])},
			wantWords: "orbit run", target: lab1Responder, probe: true},
		{name: "load", in: plan.RenderInput{RunYAML: []byte(tmpl("load")), Template: "load"}, wantWords: "orbit test load"},
		{name: "fleet", in: plan.RenderInput{RunYAML: []byte(tmpl("fleet")), Template: "fleet"}, wantWords: "orbit test fleet"},
		{name: "conformance", in: plan.RenderInput{RunYAML: []byte(tmpl("conformance")), Template: "conformance"}, wantWords: "orbit test conformance"},
		{name: "load edited", in: plan.RenderInput{Template: "load", RunYAML: []byte(edit("load", "count: 100", "count: 250", "rate: 20", "rate: 7.5",
			"until: {complete: true}", "until: {duration: 90s}", `threshold: "0.99"`, `threshold: "0.95"`))}, wantWords: "orbit test load"},
		{name: "load with a second workload and gate", in: plan.RenderInput{Template: "load", RunYAML: []byte(edit("load",
			"until:", "  - name: suite\n    start: {after: load, phase: attached}\n    conformance: {categories: [nas], per_test_timeout_ms: 4000}\nuntil:",
			`threshold: "0.99"}`, "threshold: \"0.99\"}\n  - name: reg\n    workload: load\n    slo: {metric: registration.p99_ms, op: \"<=\", threshold: 500ms}"))},
			wantWords: "orbit test load"},
		{name: "fleet behaviours", in: plan.RenderInput{Template: "fleet", RunYAML: []byte(edit("fleet", "behaviors: {}",
			"behaviors: {mobility: {model: random_walk, speed_mps: 3}, latency: {target: 198.51.100.9, interval_ms: 500}}"))}, wantWords: "orbit test fleet"},
		{name: "conformance with an inline list", in: plan.RenderInput{Template: "conformance", RunYAML: []byte(edit("conformance", "conformance: {categories: []}",
			"conformance:\n      categories: [ngap]\n      expected:\n        core: lab\n        version: \"1\"\n        entries:\n          - {check: NGAP-NG-RESET-ACK, observed: FAIL, finding: EX-1, added: 2026-10-28, reason: a known gap}"))},
			wantWords: "orbit test conformance"},
		{name: "the flag layer", in: plan.RenderInput{RunYAML: []byte(tmpl("load")), Template: "load",
			Sets: []string{"workloads[load].attach.concurrency=8"}, Gates: []string{"attach.p99_ms<=1s"}}, wantWords: "orbit test load"},
		// What --set cannot say goes as orbit run and the exported file.
		{name: "two categories", in: plan.RenderInput{Template: "conformance", RunYAML: []byte(edit("conformance", "categories: []", "categories: [nas, ngap]"))},
			wantWords: "orbit run"},
		{name: "a removed gate", in: plan.RenderInput{Template: "load", RunYAML: []byte(tmpl("load")[:strings.Index(tmpl("load"), "gates:")])}, wantWords: "orbit run"},
		{name: "no template", in: plan.RenderInput{RunYAML: []byte(edit("fleet", "count: 100", "count: 3"))}, wantWords: "orbit run"},
		{name: "the run-files example", in: plan.RenderInput{RunYAML: []byte(runFilesExample)}, wantWords: "orbit run"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Target = "lab1"
			doc := lab1
			if tc.target != "" {
				doc = tc.target
			}
			// Keyless, as the Targets page keeps a target: RenderRun is
			// sent it, and refuses a key written out.
			doc = strings.Replace(doc, "opc: 981d464c7c52eb6e5036234984ad0bcf", "opc: ${ORBIT_TEST_OPC}", 1)
			tc.in.TargetYAML = []byte(doc)
			loc := targetDir(t, doc)
			want, err := plan.FromDocuments(plan.Documents{TargetYAML: []byte(doc), RunYAML: tc.in.RunYAML, Template: tc.in.Template,
				Sets: tc.in.Sets, Gates: tc.in.Gates, Vars: map[string]string{"ORBIT_TEST_KI": renderKi, "ORBIT_TEST_OPC": renderOPc}})
			if err != nil {
				t.Fatal(err)
			}
			r, err := plan.Render(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(r.Command, tc.wantWords+" ") {
				t.Fatalf("command %s, want %s …", r.Command, tc.wantWords)
			}
			dir := t.TempDir()
			if r.FileName != "" {
				if err := os.WriteFile(filepath.Join(dir, r.FileName), []byte(r.RunYAML), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			pasteSafe(t, r.Command)
			argv := shellArgv(t, dir, r.Command)
			t.Chdir(dir)
			stub := &stubRunService{startRunID: "run-r1"}
			url := stubRunServer(t, stub)
			args := append(argv, "--server", *url)
			if argv[0] == "run" {
				args = append(args, "--detach") // orbit run waits for the end; the stub has none
			}
			run := runOrbit(t, loc, args...)
			if run.execErr != nil {
				t.Fatalf("%v\n%s", run.execErr, run.err)
			}
			got := proto.Clone(stub.startReq.GetPlan()).(*orbitv1.RunPlan)
			if l := got.GetWorkloads()[0].GetPopulation().GetBehaviors().GetLatency(); tc.probe && l == nil {
				t.Errorf("the plan lost the fleet's latency probe: %v", got.GetWorkloads()[0])
			}
			wantPlan := proto.Clone(want.Request.GetPlan()).(*orbitv1.RunPlan)
			got.Provenance, wantPlan.Provenance = nil, nil // the layers differ; the plan must not
			if !proto.Equal(got, wantPlan) {
				t.Errorf("the CLI's plan differs from PlanRun's\n got %v\nwant %v", got, wantPlan)
			}
			if stub.startReq.GetName() != want.Request.GetName() || stub.startReq.GetEventVerbosity() != want.Request.GetEventVerbosity() {
				t.Errorf("name/verbosity %q/%v, want %q/%v", stub.startReq.GetName(), stub.startReq.GetEventVerbosity(), want.Request.GetName(), want.Request.GetEventVerbosity())
			}
		})
	}
}

// lab1Responder is lab1 with a responder whose data_ip the fleet's
// latency probe echoes.
var lab1Responder = lab1 + `app:
  responders:
    - {name: n6, control: 198.51.100.9:9551, data_ip: 198.51.100.9}
`

// runFilesExample is docs/run-files.md's document, its expected list
// inline (the page reads no file).
const runFilesExample = `kind: run
version: 1
run:
  name: nightly-soak
  event_verbosity: normal
until: {duration: 2h}
workloads:
  - name: pop
    attach: {count: 500, rate: 20, concurrency: 64}
    topology: {gnb_count: 4}
  - name: suite
    start: {after: pop, phase: attached}
    conformance: {categories: [registration], expected: {core: lab, version: "1", entries: []}}
gates:
  - name: reg
    workload: pop
    slo: {metric: registration.p99_ms, op: "<=", threshold: 500ms}
  - name: suite
    workload: suite
    conformance: true
`
