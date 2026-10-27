package nas_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/bgrewell/orbit/internal/nas"
)

// pairCtx is a UE and an AMF context sharing keys, under ea.
func pairCtx(ea uint8) (ue, amf *nas.SecurityContext) {
	var kInt, kEnc [16]byte
	for i := range kInt {
		kInt[i], kEnc[i] = byte(0x30+i), byte(0x40+i)
	}
	ue = &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: ea, KNASint: kInt, KNASenc: kEnc}
	amf = &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: ea, KNASint: kInt, KNASenc: kEnc, Network: true}
	return ue, amf
}

// DecodeSecurePlain returns the inner plain message once the MAC verified
// and it deciphered, even when the codec refuses it; nothing from a PDU
// that fails the MAC, which also leaves the COUNT where it was.
func TestDecodeSecurePlain(t *testing.T) {
	complete, err := plainMsg().PlainNasEncode()
	if err != nil {
		t.Fatal(err)
	}
	// A Registration Reject without its cause octet: the codec refuses it.
	truncated := []byte{0x7e, 0x00, 0x44}
	tests := []struct {
		name       string
		ea         uint8
		inner      []byte
		breakMAC   bool
		wantPlain  bool
		wantMsg    bool
		wantErrIs  error
		wantAnyErr bool
	}{
		{name: "NEA0, valid MAC", ea: nas.NEA0, inner: complete, wantPlain: true, wantMsg: true},
		{name: "NEA2, valid MAC", ea: nas.NEA2, inner: complete, wantPlain: true, wantMsg: true},
		{name: "NEA2, MAC broken", ea: nas.NEA2, inner: complete, breakMAC: true, wantErrIs: nas.ErrIntegrity},
		{name: "NEA0, MAC broken", ea: nas.NEA0, inner: complete, breakMAC: true, wantErrIs: nas.ErrIntegrity},
		{name: "NEA2, valid MAC, codec refuses", ea: nas.NEA2, inner: truncated, wantPlain: true, wantAnyErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ue, amf := pairCtx(tc.ea)
			pdu, err := amf.EncodeSecureRaw(append([]byte(nil), tc.inner...), nas.SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			if tc.breakMAC {
				pdu[3] ^= 0xff
			}
			msg, plain, err := ue.DecodeSecurePlain(pdu)
			switch {
			case tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs):
				t.Fatalf("err = %v, want %v", err, tc.wantErrIs)
			case tc.wantErrIs == nil && !tc.wantAnyErr && err != nil:
				t.Fatalf("err = %v", err)
			case tc.wantAnyErr && err == nil:
				t.Fatal("decoded a message the codec should refuse")
			}
			if tc.wantPlain != (plain != nil) || (tc.wantPlain && !bytes.Equal(plain, tc.inner)) {
				t.Errorf("plain = %x, want %x (set %t)", plain, tc.inner, tc.wantPlain)
			}
			if tc.wantMsg != (msg != nil) {
				t.Errorf("msg = %v, want set %t", msg, tc.wantMsg)
			}
			if tc.breakMAC {
				// The COUNT did not advance: the genuine PDU at the same
				// COUNT still verifies.
				pdu[3] ^= 0xff
				if _, _, err := ue.DecodeSecurePlain(pdu); err != nil {
					t.Errorf("the genuine PDU after a failed MAC: %v (COUNT moved)", err)
				}
			}
		})
	}
}

// The downlink COUNT survives its sequence-number byte wrapping: the PDU
// after the 256th still verifies and deciphers.
func TestDecodeSecurePlainCountWrap(t *testing.T) {
	complete, err := plainMsg().PlainNasEncode()
	if err != nil {
		t.Fatal(err)
	}
	ue, amf := pairCtx(nas.NEA2)
	for i := 0; i < 300; i++ {
		pdu, err := amf.EncodeSecureRaw(append([]byte(nil), complete...), nas.SecHdrIntegrityCiphered)
		if err != nil {
			t.Fatal(err)
		}
		_, plain, err := ue.DecodeSecurePlain(pdu)
		if err != nil || !bytes.Equal(plain, complete) {
			t.Fatalf("PDU %d (sequence byte %#02x): plain %x, err %v", i, pdu[6], plain, err)
		}
	}
}
