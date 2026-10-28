package plan

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/secrets"
	"github.com/bgrewell/orbit/internal/target"
)

// RenderInput is a run as a client has it: its run file, the template the
// file started from ("" for none), the target it runs against (its name,
// and its document, keyless, to decide a template's fill), and the flag
// layer (--set and --gate).
type RenderInput struct {
	RunYAML    []byte
	Template   string
	Target     string
	TargetYAML []byte
	Sets       []string // path=value
	Gates      []string // --gate specs
}

// Rendered is a run as the CLI runs it: the command, and the run file the
// command names when it is orbit run.
type Rendered struct {
	Command  string
	RunYAML  string // the run file, re-encoded; the export
	FileName string // the run file's name in Command, when it names one
}

// ErrLiteralSecret is a secret written out in a run file: a run file to
// render or export holds ${VAR}s, never keys or tokens.
var ErrLiteralSecret = errors.New("holds a literal secret; write it as a ${VAR}")

// Render gives in as the orbit command that runs it and its run file. It
// is keyless: a run file holds no target fields, and a secret written out
// in it (a cohort's token in behaviors) is refused, naming its path. The
// command is `orbit test <template> --set …` when the template and --set
// give exactly the run file's plan (Render builds both and compares), and
// otherwise `orbit run <file>`. Every argument but the command words is
// quoted for a POSIX shell (ShellQuote).
func Render(in RenderInput) (*Rendered, error) {
	if paths := secrets.YAMLSecrets(string(in.RunYAML)); len(paths) > 0 {
		return nil, fmt.Errorf("%s: %w", strings.Join(paths, ", "), ErrLiteralSecret)
	}
	if paths := secrets.YAMLSecrets(string(in.TargetYAML)); len(paths) > 0 {
		return nil, fmt.Errorf("target: %s: %w", strings.Join(paths, ", "), ErrLiteralSecret)
	}
	if !target.ValidName(in.Target) {
		return nil, fmt.Errorf("target %q: want letters, digits, '-' and '_'", in.Target)
	}
	// ${VAR}s stay as written: the command's shell, not this one, fills them.
	rf, err := target.ParseRunFile("run file", in.RunYAML, keepRefs)
	if err != nil {
		return nil, err
	}
	if rf.Target != "" && rf.Target != in.Target {
		return nil, fmt.Errorf("the run file's target: is %q, not %q", rf.Target, in.Target)
	}
	flags, err := flagSettings(in.Sets, in.Gates)
	if err != nil {
		return nil, err
	}
	tail := []string{"--target=" + in.Target}
	for _, s := range in.Sets {
		tail = append(tail, "--set="+s)
	}
	for _, g := range in.Gates {
		tail = append(tail, "--gate="+g)
	}
	if in.Template != "" {
		if sets, ok := templateSets(in.Template, rf, flags); ok && pasteSafe(sets) {
			args := append([]string{"--target=" + in.Target}, sets...)
			text, err := CanonicalRunYAML(in.RunYAML)
			if err != nil {
				return nil, err
			}
			return &Rendered{Command: command([]string{"orbit", "test", in.Template}, append(args, tail[1:]...)), RunYAML: text}, nil
		}
	}
	// orbit run <file>: the file carries what the template's fill would
	// give (the fleet's latency probe), as PlanRun's plan does on this path.
	runYAML := in.RunYAML
	if in.Template != "" && len(in.TargetYAML) > 0 {
		if runYAML, err = withProbe(in, rf); err != nil {
			return nil, err
		}
	}
	text, err := CanonicalRunYAML(runYAML)
	if err != nil {
		return nil, err
	}
	file := in.Target + ".run.yaml"
	return &Rendered{Command: command([]string{"orbit", "run"}, append([]string{"./" + file}, tail...)), RunYAML: text, FileName: file}, nil
}

// pasteSafe reports whether every argument can go in a copied command:
// none pasteUnsafe, none over MaxArg.
func pasteSafe(args []string) bool {
	for _, a := range args {
		if pasteUnsafe(a) || len(a) > MaxArg {
			return false
		}
	}
	return true
}

// withProbe is in's run file with the fleet's latency probe written in
// (FleetProbe), when the template's fill applies: the run file leaves
// behaviors as the template has them and the target gives the probe a
// target. The target is read keyless; only its first responder's shape
// decides.
func withProbe(in RenderInput, rf *target.RunFile) ([]byte, error) {
	base, err := Template(in.Template)
	if err != nil {
		return nil, err
	}
	if !sameAsTemplate(rf, base, fleetBehaviors) {
		return in.RunYAML, nil
	}
	// Only the first responder's shape decides (a data_ip; or named only,
	// registered by join), so the document is decoded as written: its
	// ${VAR}s (keys among them) are neither expanded nor checked here.
	var t target.Target
	if err := yaml.Unmarshal(in.TargetYAML, &t); err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	if !fillProbe(in.Template, rf, t) {
		return in.RunYAML, nil
	}
	return setWorkloadField(in.RunYAML, "fleet", "behaviors", FleetProbe)
}

// setWorkloadField is run file b with workload name's key set to value
// (YAML text), comments and the rest as written.
func setWorkloadField(b []byte, name, key, value string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	var v yaml.Node
	if err := yaml.Unmarshal([]byte(value), &v); err != nil || len(v.Content) == 0 {
		return nil, fmt.Errorf("%s: %q is not YAML", key, value)
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "workloads" {
			continue
		}
		for _, w := range root.Content[i+1].Content {
			for j := 0; j+1 < len(w.Content); j += 2 {
				if w.Content[j].Value == "name" && w.Content[j+1].Value == name {
					for k := 0; k+1 < len(w.Content); k += 2 {
						if w.Content[k].Value == key {
							w.Content[k+1] = v.Content[0]
						}
					}
				}
			}
		}
	}
	return yaml.Marshal(&doc)
}

// keepRefs is a Getenv that leaves each ${VAR} as written.
func keepRefs(name string) (string, bool) { return "${" + name + "}", true }

// command is words, unquoted, then args, each quoted.
func command(words, args []string) string {
	out := append([]string(nil), words...)
	for _, a := range args {
		out = append(out, ShellQuote(a))
	}
	return strings.Join(out, " ")
}

// ShellQuote is s as one POSIX shell word: single-quoted, an embedded '
// written '\”. Nothing in it expands, so a value reaches the command as
// written, whatever it holds.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// MaxArg is the longest argument a copied command carries: a longer one
// goes in the run file (orbit run <file>), as does one a terminal would
// act on when pasted (pasteUnsafe).
const MaxArg = 4096

// pasteUnsafe reports whether s holds a code point a terminal may act on
// when the command is pasted, or that shows text other than it runs: a C0
// control (tab and newline included), DEL, a C1 control, a bidirectional
// control (U+202A–U+202E, U+2066–U+2069, U+200E, U+200F, U+061C), U+2028,
// U+2029, or any other Unicode format character (a zero-width space).
func pasteUnsafe(s string) bool {
	for _, r := range s {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0:
			return true
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f, r == 0x061c:
			return true
		case r == 0x2028, r == 0x2029, unicode.Is(unicode.Cf, r):
			return true
		}
	}
	return false
}

// ErrSecretSet is a --set or --gate that would carry a key: keys go in the
// target, as ${VAR}s, where the request's vars fill them.
var ErrSecretSet = errors.New("keys go in the target, as ${VAR}s, not in --set")

// flagSettings is the flag layer --set and --gate give, as a client sends
// it. A --set at a secret field (ki, opc, a responder token), whatever its
// value (a ${VAR} too: the flag layer expands none, so it could only reach
// the plan as that text), or a whole value with a secret-named key in it,
// is refused, naming the path. So is a --set or --gate the copied command
// could not carry: one with a pasteUnsafe code point or over MaxArg. The
// CLI's own --set is its own (internal/cli): this is the RPCs' rule.
func flagSettings(sets, gates []string) ([]target.Setting, error) {
	var out []target.Setting
	for _, g := range gates {
		if pasteUnsafe(g) || len(g) > MaxArg {
			return nil, fmt.Errorf("--gate: a control or format character, or over %d bytes; it could not be copied safely", MaxArg)
		}
		s, err := ParseGate(g)
		if err != nil {
			return nil, err
		}
		out = append(out, s...)
	}
	for _, kv := range sets {
		path, value, ok := strings.Cut(kv, "=")
		path = strings.TrimSpace(path)
		if !ok || path == "" {
			return nil, errors.New("--set: want path=value")
		}
		if pasteUnsafe(path) {
			return nil, errors.New("--set: a path with a control or format character")
		}
		if target.IsSecret(path) {
			return nil, fmt.Errorf("--set %s: %w", path, ErrSecretSet)
		}
		if target.IsWholeValue(path) {
			var v any
			if err := yaml.Unmarshal([]byte(value), &v); err == nil && target.HoldsSecret(v) {
				return nil, fmt.Errorf("--set %s: %w", path, ErrSecretSet)
			}
		}
		if pasteUnsafe(value) || len(kv) > MaxArg {
			return nil, fmt.Errorf("--set %s: a control or format character, or over %d bytes; it could not be copied safely", path, MaxArg)
		}
		out = append(out, target.Setting{Path: path, Value: value, Source: target.Source{Name: "--set"}})
	}
	return out, nil
}

// templateSets is the --set list that turns template name into rf, ok
// false when there is none: a field --set cannot say (SetText), a field
// the template sets and rf does not that --set cannot unset, a ${VAR},
// workloads or gates in another order, or a plan that differs however
// built. The plans are compared, so a list that is ok runs as rf does.
func templateSets(name string, rf *target.RunFile, flags []target.Setting) ([]string, bool) {
	base, err := Template(name)
	if err != nil {
		return nil, false
	}
	resT, err := target.Resolve(base.Input)
	if err != nil {
		return nil, false
	}
	resR, err := target.Resolve(rf.Input)
	if err != nil {
		return nil, false
	}
	for _, list := range []string{"workloads", "gates"} {
		t, r := resT.Names(list), resR.Names(list)
		if len(r) < len(t) || !slicesEqual(t, r[:len(t)]) {
			return nil, false
		}
	}
	var sets []string
	var flagLayer []target.Setting
	add := func(path, text string) {
		sets = append(sets, "--set="+path+"="+text)
		flagLayer = append(flagLayer, target.Setting{Path: path, Value: text, Source: target.Source{Name: "--set"}})
	}
	for _, s := range rf.Input.Settings {
		if s.Path == "target" {
			continue
		}
		if len(s.Source.Via) > 0 {
			return nil, false
		}
		v := resR.Run[s.Path]
		if tv, inT := resT.Run[s.Path]; inT && reflect.DeepEqual(tv, v) {
			continue
		}
		text, ok, err := target.SetText(s.Path, v)
		if err != nil || !ok {
			return nil, false
		}
		add(s.Path, text)
	}
	for _, s := range base.Input.Settings {
		if _, inR := resR.Run[s.Path]; inR {
			continue
		}
		switch {
		case target.IsRunBool(s.Path):
			add(s.Path, "false")
		case target.IsRunString(s.Path):
			add(s.Path, "")
		default:
			return nil, false
		}
	}
	viaTemplate, err := comparablePlan(base.Input, append(flagLayer, flags...))
	if err != nil {
		return nil, false
	}
	asWritten, err := comparablePlan(rf.Input, flags)
	if err != nil || !proto.Equal(viaTemplate, asWritten) {
		return nil, false
	}
	// The same, said as the CLI would: --for for the stop, --slo-* for the
	// gates they alias. Kept only when it, too, builds the run file's plan.
	if args, layer, ok := idiomatic(flagLayer); ok {
		if p, err := comparablePlan(base.Input, append(layer, flags...)); err == nil && proto.Equal(p, asWritten) {
			return args, true
		}
	}
	return sets, true
}

// sloFlags are the gates the --slo-* flags alias (target's mapping): the
// gate's name, the flag, and whether its threshold is a duration (else a
// ratio).
var sloFlags = []struct {
	gate, flag string
	duration   bool
}{
	{target.GateName("", target.MetricRegP99), "slo-reg-p99", true},
	{target.GateName("", target.MetricAttachP99), "slo-attach-p99", true},
	{target.GateName("", target.MetricSuccessRatio), "slo-min-success", false},
}

// untilPaths are the stop's fields, which --for sets together.
var untilPaths = map[string]bool{"until.duration": true, "until.duration_ms": true, "until.complete": true, "until.forever": true}

// idiomatic is the --set list layer gives, said with --for and --slo-*
// where they say it: the until fields as --for <d> when they stop the run
// after a duration d, and an alias gate's fields as its --slo-* flag. It
// returns the arguments and the flag layer they give (ForSettings, and the
// alias's threshold setting), ok false when neither applies.
func idiomatic(layer []target.Setting) (args []string, out []target.Setting, ok bool) {
	var forText string
	var forDur time.Duration
	var rest []target.Setting
	untilSet := false
	slo := map[string]string{} // flag → threshold
	sloGate := map[string]bool{}
	for _, sf := range sloFlags {
		sloGate[sf.gate] = true
	}
	for _, s := range layer {
		text, _ := s.Value.(string)
		switch {
		case untilPaths[s.Path]:
			untilSet = true
			switch s.Path {
			case "until.duration":
				if d, err := time.ParseDuration(text); err == nil && d > 0 {
					forText, forDur = text, d
				}
			case "until.duration_ms":
				if ms, err := strconv.ParseUint(text, 10, 64); err == nil && ms > 0 {
					forDur = time.Duration(ms) * time.Millisecond
					forText = forDur.String()
				}
			}
		case strings.HasPrefix(s.Path, "gates[") && sloGate[gateOf(s.Path)]:
			if !strings.HasSuffix(s.Path, ".slo.threshold") {
				// The alias sets the threshold only; plan.Build names the
				// metric and the comparison from the gate's name.
				continue
			}
			for _, sf := range sloFlags {
				if sf.gate == gateOf(s.Path) && thresholdParses(text, sf.duration) {
					slo[sf.flag] = text
				}
			}
		default:
			rest = append(rest, s)
		}
	}
	if (untilSet && forDur == 0) || (!untilSet && len(slo) == 0) {
		return nil, nil, false
	}
	for _, s := range rest {
		text, _ := s.Value.(string)
		args = append(args, "--set="+s.Path+"="+text)
		out = append(out, s)
	}
	if untilSet {
		args = append(args, "--for="+forText)
		out = append(out, ForSettings(forDur)...)
	}
	for _, sf := range sloFlags {
		if t, ok := slo[sf.flag]; ok {
			args = append(args, "--"+sf.flag+"="+t)
			out = append(out, target.Setting{Path: "gates[" + sf.gate + "].slo.threshold", Value: t, Source: target.Source{Name: "--" + sf.flag}})
		}
	}
	return args, out, true
}

// gateOf is the gate a gates[<name>]… path names, "" for another path.
func gateOf(path string) string {
	name, ok := strings.CutPrefix(path, "gates[")
	if !ok {
		return ""
	}
	name, _, _ = strings.Cut(name, "]")
	return name
}

// thresholdParses reports whether text is what a --slo-* flag takes: a
// duration (the P99 aliases) or a number (the success ratio). A threshold
// the flag would refuse stays a --set.
func thresholdParses(text string, duration bool) bool {
	if duration {
		_, err := time.ParseDuration(text)
		return err == nil
	}
	_, err := strconv.ParseFloat(text, 64)
	return err == nil
}

// ForSettings is the flag layer --for d gives: the run stops after d,
// whatever the lower layers say (until.duration_ms set; until.duration,
// complete and forever cleared). The CLI's --for and Render's proof share
// it, so a rendered --for is the --for the CLI runs.
func ForSettings(d time.Duration) []target.Setting {
	src := target.Source{Name: "--for"}
	return []target.Setting{
		{Path: "until.duration_ms", Value: uint64(d.Milliseconds()), Source: src},
		{Path: "until.duration", Value: "", Source: src},
		{Path: "until.complete", Value: false, Source: src},
		{Path: "until.forever", Value: false, Source: src},
	}
}

// renderTarget is the target templateSets builds both plans over: any
// valid target does, the comparison being of the run-file fields.
const renderTarget = `version: 1
kind: target
name: render
core:
  amf: 192.0.2.1:38412
  plmn: {mcc: "001", mnc: "01"}
subscribers:
  - name: main
    first_supi: "001010000000001"
    count: 1
    ki: 00112233445566778899aabbccddeeff
    opc: 00112233445566778899aabbccddeeff
`

// comparablePlan is the plan run and flags give over renderTarget, its
// provenance dropped (the layers differ; the plan must not).
func comparablePlan(run target.Input, flags []target.Setting) (*orbitv1.RunPlan, error) {
	fs, err := target.Parse("render", []byte(renderTarget), func(string) (string, bool) { return "", false })
	if err != nil {
		return nil, err
	}
	res, err := target.Resolve(target.DefaultInput(), fs[0].Input, run, target.Input{Layer: target.LayerFlag, Settings: flags})
	if err != nil {
		return nil, err
	}
	p, _, err := Build(res, WithReadFile(func(string) ([]byte, error) { return nil, errors.New("no file") }))
	if err != nil {
		return nil, err
	}
	return p, nil
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CanonicalRunYAML is a run file re-encoded: two-space indents, comments
// kept, the document otherwise as written. It is the run file the page
// exports, and re-encoding it again changes nothing.
func CanonicalRunYAML(b []byte) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", err
	}
	if doc.Kind == 0 {
		return "", errors.New("empty run file")
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return out.String(), nil
}
