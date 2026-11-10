package engine

import (
	"testing"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
)

// nasCount names both candidate COUNTs of a protected downlink and finds
// the one its MAC verifies at, with the message type it deciphers to there,
// without moving the stored COUNT: the next COUNT, a repeat of the stored
// one, a lower one, the sequence number's wrap, and a corrupted MAC, under
// NEA0 and NEA2.
func TestNASCount(t *testing.T) {
	tests := []struct {
		name       string
		stored     uint32 // the UE's stored downlink COUNT
		sent       uint32 // the COUNT the PDU is protected at
		corrupt    bool
		wantCands  [2]uint32
		wantCount  uint32
		wantVerify bool
	}{
		{name: "next COUNT", stored: 1, sent: 2, wantCands: [2]uint32{2, 258}, wantCount: 2, wantVerify: true},
		{name: "repeat of the stored COUNT", stored: 1, sent: 1, wantCands: [2]uint32{1, 257}, wantCount: 1, wantVerify: true},
		{name: "lower COUNT", stored: 5, sent: 4, wantCands: [2]uint32{4, 260}, wantCount: 4, wantVerify: true},
		{name: "wrap", stored: 255, sent: 256, wantCands: [2]uint32{0, 256}, wantCount: 256, wantVerify: true},
		{name: "second overflow", stored: 0x1fe, sent: 0x200, wantCands: [2]uint32{0x100, 0x200}, wantCount: 0x200, wantVerify: true},
		{name: "corrupted MAC", stored: 1, sent: 2, corrupt: true, wantCands: [2]uint32{2, 258}},
	}
	for _, ea := range []uint8{nas.NEA0, nas.NEA2} {
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				var kInt, kEnc [16]byte
				for i := range kInt {
					kInt[i], kEnc[i] = byte(0x30+i), byte(0x40+i)
				}
				ueCtx := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: ea, KNASint: kInt, KNASenc: kEnc}
				amf := &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: ea, KNASint: kInt, KNASenc: kEnc, Network: true}
				ueCtx.ForceRxCount(tc.stored)
				amf.ForceTxCount(tc.sent)
				msg, err := ue.BuildRegistrationComplete()
				if err != nil {
					t.Fatal(err)
				}
				pdu, err := amf.EncodeSecure(msg, nas.SecHdrIntegrityCiphered)
				if err != nil {
					t.Fatal(err)
				}
				if tc.corrupt {
					pdu[2] ^= 0xff
				}
				c := nasCount(ueCtx, pdu)
				if c == nil || c.Stored != tc.stored || c.Candidates != tc.wantCands {
					t.Fatalf("nasCount = %+v, want stored %d candidates %v", c, tc.stored, tc.wantCands)
				}
				count, mt, ok := c.Verifies()
				if ok != tc.wantVerify || count != tc.wantCount {
					t.Errorf("Verifies = %d, %v, want %d, %v", count, ok, tc.wantCount, tc.wantVerify)
				}
				if ok && mt != f5nas.MsgTypeRegistrationComplete {
					t.Errorf("message type %#x at the verified COUNT, want Registration Complete", mt)
				}
				if ueCtx.RxCount() != tc.stored {
					t.Errorf("stored COUNT moved to %d", ueCtx.RxCount())
				}
			})
		}
	}
	if c := nasCount(&nas.SecurityContext{}, []byte{0x7e, 0x02, 0}); c != nil {
		t.Errorf("nasCount of a PDU without a security header = %+v, want nil", c)
	}
}
