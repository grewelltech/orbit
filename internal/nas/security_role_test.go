package nas_test

import (
	"bytes"
	"errors"
	"testing"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/nas"
)

// plainMsg builds a minimal valid plain 5GMM message (Registration Complete)
// to exercise the security wrapping without pulling in the ue package.
func plainMsg() *f5nas.Message {
	m := f5nas.NewMessage()
	m.GmmMessage = f5nas.NewGmmMessage()
	m.GmmHeader.SetMessageType(f5nas.MsgTypeRegistrationComplete)
	rc := nasMessage.NewRegistrationComplete(f5nas.MsgTypeRegistrationComplete)
	rc.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSMobilityManagementMessage)
	rc.SpareHalfOctetAndSecurityHeaderType.SetSecurityHeaderType(f5nas.SecurityHeaderTypePlainNas)
	rc.SpareHalfOctetAndSecurityHeaderType.SetSpareHalfOctet(0)
	var mid nasType.RegistrationCompleteMessageIdentity
	mid.SetMessageType(f5nas.MsgTypeRegistrationComplete)
	rc.RegistrationCompleteMessageIdentity = mid
	m.GmmMessage.RegistrationComplete = rc
	return m
}

// TestSecurityContextBothRoles proves the Network flag lets one
// implementation serve both ends: a UE context and an AMF context sharing
// keys interoperate in both directions. A message the UE sends (uplink) is
// verified by the AMF; a message the AMF sends (downlink) is verified by the
// UE. A tampered MAC is rejected.
func TestSecurityContextBothRoles(t *testing.T) {
	var kInt, kEnc [16]byte
	for i := range kInt {
		kInt[i] = byte(0x30 + i)
		kEnc[i] = byte(0x40 + i)
	}
	ue := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA0, KNASint: kInt, KNASenc: kEnc}
	amf := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA0, KNASint: kInt, KNASenc: kEnc, Network: true}

	// Uplink: UE sends, AMF verifies.
	up, err := ue.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := amf.DecodeSecure(up); err != nil {
		t.Fatalf("AMF failed to verify UE uplink: %v", err)
	}

	// Downlink: AMF sends, UE verifies.
	down, err := amf.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ue.DecodeSecure(down); err != nil {
		t.Fatalf("UE failed to verify AMF downlink: %v", err)
	}

	// A tampered downlink is rejected.
	down2, _ := amf.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
	down2[4] ^= 0xFF
	if _, err := ue.DecodeSecure(down2); !errors.Is(err, nas.ErrIntegrity) {
		t.Errorf("tampered downlink: err = %v, want ErrIntegrity", err)
	}
}

// EncodeSecureRaw wraps bytes that need not decode: the MAC verifies, the
// payload comes back as sent and only the plain decode fails. Its output
// equals EncodeSecure's for the same message, new-context reset included.
func TestEncodeSecureRaw(t *testing.T) {
	var kInt, kEnc [16]byte
	for i := range kInt {
		kInt[i] = byte(0x30 + i)
		kEnc[i] = byte(0x40 + i)
	}
	ctx := func(network bool) *nas.SecurityContext {
		return &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA2, KNASint: kInt, KNASenc: kEnc, Network: network}
	}
	plain, err := plainMsg().PlainNasEncode()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		plain   []byte
		hdr     uint8
		wantErr bool
	}{
		{name: "valid message, ciphered", plain: plain, hdr: nas.SecHdrIntegrityCiphered},
		{name: "valid message, new context", plain: plain, hdr: nas.SecHdrIntegrityCipheredNewCtx},
		{name: "truncated message", plain: []byte{0x7e, 0x00, 0x68}, hdr: nas.SecHdrIntegrityCiphered, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := ctx(true), ctx(true)
			raw, err := a.EncodeSecureRaw(tc.plain, tc.hdr)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantErr {
				msg := f5nas.NewMessage()
				buf := append([]byte(nil), tc.plain...)
				if err := msg.PlainNasDecode(&buf); err != nil {
					t.Fatal(err)
				}
				viaMsg, err := b.EncodeSecure(plainMsg(), tc.hdr)
				if err != nil || !bytes.Equal(raw, viaMsg) {
					t.Errorf("EncodeSecureRaw %x, EncodeSecure %x (%v)", raw, viaMsg, err)
				}
			}
			_, err = ctx(false).DecodeSecure(raw)
			if errors.Is(err, nas.ErrIntegrity) || (err != nil) != tc.wantErr {
				t.Errorf("DecodeSecure err = %v, want error %v and no integrity failure", err, tc.wantErr)
			}
		})
	}
	if _, err := ctx(true).EncodeSecureRaw(plain, nas.SecHdrPlain); err == nil {
		t.Error("EncodeSecureRaw accepted the plain header")
	}
}

// A downlink that fails the integrity check leaves the receive COUNT as it
// was: neither a sequence byte that would wrap the overflow counter nor a
// new-context header may shift it, so the next valid downlink still
// verifies. The new-context case runs past COUNT 255 first, so a reset
// would lose a non-zero overflow.
func TestBadMACLeavesCountAlone(t *testing.T) {
	var kInt, kEnc [16]byte
	for i := range kInt {
		kInt[i] = byte(0x30 + i)
		kEnc[i] = byte(0x40 + i)
	}
	tests := []struct {
		name string
		// before is how many valid downlinks arrive first.
		before int
		// bad turns a valid downlink into the bad-MAC PDU.
		bad func(pdu []byte)
	}{
		{name: "sequence byte that would bump the overflow", before: 6, bad: func(pdu []byte) { pdu[6] = 2 }},
		{name: "new-context header", before: 300, bad: func(pdu []byte) { pdu[1] = nas.SecHdrIntegrityCipheredNewCtx }},
		{name: "corrupted MAC only", before: 6, bad: func(pdu []byte) {}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ue := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA2, KNASint: kInt, KNASenc: kEnc}
			amf := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA2, KNASint: kInt, KNASenc: kEnc, Network: true}
			// Downlink COUNTs 0 to before-1 arrive and verify.
			for i := 0; i < tc.before; i++ {
				pdu, err := amf.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ue.DecodeSecure(pdu); err != nil {
					t.Fatalf("downlink %d: %v", i, err)
				}
			}
			// A bad-MAC PDU, built from a copy so the AMF's COUNT does not move.
			shadow := *amf
			bad, err := shadow.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			tc.bad(bad)
			bad[2] ^= 0xff
			if _, err := ue.DecodeSecure(bad); !errors.Is(err, nas.ErrIntegrity) {
				t.Fatalf("bad PDU: err = %v, want ErrIntegrity", err)
			}
			// The next valid downlink still verifies.
			next, err := amf.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ue.DecodeSecure(next); err != nil {
				t.Errorf("valid downlink after the bad one: %v", err)
			}
		})
	}
}
