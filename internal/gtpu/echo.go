package gtpu

import "encoding/binary"

// EncodeEchoRequest builds a GTP-U Echo Request (type 1, TS 29.281 7.2.1)
// with sequence number seq. Echo messages carry a sequence number, so the S
// flag is set and the 4-byte optional field is present (5.1); the TEID is 0
// and the message has no information elements.
func EncodeEchoRequest(seq uint16) []byte {
	buf := make([]byte, HeaderSizeN3)
	buf[0] = 0x32 // version 1, PT, S
	buf[1] = MsgTypeEchoRequest
	binary.BigEndian.PutUint16(buf[2:4], 4) // the optional field; TEID excluded
	// TEID (4..8) is 0.
	binary.BigEndian.PutUint16(buf[8:10], seq)
	// N-PDU number and next extension header type (10, 11) are 0.
	return buf
}

// EchoResponseSeq returns the sequence number of a GTP-U Echo Response (type
// 2, TS 29.281 7.2.2), and false if pkt is not an Echo Response carrying one.
func EchoResponseSeq(pkt []byte) (uint16, bool) {
	if len(pkt) < HeaderSizeN3 || pkt[1] != MsgTypeEchoResponse || pkt[0]&0x02 == 0 {
		return 0, false
	}
	return binary.BigEndian.Uint16(pkt[8:10]), true
}
