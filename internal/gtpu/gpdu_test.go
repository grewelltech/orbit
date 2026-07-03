package gtpu

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestEncodeGPDUGolden(t *testing.T) {
	// TEID 0x00000251 (593, a real UPF-allocated value from the live core),
	// QFI 1, a 4-byte dummy inner payload.
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	got := EncodeGPDU(0x00000251, 1, payload)

	// 34 ff | len | 00000251 | 0000 00 85 | 01 10 01 00 | deadbeef
	// len = 4 (opt) + 4 (container) + 4 (payload) = 12 = 0x000c
	want := "34ff000c000002510000008501100100deadbeef"
	if hex.EncodeToString(got) != want {
		t.Errorf("G-PDU bytes:\n got  %s\n want %s", hex.EncodeToString(got), want)
	}

	// The header up to and including the next-ext-type byte is 12 octets;
	// the container follows (DESIGN §2.2 wire trap).
	if got[0] != 0x34 {
		t.Errorf("flags = %#x, want 0x34 (E flag set)", got[0])
	}
	if got[11] != 0x85 {
		t.Errorf("octet 12 = %#x, want 0x85 (PDU Session Container first)", got[11])
	}
}

func TestGPDURoundTrip(t *testing.T) {
	for _, qfi := range []uint8{1, 5, 9, 63} {
		payload := bytes.Repeat([]byte{0xAB}, 40)
		enc := EncodeGPDU(0xCAFEF00D, qfi, payload)
		dec, err := DecodeGPDU(enc)
		if err != nil {
			t.Fatalf("qfi %d: decode: %v", qfi, err)
		}
		if dec.TEID != 0xCAFEF00D {
			t.Errorf("qfi %d: TEID = %#x", qfi, dec.TEID)
		}
		if !dec.HasQFI || dec.QFI != qfi {
			t.Errorf("qfi %d: decoded QFI = %d (has=%v)", qfi, dec.QFI, dec.HasQFI)
		}
		if !bytes.Equal(dec.Payload, payload) {
			t.Errorf("qfi %d: payload mismatch", qfi)
		}
	}
}

func TestDecodeGPDUShort(t *testing.T) {
	if _, err := DecodeGPDU([]byte{0x34, 0xff}); err == nil {
		t.Error("expected error on short packet")
	}
}

func TestEncodeEndMarker(t *testing.T) {
	em := EncodeEndMarker(0x00000251)
	// 30 fe 0000 00000251
	if hex.EncodeToString(em) != "30fe000000000251" {
		t.Errorf("End Marker = %s", hex.EncodeToString(em))
	}
	if em[1] != MsgTypeEndMarker {
		t.Errorf("message type = %#x, want %#x", em[1], MsgTypeEndMarker)
	}
}
