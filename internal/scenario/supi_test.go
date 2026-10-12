package scenario

import (
	"strings"
	"testing"
)

// A step scenario's supi and range.base accept either spelling and resolve to
// digits-only SUPIs; a malformed value is the shared normalisation error.
func TestStepScenarioSUPIForms(t *testing.T) {
	const head = "core:\n  amf: 1.2.3.4:38412\n  plmn: { mcc: \"208\", mnc: \"93\" }\ngnbs:\n  - {id: 1, name: g}\nues:\n"
	tests := []struct {
		name    string
		ues     string
		want    []string
		wantErr string
	}{
		{name: "bare supi", ues: "  - {supi: \"208930100007500\", gnb: g}\n", want: []string{"208930100007500"}},
		{name: "nai supi", ues: "  - {supi: \"imsi-208930100007500\", gnb: g}\n", want: []string{"208930100007500"}},
		{name: "nai range base", ues: "  - {range: {base: \"imsi-208930100007500\", count: 2}, gnb: g}\n", want: []string{"208930100007500", "208930100007501"}},
		{name: "malformed supi", ues: "  - {supi: \"IMSI-208930100007500\", gnb: g}\n", wantErr: "supi: invalid SUPI \"IMSI-208930100007500\": want 5-15 digits or imsi-<digits>"},
		{name: "malformed range base", ues: "  - {range: {base: \"2089\", count: 2}, gnb: g}\n", wantErr: "range.base: invalid SUPI \"2089\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc, err := Parse([]byte(head + tc.ues))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			ues, err := sc.ResolveUEs()
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, u := range ues {
				got = append(got, u.SUPI)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("SUPIs = %v, want %v", got, tc.want)
			}
		})
	}
}

// A fleet scenario's supi_base accepts either spelling, is stored digits-only
// and generates digits-only SUPIs; a malformed value is rejected at parse.
func TestFleetScenarioSUPIBaseForms(t *testing.T) {
	const tmpl = `kind: fleet
core:
  amf: 1.2.3.4:38412
  plmn: { mcc: "208", mnc: "93" }
credentials: { ki: "00112233445566778899aabbccddeeff", opc: "000102030405060708090a0b0c0d0e0f" }
topology:
  gnbs: { count: 1, id_base: 1, layout: grid, source_ips: ["127.0.0.1"] }
fleet: { count: 2, supi_base: "%s" }
`
	tests := []struct {
		name    string
		base    string
		want    []string
		wantErr string
	}{
		{name: "bare", base: "208930100007500", want: []string{"208930100007500", "208930100007501"}},
		{name: "nai", base: "imsi-208930100007500", want: []string{"208930100007500", "208930100007501"}},
		{name: "malformed", base: "imsi-2089", wantErr: "fleet.supi_base: invalid SUPI \"imsi-2089\": want 5-15 digits or imsi-<digits>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseFleet([]byte(strings.Replace(tmpl, "%s", tc.base, 1)))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFleet: %v", err)
			}
			if f.Fleet.SUPIBase != "208930100007500" {
				t.Errorf("stored supi_base = %q, want digits only", f.Fleet.SUPIBase)
			}
			var got []string
			for _, u := range f.GenFleet(f.GenGNBs()) {
				got = append(got, u.SUPI)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("SUPIs = %v, want %v", got, tc.want)
			}
		})
	}
}
