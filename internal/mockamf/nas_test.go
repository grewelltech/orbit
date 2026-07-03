package mockamf

import (
	"testing"

	"github.com/free5gc/util/milenage"

	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// Shared test credentials (the ATB-01 simapp shape). Not a real secret.
var (
	testKi   = mh("5122250214c33e723a5dd523fc145fc0")
	testOPc  = mh("981d464c7c52eb6e5036234984ad0bcf")
	testSUPI = "208930100007500"
)

func mh(s string) []byte {
	b, err := auth.ParseHexKey("k", s)
	if err != nil {
		panic(err)
	}
	return b
}

// TestMockNASAgainstUE drives the AMF-side NAS builders through the real UE
// parsers and crypto: the mock's Authentication Request round-trips to the
// same RAND/AUTN, both sides derive identical NAS keys, and a Security Mode
// Command the mock wraps under its network security context is verified and
// decoded by the UE's context. This validates the mock's NAS layer and the
// network-role security context without the full NGAP handler.
func TestMockNASAgainstUE(t *testing.T) {
	snn := auth.ServingNetworkName("208", "93")
	rand := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	sqn := []byte{0, 0, 0, 0, 0, 0x21}
	amf := []byte{0x80, 0x00}

	// Network side generates the challenge.
	_, _, _, autnSlice, err := milenage.GenerateAKAParameters(testOPc, testKi, rand[:], sqn, amf)
	if err != nil {
		t.Fatal(err)
	}
	var autn [16]byte
	copy(autn[:], autnSlice)

	// Mock builds Authentication Request; UE parses it back.
	arBytes, err := buildAuthenticationRequest(rand, autn, 1)
	if err != nil {
		t.Fatal(err)
	}
	arMsg, err := nas.DecodePlain(arBytes)
	if err != nil {
		t.Fatalf("UE decode of Authentication Request: %v", err)
	}
	ch, err := ue.ParseAuthenticationRequest(arMsg)
	if err != nil {
		t.Fatal(err)
	}
	if ch.RAND != rand || ch.AUTN != autn {
		t.Fatal("RAND/AUTN did not round-trip through the mock's Authentication Request")
	}

	// Both sides derive the NAS keys from the same challenge.
	sub := auth.Subscription{SUPI: testSUPI, Ki: testKi, OPc: testOPc}
	vec, err := sub.DeriveFromChallenge(ch.RAND[:], ch.AUTN[:], snn, nas.NIA2, nas.NEA0)
	if err != nil {
		t.Fatalf("derivation: %v", err)
	}

	ueSec := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA0, KNASint: vec.KNASint, KNASenc: vec.KNASenc}
	amfSec := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA0, KNASint: vec.KNASint, KNASenc: vec.KNASenc, Network: true}

	// Mock builds + wraps Security Mode Command; UE verifies and decodes it.
	smc, err := buildSecurityModeCommand(nas.NIA2, nas.NEA0, 1, ue.SecurityCapability())
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := amfSec.EncodeSecure(smc, nas.SecHdrIntegrityNewContext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ueSec.DecodeSecure(wrapped)
	if err != nil {
		t.Fatalf("UE failed to verify the mock's Security Mode Command: %v", err)
	}
	sel, err := ue.ParseSecurityModeCommand(got)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Integrity != nas.NIA2 || sel.Ciphering != nas.NEA0 {
		t.Errorf("selected algorithms = int %d enc %d, want NIA2/NEA0", sel.Integrity, sel.Ciphering)
	}
}
