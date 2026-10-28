package target

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A key or token pasted into the wrong field is refused without showing
// it: the refusal for a field that is not secret masks a value shaped like
// a key (32 or 64 hex digits) or one of orbit's tokens, in a file and in
// the flag layer alike (the CLI's and the server's refusals are these).
func TestRefusalsMaskKeyShapedValues(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	base := "version: 1\nkind: target\nname: a\ncore:\n  amf: 192.0.2.1:38412\n"
	values := map[string]string{
		"a Ki":           "00112233445566778899aabbccddeeff",
		"a 64-digit key": strings.Repeat("0123456789abcdef", 4),
		"a credential":   "orc1_abcdefghijklmnopqrstuv",
		"a join token":   "oj1_abcdefghijklmnopqrstuv",
		"a loom token":   "orl1_abcdefghijklmnopqrstuv",
	}
	for what, v := range values {
		for _, tc := range []struct {
			name string
			err  func() error
		}{
			{"sd in a file's slices", func() error {
				_, err := Parse("lab.yaml", []byte(base+"  slices:\n    - {sst: 1, sd: "+v+"}\n"), none)
				return err
			}},
			{"sd by --set", func() error {
				_, err := Resolve(Input{Layer: LayerFlag, Settings: []Setting{{Path: "core.slices[].sd", Value: v, Source: Source{Name: "--set"}}}})
				return err
			}},
			{"control in a file", func() error {
				_, err := Parse("lab.yaml", []byte(base+"app:\n  responders:\n    - {name: r, control: "+v+", data_ip: 192.0.2.5}\n"), none)
				return err
			}},
			{"control by --set", func() error {
				_, err := Resolve(Input{Layer: LayerFlag, Settings: []Setting{{Path: "app.responders[r].control", Value: v, Source: Source{Name: "--set"}}}})
				return err
			}},
		} {
			err := tc.err()
			if err == nil {
				t.Errorf("%s, %s: accepted", tc.name, what)
				continue
			}
			if strings.Contains(err.Error(), v) || !strings.Contains(err.Error(), Masked) {
				t.Errorf("%s, %s: %v", tc.name, what, err)
			}
		}
	}
	// A run file's: a key in a number field is refused when resolved.
	rf, err := ParseRunFile("soak.yaml", []byte("kind: run\nworkloads:\n  - name: w\n    attach: {count: "+values["a Ki"]+"}\n"), none)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(rf.Input); err == nil || strings.Contains(err.Error(), values["a Ki"]) || !strings.Contains(err.Error(), "workloads[w].attach.count: "+Masked) {
		t.Errorf("a run file's count, a Ki: %v", err)
	}
	// A data_ip refusal masks a token too (a hex key is a valid host name,
	// so it is not refused at all).
	_, err = Resolve(Input{Layer: LayerFlag, Settings: []Setting{{Path: "app.responders[r].data_ip", Value: values["a credential"], Source: Source{Name: "--set"}}}})
	if err == nil || strings.Contains(err.Error(), values["a credential"]) || !strings.Contains(err.Error(), Masked) {
		t.Errorf("data_ip, a credential: %v", err)
	}
	// A bad value that is not key-shaped is still shown, quoted.
	_, err = Resolve(Input{Layer: LayerFlag, Settings: []Setting{{Path: "core.slices[].sd", Value: "zz", Source: Source{Name: "--set"}}}})
	if err == nil || !strings.Contains(err.Error(), `"zz"`) {
		t.Errorf("sd zz: %v, want it shown", err)
	}
}

func TestKeyShaped(t *testing.T) {
	const ki = "00112233445566778899aabbccddeeff"
	pairs := func(sep string) string {
		var p []string
		for i := 0; i < len(ki); i += 2 {
			p = append(p, ki[i:i+2])
		}
		return strings.Join(p, sep)
	}
	quads := func(sep string) string {
		var q []string
		for i := 0; i < len(ki); i += 4 {
			q = append(q, ki[i:i+4])
		}
		return strings.Join(q, sep)
	}
	for _, tc := range []struct {
		name string
		s    string
		want bool
	}{
		{"a Ki", ki, true},
		{"upper case, spaces round it", " 00112233445566778899AABBCCDDEEFF ", true},
		{"a 64-digit key", strings.Repeat("ab", 32), true},
		{"0x", "0x" + ki, true},
		{"0X", "0X" + ki, true},
		{"31 digits (a digit lost)", ki[:31], true},
		{"33 digits (a digit doubled)", ki + "0", true},
		{"63 digits", strings.Repeat("ab", 32)[:63], true},
		{"65 digits", strings.Repeat("ab", 32) + "c", true},
		{"pairs by ':'", pairs(":"), true},
		{"pairs by '-'", pairs("-"), true},
		{"pairs by ' '", pairs(" "), true},
		{"quads by ':'", quads(":"), true},
		{"quads by '-'", quads("-"), true},
		{"quads by ' '", quads(" "), true},
		{"a credential", "orc1_x", true},
		{"a join token", "oj1_x", true},
		{"a loom token", "orl1_x", true},
		// Kept readable.
		{"30 digits", ki[:30], false},
		{"34 digits", ki + "00", false},
		{"an IMSI", "001010000000001", false},
		{"a 6-hex SD", "010203", false},
		{"a 4-hex value", "abcd", false},
		{"a PLMN", "001/01", false},
		{"a UUID (uneven groups)", "123e4567-e89b-12d3-a456-426614174000", false},
		{"another prefix", "orx1_x", false},
		// An IPv6 address written out in full is a Ki's shape in eight
		// colon quads: masked (the safe failure for a diagnostic). Its
		// compressed form stays readable.
		{"an IPv6 address in full", "2001:0db8:85a3:0000:0000:8a2e:0370:7334", true},
		{"an IPv6 address compressed", "2001:db8::1", false},
		{"an IPv6 address, zeros dropped", "2001:db8:85a3:0:0:8a2e:370:7334", false},
		{"a ${VAR}", "${ORBIT_KI}", false},
		{"empty", "", false},
	} {
		if got := keyShaped(tc.s); got != tc.want {
			t.Errorf("%s: keyShaped(%q) = %t, want %t", tc.name, tc.s, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"[1/" + ki + "] and orc1_ab-c", "[1/" + Masked + "] and " + Masked},
		{"tok_" + ki, "tok_" + Masked},         // '_' splits, as \b would not
		{"x" + ki, "x" + Masked},               // a non-hex letter bounds it
		{"ab" + ki, "ab" + ki},                 // a run of 34 hex digits is not a key's length, and is not taken apart
		{ki + "," + ki, Masked + "," + Masked}, // two in a row: both
		{"key " + pairs(":") + " set", "key " + Masked + " set"},
		{"imsi-001010000000001 sd 010203 plmn 001/01", "imsi-001010000000001 sd 010203 plmn 001/01"},
	} {
		if got := maskKeyShaped(tc.in); got != tc.want {
			t.Errorf("maskKeyShaped(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A YAML type error names the field and the types, never the value: yaml
// quotes a prefix of the value it could not decode, which for a key pasted
// into a number field is the start of the key.
func TestYAMLTypeErrorsShowNoValue(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	base := "version: 1\nkind: target\nname: a\ncore:\n  amf: 192.0.2.1:38412\n"
	const key = "abcdef0123456789abcdef0123456789"
	for _, tc := range []struct {
		name, doc string
		want      []string
	}{
		{"a key in a number field", base + "  tac: " + key + "\n", []string{"lab.yaml: line 6: core.tac: want a number, not a string"}},
		{"in a flow mapping (no one field on the line)", "version: 1\nkind: target\nname: a\ncore: {amf: 192.0.2.1:38412, tac: " + key + "}\n", []string{"line 4: want a number, not a string"}},
		{"a slice that is not a mapping", base + "  slices:\n    - " + key + "\n", []string{"core.slices", "want a mapping, not a string"}},
		{"under a tag yaml does not know (the line, not the value)", base + "  tac: !x " + key + "\n", []string{"line 6: cannot unmarshal !x a value into uint32"}},
		{"a number too big for its field (not 'want a number, not a number')", base + "  tac: 99999999999\n", []string{"line 6: core.tac: out of range (at most 4294967295)"}},
		{"a leading zero on a key-shaped number", base + "  tac: 00112233445566778899001122334455\n", []string{"core.tac", Masked + " has a leading zero"}},
	} {
		_, err := Parse("lab.yaml", []byte(tc.doc), none)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		msg := err.Error()
		for _, w := range tc.want {
			if !strings.Contains(msg, w) {
				t.Errorf("%s: %q, want %q in it", tc.name, msg, w)
			}
		}
		if strings.Contains(msg, key[:7]) || strings.Contains(msg, "0011223344") || strings.Contains(msg, "`") {
			t.Errorf("%s: shows the value: %s", tc.name, msg)
		}
	}
}

// Key text used as a YAML key or as a pool's name is masked in every error
// the target package gives (Parse, ParseRunFile, Resolve, Report's
// problems), as the dashboard's strayKey never quotes it; errors.Is still
// sees the error underneath.
func TestErrorsMaskKeyTextInKeysAndNames(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	const key = "465b5ce8b199b49faa5f0a2ee238a6bc"
	base := "version: 1\nkind: target\nname: a\ncore:\n  amf: 192.0.2.1:38412\n  plmn: {mcc: \"001\", mnc: \"01\"}\n"
	for _, tc := range []struct {
		name string
		err  func() error
		want string
	}{
		{"a key as a YAML key", func() error {
			_, err := Parse("lab.yaml", []byte(base+"  "+key+": 1\n"), none)
			return err
		}, "unknown key core." + Masked},
		{"a key as a pool name, twice", func() error {
			_, err := Parse("lab.yaml", []byte(base+"subscribers:\n  - {name: "+key+", first_supi: \"001010000000001\"}\n  - {name: "+key+", first_supi: \"001010000000002\"}\n"), none)
			return err
		}, "subscribers[1].name is the same as subscribers[0].name"},
		{"a key as a pool name, in a later error", func() error {
			_, err := Parse("lab.yaml", []byte(base+"subscribers:\n  - {name: "+key+", first_supi: \"999990000000001\"}\n"), none)
			return err
		}, "subscribers[" + Masked + "]"},
		{"a key as a run file's key", func() error {
			_, err := ParseRunFile("soak.yaml", []byte("kind: run\n"+key+": 1\n"), none)
			return err
		}, "unknown key " + Masked},
		{"a key as a --set path's selector", func() error {
			_, err := Resolve(Input{Layer: LayerFlag, Settings: []Setting{{Path: "subscribers[" + key + "].count", Value: "x", Source: Source{Name: "--set"}}}})
			return err
		}, "subscribers[" + Masked + "].count"},
	} {
		err := tc.err()
		if err == nil || strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q in it and no key", tc.name, err, tc.want)
		}
	}
	res, err := Report(Input{Layer: LayerFlag, Settings: []Setting{{Path: "subscribers[" + key + "].count", Value: "x", Source: Source{Name: "--set"}}}})
	if err != nil || len(res.Problems) == 0 {
		t.Fatalf("Report: %v %v", res, err)
	}
	for _, p := range res.Problems {
		if strings.Contains(p.Err, key) {
			t.Errorf("Report's problem shows the key: %s", p.Err)
		}
	}
	// errors.Is sees through the mask.
	if _, err := Parse("lab.yaml", []byte(base+"ran: {node: far}\n"), none); !errors.Is(err, ErrNodeReserved) {
		t.Errorf("errors.Is(ErrNodeReserved) through the mask: %v", err)
	}
}

// A target file is one YAML document: a target, or a list of targets. A
// second document (after ---) was read past and dropped; it is refused,
// naming its line. A trailing --- with nothing after it is not one.
func TestSecondYAMLDocumentRefused(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	one := "version: 1\nkind: target\nname: a\ncore:\n  amf: 192.0.2.1:38412\n"
	for _, tc := range []struct {
		name, doc string
		want      string // "" when it parses
	}{
		{"one document", one, ""},
		{"one, with a leading ---", "---\n" + one, ""},
		{"one, with a trailing ---", one + "---\n", ""},
		{"two documents", one + "---\n" + strings.Replace(one, "name: a", "name: b", 1), "lab.yaml:7: a second YAML document"},
		{"a second document that does not parse", one + "---\nfoo: [\n", "a second YAML document"},
		{"a list, then a second document", "- " + strings.ReplaceAll(strings.TrimSuffix(one, "\n"), "\n", "\n  ") + "\n---\nversion: 1\n", "a second YAML document"},
	} {
		_, err := Parse("lab.yaml", []byte(tc.doc), none)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// A run file is one YAML document too: a second is refused, as a target
// file's is, whether it parses or not.
func TestRunFileSecondDocumentRefused(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	one := "kind: run\nworkloads:\n  - name: w\n    attach: {count: 1}\n"
	for _, tc := range []struct {
		name, doc string
		want      string
	}{
		{"one document", one, ""},
		{"a trailing ---", one + "---\n", ""},
		{"two documents", one + "---\n" + one, "soak.yaml:6: a second YAML document (---); a run file is one document"},
		{"a second that does not parse", one + "---\nfoo: [\n", "a second YAML document"},
	} {
		_, err := ParseRunFile("soak.yaml", []byte(tc.doc), none)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// A target the dashboard sends (ResolveSent, the probe's path) shows no key
// text in its refusals either: a key-shaped pool name with a '.' or given
// twice. The refusal is still a *FieldError, which the page marks the field
// from.
func TestResolveSentMasksKeyText(t *testing.T) {
	const key = "465b5ce8b199b49faa5f0a2ee238a6bc"
	base := Target{Name: "lab1", Core: Core{AMF: "192.0.2.1:38412", PLMN: PLMN{MCC: "001", MNC: "01"}}}
	for _, tc := range []struct {
		name  string
		pools []Pool
	}{
		{"a key-shaped name with a '.'", []Pool{{Name: key + ".x", FirstSUPI: "001010000000001"}}},
		{"a key-shaped name twice", []Pool{{Name: key, FirstSUPI: "001010000000001"}, {Name: key, FirstSUPI: "001010000000002"}}},
	} {
		tg := base
		tg.Subscribers = tc.pools
		_, err := ResolveSent(tg)
		var fe *FieldError
		if !errors.As(err, &fe) {
			t.Fatalf("%s: %v, want a *FieldError", tc.name, err)
		}
		if strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "subscribers[") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// No caller in internal/cli or internal/server shows a target error's
// unmasked text: none unwraps one (errors.Unwrap, or an .Unwrap() method
// call), and none reaches the
// yaml error a target error wraps, whose text quotes the value.
func TestNoUnmaskedTargetErrorShown(t *testing.T) {
	for _, dir := range []string{"../cli", "../server"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			// Deliberately coarse: any .Unwrap() call in these packages fails,
			// a non-target error's too. Allow one by name here only after
			// checking it can never reach a target error.
			for _, banned := range []string{"errors.Unwrap(", ".Unwrap()", "yaml.TypeError"} {
				if strings.Contains(string(b), banned) {
					t.Errorf("%s uses %s: a target error's unmasked text could be shown", f, banned)
				}
			}
		}
	}
}

// A key typed as a name and split by '.' (no key's shape) is never echoed:
// name refusals identify the entry by index and line, not by its name, in
// a target file, a run file, and a target the dashboard sends.
func TestNameRefusalsDoNotEchoTheName(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	const half1, half2 = "465b5ce8b199b49f", "aa5f0a2ee238a6bc"
	dotted := half1 + "." + half2
	base := "version: 1\nkind: target\nname: a\ncore:\n  amf: 192.0.2.1:38412\n  plmn: {mcc: \"001\", mnc: \"01\"}\n"
	check := func(name string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), half1) || strings.Contains(err.Error(), half2) {
			t.Errorf("%s: %v, want %q and no part of the name", name, err, want)
		}
	}
	_, err := Parse("lab.yaml", []byte(base+"subscribers:\n  - {name: "+dotted+", first_supi: \"001010000000001\"}\n"), none)
	check("a dotted pool name", err, "subscribers[0].name may not contain '.', '[' or ']'")
	_, err = Parse("lab.yaml", []byte(base+"subscribers:\n  - {name: "+half1+", first_supi: \"001010000000001\"}\n  - {name: "+half1+", first_supi: \"001010000000002\"}\n"), none)
	check("a duplicate pool name", err, "subscribers[1].name is the same as subscribers[0].name")
	_, err = Parse("lab.yaml", []byte(base+"app:\n  responders:\n    - {name: "+dotted+"}\n"), none)
	check("a dotted responder name", err, "app.responders[0].name may not contain")
	_, err = ParseRunFile("soak.yaml", []byte("kind: run\nworkloads:\n  - name: "+dotted+"\n    attach: {count: 1}\n"), none)
	check("a dotted workload name", err, "workloads[0].name may not contain")
	_, err = ParseRunFile("soak.yaml", []byte("kind: run\nworkloads:\n  - name: w\n    attach: {count: 1}\ngates:\n  - name: "+dotted+"\n    conformance: true\n"), none)
	check("a dotted gate name", err, "gates[0].name may not contain")
	// A target the dashboard sends (the probe's path).
	sent := Target{Name: "lab1", Core: Core{AMF: "192.0.2.1:38412", PLMN: PLMN{MCC: "001", MNC: "01"}}}
	sent.Subscribers = []Pool{{Name: dotted, FirstSUPI: "001010000000001"}}
	_, err = ResolveSent(sent)
	check("sent: a dotted pool name", err, "subscribers[0].name: may not contain")
	sent.Subscribers = []Pool{{Name: half1, FirstSUPI: "001010000000001"}, {Name: half1, FirstSUPI: "001010000000002"}}
	_, err = ResolveSent(sent)
	check("sent: a duplicate pool name", err, "subscribers[1].name: is the same as subscribers[0].name")
	sent.Subscribers = nil
	sent.App.Responders = []Responder{{Name: dotted}}
	_, err = ResolveSent(sent)
	check("sent: a dotted responder name", err, "app.responders[0].name: may not contain")
	// Target.Validate (a target built in code: no file, no ResolveSent).
	v := Target{Version: Version, Kind: Kind, Name: "lab1", Subscribers: []Pool{{Name: dotted}, {Name: dotted}}}
	check("Validate: a duplicate pool name", v.Validate(), "subscribers[1] needs a name of its own")
	v = Target{Version: Version, Kind: Kind, Name: "lab1", App: App{Responders: []Responder{{Name: dotted}, {Name: dotted}}}}
	check("Validate: a duplicate responder name", v.Validate(), "app.responders[1] needs a name of its own")
}
