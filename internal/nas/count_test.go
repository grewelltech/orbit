package nas_test

import (
	"errors"
	"testing"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/nas"
)

// DecodeUnverified reads a PDU whose MAC is wrong, under either cipher, and
// leaves the stored COUNT alone: the next genuine PDU still verifies. A PDU
// too short to hold a security header is refused.
func TestDecodeUnverified(t *testing.T) {
	tests := []struct {
		name    string
		ea      uint8
		corrupt bool
		short   bool
		wantErr bool
	}{
		{name: "bad MAC, NEA0", ea: nas.NEA0, corrupt: true},
		{name: "bad MAC, NEA2", ea: nas.NEA2, corrupt: true},
		{name: "good MAC", ea: nas.NEA2},
		{name: "too short", ea: nas.NEA0, short: true, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ue, amf := pairCtx(tc.ea)
			first, err := ue.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := amf.DecodeSecure(first); err != nil {
				t.Fatal(err)
			}
			pdu, err := ue.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			if tc.corrupt {
				pdu[3] ^= 0xff
				if _, err := amf.DecodeSecure(pdu); !errors.Is(err, nas.ErrIntegrity) {
					t.Fatalf("DecodeSecure of the corrupted PDU = %v, want ErrIntegrity", err)
				}
			}
			if tc.short {
				pdu = pdu[:5]
			}
			m, err := amf.DecodeUnverified(pdu)
			if (err != nil) != tc.wantErr {
				t.Fatalf("DecodeUnverified = %v, want error %v", err, tc.wantErr)
			}
			if err == nil && m.GmmHeader.GetMessageType() != f5nas.MsgTypeRegistrationComplete {
				t.Errorf("decoded message type %#x, want Registration Complete", m.GmmHeader.GetMessageType())
			}
			next, err := ue.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := amf.DecodeSecure(next); err != nil {
				t.Errorf("the next genuine PDU no longer verifies: %v", err)
			}
		})
	}
}
