package gtpu

import (
	"encoding/binary"
	"fmt"
)

// GTP-U G-PDU encode/decode for the 5G N3 data path (TS 29.281 §5, TS
// 38.415). ORBIT stamps the QFI in a PDU Session Container extension header,
// which forces the E flag and therefore the 4-byte optional field — a
// 12-byte GTP-U header before the container, never 8 (DESIGN §2.2). Byte
// layout verified against gnbsim's field-used encoder (works against this
// same BESS-UPF) and the specs.
//
//	octet 1     flags 0x34  = version 1 (001) | PT 1 | E 1  (S,PN = 0)
//	octet 2     message type 0xFF (G-PDU)
//	octet 3-4   length = 4 (optional field) + 4 (container) + len(payload)
//	octet 5-8   TEID
//	octet 9-10  sequence number (0; present because E is set)
//	octet 11    N-PDU number (0)
//	octet 12    next extension header type 0x85 (PDU Session Container)
//	--- PDU Session Container extension header (4 octets) ---
//	octet 13    ext header length = 1 (unit: 4 octets)
//	octet 14    UL PDU SESSION INFORMATION: PDU type 1 → 0x10
//	octet 15    QFI (6 bits)
//	octet 16    next extension header type = 0 (none)
const (
	flagsGPDU     = 0x34 // version 1, PT=GTP, E set
	pduSessTypeUL = 0x10 // UL PDU SESSION INFORMATION, PDU type 1 in the high nibble
)

// EncodeGPDU builds a GTP-U G-PDU carrying an inner IP packet with its QFI.
func EncodeGPDU(teid uint32, qfi uint8, payload []byte) []byte {
	container := []byte{0x01, pduSessTypeUL, qfi & 0x3F, 0x00}
	// Length field counts everything after the 8 mandatory octets.
	length := len(container) + len(payload) + 4 // +4 optional (seq,npdu,next-ext)

	buf := make([]byte, 0, HeaderSizeN3+len(container)+len(payload))
	buf = append(buf, flagsGPDU, MsgTypeGPDU)
	buf = append(buf, byte(length>>8), byte(length))
	var teidb [4]byte
	binary.BigEndian.PutUint32(teidb[:], teid)
	buf = append(buf, teidb[:]...)
	buf = append(buf, 0x00, 0x00) // sequence number
	buf = append(buf, 0x00)       // N-PDU number
	buf = append(buf, ExtHeaderTypePDUSessionContainer)
	buf = append(buf, container...)
	buf = append(buf, payload...)
	return buf
}

// GPDU is a decoded G-PDU.
type GPDU struct {
	TEID    uint32
	MsgType uint8 // GTP-U message type (octet 2); MsgTypeGPDU for user data
	QFI     uint8
	HasQFI  bool
	Payload []byte
}

// DecodeGPDU parses a GTP-U packet, returning the TEID, the QFI from the PDU
// Session Container (if present), and the inner IP payload. It walks the
// extension-header chain so a container that is not first still decodes,
// though ORBIT (and the spec) place it first.
func DecodeGPDU(pkt []byte) (*GPDU, error) {
	if len(pkt) < HeaderSizeMandatory {
		return nil, fmt.Errorf("GTP-U packet too short: %d bytes", len(pkt))
	}
	flags := pkt[0]
	if flags&0xE0 != 0x20 {
		return nil, fmt.Errorf("not GTP version 1 (flags %#x)", flags)
	}
	msgType := pkt[1]
	length := int(binary.BigEndian.Uint16(pkt[2:4]))
	teid := binary.BigEndian.Uint32(pkt[4:8])
	if HeaderSizeMandatory+length > len(pkt) {
		return nil, fmt.Errorf("GTP-U length %d exceeds packet (%d bytes)", length, len(pkt))
	}
	out := &GPDU{TEID: teid, MsgType: msgType}
	if msgType != MsgTypeGPDU {
		// Echo/End Marker/etc. carry no user payload for our purposes.
		return out, nil
	}

	pos := HeaderSizeMandatory
	nextExt := byte(0)
	if flags&0x07 != 0 { // E, S, or PN set → 4-byte optional field present
		if pos+4 > len(pkt) {
			return nil, fmt.Errorf("truncated GTP-U optional header")
		}
		nextExt = pkt[pos+3]
		pos += 4
	}

	for nextExt != 0 {
		if pos+1 > len(pkt) {
			return nil, fmt.Errorf("truncated GTP-U extension header")
		}
		extLen := int(pkt[pos]) * 4
		if extLen == 0 || pos+extLen > len(pkt) {
			return nil, fmt.Errorf("bad GTP-U extension header length %d", extLen)
		}
		content := pkt[pos+1 : pos+extLen-1]
		thisType := nextExt
		nextExt = pkt[pos+extLen-1]
		if thisType == ExtHeaderTypePDUSessionContainer && len(content) >= 2 {
			out.QFI = content[1] & 0x3F
			out.HasQFI = true
		}
		pos += extLen
	}

	out.Payload = pkt[pos : HeaderSizeMandatory+length]
	return out, nil
}

// EncodeEndMarker builds a GTP-U End Marker (type 0xFE) for the given
// downlink tunnel, sent on the old path after a handover switch so the UPF
// does not reorder (TS 29.281 §7.3). Scaffolded here; exercised in Phase 3.
func EncodeEndMarker(teid uint32) []byte {
	buf := make([]byte, 0, HeaderSizeMandatory)
	buf = append(buf, 0x30, MsgTypeEndMarker) // version 1, PT, no optional fields
	buf = append(buf, 0x00, 0x00)             // length 0
	var teidb [4]byte
	binary.BigEndian.PutUint32(teidb[:], teid)
	buf = append(buf, teidb[:]...)
	return buf
}
