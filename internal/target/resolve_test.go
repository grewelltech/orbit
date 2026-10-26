package target

import (
	"errors"
	"reflect"
	"strings"
	"testing"
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
		set("subscribers[main].first_supi", "001010000000001", "t"),
		set("subscribers[main].count", 100, "t"),
		set("subscribers[extra].first_supi", "001010000000500", "t"),
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
