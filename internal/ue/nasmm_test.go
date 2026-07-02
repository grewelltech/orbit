package ue

import (
	"testing"

	"github.com/free5gc/nas/security"

	"github.com/bgrewell/orbit/internal/nas"
)

// buildSUCI is a small helper for the tests.
func buildSUCI(t *testing.T) []byte {
	t.Helper()
	id, err := ParseIdentity("208930100007500", "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	suci, err := id.EncodeNullSUCI()
	if err != nil {
		t.Fatal(err)
	}
	return suci
}

// TestRegistrationRequestRoundTrip builds a Registration Request and decodes
// it back with free5gc, confirming the mandatory IEs and the SUCI survive.
func TestRegistrationRequestRoundTrip(t *testing.T) {
	suci := buildSUCI(t)
	raw, err := BuildRegistrationRequest(suci, SecurityCapability(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := nas.DecodePlain(raw)
	if err != nil {
		t.Fatalf("free5gc decode of our Registration Request failed: %v", err)
	}
	if m.GmmMessage == nil || m.RegistrationRequest == nil {
		t.Fatal("decoded message is not a Registration Request")
	}
	rr := m.RegistrationRequest
	if rr.GetRegistrationType5GS() != 0x01 {
		t.Errorf("registration type = %d, want 1 (initial)", rr.GetRegistrationType5GS())
	}
	if int(rr.MobileIdentity5GS.Len) != len(suci) {
		t.Errorf("SUCI length = %d, want %d", rr.MobileIdentity5GS.Len, len(suci))
	}
	if rr.UESecurityCapability == nil {
		t.Error("UE security capability missing")
	}
}

func TestAuthenticationResponseRoundTrip(t *testing.T) {
	resStar := make([]byte, 16)
	for i := range resStar {
		resStar[i] = byte(i + 1)
	}
	raw, err := BuildAuthenticationResponse(resStar)
	if err != nil {
		t.Fatal(err)
	}
	m, err := nas.DecodePlain(raw)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if m.AuthenticationResponse == nil {
		t.Fatal("not an Authentication Response")
	}
	got := m.AuthenticationResponse.AuthenticationResponseParameter.GetRES()
	for i := range resStar {
		if got[i] != resStar[i] {
			t.Fatalf("RES* mismatch at %d: %x != %x", i, got[i], resStar[i])
		}
	}

	if _, err := BuildAuthenticationResponse(make([]byte, 8)); err == nil {
		t.Error("expected error for wrong-length RES*")
	}
}

// keyPair returns deterministic NIA2/NEA0 keys for the security tests.
func keyPair() (kInt, kEnc [16]byte) {
	for i := 0; i < 16; i++ {
		kInt[i] = byte(0x10 + i)
		kEnc[i] = byte(0x20 + i)
	}
	return kInt, kEnc
}

// TestEncodeSecureUplink checks that EncodeSecure produces the correct
// framing (TS 24.501 §9.1.1) and an integrity MAC that an independent
// recomputation with free5gc's primitive confirms — for the uplink
// direction the UE actually uses. NIA2 + NEA0 are the ATB-01 core's
// selected algorithms (D-8).
func TestEncodeSecureUplink(t *testing.T) {
	kInt, kEnc := keyPair()
	ctx := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA0, KNASint: kInt, KNASenc: kEnc}

	msg, err := BuildSecurityModeComplete(nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := msg.PlainNasEncode()
	if err != nil {
		t.Fatal(err)
	}

	wrapped, err := ctx.EncodeSecure(msg, nas.SecHdrIntegrityCipheredNewCtx)
	if err != nil {
		t.Fatalf("EncodeSecure: %v", err)
	}
	if wrapped[0] != 0x7E || wrapped[1] != nas.SecHdrIntegrityCipheredNewCtx {
		t.Errorf("header = %x %x, want 7e %x", wrapped[0], wrapped[1], nas.SecHdrIntegrityCipheredNewCtx)
	}
	seq := wrapped[6]
	if seq != 0 {
		t.Errorf("first sequence number = %d, want 0", seq)
	}
	// NEA0: payload is unchanged from plain.
	if got := wrapped[7:]; string(got) != string(plain) {
		t.Errorf("payload changed under NEA0:\n got  %x\n want %x", got, plain)
	}
	// Independent MAC over seq‖payload with uplink direction, count 0.
	want, err := security.NASMacCalculate(security.AlgIntegrity128NIA2, kInt, 0,
		security.Bearer3GPP, security.DirectionUplink, append([]byte{seq}, plain...))
	if err != nil {
		t.Fatal(err)
	}
	if got := wrapped[2:6]; string(got) != string(want) {
		t.Errorf("MAC mismatch:\n got  %x\n want %x", got, want)
	}
	if ctx.UplinkCount() != 1 {
		t.Errorf("uplink count = %d, want 1 after one send", ctx.UplinkCount())
	}
}

// TestDecodeSecureDownlink builds a downlink secured message the way the AMF
// would (downlink-direction MAC) and confirms DecodeSecure verifies it,
// recovers the message, and rejects a tampered MAC.
func TestDecodeSecureDownlink(t *testing.T) {
	kInt, kEnc := keyPair()

	msg, err := BuildSecurityModeComplete(nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := msg.PlainNasEncode()
	if err != nil {
		t.Fatal(err)
	}
	seq := byte(0)
	mac, err := security.NASMacCalculate(security.AlgIntegrity128NIA2, kInt, uint32(seq),
		security.Bearer3GPP, security.DirectionDownlink, append([]byte{seq}, plain...))
	if err != nil {
		t.Fatal(err)
	}
	down := append([]byte{0x7E, nas.SecHdrIntegrityCipheredNewCtx}, mac...)
	down = append(down, seq)
	down = append(down, plain...)

	ctx := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA0, KNASint: kInt, KNASenc: kEnc}
	got, err := ctx.DecodeSecure(down)
	if err != nil {
		t.Fatalf("DecodeSecure of valid message: %v", err)
	}
	if got.GmmMessage == nil || got.SecurityModeComplete == nil {
		t.Error("decoded downlink is not a Security Mode Complete")
	}

	tampered := append([]byte(nil), down...)
	tampered[4] ^= 0xFF
	ctx2 := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA0, KNASint: kInt, KNASenc: kEnc}
	if _, err := ctx2.DecodeSecure(tampered); err == nil {
		t.Error("expected integrity failure on tampered MAC")
	}
}
