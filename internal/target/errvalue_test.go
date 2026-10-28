package target

import (
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
	for _, tc := range []struct {
		s    string
		want bool
	}{
		{"00112233445566778899aabbccddeeff", true},
		{" 00112233445566778899AABBCCDDEEFF ", true},
		{strings.Repeat("ab", 32), true},
		{"0011223344556677889900aabbccdde", false},   // 31
		{"00112233445566778899aabbccddeeff0", false}, // 33
		{"orc1_x", true},
		{"oj1_x", true},
		{"orl1_x", true},
		{"orx1_x", false},
		{"${ORBIT_KI}", false},
		{"010203", false},
		{"", false},
	} {
		if got := keyShaped(tc.s); got != tc.want {
			t.Errorf("keyShaped(%q) = %t, want %t", tc.s, got, tc.want)
		}
	}
	if got := maskKeyShaped("[1/00112233445566778899aabbccddeeff] and orc1_ab-c"); got != "[1/"+Masked+"] and "+Masked {
		t.Errorf("maskKeyShaped: %s", got)
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
