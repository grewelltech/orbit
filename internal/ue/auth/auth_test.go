package auth

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/free5gc/util/milenage"
	"github.com/free5gc/util/ueauth"
)

// Test credentials mirror the ATB-01 simapp provisioning shape (a shared
// test key across the subscriber range). Not a real subscriber secret.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const (
	testKi   = "5122250214c33e723a5dd523fc145fc0"
	testOPc  = "981d464c7c52eb6e5036234984ad0bcf"
	testMCC  = "208"
	testMNC  = "93"
	testSUPI = "208930100007500"
)

// TestDeriveFromChallengeSelfConsistent generates the challenge the way the
// network (AUSF/UDM) does with free5gc's Milenage, then runs the UE side and
// checks it inverts it: RES == XRES, no MAC failure, and the UE-computed
// RES* equals the XRES* the network would compute from the same inputs.
//
// The Milenage f1–f5 and the KDF are already validated against 3GPP
// conformance vectors by free5gc's own tests; this proves ORBIT composes
// them correctly (parameter order, AUTN slicing, RES* truncation). The live
// core accepting the Authentication Response is the ultimate arbiter.
func TestDeriveFromChallengeSelfConsistent(t *testing.T) {
	opc := mustHex(t, testOPc)
	k := mustHex(t, testKi)
	rand := mustHex(t, "0102030405060708090a0b0c0d0e0f10")
	sqn := mustHex(t, "000000000021")
	amf := mustHex(t, "8000")

	// Network side.
	ik, ck, xres, autn, err := milenage.GenerateAKAParameters(opc, k, rand, sqn, amf)
	if err != nil {
		t.Fatal(err)
	}
	snn := ServingNetworkName(testMCC, testMNC)
	ckik := append(append([]byte{}, ck...), ik...)
	xresStar, err := ueauth.GetKDFValue(ckik, ueauth.FC_FOR_RES_STAR_XRES_STAR_DERIVATION,
		[]byte(snn), ueauth.KDFLen([]byte(snn)), rand, ueauth.KDFLen(rand), xres, ueauth.KDFLen(xres))
	if err != nil {
		t.Fatal(err)
	}

	// UE side.
	sub := Subscription{SUPI: testSUPI, Ki: k, OPc: opc}
	v, err := sub.DeriveFromChallenge(rand, autn, snn, NIA2, NEA0)
	if err != nil {
		t.Fatalf("UE derivation failed: %v", err)
	}
	if !bytes.Equal(v.ResStar, xresStar[16:]) {
		t.Errorf("RES* mismatch:\n ue  %x\n net %x", v.ResStar, xresStar[16:])
	}
	if len(v.KAmf) != 32 || len(v.KSeaf) != 32 || len(v.KAusf) != 32 {
		t.Errorf("key sizes: KAUSF=%d KSEAF=%d KAMF=%d, want 32 each", len(v.KAusf), len(v.KSeaf), len(v.KAmf))
	}
	// Keys must be non-zero and distinct (a wiring bug tends to zero or
	// alias them).
	if v.KNASint == ([16]byte{}) || v.KNASenc == ([16]byte{}) {
		t.Error("NAS keys are all-zero")
	}
	if v.KNASint == v.KNASenc {
		t.Error("KNASint == KNASenc (algorithm-type distinguisher not applied)")
	}
}

// TestDeriveFromChallengeMACFailure corrupts AUTN and expects the UE to
// reject the network rather than derive keys (TS 33.501 §6.1.3.2).
func TestDeriveFromChallengeMACFailure(t *testing.T) {
	opc := mustHex(t, testOPc)
	k := mustHex(t, testKi)
	rand := mustHex(t, "0102030405060708090a0b0c0d0e0f10")
	sqn := mustHex(t, "000000000021")
	amf := mustHex(t, "8000")
	_, _, _, autn, err := milenage.GenerateAKAParameters(opc, k, rand, sqn, amf)
	if err != nil {
		t.Fatal(err)
	}
	autn[len(autn)-1] ^= 0xFF // corrupt the MAC

	sub := Subscription{SUPI: testSUPI, Ki: k, OPc: opc}
	_, err = sub.DeriveFromChallenge(rand, autn, ServingNetworkName(testMCC, testMNC), NIA2, NEA0)
	if err == nil {
		t.Fatal("expected MAC failure, got nil")
	}
	if _, ok := err.(*MACFailure); !ok {
		t.Errorf("expected *MACFailure, got %T: %v", err, err)
	}
}

func TestServingNetworkName(t *testing.T) {
	// TS 24.501 §9.11.3.15 example form; MNC zero-padded to 3 digits.
	if got := ServingNetworkName("208", "93"); got != "5G:mnc093.mcc208.3gppnetwork.org" {
		t.Errorf("got %q", got)
	}
	if got := ServingNetworkName("234", "150"); got != "5G:mnc150.mcc234.3gppnetwork.org" {
		t.Errorf("got %q", got)
	}
}

func TestParseHexKey(t *testing.T) {
	if _, err := ParseHexKey("Ki", testKi); err != nil {
		t.Errorf("valid key rejected: %v", err)
	}
	if _, err := ParseHexKey("Ki", "abcd"); err == nil {
		t.Error("short key accepted")
	}
	if _, err := ParseHexKey("Ki", "zz22250214c33e723a5dd523fc145fc0"); err == nil {
		t.Error("non-hex key accepted")
	}
}
