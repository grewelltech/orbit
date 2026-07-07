package gtpu

import "encoding/binary"

// MessageType returns the GTP-U message type (octet 2 of the header,
// TS 29.281 §5.1) of a received packet, and false if it is too short to have a
// header. Used to distinguish an Error Indication (26) from a G-PDU (255).
func MessageType(pkt []byte) (uint8, bool) {
	if len(pkt) < 2 {
		return 0, false
	}
	return pkt[1], true
}

// ErrorIndicationTEID extracts the Tunnel Endpoint Identifier Data I (IE type
// 16) from a GTP-U Error Indication (TS 29.281 §7.3.1). Per §7.3.1 the TEID
// Data I echoes the TEID of the G-PDU that triggered the error, so this
// confirms an Error Indication belongs to a specific probe. Best-effort: it
// reads the first IE after the header (TEID Data I is the mandatory first IE)
// and returns false if the packet is not a parseable Error Indication.
func ErrorIndicationTEID(pkt []byte) (uint32, bool) {
	if len(pkt) < 2 || pkt[1] != MsgTypeErrorInd {
		return 0, false
	}
	off := HeaderSizeMandatory // flags, type, length, TEID
	if pkt[0]&0x07 != 0 {      // E/S/PN flag set → the 4-byte optional field is present
		off += 4
	}
	// TEID Data I is IE type 16, TV format: 1 type octet + 4 TEID octets.
	if off+5 <= len(pkt) && pkt[off] == 16 {
		return binary.BigEndian.Uint32(pkt[off+1 : off+5]), true
	}
	return 0, false
}
