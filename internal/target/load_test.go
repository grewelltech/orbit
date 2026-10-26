package target

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func envOf(m map[string]string) Getenv {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

const lab1 = `version: 1
kind: target
name: lab1
core:
  amf: 192.0.2.10:38412
  plmn: { mcc: "001", mnc: "01" }
  tac: 1
  slices:
    - { sst: 1, sd: "000001" }
  dnn: internet
  profile: sdcore
subscribers:
  - name: main
    first_supi: "001010000000001"
    count: 100
    ki:  ${ORBIT_KI}
    opc: ${ORBIT_OPC}
ran:
  node: ""
  n2_source_ips: [192.0.2.50-192.0.2.59]
  n3_ips: [198.51.100.50]
  gnb_ids: { first: 100, count: 900, bits: 24 }
app:
  responders:
    - name: app1
    - name: lab-loomd
      control: 203.0.113.20:9551
      token: ${ORBIT_LOOM_TOKEN}
      data_ip: 203.0.113.21
`

var labEnv = envOf(map[string]string{"ORBIT_KI": "11111111111111111111111111111111", "ORBIT_OPC": "22222222222222222222222222222222", "ORBIT_LOOM_TOKEN": "t1"})

// ADR-0008 §1's example loads: every field, ${ENV} expanded.
func TestParseExample(t *testing.T) {
	files, err := Parse("lab1.yaml", []byte(lab1), labEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("%d targets", len(files))
	}
	got := files[0].Target
	want := Target{Version: 1, Kind: "target", Name: "lab1",
		Core:        Core{AMF: "192.0.2.10:38412", PLMN: PLMN{MCC: "001", MNC: "01"}, TAC: 1, Slices: []Slice{{SST: 1, SD: "000001"}}, DNN: "internet", Profile: "sdcore"},
		Subscribers: []Pool{{Name: "main", FirstSUPI: "001010000000001", Count: 100, Ki: "11111111111111111111111111111111", OPc: "22222222222222222222222222222222"}},
		RAN:         RAN{N2SourceIPs: []string{"192.0.2.50-192.0.2.59"}, N3IPs: []string{"198.51.100.50"}, GNBIDs: GNBIDs{First: 100, Count: 900, Bits: 24}},
		App:         App{Responders: []Responder{{Name: "app1"}, {Name: "lab-loomd", Control: "203.0.113.20:9551", Token: "t1", DataIP: "203.0.113.21"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("target =\n%+v\nwant\n%+v", got, want)
	}
}

// An ${ENV} value belongs to the target layer, with the variable noted and
// the file's line, and is masked when secret.
func TestEnvExpansionAttributedToTarget(t *testing.T) {
	files, err := Parse("lab1.yaml", []byte(lab1), labEnv)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(files[0].Input)
	if err != nil {
		t.Fatal(err)
	}
	p := prov(t, r, "subscribers[main].ki")
	if p.Source.Layer != LayerTarget || p.Source.Name != "lab1.yaml" || p.Source.Line != 16 || !reflect.DeepEqual(p.Source.Via, []string{"ORBIT_KI"}) {
		t.Errorf("ki source = %+v", p.Source)
	}
	if p.Source.String() != "target lab1.yaml:16 via ${ORBIT_KI}" || p.Value != Masked {
		t.Errorf("ki provenance = %s %q", p.Source, p.Value)
	}
	if p := prov(t, r, "core.amf"); p.Source.Line != 5 || len(p.Source.Via) != 0 {
		t.Errorf("amf source = %+v", p.Source)
	}
}

// orbit.yaml may hold a list of targets.
func TestParseList(t *testing.T) {
	doc := "- {version: 1, kind: target, name: a, core: {amf: 192.0.2.1:38412}}\n- {version: 1, kind: target, name: b}\n"
	files, err := Parse("orbit.yaml", []byte(doc), labEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Target.Name != "a" || files[1].Target.Name != "b" || files[0].Target.Core.AMF != "192.0.2.1:38412" {
		t.Errorf("targets = %+v", files)
	}
}

// Load errors name the file and line: unknown keys anywhere (strict), a
// reserved ran.node, the version and kind, an unset ${VAR}, a pool with no
// name, duplicate names, and a wrong type.
func TestParseErrors(t *testing.T) {
	base := "version: 1\nkind: target\nname: t\n"
	tests := []struct {
		name string
		doc  string
		want string
		is   error
	}{
		{name: "unknown top-level key", doc: base + "cores: {}\n", want: "cores"},
		{name: "unknown nested key", doc: base + "core: {amf: x, amff: y}\n", want: "amff"},
		{name: "unknown pool key", doc: base + "subscribers: [{name: m, kii: x}]\n", want: "kii"},
		{name: "ran.node", doc: base + "ran: {node: far}\n", is: ErrNodeReserved},
		{name: "version", doc: "version: 2\nkind: target\nname: t\n", want: "version 2"},
		{name: "kind", doc: "version: 1\nkind: run\nname: t\n", want: `kind "run"`},
		{name: "no name", doc: "version: 1\nkind: target\n", want: "name is required"},
		{name: "unset variable", doc: base + "subscribers: [{name: m, ki: '${NOPE}'}]\n", want: "lab.yaml:4: subscribers[].ki: ${NOPE} is not set"},
		{name: "pool without a name", doc: base + "subscribers: [{count: 1}]\n", want: "entries need a name"},
		{name: "duplicate pools", doc: base + "subscribers: [{name: m}, {name: m}]\n", want: "distinct names"},
		{name: "pool name with a dot", doc: base + "subscribers: [{name: a.b}]\n", want: "may not contain"},
		{name: "wrong type", doc: base + "core: {tac: one}\n", want: "lab.yaml"},
		{name: "duplicate targets", doc: "- {version: 1, kind: target, name: a}\n- {version: 1, kind: target, name: a}\n", want: "defined twice"},
		{name: "not a mapping", doc: "just a string\n", want: "want a target or a list"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse("lab.yaml", []byte(tc.doc), labEnv)
			if err == nil {
				t.Fatal("parsed")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("err = %v, want %v", err, tc.is)
			}
		})
	}
}

// Find looks in ./orbit.yaml first, then <config>/targets/<name>.yaml.
func TestFind(t *testing.T) {
	dir, cfg := t.TempDir(), t.TempDir()
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "orbit.yaml"), "- {version: 1, kind: target, name: here, core: {amf: 192.0.2.1:1}}\n- {version: 1, kind: target, name: both, core: {amf: 192.0.2.2:1}}\n")
	write(filepath.Join(cfg, "targets", "both.yaml"), "{version: 1, kind: target, name: both, core: {amf: 192.0.2.3:1}}\n")
	write(filepath.Join(cfg, "targets", "far.yaml"), "{version: 1, kind: target, name: far, core: {amf: 192.0.2.4:1}}\n")
	write(filepath.Join(cfg, "targets", "misnamed.yaml"), "{version: 1, kind: target, name: other}\n")
	loc := Locations{Dir: dir, ConfigDir: cfg}
	tests := []struct {
		name, want, wantErr string
	}{
		{name: "here", want: "192.0.2.1:1"},
		{name: "both", want: "192.0.2.2:1"}, // ./orbit.yaml wins
		{name: "far", want: "192.0.2.4:1"},
		{name: "misnamed", wantErr: `want the one target "misnamed"`},
		{name: "nowhere", wantErr: "target not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Find(tc.name, loc, labEnv)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if f.Target.Core.AMF != tc.want {
				t.Errorf("AMF = %s, want %s", f.Target.Core.AMF, tc.want)
			}
		})
	}
}

// The highest layer that names a target picks it.
func TestSelectName(t *testing.T) {
	run := Input{Layer: LayerRunFile, Settings: []Setting{set("target", "fromrun", "run.yaml")}}
	env := Input{Layer: LayerEnv, Settings: []Setting{set("target", "fromenv", "$ORBIT_TARGET")}}
	flag := Input{Layer: LayerFlag, Settings: []Setting{set("target", "fromflag", "--target")}}
	tests := []struct {
		in   []Input
		want string
	}{
		{nil, ""},
		{[]Input{run}, "fromrun"},
		{[]Input{env, run}, "fromenv"},
		{[]Input{run, flag, env}, "fromflag"},
	}
	for _, tc := range tests {
		if got := SelectName(tc.in...); got != tc.want {
			t.Errorf("SelectName = %q, want %q", got, tc.want)
		}
	}
}
