package cli

import (
	"strings"
	"testing"

	"github.com/bgrewell/orbit/internal/conformance"
)

// The conformance UE flags: none means no UE; --supi needs keys (flags or
// environment) and a gNB N3 IP; UE flags without --supi are refused.
func TestConformanceUEFlagsSpec(t *testing.T) {
	const ki, opc = "5122250214c33e723a5dd523fc145fc0", "981d464c7c52eb6e5036234984ad0bcf"
	tests := []struct {
		name    string
		flags   conformanceUEFlags
		envKi   string
		envOPc  string
		want    conformance.UESpec
		wantErr string
	}{
		{name: "no UE", want: conformance.UESpec{}},
		{name: "full", flags: conformanceUEFlags{supi: "208930100000011", ki: ki, opc: opc, gnbN3: "10.0.0.5", traffic: "10.1.1.1:7"},
			want: conformance.UESpec{SUPI: "208930100000011", Ki: ki, OPc: opc, DNN: "internet", GNBN3: "10.0.0.5", Traffic: "10.1.1.1:7"}},
		{name: "keys from the environment", flags: conformanceUEFlags{supi: "208930100000011", gnbN3: "10.0.0.5"}, envKi: ki, envOPc: opc,
			want: conformance.UESpec{SUPI: "208930100000011", Ki: ki, OPc: opc, DNN: "internet", GNBN3: "10.0.0.5"}},
		{name: "no keys", flags: conformanceUEFlags{supi: "208930100000011", gnbN3: "10.0.0.5"}, wantErr: "subscriber keys are required"},
		{name: "bad key", flags: conformanceUEFlags{supi: "208930100000011", ki: "zz", opc: opc, gnbN3: "10.0.0.5"}, wantErr: "Ki"},
		{name: "no gNB N3", flags: conformanceUEFlags{supi: "208930100000011", ki: ki, opc: opc}, wantErr: "--gnb-n3"},
		{name: "IPv6 gNB N3", flags: conformanceUEFlags{supi: "208930100000011", ki: ki, opc: opc, gnbN3: "0:0:0:0:0:0:0:1"}, wantErr: "IPv6 N3 is not supported yet (#156)"},
		{name: "IPv4-mapped gNB N3 is canonicalised", flags: conformanceUEFlags{supi: "208930100000011", ki: ki, opc: opc, gnbN3: "::ffff:10.0.0.5"},
			want: conformance.UESpec{SUPI: "208930100000011", Ki: ki, OPc: opc, DNN: "internet", GNBN3: "10.0.0.5"}},
		{name: "gNB N3 with a port", flags: conformanceUEFlags{supi: "208930100000011", ki: ki, opc: opc, gnbN3: "10.0.0.5:2152"}, wantErr: "--gnb-n3"},
		{name: "traffic without a port", flags: conformanceUEFlags{supi: "208930100000011", ki: ki, opc: opc, gnbN3: "10.0.0.5", traffic: "10.1.1.1"}, wantErr: "--traffic"},
		{name: "SUPI outside the PLMN", flags: conformanceUEFlags{supi: "001010000000001", ki: ki, opc: opc, gnbN3: "10.0.0.5"}, wantErr: "--supi"},
		{name: "gNB N3 without --supi", flags: conformanceUEFlags{gnbN3: "10.0.0.5"}, wantErr: "need --supi"},
		{name: "keys without --supi", flags: conformanceUEFlags{ki: ki}, wantErr: "need --supi"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBIT_KI", tc.envKi)
			t.Setenv("ORBIT_OPC", tc.envOPc)
			got, err := tc.flags.spec("internet", "208", "93", func(string) bool { return true })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("spec = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// gnbN3Flag applies the shared rule to every --gnb-n3: IPv4 and IPv4-mapped
// (canonicalised) pass, "" stays "", IPv6 is refused citing #156.
func TestGNBN3Flag(t *testing.T) {
	tests := []struct {
		in, want, wantErr string
	}{
		{in: "", want: ""},
		{in: "10.0.0.5", want: "10.0.0.5"},
		{in: "::ffff:10.0.0.5", want: "10.0.0.5"},
		{in: "0:0:0:0:0:0:0:1", wantErr: "--gnb-n3: gNB N3 address must be IPv4 (IPv4-mapped accepted); IPv6 N3 is not supported yet (#156)"},
		{in: "10.0.0.5:2152", wantErr: "--gnb-n3"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := gnbN3Flag(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("gnbN3Flag(%q) = %q, %v; want an error containing %q", tc.in, got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("gnbN3Flag(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
}
