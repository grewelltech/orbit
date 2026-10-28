package plan

import (
	"strings"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"testing"

	"github.com/bgrewell/orbit/internal/target"
)

// An env value that beat a file's is a notice: the shell overriding the
// target (a forgotten ORBIT_MCC) or the run file. A flag is meant; an env
// value over the defaults alone beat no file.
func TestOverrideNotices(t *testing.T) {
	file := func(layer target.Layer, name string, line int) target.Source {
		return target.Source{Layer: layer, Name: name, Line: line}
	}
	// The target layer as a target file gives it (lab1.yaml, by line).
	tgt := target.Input{Layer: target.LayerTarget, Settings: []target.Setting{
		{Path: "name", Value: "lab1", Source: file(target.LayerTarget, "lab1.yaml", 3)},
		{Path: "core.amf", Value: "192.0.2.1:38412", Source: file(target.LayerTarget, "lab1.yaml", 5)},
		{Path: "core.plmn.mcc", Value: "001", Source: file(target.LayerTarget, "lab1.yaml", 6)},
		{Path: "core.plmn.mnc", Value: "01", Source: file(target.LayerTarget, "lab1.yaml", 6)},
		{Path: "subscribers[main].name", Value: "main", Source: file(target.LayerTarget, "lab1.yaml", 8)},
		{Path: "subscribers[main].first_supi", Value: "999020000000001", Source: file(target.LayerTarget, "lab1.yaml", 8)},
		{Path: "subscribers[main].ki", Value: "00112233445566778899aabbccddeeff", Source: file(target.LayerTarget, "lab1.yaml", 8)},
		{Path: "subscribers[main].opc", Value: "00112233445566778899aabbccddeeff", Source: file(target.LayerTarget, "lab1.yaml", 8)},
	}}
	run := &target.RunFile{Input: target.Input{Layer: target.LayerRunFile, Settings: []target.Setting{
		{Path: "workloads[pop].attach.seed", Value: uint64(1), Source: file(target.LayerRunFile, "soak.yaml", 5)},
	}}}
	env := target.Input{Layer: target.LayerEnv, Settings: []target.Setting{
		{Path: "core.plmn.mcc", Value: "999", Source: target.Source{Name: "ORBIT_MCC"}},
		{Path: "workloads[pop].attach.seed", Value: uint64(7), Source: target.Source{Name: "ORBIT_LOAD_SEED"}},
		{Path: "subscribers[].ki", Value: "ffeeddccbbaa99887766554433221100", Source: target.Source{Name: "ORBIT_KI"}},
		{Path: "core.tac", Value: uint64(9), Source: target.Source{Name: "ORBIT_TAC"}}, // over the default only
	}}
	flags := target.Input{Layer: target.LayerFlag, Settings: []target.Setting{
		{Path: "core.plmn.mnc", Value: "02", Source: target.Source{Name: "--mnc"}},
	}}
	res, err := target.Resolve(target.DefaultInput(), tgt, run.Input, env, flags)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, n := range OverrideNotices(ProvenanceProto(res.Provenance)) {
		got[n.GetPath()] = NoticeText(n)
	}
	want := map[string]string{
		"core.plmn.mcc":              `core.plmn.mcc: env ORBIT_MCC (999) overrode target lab1.yaml:6 (001)`,
		"workloads[pop].attach.seed": `workloads[pop].attach.seed: env ORBIT_LOAD_SEED (7) overrode run file soak.yaml:5 (1)`,
		"subscribers[main].ki":       "",
	}
	if len(got) != len(want) {
		t.Errorf("notices %v, want for %v", got, want)
	}
	for path, text := range want {
		g, ok := got[path]
		if !ok {
			t.Errorf("no notice for %s", path)
			continue
		}
		if text != "" && g != text {
			t.Errorf("%s:\n got %s\nwant %s", path, g, text)
		}
	}
	// A key beaten by the shell is a notice, its values masked both sides.
	if k := got["subscribers[main].ki"]; strings.Contains(k, "ffeedd") || strings.Contains(k, "00112233") || strings.Count(k, target.Masked) != 2 {
		t.Errorf("the ki notice shows a key: %s", k)
	}
	// tac (env over the default) and mnc (a flag over the target) are not notices.
	for _, p := range []string{"core.tac", "core.plmn.mnc"} {
		if _, ok := got[p]; ok {
			t.Errorf("%s: a notice, want none", p)
		}
	}
}

// A notice names the highest file value the env beat: the run file's over
// the target's, when both set the field. An env value equal to the file's
// is still a notice, marked.
func TestOverrideNoticeHighestFileAndSameValue(t *testing.T) {
	src := func(l orbitv1.ProvenanceLayer, d string) *orbitv1.ProvenanceSource {
		return &orbitv1.ProvenanceSource{Layer: l, Detail: d}
	}
	prov := []*orbitv1.FieldProvenance{
		{Path: "workloads[pop].attach.seed", Source: src(orbitv1.ProvenanceLayer_PROVENANCE_LAYER_ENV, "env $ORBIT_LOAD_SEED"), Value: "7",
			Overridden: []*orbitv1.ProvenanceOverride{
				{Source: src(orbitv1.ProvenanceLayer_PROVENANCE_LAYER_DEFAULT, "default built-in"), Value: "0"},
				{Source: src(orbitv1.ProvenanceLayer_PROVENANCE_LAYER_TARGET, "target lab1.yaml:9"), Value: "3"},
				{Source: src(orbitv1.ProvenanceLayer_PROVENANCE_LAYER_RUN_FILE, "run file soak.yaml:5"), Value: "5"},
			}},
		{Path: "core.plmn.mcc", Source: src(orbitv1.ProvenanceLayer_PROVENANCE_LAYER_ENV, "env $ORBIT_MCC"), Value: "001",
			Overridden: []*orbitv1.ProvenanceOverride{{Source: src(orbitv1.ProvenanceLayer_PROVENANCE_LAYER_TARGET, "target lab1.yaml:6"), Value: "001"}}},
	}
	got := OverrideNotices(prov)
	if len(got) != 2 {
		t.Fatalf("notices %v", got)
	}
	if want := "workloads[pop].attach.seed: env $ORBIT_LOAD_SEED (7) overrode run file soak.yaml:5 (5)"; NoticeText(got[0]) != want {
		t.Errorf("got %s\nwant %s", NoticeText(got[0]), want)
	}
	if want := "core.plmn.mcc: env $ORBIT_MCC (001) overrode target lab1.yaml:6 (001) (same value)"; NoticeText(got[1]) != want {
		t.Errorf("got %s\nwant %s", NoticeText(got[1]), want)
	}
}
