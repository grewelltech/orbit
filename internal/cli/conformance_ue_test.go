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
			got, err := tc.flags.spec("internet", "208", "93")
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

// --nas-integrity and --nas-ciphering: unset keeps the default, names are
// case-insensitive and comma lists combine, NIA0 and unknown names are
// refused, and they need --supi.
func TestConformanceNASAlgorithmFlags(t *testing.T) {
	const ki, opc = "5122250214c33e723a5dd523fc145fc0", "981d464c7c52eb6e5036234984ad0bcf"
	ueFlags := func(ia, ea []string) conformanceUEFlags {
		return conformanceUEFlags{supi: "208930100000011", ki: ki, opc: opc, gnbN3: "10.0.0.5", nasIntegrity: ia, nasCiphering: ea}
	}
	tests := []struct {
		name           string
		flags          conformanceUEFlags
		wantIA, wantEA uint8
		wantErr        string
	}{
		{name: "default", flags: ueFlags(nil, nil)},
		{name: "one each", flags: ueFlags([]string{"NIA1"}, []string{"NEA2"}), wantIA: 0x40, wantEA: 0x20},
		{name: "fully featured", flags: ueFlags([]string{"nia1", "NIA2", "Nia3"}, []string{"NEA0", "NEA1", "NEA2", "NEA3"}), wantIA: 0x70, wantEA: 0xf0},
		{name: "NIA0 refused", flags: ueFlags([]string{"NIA0", "NIA2"}, nil), wantErr: "NIA0 is refused"},
		{name: "unknown integrity", flags: ueFlags([]string{"NIA4"}, nil), wantErr: "--nas-integrity"},
		{name: "ciphering name for integrity", flags: ueFlags([]string{"NEA1"}, nil), wantErr: "--nas-integrity"},
		{name: "unknown ciphering", flags: ueFlags(nil, []string{"AES"}), wantErr: "--nas-ciphering"},
		{name: "empty list", flags: ueFlags([]string{}, nil), wantErr: "name at least one"},
		{name: "without --supi", flags: conformanceUEFlags{nasIntegrity: []string{"NIA1"}}, wantErr: "need --supi"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.flags.spec("internet", "208", "93")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.NASIntegrity != tc.wantIA || got.NASCiphering != tc.wantEA {
				t.Errorf("NAS algorithms = %#02x/%#02x, want %#02x/%#02x", got.NASIntegrity, got.NASCiphering, tc.wantIA, tc.wantEA)
			}
		})
	}
}

// The flags reach the command: parsed from the command line, a comma list
// and a repeated flag combine, and an unset flag stays nil (the default).
func TestNASAlgorithmFlagsParse(t *testing.T) {
	var ia, ea []string
	cmd := newConformanceCmd()
	f := cmd.Flags()
	f.Lookup("nas-integrity").Value = newNilStringSlice(&ia)
	f.Lookup("nas-ciphering").Value = newNilStringSlice(&ea)
	if err := f.Parse([]string{"--nas-integrity", "NIA1,NIA3", "--nas-integrity", "NIA2"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ia, ",") != "NIA1,NIA3,NIA2" {
		t.Errorf("--nas-integrity = %q", ia)
	}
	if ea != nil {
		t.Errorf("unset --nas-ciphering = %q, want nil", ea)
	}
}
