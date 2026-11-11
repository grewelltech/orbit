package nas

import (
	"bytes"
	"testing"
)

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

// oneRegistrationIE encodes a single optional IE with IEI key in format f
// around a small value.
func oneRegistrationIE(key uint8, f ieFormat) []byte {
	switch f {
	case ieTV1:
		return []byte{key | 0x01}
	case ieTV7:
		return []byte{key, 1, 2, 3, 4, 5, 6}
	case ieTLVE:
		return []byte{key, 0x00, 0x02, 0xaa, 0xbb}
	}
	return []byte{key, 0x02, 0xaa, 0xbb}
}

// The cleartext form and the scan are built from one list and agree on
// every optional IE of the message: an IE the scan accepts is carried byte
// for byte, any other is left out, and the result always passes the scan.
// The cleartext IEs are exactly those of TS 24.501 4.4.6.
func TestCleartextRegistrationRequestMatchesTheScan(t *testing.T) {
	head := []byte{0x7e, 0x00, 0x41, 0x79, 0x00, 0x02, 0x01, 0x02}
	wantCleartext := map[uint8]bool{
		0x2E: true, // UE security capability
		0x77: true, // Additional GUTI
		0x2B: true, // UE status
		0x70: true, // EPS NAS message container
		0x32: true, // NID
		0x16: true, // UE determined PLMN with disaster condition
	}
	var all, allCleartext []byte
	all, allCleartext = append(all, head...), append(allCleartext, head...)
	for key := 0; key < 256; key++ {
		ie, known := registrationIEs[uint8(key)]
		if !known {
			continue
		}
		if ie.cleartext != wantCleartext[uint8(key)] {
			t.Errorf("IE 0x%02X: cleartext = %t, want %t", key, ie.cleartext, wantCleartext[uint8(key)])
		}
		enc := oneRegistrationIE(uint8(key), ie.format)
		msg := append(append([]byte(nil), head...), enc...)
		_, nonCleartext, err := NonCleartextRegistrationIE(msg)
		if err != nil {
			t.Fatalf("IE 0x%02X: scan: %v", key, err)
		}
		got, err := CleartextRegistrationRequest(msg)
		if err != nil {
			t.Fatalf("IE 0x%02X: cleartext form: %v", key, err)
		}
		want := msg
		if nonCleartext {
			want = head
		}
		if !bytes.Equal(got, want) {
			t.Errorf("IE 0x%02X (scan says cleartext %t): cleartext form = %x, want %x", key, !nonCleartext, got, want)
		}
		all = append(all, enc...)
		if !nonCleartext {
			allCleartext = append(allCleartext, enc...)
		}
	}
	if len(registrationIEs) != 43 {
		t.Errorf("%d optional IEs listed, want the 43 of table 8.2.6.1.1", len(registrationIEs))
	}

	// Every optional IE at once: only the cleartext ones remain, in order.
	got, err := CleartextRegistrationRequest(all)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, allCleartext) {
		t.Errorf("cleartext form of every IE = %x, want %x", got, allCleartext)
	}
	if iei, found, err := NonCleartextRegistrationIE(got); err != nil || found {
		t.Errorf("the cleartext form fails the scan: IE 0x%02X (found %t, err %v)", iei, found, err)
	}
}

func TestCleartextRegistrationRequestErrors(t *testing.T) {
	head := []byte{0x7e, 0x00, 0x41, 0x79, 0x00, 0x02, 0x01, 0x02}
	with := func(ies ...byte) []byte { return append(append([]byte(nil), head...), ies...) }
	tests := []struct {
		name string
		in   []byte
	}{
		{name: "empty"},
		{name: "another message type", in: []byte{0x7e, 0x00, 0x5e, 0x79, 0x00, 0x00}},
		{name: "protected", in: append([]byte{0x7e, 0x02}, head...)},
		{name: "mobile identity past the end", in: []byte{0x7e, 0x00, 0x41, 0x79, 0x00, 0x09, 0x01}},
		{name: "IEI the message does not define", in: with(0x2e, 0x02, 0x80, 0x20, 0x7f, 0x00)},
		{name: "truncated TLV length", in: with(0x10)},
		{name: "non-cleartext TLV value past the end", in: with(0x10, 0x05, 0x00)},
		{name: "truncated TLV-E length", in: with(0x71, 0x00)},
		{name: "cleartext TLV-E value past the end", in: with(0x77, 0x00, 0x0b, 0xf2)},
		{name: "truncated 7-octet TV", in: with(0x52, 0x01, 0x02)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if out, err := CleartextRegistrationRequest(tc.in); err == nil {
				t.Errorf("= %x, want an error", out)
			}
		})
	}
}
