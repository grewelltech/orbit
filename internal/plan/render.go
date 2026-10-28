package plan

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/secrets"
	"github.com/bgrewell/orbit/internal/target"
)

// RenderInput is a run as a client has it: its run file, the template the
// file started from ("" for none), the target it runs against, and the
// flag layer (--set and --gate).
type RenderInput struct {
	RunYAML  []byte
	Template string
	Target   string
	Sets     []string // path=value
	Gates    []string // --gate specs
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
	text, err := CanonicalRunYAML(in.RunYAML)
	if err != nil {
		return nil, err
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
		if sets, ok := templateSets(in.Template, rf, flags); ok {
			args := append([]string{"--target=" + in.Target}, sets...)
			return &Rendered{Command: command([]string{"orbit", "test", in.Template}, append(args, tail[1:]...)), RunYAML: text}, nil
		}
	}
	file := in.Target + ".run.yaml"
	return &Rendered{Command: command([]string{"orbit", "run"}, append([]string{"./" + file}, tail...)), RunYAML: text, FileName: file}, nil
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

// flagSettings is the flag layer --set and --gate give.
func flagSettings(sets, gates []string) ([]target.Setting, error) {
	var out []target.Setting
	for _, g := range gates {
		s, err := ParseGate(g)
		if err != nil {
			return nil, err
		}
		out = append(out, s...)
	}
	for _, kv := range sets {
		path, value, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("--set %q: want path=value", kv)
		}
		out = append(out, target.Setting{Path: strings.TrimSpace(path), Value: value, Source: target.Source{Name: "--set"}})
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
	return sets, true
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
