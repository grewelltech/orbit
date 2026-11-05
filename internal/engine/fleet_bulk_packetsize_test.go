package engine

import "testing"

func TestResolveBulkPacketSize(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		want      int
	}{
		{"zero auto-detects the tunnel max", 0, maxBulkPacketSize},
		{"negative also auto-detects", -1, maxBulkPacketSize},
		{"explicit small size is honored", 64, 64},
		{"explicit mid size is honored", 1200, 1200},
		{"explicit max is honored", maxBulkPacketSize, maxBulkPacketSize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBulkPacketSize(tc.requested); got != tc.want {
				t.Errorf("resolveBulkPacketSize(%d) = %d, want %d", tc.requested, got, tc.want)
			}
		})
	}
}

func TestValidateBulkPacketSize(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"zero is valid (auto)", 0, false},
		{"one is valid", 1, false},
		{"mid is valid", 1200, false},
		{"at the ceiling is valid", maxBulkPacketSize, false},
		{"negative is rejected", -1, true},
		{"over the ceiling is rejected", maxBulkPacketSize + 1, true},
		{"jumbo is rejected on the tunnel path", 8972, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBulkPacketSize(tc.size)
			if tc.wantErr != (err != nil) {
				t.Errorf("ValidateBulkPacketSize(%d) err = %v, wantErr %v", tc.size, err, tc.wantErr)
			}
		})
	}
}
