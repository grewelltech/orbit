package plan

import (
	"errors"
	"strings"
	"testing"
)

// A run file renders as orbit test <template> --set … when the template
// and --set give its plan, else as orbit run <file>; each argument quoted.
func TestRender(t *testing.T) {
	tmpl := func(name string) string {
		s, err := TemplateText(name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	edit := func(name, from, to string) string {
		s := tmpl(name)
		if !strings.Contains(s, from) {
			t.Fatalf("%q not in template %s", from, name)
		}
		return strings.Replace(s, from, to, 1)
	}
	tests := []struct {
		name     string
		in       RenderInput
		want     string // the whole command
		wantFile string
		wantErr  string
	}{
		{name: "load as written", in: RenderInput{RunYAML: []byte(tmpl("load")), Template: "load", Target: "lab1"},
			want: `orbit test load '--target=lab1'`},
		{name: "fleet as written", in: RenderInput{RunYAML: []byte(tmpl("fleet")), Template: "fleet", Target: "lab1"},
			want: `orbit test fleet '--target=lab1'`},
		{name: "conformance as written", in: RenderInput{RunYAML: []byte(tmpl("conformance")), Template: "conformance", Target: "lab1"},
			want: `orbit test conformance '--target=lab1'`},
		{name: "a changed count", in: RenderInput{RunYAML: []byte(edit("load", "count: 100", "count: 500")), Template: "load", Target: "lab1"},
			want: `orbit test load '--target=lab1' '--set=workloads[load].attach.count=500'`},
		{name: "a duration instead of complete", in: RenderInput{RunYAML: []byte(edit("load", "until: {complete: true}", "until: {duration: 10m}")), Template: "load", Target: "lab1"},
			want: `orbit test load '--target=lab1' '--for=10m'`},
		// Said as the CLI would: --for and --slo-*, each kept only when it
		// builds the run file's plan.
		{name: "a duration and a count", in: RenderInput{RunYAML: []byte(strings.NewReplacer("until: {complete: true}", "until: {duration: 90s}", "count: 100", "count: 500").Replace(tmpl("load"))), Template: "load", Target: "lab1"},
			want: `orbit test load '--target=lab1' '--set=workloads[load].attach.count=500' '--for=90s'`},
		{name: "run forever: no --for", in: RenderInput{RunYAML: []byte(edit("load", "until: {complete: true}", "until: {forever: true}")), Template: "load", Target: "lab1"},
			want: `orbit test load '--target=lab1' '--set=until.forever=true' '--set=until.complete=false'`},
		{name: "the alias gate's threshold", in: RenderInput{RunYAML: []byte(edit("load", `threshold: "0.99"`, `threshold: "0.95"`)), Template: "load", Target: "lab1"},
			want: `orbit test load '--target=lab1' '--slo-min-success=0.95'`},
		{name: "a P99 alias gate added", in: RenderInput{RunYAML: []byte(tmpl("load") + "  - name: registration_p99_ms\n    slo: {metric: registration.p99_ms, op: \"<=\", threshold: 500ms}\n"), Template: "load", Target: "lab1"},
			want: `orbit test load '--target=lab1' '--slo-reg-p99=500ms'`},
		{name: "a P99 gate under the alias's name, another comparison: no --slo", in: RenderInput{RunYAML: []byte(tmpl("load") + "  - name: registration_p99_ms\n    slo: {metric: registration.p99_ms, op: \"<\", threshold: 500ms}\n"), Template: "load", Target: "lab1"},
			want: `orbit test load '--target=lab1' '--set=gates[registration_p99_ms].name=registration_p99_ms' '--set=gates[registration_p99_ms].slo.metric=registration.p99_ms' '--set=gates[registration_p99_ms].slo.op=<' '--set=gates[registration_p99_ms].slo.threshold=500ms'`},
		{name: "a P99 alias threshold the flag would refuse (no unit): no --slo", in: RenderInput{RunYAML: []byte(tmpl("load") + "  - name: registration_p99_ms\n    slo: {metric: registration.p99_ms, op: \"<=\", threshold: \"500\"}\n"), Template: "load", Target: "lab1"},
			want: `orbit test load '--target=lab1' '--set=gates[registration_p99_ms].name=registration_p99_ms' '--set=gates[registration_p99_ms].slo.metric=registration.p99_ms' '--set=gates[registration_p99_ms].slo.op=<=' '--set=gates[registration_p99_ms].slo.threshold=500'`},
		{name: "a duration and an alias threshold together", in: RenderInput{RunYAML: []byte(strings.NewReplacer("until: {complete: true}", "until: {duration: 10m}", `threshold: "0.99"`, `threshold: "0.9"`).Replace(tmpl("load"))), Template: "load", Target: "lab1"},
			want: `orbit test load '--target=lab1' '--for=10m' '--slo-min-success=0.9'`},
		{name: "a whole value", in: RenderInput{RunYAML: []byte(edit("fleet", "behaviors: {}", "behaviors: {latency: {target: 10.0.0.9}}")), Template: "fleet", Target: "lab1"},
			want: `orbit test fleet '--target=lab1' '--set=workloads[fleet].behaviors={latency: {target: 10.0.0.9}}'`},
		{name: "one category", in: RenderInput{RunYAML: []byte(edit("conformance", "categories: []", "categories: [nas]")), Template: "conformance", Target: "lab1"},
			want: `orbit test conformance '--target=lab1' '--set=workloads[conformance].conformance.categories=nas'`},
		{name: "a quote in a value", in: RenderInput{RunYAML: []byte(edit("load", "  - name: load\n", "  - name: load\n    subscriber_pool: \"it's\"\n")), Template: "load", Target: "lab1"},
			want: `orbit test load '--target=lab1' '--set=workloads[load].subscriber_pool=it'\''s'`},
		{name: "the flag layer passes through", in: RenderInput{RunYAML: []byte(tmpl("load")), Template: "load", Target: "lab1",
			Sets: []string{"workloads[load].attach.rate=5"}, Gates: []string{"registration.p99_ms<=500ms"}},
			want: `orbit test load '--target=lab1' '--set=workloads[load].attach.rate=5' '--gate=registration.p99_ms<=500ms'`},
		// What --set cannot say: a run file.
		{name: "two categories", in: RenderInput{RunYAML: []byte(edit("conformance", "categories: []", "categories: [nas, ngap]")), Template: "conformance", Target: "lab1"},
			want: `orbit run './lab1.run.yaml' '--target=lab1'`, wantFile: "lab1.run.yaml"},
		{name: "a removed gate", in: RenderInput{RunYAML: []byte(tmpl("load")[:strings.Index(tmpl("load"), "gates:")]), Template: "load", Target: "lab1"},
			want: `orbit run './lab1.run.yaml' '--target=lab1'`, wantFile: "lab1.run.yaml"},
		{name: "a ${VAR}", in: RenderInput{RunYAML: []byte(edit("fleet", "behaviors: {}", "behaviors: {latency: {target: \"${ECHO}\"}}")), Template: "fleet", Target: "lab1"},
			want: `orbit run './lab1.run.yaml' '--target=lab1'`, wantFile: "lab1.run.yaml"},
		{name: "a token as a ${VAR}", in: RenderInput{RunYAML: []byte(edit("fleet", "behaviors: {}", "behaviors: {traffic: {bulk: {ues: 1, responder: app1, token: \"${APP1_TOKEN}\"}}}")), Template: "fleet", Target: "lab1"},
			want: `orbit run './lab1.run.yaml' '--target=lab1'`, wantFile: "lab1.run.yaml"},
		// --set can clear a removed string only to "", which a gate's
		// threshold still counts as set: the plans differ, so orbit run.
		{name: "a gate made a conformance gate", in: RenderInput{RunYAML: []byte(edit("load",
			`slo: {metric: attach.success_ratio, op: ">=", threshold: "0.99"}`, "conformance: true")), Template: "load", Target: "lab1"},
			want: `orbit run './lab1.run.yaml' '--target=lab1'`, wantFile: "lab1.run.yaml"},
		{name: "no template", in: RenderInput{RunYAML: []byte(tmpl("load")), Target: "lab1"},
			want: `orbit run './lab1.run.yaml' '--target=lab1'`, wantFile: "lab1.run.yaml"},
		// Refused.
		{name: "a literal token in behaviors", in: RenderInput{RunYAML: []byte(edit("fleet", "behaviors: {}", "behaviors: {traffic: {bulk: {ues: 1, responder: app1, token: s3cr3t-value}}}")), Template: "fleet", Target: "lab1"},
			wantErr: "literal secret"},
		{name: "another target", in: RenderInput{RunYAML: []byte("target: lab2\n" + tmpl("load")), Template: "load", Target: "lab1"},
			wantErr: `target: is "lab2"`},
		{name: "a target field", in: RenderInput{RunYAML: []byte(tmpl("load") + "core: {amf: x}\n"), Template: "load", Target: "lab1"},
			wantErr: "unknown key core"},
		{name: "a bad target name", in: RenderInput{RunYAML: []byte(tmpl("load")), Target: "-x"}, wantErr: "want letters"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Render(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "s3cr3t") {
					t.Errorf("the secret in %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Command != tc.want {
				t.Errorf("command\n got %s\nwant %s", r.Command, tc.want)
			}
			if r.FileName != tc.wantFile {
				t.Errorf("file = %q, want %q", r.FileName, tc.wantFile)
			}
		})
	}
	if _, err := Render(RenderInput{RunYAML: []byte(tmpl("fleet") + "x: {token: abc}\n"), Target: "lab1"}); !errors.Is(err, ErrLiteralSecret) {
		t.Errorf("a literal secret anywhere: err = %v, want ErrLiteralSecret", err)
	}
}

// The export re-encodes once: again changes nothing.
func TestCanonicalRunYAML(t *testing.T) {
	for _, name := range Templates() {
		text, err := TemplateText(name)
		if err != nil {
			t.Fatal(err)
		}
		once, err := CanonicalRunYAML([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		twice, err := CanonicalRunYAML([]byte(once))
		if err != nil || twice != once {
			t.Errorf("%s: not stable (%v):\n%s\n---\n%s", name, err, once, twice)
		}
	}
	if _, err := CanonicalRunYAML(nil); err == nil {
		t.Error("an empty run file re-encoded")
	}
}

// On the orbit run fallback, the fleet's probe goes in the file only when
// the target gives it a target and the run file leaves behaviors as the
// template has them.
func TestRenderFleetProbe(t *testing.T) {
	fleet, err := TemplateText("fleet")
	if err != nil {
		t.Fatal(err)
	}
	noGate := fleet[:strings.Index(fleet, "gates:")]
	base := "kind: target\nname: lab1\n"
	tests := []struct {
		name, target, run string
		want              bool
	}{
		{"a responder with a data_ip", base + "app:\n  responders:\n    - {name: n6, control: \"h:1\", data_ip: \"${N6_IP}\"}\n", noGate, true},
		{"a registered responder (named only)", base + "app:\n  responders:\n    - {name: app1}\n", noGate, true},
		{"no responder", base, noGate, false},
		{"a responder with no data_ip", base + "app:\n  responders:\n    - {name: n6, control: \"h:1\"}\n", noGate, false},
		{"behaviors edited", base + "app:\n  responders:\n    - {name: app1}\n", strings.Replace(noGate, "behaviors: {}", "behaviors: {mobility: {model: random_walk}}", 1), false},
		{"no target sent", "", noGate, false},
	}
	for _, tc := range tests {
		r, err := Render(RenderInput{RunYAML: []byte(tc.run), Template: "fleet", Target: "lab1", TargetYAML: []byte(tc.target)})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := strings.Contains(r.RunYAML, "behaviors: {latency: {}}"); got != tc.want || r.FileName == "" {
			t.Errorf("%s: probe in the file %t, want %t (file %q)\n%s", tc.name, got, tc.want, r.FileName, r.RunYAML)
		}
	}
}

// pasteUnsafe takes out what a terminal may act on when a copied command
// is pasted, or that shows text other than it runs: among them a zero-width
// space (U+200B, a format character) and a right-to-left override (U+202E,
// a bidirectional control), which make an argument read as something else.
func TestPasteUnsafe(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		want bool
	}{
		{"plain", "workloads[pop].attach.count=10", false},
		{"non-ASCII text", "name=café", false},
		{"ZWSP", "name=a\u200bb", true},
		{"ZWSP alone", "\u200b", true},
		{"RLO", "name=\u202eexe.txt", true},
		{"LRI (U+2066)", "a\u2066b", true},
		{"PDI (U+2069)", "a\u2069b", true},
		{"LRM", "a\u200eb", true},
		{"ALM (U+061C)", "a\u061cb", true},
		{"word joiner (U+2060, Cf)", "a\u2060b", true},
		{"BOM (U+FEFF, Cf)", "\ufeffa", true},
		{"line separator", "a\u2028b", true},
		{"tab", "a\tb", true},
		{"newline", "a\nb", true},
		{"DEL", "a\x7fb", true},
		{"C1 (U+0085)", "a\u0085b", true},
	} {
		if got := pasteUnsafe(tc.s); got != tc.want {
			t.Errorf("%s: pasteUnsafe(%q) = %t, want %t", tc.name, tc.s, got, tc.want)
		}
	}
}
