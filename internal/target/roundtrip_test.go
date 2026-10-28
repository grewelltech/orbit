package target

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// roundTrip is testdata/roundtrip/cases.json: the cases both this test and
// the dashboard's (web/src/data/targets/roundtrip.test.ts) run.
type roundTrip struct {
	Env   map[string]string `json:"env"`
	Cases []struct {
		Name  string `json:"name"`
		From  string `json:"from"`
		Keys  bool   `json:"keys"`
		Edits []struct {
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		} `json:"edits"`
	} `json:"cases"`
}

// parseFixture loads one round-trip file as the CLI loads a target file,
// with env as the only environment.
func parseFixture(t *testing.T, path string, env map[string]string) *File {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	files, err := Parse(path, b, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Err != nil {
		t.Fatalf("%s: %d targets, %v", path, len(files), files[0].Err)
	}
	return files[0]
}

// editSetting is an edit as a target-layer setting, typed as the field
// table says; ok is false for an edit that removes the field.
func editSetting(t *testing.T, path string, raw json.RawMessage) (Setting, bool) {
	t.Helper()
	pat := pathPattern(path)
	f, ok := fields[pat]
	if !ok {
		t.Fatalf("edit %s: no such field (%s)", path, pat)
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		vals := make([]any, len(list))
		for i, s := range list {
			vals[i] = s
		}
		return Setting{Path: path, Value: vals}, len(list) > 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("edit %s: %v", path, err)
	}
	if s == "" {
		return Setting{}, false
	}
	if f.typ == tUint {
		n, err := strconv.ParseUint(s, 0, 64)
		if err != nil {
			t.Fatalf("edit %s: %v", path, err)
		}
		return Setting{Path: path, Value: n}, true
	}
	return Setting{Path: path, Value: s}, true
}

// pathPattern is a canonical path's pattern: every [name] as [].
func pathPattern(path string) string {
	var b strings.Builder
	depth := 0
	for _, r := range path {
		switch {
		case r == '[':
			depth++
			b.WriteString("[")
		case r == ']':
			depth--
			b.WriteString("]")
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// values is what `orbit target show --resolved` shows of r, path by path,
// with the keys themselves (show masks them) from the resolved target.
func values(r *Resolved) map[string]string {
	out := map[string]string{}
	for _, p := range r.Provenance {
		out[p.Path] = p.Value
	}
	for _, s := range r.Target.Subscribers {
		out["subscribers["+s.Name+"].ki"], out["subscribers["+s.Name+"].opc"] = s.Ki, s.OPc
	}
	for _, rs := range r.Target.App.Responders {
		out["app.responders["+rs.Name+"].token"] = rs.Token
	}
	return out
}

// What the dashboard writes, the CLI loads to the same target: each
// ui/<case>.yaml, exported by the dashboard from cli/<from>.yaml with the
// case's edits, resolves (as show --resolved has it, and as the Target
// struct) to cli/<from>.yaml's resolution with the same edits applied.
// The dashboard's test writes the ui files; a stale one fails there.
func TestRoundTripDashboardFiles(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "roundtrip", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rt roundTrip
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatal(err)
	}
	if len(rt.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range rt.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			ui := parseFixture(t, filepath.Join("testdata", "roundtrip", "ui", tc.Name+".yaml"), rt.Env)
			cli := parseFixture(t, filepath.Join("testdata", "roundtrip", "cli", tc.From+".yaml"), rt.Env)
			edited := map[string]bool{}
			var add []Setting
			for _, e := range tc.Edits {
				edited[e.Path] = true
				if s, ok := editSetting(t, e.Path, e.Value); ok {
					add = append(add, s)
				}
			}
			want := Input{Layer: LayerTarget}
			for _, s := range cli.Input.Settings {
				if !edited[s.Path] {
					want.Settings = append(want.Settings, s)
				}
			}
			want.Settings = append(want.Settings, add...)
			got, err := Resolve(DefaultInput(), ui.Input)
			if err != nil {
				t.Fatal(err)
			}
			exp, err := Resolve(DefaultInput(), want)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Target, exp.Target) {
				t.Errorf("target:\n got %+v\nwant %+v", got.Target, exp.Target)
			}
			gv, ev := values(got), values(exp)
			for p, v := range ev {
				if gv[p] != v {
					t.Errorf("%s = %q, want %q", p, gv[p], v)
				}
			}
			for p := range gv {
				if _, ok := ev[p]; !ok {
					t.Errorf("%s is set (%q) and should not be", p, gv[p])
				}
			}
			if t.Failed() {
				t.Logf("ui file: testdata/roundtrip/ui/%s.yaml", tc.Name)
			}
		})
	}
}
