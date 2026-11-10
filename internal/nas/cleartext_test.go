package nas

import "testing"

// The cleartext IEs of TS 24.501 4.4.6 pass; any other optional IE of the
// REGISTRATION REQUEST (table 8.2.6.1.1) is reported by its IEI.
func TestNonCleartextRegistrationIE(t *testing.T) {
	// Header, initial registration with ngKSI 7, and a 2-octet 5GS mobile
	// identity (the scan reads only its length).
	head := []byte{0x7e, 0x00, 0x41, 0x79, 0x00, 0x02, 0x01, 0x02}
	secCap := []byte{0x2e, 0x02, 0x80, 0x20}
	addGUTI := append([]byte{0x77, 0x00, 0x0b}, make([]byte, 11)...)
	ueStatus := []byte{0x2b, 0x01, 0x00}
	epsNAS := []byte{0x70, 0x00, 0x02, 0x07, 0x41}
	nid := []byte{0x32, 0x06, 0, 0, 0, 0, 0, 0}
	disasterPLMN := []byte{0x16, 0x03, 0x02, 0xf8, 0x39}
	cap5GMM := []byte{0x10, 0x01, 0x00}
	nssai := []byte{0x2f, 0x05, 0x04, 0x01, 0x01, 0x02, 0x03}
	msg := func(ies ...[]byte) []byte {
		out := append([]byte(nil), head...)
		for _, ie := range ies {
			out = append(out, ie...)
		}
		return out
	}
	tests := []struct {
		name    string
		in      []byte
		wantIEI uint8
		want    bool
		wantErr bool
	}{
		{name: "mandatory IEs only", in: msg()},
		{name: "UE security capability", in: msg(secCap)},
		{name: "every cleartext IE", in: msg(secCap, addGUTI, ueStatus, epsNAS, nid, disasterPLMN)},
		{name: "5GMM capability", in: msg(cap5GMM, secCap), wantIEI: 0x10, want: true},
		{name: "requested NSSAI after cleartext IEs", in: msg(secCap, nssai), wantIEI: 0x2f, want: true},
		{name: "5GMM capability and requested NSSAI", in: msg(cap5GMM, secCap, nssai), wantIEI: 0x10, want: true},
		{name: "type 1 IE (MICO indication)", in: msg(secCap, []byte{0xb1}), wantIEI: 0xb0, want: true},
		{name: "NAS message container", in: msg(secCap, []byte{0x71, 0x00, 0x01, 0x00}), wantIEI: 0x71, want: true},
		{name: "empty", in: nil, wantErr: true},
		{name: "another message type", in: []byte{0x7e, 0x00, 0x5e, 0x79, 0x00, 0x00}, wantErr: true},
		{name: "protected", in: append([]byte{0x7e, 0x02}, head...), wantErr: true},
		{name: "mobile identity past the end", in: []byte{0x7e, 0x00, 0x41, 0x79, 0x00, 0x09, 0x01}, wantErr: true},
		{name: "truncated TLV length", in: msg([]byte{0x2e}), wantErr: true},
		{name: "TLV value past the end", in: msg([]byte{0x2e, 0x04, 0x80}), wantErr: true},
		{name: "truncated TLV-E length", in: msg([]byte{0x77, 0x00}), wantErr: true},
		{name: "TLV-E value past the end", in: msg([]byte{0x70, 0x00, 0x05, 0x07}), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			iei, found, err := NonCleartextRegistrationIE(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if found != tc.want || iei != tc.wantIEI {
				t.Errorf("= 0x%02X %t, want 0x%02X %t", iei, found, tc.wantIEI, tc.want)
			}
		})
	}
}
