package ngap

import "testing"

func TestEncodePLMN(t *testing.T) {
	tests := []struct {
		mcc, mnc string
		want     [3]byte
		wantErr  bool
	}{
		// Field-used reference bytes from gnbsim's builder (PLMN 208/93,
		// the ATB-01 / Aether default).
		{"208", "93", [3]byte{0x02, 0xF8, 0x39}, false},
		// 3-digit MNC per the TS 24.008 §10.5.1.3 layout (octet 2 high
		// nibble = MNC digit 3). Not capture-verified; free5gc's converter
		// disagrees here — see the EncodePLMN warning.
		{"234", "150", [3]byte{0x32, 0x04, 0x51}, false},
		{"20", "93", [3]byte{}, true},
		{"2089", "93", [3]byte{}, true},
		{"208", "9", [3]byte{}, true},
		{"2o8", "93", [3]byte{}, true},
	}
	for _, tt := range tests {
		got, err := EncodePLMN(tt.mcc, tt.mnc)
		if tt.wantErr {
			if err == nil {
				t.Errorf("EncodePLMN(%q, %q): expected error, got %x", tt.mcc, tt.mnc, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("EncodePLMN(%q, %q): %v", tt.mcc, tt.mnc, err)
			continue
		}
		if got != tt.want {
			t.Errorf("EncodePLMN(%q, %q) = %x, want %x", tt.mcc, tt.mnc, got, tt.want)
		}
	}
}

func TestPLMNRoundTrip(t *testing.T) {
	for _, pair := range [][2]string{{"208", "93"}, {"234", "150"}, {"001", "01"}, {"999", "999"}} {
		enc, err := EncodePLMN(pair[0], pair[1])
		if err != nil {
			t.Fatalf("encode %v: %v", pair, err)
		}
		mcc, mnc := DecodePLMN(enc)
		if mcc != pair[0] || mnc != pair[1] {
			t.Errorf("round trip %v → %x → %s/%s", pair, enc, mcc, mnc)
		}
	}
}
