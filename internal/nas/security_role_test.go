package nas_test

import (
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
	if _, err := ue.DecodeSecure(down2); err == nil {
		t.Error("UE accepted a tampered downlink")
	}
}
