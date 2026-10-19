package ue

import (
	"bytes"
	"testing"
)

// NewSecurityCapability sets 5G-EAn at bit 8-n of octet 1 and 5G-IAn of
// octet 2, and refuses an algorithm above 7.
func TestNewSecurityCapability(t *testing.T) {
	tests := []struct {
		name    string
		ea, ia  []uint8
		want    []byte
		wantErr bool
	}{
		{name: "default pair", ea: []uint8{0}, ia: []uint8{2}, want: []byte{0x80, 0x20}},
		{name: "EA0-2, IA0-2", ea: []uint8{0, 1, 2}, ia: []uint8{0, 1, 2}, want: []byte{0xe0, 0xe0}},
		{name: "NIA0 and NIA3 only", ea: []uint8{0, 3}, ia: []uint8{0, 3}, want: []byte{0x90, 0x90}},
		{name: "EA7", ea: []uint8{7}, want: []byte{0x01, 0x00}},
		{name: "out of range", ia: []uint8{8}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewSecurityCapability(tc.ea, tc.ia)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && (!bytes.Equal(c.Buffer, tc.want) || c.GetLen() != 2) {
				t.Errorf("capability = %x (len %d), want %x", c.Buffer, c.GetLen(), tc.want)
			}
		})
	}
	if d := SecurityCapability(); !bytes.Equal(d.Buffer, []byte{0x80, 0x20}) {
		t.Errorf("default capability = %x, want 8020", d.Buffer)
	}
}
