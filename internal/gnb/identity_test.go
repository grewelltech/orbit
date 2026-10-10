package gnb

import (
	"strings"
	"testing"
)

func TestValidatePLMN(t *testing.T) {
	tests := []struct {
		mcc, mnc string
		wantErr  string
	}{
		{"001", "01", ""},
		{"001", "001", ""},
		{"208", "93", ""},
		{"999", "999", ""},
		{"", "01", "mcc"},
		{"01", "01", "mcc"},
		{"0001", "01", "mcc"},
		{"20a", "01", "mcc"},
		{"001", "", "mnc"},
		{"001", "1", "mnc"},
		{"001", "0001", "mnc"},
		{"001", "9x", "mnc"},
	}
	for _, tc := range tests {
		t.Run(tc.mcc+"/"+tc.mnc, func(t *testing.T) {
			err := ValidatePLMN(tc.mcc, tc.mnc)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidatePLMN(%q, %q): %v", tc.mcc, tc.mnc, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidatePLMN(%q, %q) = %v, want error naming %s", tc.mcc, tc.mnc, err, tc.wantErr)
			}
		})
	}
}

func TestValidateTAC(t *testing.T) {
	for _, tc := range []struct {
		tac uint32
		ok  bool
	}{{0, false}, {1, true}, {0xFFFFFF, true}, {0x1000000, false}, {^uint32(0), false}} {
		if err := ValidateTAC(tc.tac); (err == nil) != tc.ok {
			t.Errorf("ValidateTAC(%#x) = %v, want ok=%v", tc.tac, err, tc.ok)
		}
	}
}

func TestValidateSNSSAI(t *testing.T) {
	tests := []struct {
		sst uint32
		sd  string
		ok  bool
	}{
		{1, "", true}, {0, "", true}, {255, "", true}, {256, "", false},
		{1, "010203", true}, {1, "ABCDEF", true},
		{1, "01020", false}, {1, "0102030", false}, {1, "01020g", false}, {1, "0x0102", false},
	}
	for _, tc := range tests {
		if err := ValidateSNSSAI(tc.sst, tc.sd); (err == nil) != tc.ok {
			t.Errorf("ValidateSNSSAI(%d, %q) = %v, want ok=%v", tc.sst, tc.sd, err, tc.ok)
		}
	}
}

func TestConfigValidateIdentity(t *testing.T) {
	good := Config{MCC: "001", MNC: "01", TAC: 1, Slices: []SNSSAI{{SST: 1, SD: "010203"}}}
	if err := good.ValidateIdentity(); err != nil {
		t.Fatalf("valid identity rejected: %v", err)
	}
	tests := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"missing plmn", func(c *Config) { c.MCC, c.MNC = "", "" }, "mcc"},
		{"bad mnc", func(c *Config) { c.MNC = "1" }, "mnc"},
		{"tac zero", func(c *Config) { c.TAC = 0 }, "tac"},
		{"tac too big", func(c *Config) { c.TAC = MaxTAC + 1 }, "tac"},
		{"no slices", func(c *Config) { c.Slices = nil }, "S-NSSAI"},
		{"bad sd", func(c *Config) { c.Slices[0].SD = "xyz" }, "sd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			c.Slices = append([]SNSSAI(nil), good.Slices...)
			tc.mut(&c)
			err := c.ValidateIdentity()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want naming %s", err, tc.want)
			}
		})
	}
}
