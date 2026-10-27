package target

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

var allLayers = []Layer{LayerDefault, LayerTarget, LayerRunFile, LayerEnv, LayerFlag}

func set(path string, v any, name string) Setting {
	return Setting{Path: path, Value: v, Source: Source{Name: name}}
}

func prov(t *testing.T, r *Resolved, path string) FieldProvenance {
	t.Helper()
	for _, p := range r.Provenance {
		if p.Path == path {
			return p
		}
	}
	t.Fatalf("no provenance for %s in %+v", path, r.Provenance)
	return FieldProvenance{}
}

// Over every pair of layers the higher one wins, whatever order the inputs
// are given in, and provenance names the winner and what it overrode.
func TestPrecedenceEveryLayerPair(t *testing.T) {
	for i, lo := range allLayers {
		for _, hi := range allLayers[i+1:] {
			t.Run(lo.String()+" < "+hi.String(), func(t *testing.T) {
				low := Input{Layer: lo, Settings: []Setting{set("core.amf", "198.51.100.1:38412", "low")}}
				high := Input{Layer: hi, Settings: []Setting{set("core.amf", "198.51.100.2:38412", "high")}}
				for _, order := range [][]Input{{low, high}, {high, low}} {
					r, err := Resolve(order...)
					if err != nil {
						t.Fatal(err)
					}
					if r.Target.Core.AMF != "198.51.100.2:38412" {
						t.Errorf("AMF = %s, want the %s layer's", r.Target.Core.AMF, hi)
					}
					p := prov(t, r, "core.amf")
					if p.Source.Layer != hi || len(p.Overridden) != 1 || p.Overridden[0].Source.Layer != lo ||
						p.Overridden[0].Value != "198.51.100.1:38412" {
						t.Errorf("provenance = %+v", p)
					}
				}
			})
		}
	}
}

// Provenance lists what a field overrode, lowest layer first, with sources.
func TestProvenanceOverrideChain(t *testing.T) {
	r, err := Resolve(
		DefaultInput(),
		Input{Layer: LayerTarget, Settings: []Setting{{Path: "core.tac", Value: uint64(7), Source: Source{Name: "lab1.yaml", Line: 4}}}},
		Input{Layer: LayerEnv},
		Input{Layer: LayerFlag, Settings: []Setting{set("core.tac", "9", "--tac")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	p := prov(t, r, "core.tac")
	if p.Value != "9" || p.Source.String() != "flag --tac" {
		t.Errorf("winner = %s %q", p.Source, p.Value)
	}
	var got []string
	for _, o := range p.Overridden {
		got = append(got, o.Source.String()+"="+o.Value)
	}
	want := []string{"default built-in=1", "target lab1.yaml:4=7"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("overridden = %q, want %q", got, want)
	}
	if r.Target.Core.TAC != 9 {
		t.Errorf("TAC = %d", r.Target.Core.TAC)
	}
}

// The merge rules: scalars replace, maps merge key by key, plain lists
// replace whole, named lists merge by name, and an empty selector picks the
// default (first) element, or creates "default".
func TestMergeRules(t *testing.T) {
	target := Input{Layer: LayerTarget, Settings: []Setting{
		set("core.plmn.mcc", "001", "t"),
		set("core.plmn.mnc", "01", "t"),
		set("ran.n2_source_ips", []any{"192.0.2.50", "192.0.2.51"}, "t"),
		// In the PLMN the run layer ends on (001/02): a first SUPI outside
		// the resolved PLMN is an error.
		set("subscribers[main].first_supi", "001020000000001", "t"),
		set("subscribers[main].count", 100, "t"),
		set("subscribers[extra].first_supi", "001020000000500", "t"),
		set("subscribers[extra].count", 10, "t"),
	}}
	run := Input{Layer: LayerRunFile, Settings: []Setting{
		set("subscribers[extra].count", 20, "run"),
		set("core.plmn.mnc", "02", "run"),
	}}
	flags := Input{Layer: LayerFlag, Settings: []Setting{
		set("ran.n2_source_ips", "192.0.2.59", "--bind"),
		set("subscribers[].ki", "00112233445566778899aabbccddeeff", "--ki"),
	}}
	r, err := Resolve(target, run, flags)
	if err != nil {
		t.Fatal(err)
	}
	tg := r.Target
	if tg.Core.PLMN != (PLMN{MCC: "001", MNC: "02"}) {
		t.Errorf("PLMN = %+v: maps merge key by key", tg.Core.PLMN)
	}
	if !reflect.DeepEqual(tg.RAN.N2SourceIPs, []string{"192.0.2.59"}) {
		t.Errorf("n2_source_ips = %v: a list is replaced whole", tg.RAN.N2SourceIPs)
	}
	if p := prov(t, r, "ran.n2_source_ips"); len(p.Overridden) != 1 || p.Overridden[0].Value != "[192.0.2.50, 192.0.2.51]" {
		t.Errorf("list provenance = %+v", p)
	}
	if len(tg.Subscribers) != 2 || tg.Subscribers[0].Name != "main" || tg.Subscribers[1].Name != "extra" {
		t.Fatalf("pools = %+v", tg.Subscribers)
	}
	if tg.Subscribers[0].Count != 100 || tg.Subscribers[1].Count != 20 {
		t.Errorf("counts = %d, %d: named lists merge by name", tg.Subscribers[0].Count, tg.Subscribers[1].Count)
	}
	if tg.Subscribers[0].Ki == "" || tg.Subscribers[1].Ki != "" {
		t.Errorf("--ki went to %+v: an empty selector is the default (first) pool", tg.Subscribers)
	}

	// No pool in any lower layer: the default element is created.
	r, err = Resolve(Input{Layer: LayerEnv, Settings: []Setting{set("subscribers[].ki", "11111111111111111111111111111111", "$ORBIT_KI")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Target.Subscribers) != 1 || r.Target.Subscribers[0].Name != "default" || r.Target.Subscribers[0].Ki != "11111111111111111111111111111111" {
		t.Errorf("pools = %+v, want one named default", r.Target.Subscribers)
	}
}

// Secret fields are masked in provenance, including the values they
// overrode; other fields, SUPIs and addresses included, are not.
func TestSecretsMasked(t *testing.T) {
	r, err := Resolve(
		Input{Layer: LayerTarget, Settings: []Setting{
			set("subscribers[main].ki", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "t"), set("subscribers[main].opc", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "t"),
			set("subscribers[main].first_supi", "001010000000001", "t"),
			set("app.responders[app1].token", "tok", "t"), set("app.responders[app1].control", "203.0.113.20:9551", "t"),
		}},
		Input{Layer: LayerFlag, Settings: []Setting{set("subscribers[].ki", "cccccccccccccccccccccccccccccccc", "--ki")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Provenance {
		secret := IsSecret(p.Path)
		for _, v := range append([]string{p.Value}, overriddenValues(p)...) {
			if secret && v != Masked {
				t.Errorf("%s shows %q", p.Path, v)
			}
			if !secret && v == Masked {
				t.Errorf("%s is masked but not secret", p.Path)
			}
		}
	}
	for _, s := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccc", "tok"} {
		for _, p := range r.Provenance {
			if strings.Contains(p.Value, s) {
				t.Errorf("provenance leaks %q at %s", s, p.Path)
			}
		}
	}
	if r.Target.Subscribers[0].Ki != "cccccccccccccccccccccccccccccccc" {
		t.Error("the resolved target lost the key itself")
	}
	if p := prov(t, r, "subscribers[main].first_supi"); p.Value != "001010000000001" {
		t.Errorf("SUPI provenance = %q, want it shown", p.Value)
	}
}

func overriddenValues(p FieldProvenance) []string {
	var out []string
	for _, o := range p.Overridden {
		out = append(out, o.Value)
	}
	return out
}

// Run-file fields resolve too and are returned by path; they are not part
// of the target.
func TestRunFields(t *testing.T) {
	r, err := Resolve(
		Input{Layer: LayerRunFile, Settings: []Setting{set("workloads[pop].attach.rate", 10, "run"), set("until.duration_ms", 60000, "run")}},
		Input{Layer: LayerFlag, Settings: []Setting{set("workloads[].attach.rate", "25.5", "--rate")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if r.Run["workloads[pop].attach.rate"] != 25.5 || r.Run["until.duration_ms"] != uint64(60000) {
		t.Errorf("run fields = %v", r.Run)
	}
	if r.Run["workloads[pop].name"] != "pop" {
		t.Errorf("workload name = %v", r.Run["workloads[pop].name"])
	}
}

// Bad input fails with the source named: an unknown field, a value of the
// wrong type, a named list selector on a plain field, and a ran.node.
func TestResolveErrors(t *testing.T) {
	tests := []struct {
		name string
		s    Setting
		want string
		is   error
	}{
		{name: "unknown field", s: set("core.amff", "x", "--amff"), want: `unknown field "core.amff"`},
		{name: "wrong type", s: set("core.tac", "one", "--tac"), want: "is not a non-negative integer"},
		{name: "selector on a plain field", s: set("core[x].amf", "x", "f"), want: "unknown field"},
		{name: "ran.node", s: set("ran.node", "far-host", "t"), is: ErrNodeReserved},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(Input{Layer: LayerFlag, Settings: []Setting{tc.s}})
			if err == nil {
				t.Fatal("resolved")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("err = %v, want %v", err, tc.is)
			}
			if !strings.Contains(err.Error(), tc.s.Source.Name) && tc.is == nil {
				t.Errorf("err %v does not name the source %s", err, tc.s.Source.Name)
			}
		})
	}
}

// A secret's value never prints in an error, whatever the error kind and
// whichever layer the value came from: a wrong type (list, map, number,
// bool) or a bad format. Secret fields are strings, so no range error
// applies to them.
func TestSecretsNeverInErrors(t *testing.T) {
	const s = "5ec7e75ec7e75ec7e75ec7e75ec7e7zz" // not hex: a format error
	const digits = "987654321987"                // a number that is not 32 hex digits
	var errs []error
	try := func(name string, wantErr bool, err error) {
		t.Helper()
		if wantErr && err == nil {
			t.Errorf("%s: no error", name)
		}
		if err != nil {
			errs = append(errs, err)
			for _, b := range []string{s, digits, "5ec7e7"} {
				if strings.Contains(err.Error(), b) {
					t.Errorf("%s: error shows the secret: %v", name, err)
				}
			}
		}
	}
	values := []struct {
		name  string
		v     any
		wrong bool // a wrong type, an error on every secret field
	}{
		{name: "bad format", v: s},
		{name: "list", v: []any{s}, wrong: true},
		{name: "map", v: map[string]any{"k": s}, wrong: true},
		{name: "int", v: 987654321987},
		{name: "uint64", v: uint64(987654321987)},
		{name: "float", v: 9.87654321987e11},
		{name: "bool", v: true},
	}
	for _, path := range []string{"subscribers[].ki", "subscribers[].opc", "app.responders[r].token"} {
		for _, layer := range allLayers {
			for _, v := range values {
				_, err := Resolve(Input{Layer: layer, Settings: []Setting{set(path, v.v, "src")}})
				// ki and opc check their format; a token takes any string.
				try(path+" "+layer.String()+" "+v.name, v.wrong || (!strings.HasSuffix(path, "token") && v.name != "bool"), err)
			}
		}
	}
	for _, env := range []string{"ORBIT_KI", "ORBIT_OPC"} {
		_, err := Resolve(EnvInput(envOf(map[string]string{env: s})))
		try(env, true, err)
	}
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	fs.String("ki", "", "")
	if err := fs.Parse([]string{"--ki", s}); err != nil {
		t.Fatal(err)
	}
	in, err := FlagInput("orbit conformance", fs)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(in)
	try("flag --ki", true, err)
	for _, doc := range []string{
		"ki: " + s, "ki: [" + s + "]", "ki: {k: " + s + "}", "ki: " + digits,
		"opc: " + s, "opc: [" + s + "]",
	} {
		_, err := Parse("x.yaml", []byte("version: 1\nkind: target\nname: x\nsubscribers: [ { name: p, "+doc+" } ]\n"), envOf(nil))
		try("file "+doc, true, err)
	}
	_, err = Parse("x.yaml", []byte("version: 1\nkind: target\nname: x\napp: { responders: [ { name: r, token: ["+s+"] } ] }\n"), envOf(nil))
	try("file token list", true, err)
	if len(errs) < 50 {
		t.Errorf("only %d errors were checked", len(errs))
	}
}

// Integers are bound-checked against the field's 32-bit width before any
// cast, from text and from values a run file or an RPC hands over
// directly: too big or negative is a range error, never truncated.
func TestIntegerRange(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		v       any
		wantErr string
	}{
		{name: "count max", path: "subscribers[].count", v: 4294967295},
		{name: "count 2^32+1", path: "subscribers[].count", v: 4294967297, wantErr: "subscribers[default].count: 4294967297 is out of range (0 to 4294967295) (run file src)"},
		{name: "gnb first 2^32+5", path: "ran.gnb_ids.first", v: uint64(1<<32 + 5), wantErr: "4294967301 is out of range"},
		{name: "count negative", path: "subscribers[].count", v: -1, wantErr: "-1 is out of range"},
		{name: "count text 2^32", path: "subscribers[].count", v: "4294967296", wantErr: `"4294967296" is out of range`},
		{name: "count text negative", path: "subscribers[].count", v: "-1", wantErr: `"-1" is not a non-negative integer`},
		{name: "sst 2^32+1", path: "core.slices", v: []any{map[string]any{"sst": 4294967297}}, wantErr: "slice sst: 4294967297 is out of range"},
		{name: "seed is 64-bit", path: "workloads[].attach.seed", v: uint64(1 << 40)},
		{name: "seed text above 64 bits", path: "workloads[].attach.seed", v: "18446744073709551616", wantErr: `"18446744073709551616" is out of range (0 to 18446744073709551615)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Resolve(Input{Layer: LayerRunFile, Settings: []Setting{set(tc.path, tc.v, "src")}})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if tc.path != "subscribers[].count" {
					return
				}
				if r.Target.Subscribers[0].Count != 4294967295 {
					t.Errorf("count = %d", r.Target.Subscribers[0].Count)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// --sst and --sd set the first slice's fields on top of the merged list: a
// lone --sd keeps the target's SST and its other slices, and provenance is
// per field.
func TestFirstSliceFields(t *testing.T) {
	two := Input{Layer: LayerTarget, Settings: []Setting{{Path: "core.slices",
		Value: []any{map[string]any{"sst": 2, "sd": "000002"}, map[string]any{"sst": 1}}, Source: Source{Name: "lab1.yaml", Line: 7}}}}
	tests := []struct {
		name       string
		flags      []Setting
		want       []Slice
		wantSST    string // provenance source of core.slices[0].sst
		wantSD     string
		wantSDOver string // what --sd replaced, "" when nothing
	}{
		{name: "neither", want: []Slice{{2, "000002"}, {1, ""}}, wantSST: "target lab1.yaml:7", wantSD: "target lab1.yaml:7"},
		{name: "lone --sd", flags: []Setting{set("core.slices[0].sd", "000005", "--sd")},
			want: []Slice{{2, "000005"}, {1, ""}}, wantSST: "target lab1.yaml:7", wantSD: "flag --sd", wantSDOver: "target lab1.yaml:7=000002"},
		{name: "lone --sst", flags: []Setting{set("core.slices[0].sst", "3", "--sst")},
			want: []Slice{{3, "000002"}, {1, ""}}, wantSST: "flag --sst", wantSD: "target lab1.yaml:7"},
		{name: "both", flags: []Setting{set("core.slices[0].sst", "3", "--sst"), set("core.slices[0].sd", "", "--sd")},
			want: []Slice{{3, ""}, {1, ""}}, wantSST: "flag --sst", wantSD: "flag --sd", wantSDOver: "target lab1.yaml:7=000002"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Resolve(DefaultInput(), two, Input{Layer: LayerFlag, Settings: tc.flags})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r.Target.Core.Slices, tc.want) {
				t.Errorf("slices = %v, want %v", r.Target.Core.Slices, tc.want)
			}
			if p := prov(t, r, "core.slices[0].sst"); p.Source.String() != tc.wantSST {
				t.Errorf("sst set by %s, want %s", p.Source, tc.wantSST)
			}
			p := prov(t, r, "core.slices[0].sd")
			if p.Source.String() != tc.wantSD {
				t.Errorf("sd set by %s, want %s", p.Source, tc.wantSD)
			}
			var over []string
			for _, o := range p.Overridden {
				if o.Source.Layer == LayerTarget {
					over = append(over, o.Source.String()+"="+o.Value)
				}
			}
			if got := strings.Join(over, ";"); got != tc.wantSDOver {
				t.Errorf("sd overrode %q, want %q", got, tc.wantSDOver)
			}
		})
	}
	// With no target, a lone --sd takes the built-in SST, as the CLI does.
	r, err := Resolve(DefaultInput(), Input{Layer: LayerFlag, Settings: []Setting{set("core.slices[0].sd", "abcdef", "--sd")}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Target.Core.Slices, []Slice{{1, "abcdef"}}) {
		t.Errorf("slices = %v", r.Target.Core.Slices)
	}
	// Only the first slice has paths of its own.
	if _, err := Resolve(Input{Layer: LayerFlag, Settings: []Setting{set("core.slices[1].sd", "abcdef", "f")}}); err == nil {
		t.Error("core.slices[1].sd resolved")
	}
	for _, v := range []struct{ path, value, want string }{
		{"core.slices[0].sst", "256", "SST 256 is more than 255"},
		{"core.slices[0].sd", "00000x", `"00000x" is not 6 hex digits`},
	} {
		if _, err := Resolve(Input{Layer: LayerFlag, Settings: []Setting{set(v.path, v.value, "f")}}); err == nil || !strings.Contains(err.Error(), v.want) {
			t.Errorf("%s=%s: err = %v, want %q", v.path, v.value, err, v.want)
		}
	}
}

// Report records a bad value instead of failing: a format failure keeps the
// value, a conversion failure leaves the field to the lower layers.
func TestReport(t *testing.T) {
	r, err := Report(DefaultInput(), Input{Layer: LayerFlag, Settings: []Setting{
		set("subscribers[].first_supi", "abc", "--supi"), set("core.tac", "one", "--tac"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range r.Problems {
		got = append(got, p.Path)
	}
	if !reflect.DeepEqual(got, []string{"core.tac", "subscribers[default].first_supi"}) {
		t.Errorf("problems = %+v", r.Problems)
	}
	if r.Target.Core.TAC != 1 || r.Target.Subscribers[0].FirstSUPI != "abc" {
		t.Errorf("tac %d (want the default's 1), supi %q (kept)", r.Target.Core.TAC, r.Target.Subscribers[0].FirstSUPI)
	}
	if _, err := Resolve(DefaultInput(), Input{Layer: LayerFlag, Settings: []Setting{set("core.tac", "one", "--tac")}}); err == nil {
		t.Error("Resolve did not fail")
	}
}

// A pool's first SUPI must be in the resolved PLMN: Resolve refuses a
// mismatch, naming the field and the source; Report records it.
func TestFirstSUPIInPLMN(t *testing.T) {
	in := []Input{{Layer: LayerTarget, Settings: []Setting{
		set("core.plmn.mcc", "001", "t"), set("core.plmn.mnc", "01", "t"),
		set("subscribers[main].first_supi", "208930000000001", "lab1.yaml"),
	}}}
	_, err := Resolve(in...)
	if err == nil || !strings.Contains(err.Error(), "subscribers[main].first_supi: 208930000000001 is not in PLMN 001/01 (target lab1.yaml)") {
		t.Errorf("Resolve: %v", err)
	}
	r, err := Report(in...)
	if err != nil || len(r.Problems) != 1 || r.Problems[0].Path != "subscribers[main].first_supi" {
		t.Errorf("Report: %v %+v", err, r)
	}
	// imsi- prefixed and in the PLMN: fine; no PLMN set: not checked.
	if _, err := Resolve(Input{Layer: LayerTarget, Settings: []Setting{
		set("core.plmn.mcc", "001", "t"), set("core.plmn.mnc", "01", "t"),
		set("subscribers[main].first_supi", "imsi-001010000000001", "t")}}); err != nil {
		t.Error(err)
	}
	if _, err := Resolve(Input{Layer: LayerTarget, Settings: []Setting{set("subscribers[main].first_supi", "208930000000001", "t")}}); err != nil {
		t.Error(err)
	}
}

// show never prints a whole value's raw text: one holding a secret-named
// key is masked, and one it cannot render (a YAML map with a key that is
// not a string) is masked too, whatever it holds.
func TestShowWholeValueFailsClosed(t *testing.T) {
	_, f, err := lookup("workloads[].behaviors")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		v    any
		want string
	}{
		{name: "rendered", v: map[string]any{"rate": 2}, want: `{"rate":2}`},
		{name: "secret key, string keys", v: map[string]any{"params": map[string]any{"token": "s3cret"}}, want: Masked},
		{name: "secret key in a non-string-keyed map", v: map[any]any{"params": map[any]any{"token": "s3cret", 1: "x"}}, want: Masked},
		{name: "secret value in a non-string-keyed map", v: map[any]any{1: "s3cret", "note": "x"}, want: unrenderable},
		{name: "unrenderable in a list", v: []any{map[any]any{1: "s3cret"}}, want: unrenderable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := show(f, tc.v)
			if got != tc.want {
				t.Errorf("show = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "s3cret") {
				t.Errorf("show printed the secret: %q", got)
			}
		})
	}
}
