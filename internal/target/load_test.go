package target

import (
	"errors"
	"fmt"
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
// A string field keeps a plain scalar's text exactly as written, even
// when YAML would read it as a number: leading zeros, octal- and
// hex-looking values and long digit strings all survive.
func TestUnquotedStringFieldsKeepTheirText(t *testing.T) {
	const digits32 = "01234567890123456789012345678901"
	tests := []struct {
		name string
		yaml string // replaces the core: block of a minimal target
		path string
		want any
	}{
		{name: "mcc", yaml: "core: { plmn: { mcc: 001, mnc: 01 } }", path: "core.plmn.mcc", want: "001"},
		{name: "mnc", yaml: "core: { plmn: { mcc: 001, mnc: 01 } }", path: "core.plmn.mnc", want: "01"},
		{name: "dnn leading zero", yaml: "core: { dnn: 0123 }", path: "core.dnn", want: "0123"},
		{name: "dnn octal-looking", yaml: "core: { dnn: 0o17 }", path: "core.dnn", want: "0o17"},
		{name: "dnn hex-looking", yaml: "core: { dnn: 0x1F }", path: "core.dnn", want: "0x1F"},
		{name: "dnn float-looking", yaml: "core: { dnn: 1.50 }", path: "core.dnn", want: "1.50"},
		{name: "profile", yaml: "core: { profile: 007 }", path: "core.profile", want: "007"},
		{name: "sd", yaml: "core: { slices: [ { sst: 1, sd: 010203 } ] }", path: "core.slices", want: []Slice{{SST: 1, SD: "010203"}}},
		{name: "first_supi", yaml: "subscribers: [ { name: p, first_supi: 001010000000001 } ]", path: "subscribers[p].first_supi", want: "001010000000001"},
		{name: "ki", yaml: "subscribers: [ { name: p, ki: " + digits32 + " } ]", path: "subscribers[p].ki", want: digits32},
		{name: "opc", yaml: "subscribers: [ { name: p, opc: " + digits32 + " } ]", path: "subscribers[p].opc", want: digits32},
		{name: "routing_indicator", yaml: "subscribers: [ { name: p, routing_indicator: 0012 } ]", path: "subscribers[p].routing_indicator", want: "0012"},
		{name: "pool name", yaml: "subscribers: [ { name: 007, count: 1 } ]", path: "subscribers[007].count", want: uint64(1)},
		{name: "gnb_name", yaml: "ran: { gnb_name: 0042 }", path: "ran.gnb_name", want: "0042"},
		{name: "responder token", yaml: "app: { responders: [ { name: r, token: 0123 } ] }", path: "app.responders[r].token", want: "0123"},
		{name: "kind", yaml: "core: {}", path: "kind", want: "target"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := "version: 1\nkind: target\nname: x\n" + tc.yaml + "\n"
			files, err := Parse("x.yaml", []byte(doc), envOf(nil))
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range files[0].Input.Settings {
				if s.Path == tc.path {
					v, err := convert(fields[patternOf(t, s.Path)], s.Value)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(v, tc.want) {
						t.Errorf("%s = %#v, want %#v", tc.path, v, tc.want)
					}
					return
				}
			}
			t.Fatalf("no setting for %s in %+v", tc.path, files[0].Input.Settings)
		})
	}

	// The resolved target holds the same text, and a numeric field still
	// decodes as a number.
	files, err := Parse("x.yaml", []byte("version: 1\nkind: target\nname: 2024\ncore: { tac: 7, dnn: 0123 }\n"), envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if tg := files[0].Target; tg.Name != "2024" || tg.Core.DNN != "0123" || tg.Core.TAC != 7 {
		t.Errorf("target = name %q dnn %q tac %d", tg.Name, tg.Core.DNN, tg.Core.TAC)
	}
}

// A number written with a leading zero is an error rather than a guess
// between octal and decimal, and so is a quoted number; plain decimal,
// explicit 0x hex and 0 load. Errors name the file, line and path.
func TestNumericLeadingZero(t *testing.T) {
	fields := []struct {
		name string
		yaml string // %s is the number
		get  func(Target) uint32
		ten  string // a valid value in decimal, with a leading zero, and in hex
		zero string
		hex  string
		want uint32
	}{
		{name: "tac", yaml: "core: { tac: %s }", get: func(t Target) uint32 { return t.Core.TAC }},
		{name: "sst", yaml: "core: { slices: [ { sst: %s } ] }", get: func(t Target) uint32 { return t.Core.Slices[0].SST }},
		{name: "count", yaml: "subscribers: [ { name: p, count: %s } ]", get: func(t Target) uint32 { return t.Subscribers[0].Count }},
		{name: "gnb_ids.first", yaml: "ran: { gnb_ids: { first: %s } }", get: func(t Target) uint32 { return t.RAN.GNBIDs.First }},
		{name: "gnb_ids.count", yaml: "ran: { gnb_ids: { count: %s } }", get: func(t Target) uint32 { return t.RAN.GNBIDs.Count }},
		{name: "gnb_ids.bits", yaml: "ran: { gnb_ids: { bits: %s } }", get: func(t Target) uint32 { return t.RAN.GNBIDs.Bits },
			ten: "24", zero: "0024", hex: "0x18", want: 24},
	}
	for _, f := range fields {
		if f.ten == "" {
			f.ten, f.zero, f.hex, f.want = "10", "0010", "0x0a", 10
		}
		rows := []struct {
			text    string
			want    uint32
			wantErr string
		}{
			{text: f.zero, wantErr: f.zero + " has a leading zero; write " + f.ten + ", or 0x… for hex"},
			{text: `"` + f.ten + `"`, wantErr: `"` + f.ten + `" is quoted; write the number unquoted`},
			{text: `'` + f.zero + `'`, wantErr: `"` + f.zero + `" is quoted; write the number unquoted; ` +
				f.zero + " has a leading zero; write " + f.ten + ", or 0x… for hex"},
			{text: f.ten, want: f.want},
			{text: f.hex, want: f.want},
			{text: "0", want: 0},
		}
		for _, r := range rows {
			t.Run(f.name+" "+r.text, func(t *testing.T) {
				doc := "version: 1\nkind: target\nname: x\n" + fmt.Sprintf(f.yaml, r.text) + "\n"
				files, err := Parse("x.yaml", []byte(doc), envOf(nil))
				if r.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), r.wantErr) || !strings.HasPrefix(err.Error(), "x.yaml:4: ") {
						t.Fatalf("err = %v, want x.yaml:4: and %q", err, r.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := f.get(files[0].Target); got != r.want {
					t.Errorf("%s = %d, want %d", f.name, got, r.want)
				}
			})
		}
	}
}

func patternOf(t *testing.T, path string) string {
	t.Helper()
	segs, err := parsePath(path)
	if err != nil {
		t.Fatal(err)
	}
	return pattern(segs)
}

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

// A ${VAR} in a numeric field, quoted or not, is a clear error with the
// file, line and path: numbers come from a flag or an env-layer binding.
func TestNumericFieldTakesNoEnvRef(t *testing.T) {
	const hint = " is not expanded in numeric fields; set it with a flag or an env-layer binding"
	for _, tc := range []struct{ yaml, want string }{
		{yaml: "core:\n  tac: ${TAC}", want: "x.yaml:5: core.tac: ${TAC}" + hint},
		{yaml: `core: { tac: "${TAC}" }`, want: "x.yaml:4: core.tac: ${TAC}" + hint},
		{yaml: "subscribers: [ { name: p, count: '${N}' } ]", want: "x.yaml:4: subscribers[].count: ${N}" + hint},
		{yaml: "core:\n  slices:\n    - sst: ${SST}", want: "x.yaml:6: core.slices: slice sst: ${SST}" + hint},
	} {
		_, err := Parse("x.yaml", []byte("version: 1\nkind: target\nname: x\n"+tc.yaml+"\n"), envOf(map[string]string{"TAC": "7", "N": "1", "SST": "1"}))
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", tc.yaml, err, tc.want)
		}
	}
}
