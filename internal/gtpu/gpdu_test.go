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

// TestDecodeEndMarkerExtensions: an End Marker's PDU Session Container QFI
// is decoded; a marker without one has no QFI; an unreadable extension chain
// still yields the marker (without a QFI) rather than an error; a G-PDU
// with the same broken chain is still an error. End Markers carry no payload.
func TestDecodeEndMarkerExtensions(t *testing.T) {
	withQFI := []byte{0x34, MsgTypeEndMarker, 0x00, 0x08, 0, 0, 0x02, 0x51,
		0, 0, 0, ExtHeaderTypePDUSessionContainer, 0x01, 0x00, 0x07, 0x00}
	badLen := []byte{0x34, MsgTypeEndMarker, 0x00, 0x08, 0, 0, 0x02, 0x51,
		0, 0, 0, ExtHeaderTypePDUSessionContainer, 0x05, 0x00, 0x07, 0x00}
	badGPDU := append([]byte(nil), badLen...)
	badGPDU[1] = MsgTypeGPDU
	tests := []struct {
		name    string
		pkt     []byte
		wantErr bool
		wantHas bool
		wantQFI uint8
	}{
		{name: "container", pkt: withQFI, wantHas: true, wantQFI: 7},
		{name: "no container", pkt: EncodeEndMarker(0x251)},
		{name: "bad extension length", pkt: badLen},
		{name: "g-pdu with bad extension length", pkt: badGPDU, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, err := DecodeGPDU(tc.pkt)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeGPDU: %v", err)
			}
			if g.MsgType != MsgTypeEndMarker || g.TEID != 0x251 {
				t.Errorf("decoded type %#x TEID %#x, want an End Marker on 0x251", g.MsgType, g.TEID)
			}
			if g.HasQFI != tc.wantHas || g.QFI != tc.wantQFI {
				t.Errorf("QFI = %v/%d, want %v/%d", g.HasQFI, g.QFI, tc.wantHas, tc.wantQFI)
			}
			if len(g.Payload) != 0 {
				t.Errorf("End Marker payload = %x, want none", g.Payload)
			}
		})
	}
}
