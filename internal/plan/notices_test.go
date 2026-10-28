package plan

import (
	"strings"
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
