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

// EchoRequestSeq returns the sequence number of a GTP-U Echo Request (type
// 1, TS 29.281 7.2.1), and false if pkt is not an Echo Request carrying one.
func EchoRequestSeq(pkt []byte) (uint16, bool) {
	if len(pkt) < HeaderSizeN3 || pkt[1] != MsgTypeEchoRequest || pkt[0]&0x02 == 0 {
		return 0, false
	}
	return binary.BigEndian.Uint16(pkt[8:10]), true
}

// recoveryIE is the Recovery information element (type 14, TS 29.281 8.2).
const recoveryIE = 14

// EncodeEchoResponse builds a GTP-U Echo Response (type 2, TS 29.281 7.2.2)
// with sequence number seq, which 4.3.1 requires to be the request's. The
// TEID is 0 and the message carries the mandatory Recovery IE with a Restart
// Counter of 0.
func EncodeEchoResponse(seq uint16) []byte {
	buf := make([]byte, HeaderSizeN3+2)
	buf[0] = 0x32 // version 1, PT, S
	buf[1] = MsgTypeEchoResponse
	binary.BigEndian.PutUint16(buf[2:4], 6) // the optional field and the Recovery IE
	binary.BigEndian.PutUint16(buf[8:10], seq)
	buf[HeaderSizeN3] = recoveryIE
	return buf
}
