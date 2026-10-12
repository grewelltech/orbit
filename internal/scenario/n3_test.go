package scenario

import (
	"strings"
	"testing"
)

// A scenario gNB's n3 goes through the shared rule: an IPv4-mapped address
// is canonicalised, an IPv6 one refused citing #156, and an empty one kept.
func TestScenarioGNBN3(t *testing.T) {
	const head = "core: {amf: x, plmn: {mcc: \"001\", mnc: \"01\"}}\ngnbs:\n"
	tests := []struct {
		name, gnb, want, wantErr string
	}{
		{name: "IPv4", gnb: "  - {name: g, n3: 10.0.0.5}\n", want: "10.0.0.5"},
		{name: "IPv4-mapped", gnb: "  - {name: g, n3: \"::ffff:10.0.0.5\"}\n", want: "10.0.0.5"},
		{name: "none", gnb: "  - {name: g}\n", want: ""},
		{name: "IPv6", gnb: "  - {name: g, n3: \"0:0:0:0:0:0:0:1\"}\n", wantErr: "gnb g: n3: gNB N3 address must be IPv4 (IPv4-mapped accepted); IPv6 N3 is not supported yet (#156)"},
		{name: "not an IP", gnb: "  - {name: g, n3: gnb.example}\n", wantErr: "gnb g: n3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc, err := Parse([]byte(head + tc.gnb))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse: %v, want an error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := sc.GNBs[0].N3; got != tc.want {
				t.Errorf("n3 = %q, want %q", got, tc.want)
			}
		})
	}
}

// With PDU sessions each fleet gNB's N3 address (its n3_ips entry, else its
// source IP) must be IPv4; without them the N3 address is never announced,
// so an IPv6 N2 source is fine.
func TestFleetGNBN3(t *testing.T) {
	fleet := func(topo string, pdu bool) string {
		p := "false"
		if pdu {
			p = "true"
		}
		return "kind: fleet\ncore: { amf: x, plmn: { mcc: \"001\", mnc: \"01\" } }\ntopology: { gnbs: " + topo +
			" }\nfleet: { count: 1, supi_base: \"001010100007500\", pdu_session: " + p + " }"
	}
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "IPv4 source", yaml: fleet(`{ count: 1, source_ips: [10.0.0.1] }`, true)},
		{name: "IPv6 source, IPv4 n3_ips", yaml: fleet(`{ count: 1, source_ips: ["2001:db8::1"], n3_ips: [10.0.0.1] }`, true)},
		{name: "IPv6 source without PDU sessions", yaml: fleet(`{ count: 1, source_ips: ["2001:db8::1"] }`, false)},
		{name: "IPv6 source as N3", yaml: fleet(`{ count: 2, source_ips: [10.0.0.1, "2001:db8::2"] }`, true),
			wantErr: "topology.gnbs.source_ips[1] (gNB 2's N3 address): gNB N3 address must be IPv4"},
		{name: "IPv6 n3_ips", yaml: fleet(`{ count: 1, source_ips: [10.0.0.1], n3_ips: ["::1"] }`, true),
			wantErr: "topology.gnbs.n3_ips[0] (gNB 1's N3 address)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFleetNoEnv([]byte(tc.yaml))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseFleetNoEnv: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParseFleetNoEnv: %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
