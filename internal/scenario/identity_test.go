package scenario

import (
	"strings"
	"testing"
)

// A scenario's core must name its PLMN, and the loader applies the CLI
// defaults to the rest, so a file and a command line that omit the same
// thing mean the same thing.
func TestCorePLMNRequiredAndDefaults(t *testing.T) {
	const minimal = `
core:
  amf: 1.2.3.4:38412
  plmn: { mcc: "001", mnc: "01" }
gnbs:
  - {id: 1, name: g}
`
	sc, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("minimal scenario rejected: %v", err)
	}
	if sc.Core.TAC != 1 || sc.Core.Slice.SST != 1 || sc.Core.Slice.SD != "" || sc.Core.DNN != "internet" {
		t.Errorf("defaults = tac %d sst %d sd %q dnn %q, want 1 1 \"\" internet",
			sc.Core.TAC, sc.Core.Slice.SST, sc.Core.Slice.SD, sc.Core.DNN)
	}

	tests := []struct {
		name string
		core string
		want string
	}{
		{"no plmn", `core: { amf: x }`, "core.plmn.mcc and core.plmn.mnc are required"},
		{"mcc only", `core: { amf: x, plmn: { mcc: "001" } }`, "core.plmn.mcc and core.plmn.mnc are required"},
		{"mnc one digit", `core: { amf: x, plmn: { mcc: "001", mnc: "1" } }`, "mnc"},
		{"mcc two digits", `core: { amf: x, plmn: { mcc: "01", mnc: "01" } }`, "mcc"},
		{"tac too large", `core: { amf: x, plmn: { mcc: "001", mnc: "01" }, tac: 16777216 }`, "tac"},
		{"bad sd", `core: { amf: x, plmn: { mcc: "001", mnc: "01" }, slice: { sst: 1, sd: "12345" } }`, "sd"},
		{"sst too large", `core: { amf: x, plmn: { mcc: "001", mnc: "01" }, slice: { sst: 300 } }`, "sst"},
		{"no amf", `core: { plmn: { mcc: "001", mnc: "01" } }`, "core.amf is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.core + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// Explicit core values are kept, not overwritten by the defaults.
func TestCoreExplicitValuesKept(t *testing.T) {
	sc, err := Parse([]byte(`
core:
  amf: 1.2.3.4:38412
  plmn: { mcc: "001", mnc: "001" }
  tac: 7
  slice: { sst: 2, sd: "abcdef" }
  dnn: ims
gnbs:
  - {id: 1, name: g}
`))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Core.PLMN.MNC != "001" || sc.Core.TAC != 7 || sc.Core.Slice.SST != 2 || sc.Core.Slice.SD != "abcdef" || sc.Core.DNN != "ims" {
		t.Errorf("explicit core values changed: %+v", sc.Core)
	}
}

// An unquoted `mnc: 01` keeps its leading zero: yaml.v3 decodes a scalar into
// a string field from its source text, so there is no integer round trip.
// Pinned so a loader change that broke it would be caught.
func TestCoreUnquotedMNCKeepsLeadingZero(t *testing.T) {
	sc, err := Parse([]byte("core:\n  amf: 1.2.3.4:38412\n  plmn:\n    mcc: 001\n    mnc: 01\ngnbs:\n  - {id: 1, name: g}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Core.PLMN.MCC != "001" || sc.Core.PLMN.MNC != "01" {
		t.Errorf("unquoted plmn = %s/%s, want 001/01", sc.Core.PLMN.MCC, sc.Core.PLMN.MNC)
	}
}

// ${ORBIT_MCC}/${ORBIT_MNC} expand in a scenario like the credential refs do.
func TestCorePLMNFromEnv(t *testing.T) {
	t.Setenv("ORBIT_MCC", "208")
	t.Setenv("ORBIT_MNC", "93")
	sc, err := Parse([]byte("core:\n  amf: 1.2.3.4:38412\n  plmn: { mcc: \"${ORBIT_MCC}\", mnc: \"${ORBIT_MNC}\" }\ngnbs:\n  - {id: 1, name: g}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Core.PLMN.MCC != "208" || sc.Core.PLMN.MNC != "93" {
		t.Errorf("env plmn = %s/%s, want 208/93", sc.Core.PLMN.MCC, sc.Core.PLMN.MNC)
	}
	// Unset variables expand to nothing, which is then the required-PLMN error.
	t.Setenv("ORBIT_MCC", "")
	t.Setenv("ORBIT_MNC", "")
	_, err = Parse([]byte("core:\n  amf: 1.2.3.4:38412\n  plmn: { mcc: \"${ORBIT_MCC}\", mnc: \"${ORBIT_MNC}\" }\n"))
	if err == nil || !strings.Contains(err.Error(), "core.plmn.mcc and core.plmn.mnc are required") {
		t.Fatalf("err = %v, want the required-PLMN error", err)
	}
}

// Fleet scenarios apply the same rule and defaults through the same code.
func TestFleetCorePLMNRequiredAndDefaults(t *testing.T) {
	base := `kind: fleet
core: CORE
topology: { gnbs: { count: 1, source_ips: [a] } }
fleet: { count: 1, supi_base: "1" }
`
	if _, err := ParseFleet([]byte(strings.Replace(base, "CORE", `{ amf: x }`, 1))); err == nil || !strings.Contains(err.Error(), "core.plmn") {
		t.Fatalf("fleet without plmn: err = %v", err)
	}
	f, err := ParseFleet([]byte(strings.Replace(base, "CORE", `{ amf: x, plmn: { mcc: "001", mnc: "01" } }`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if f.Core.TAC != 1 || f.Core.Slice.SST != 1 || f.Core.DNN != "internet" {
		t.Errorf("fleet defaults not applied: %+v", f.Core)
	}
}
