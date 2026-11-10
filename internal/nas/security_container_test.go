package nas

import (
	"bytes"
	"testing"
)

// A NAS message container ciphered by the UE with the COUNT of the message
// that carries it deciphers on the network side once that message has
// verified, under every cipher; NEA0 leaves it as it is.
func TestCipherContainerRoundTrip(t *testing.T) {
	inner := []byte{0x7e, 0x00, 0x4c, 0x10, 0x00, 0x07, 0xf4, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x07, 0x40, 0x02, 0x02, 0x00}
	for _, ea := range []uint8{NEA0, NEA1, NEA2, NEA3} {
		key := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
		ueCtx := &SecurityContext{IntegrityAlg: NIA2, CipheringAlg: ea, KNASint: key, KNASenc: key}
		net := &SecurityContext{IntegrityAlg: NIA2, CipheringAlg: ea, KNASint: key, KNASenc: key, Network: true}
		// Two earlier uplink messages, so the container's COUNT is not 0.
		for i := 0; i < 2; i++ {
			b, err := ueCtx.EncodeSecureRaw([]byte{0x7e, 0x00, 0x43}, SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := net.DecodeSecurePlain(b); err != nil {
				t.Fatalf("NEA%d: earlier message: %v", ea, err)
			}
		}
		container := append([]byte(nil), inner...)
		if err := ueCtx.CipherContainer(container); err != nil {
			t.Fatal(err)
		}
		if (ea == NEA0) != bytes.Equal(container, inner) {
			t.Errorf("NEA%d: container ciphered = %v", ea, !bytes.Equal(container, inner))
		}
		outer := append([]byte{0x7e, 0x00, 0x4c, 0x10, 0x00, 0x07, 0xf4, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x07, 0x71, 0x00, byte(len(container))}, container...)
		wire, err := ueCtx.EncodeSecureRaw(outer, SecHdrIntegrity)
		if err != nil {
			t.Fatal(err)
		}
		m, _, err := net.DecodeSecurePlain(wire)
		if err != nil || m.ServiceRequest == nil || m.ServiceRequest.NASMessageContainer == nil {
			t.Fatalf("NEA%d: the carrying message: %v", ea, err)
		}
		got := append([]byte(nil), m.ServiceRequest.NASMessageContainer.Buffer...)
		if err := net.DecipherContainer(got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, inner) {
			t.Errorf("NEA%d: deciphered container % x, want % x", ea, got, inner)
		}
	}
}

// An unknown ciphering algorithm is an error from both directions.
func TestCipherContainerUnknownAlgorithm(t *testing.T) {
	s := &SecurityContext{CipheringAlg: 9}
	if err := s.CipherContainer([]byte{1}); err == nil {
		t.Error("CipherContainer with an unknown algorithm succeeded")
	}
	if err := s.DecipherContainer([]byte{1}); err == nil {
		t.Error("DecipherContainer with an unknown algorithm succeeded")
	}
}
