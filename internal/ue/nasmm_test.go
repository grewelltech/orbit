package ue

import (
	"bytes"
	"testing"

	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"
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

// WithCapability5GMM replaces the 5GMM capability value and survives a
// decode; without it the IE is the single zero octet 3; a value outside 1
// to 13 octets is refused.
func TestWithCapability5GMM(t *testing.T) {
	tests := []struct {
		name    string
		opts    []RegistrationOption
		want    []byte
		wantErr bool
	}{
		{name: "default", want: []byte{0}},
		{name: "octets 3 to 5 zero", opts: []RegistrationOption{WithCapability5GMM([]byte{0, 0, 0})}, want: []byte{0, 0, 0}},
		{name: "13 octets", opts: []RegistrationOption{WithCapability5GMM(make([]byte, 13))}, want: make([]byte, 13)},
		{name: "empty", opts: []RegistrationOption{WithCapability5GMM(nil)}, wantErr: true},
		{name: "14 octets", opts: []RegistrationOption{WithCapability5GMM(make([]byte, 14))}, wantErr: true},
	}
	suci := buildSUCI(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := BuildRegistrationRequest(suci, SecurityCapability(), nil, tc.opts...)
			if tc.wantErr {
				if err == nil {
					t.Fatal("built a Registration Request with an invalid 5GMM capability")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			m, err := nas.DecodePlain(raw)
			if err != nil {
				t.Fatal(err)
			}
			rr := m.RegistrationRequest
			c := rr.Capability5GMM
			if c == nil || int(c.GetLen()) != len(tc.want) || string(c.Octet[:c.GetLen()]) != string(tc.want) {
				t.Errorf("5GMM capability = %+v, want %x", c, tc.want)
			}
		})
	}
}

// CleartextRegistrationRequest keeps the mandatory IEs and the UE security
// capability of the complete request and drops the non-cleartext IEs
// (TS 24.501 4.4.6); the complete request travels unchanged in the Security
// Mode Complete's NAS message container.
func TestCleartextRegistrationRequest(t *testing.T) {
	suci := buildSUCI(t)
	nssai, err := BuildRequestedNSSAI([]SNSSAI{{SST: 1, SD: "010203"}})
	if err != nil {
		t.Fatal(err)
	}
	caps, err := NewSecurityCapability([]uint8{0, 2}, []uint8{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		caps nasType.UESecurityCapability
		// nssai and opts shape the complete request; wantNonCleartext is
		// the first non-cleartext IEI it carries.
		nssai            *nasType.RequestedNSSAI
		opts             []RegistrationOption
		wantNonCleartext uint8
	}{
		{name: "default", caps: SecurityCapability(), wantNonCleartext: 0x10},
		{name: "requested NSSAI", caps: SecurityCapability(), nssai: nssai, wantNonCleartext: 0x10},
		{name: "other algorithms and a 3-octet 5GMM capability", caps: caps, nssai: nssai,
			opts: []RegistrationOption{WithCapability5GMM([]byte{0, 0, 0})}, wantNonCleartext: 0x10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			complete, err := BuildRegistrationRequest(suci, tc.caps, tc.nssai, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if iei, found, err := nas.NonCleartextRegistrationIE(complete); err != nil || !found || iei != tc.wantNonCleartext {
				t.Fatalf("complete request: non-cleartext IE 0x%02X (found %t, err %v), want 0x%02X", iei, found, err, tc.wantNonCleartext)
			}
			initial, err := CleartextRegistrationRequest(complete)
			if err != nil {
				t.Fatal(err)
			}
			if iei, found, err := nas.NonCleartextRegistrationIE(initial); err != nil || found {
				t.Errorf("initial request: non-cleartext IE 0x%02X (found %t, err %v)", iei, found, err)
			}
			m, err := nas.DecodePlain(initial)
			if err != nil {
				t.Fatal(err)
			}
			rr := m.RegistrationRequest
			if rr.Capability5GMM != nil || rr.RequestedNSSAI != nil {
				t.Errorf("initial request carries 5GMM capability %v or Requested NSSAI %v", rr.Capability5GMM, rr.RequestedNSSAI)
			}
			if k := rr.NgksiAndRegistrationType5GS; k.GetRegistrationType5GS() != nasMessage.RegistrationType5GSInitialRegistration || k.GetFOR() != 1 ||
				k.GetNasKeySetIdentifiler() != 7 || k.GetTSC() != nasMessage.TypeOfSecurityContextFlagNative {
				t.Errorf("initial request: registration type and ngKSI octet = %#x", k.Octet)
			}
			if !bytes.Equal(rr.MobileIdentity5GS.Buffer[:rr.MobileIdentity5GS.Len], suci) {
				t.Errorf("initial request SUCI = %x, want %x", rr.MobileIdentity5GS.Buffer, suci)
			}
			if c := rr.UESecurityCapability; c == nil || !bytes.Equal(c.Buffer[:c.GetLen()], tc.caps.Buffer[:tc.caps.GetLen()]) {
				t.Errorf("initial request UE security capability = %+v, want %x", c, tc.caps.Buffer)
			}

			smc, err := BuildSecurityModeComplete(complete)
			if err != nil {
				t.Fatal(err)
			}
			c := smc.SecurityModeComplete.NASMessageContainer
			if c == nil || !bytes.Equal(c.Buffer[:c.GetLen()], complete) {
				t.Errorf("Security Mode Complete container = %+v, want the complete request %x", c, complete)
			}
		})
	}

	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{name: "empty", in: nil},
		{name: "truncated", in: []byte{0x7e, 0x00, 0x41}},
		{name: "another message", in: []byte{0x7e, 0x00, 0x43}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, err := CleartextRegistrationRequest(tc.in); err == nil {
				t.Errorf("= %x, want an error", out)
			}
		})
	}
}
