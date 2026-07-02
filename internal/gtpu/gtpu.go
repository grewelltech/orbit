// Package gtpu holds ORBIT's GTP-U (N3) wire constants and header rules.
//
// The Stage-1 userspace encapsulation itself lands in Phase 1b on top of
// github.com/wmnsk/go-gtp (license verified MIT on 2026-07-02); the go-gtp
// dependency is added there, when code first uses it, so go.mod carries no
// dead pins. This package exists now to fix the wire facts the design
// depends on, with citations, and to give Phase 1b a seam to build behind.
package gtpu

// Port is the GTP-U UDP port on N3 (TS 29.281 §4.4.2.3).
const Port = 2152

// Message types (TS 29.281 §6.1, Table 6.1-1).
const (
	MsgTypeEchoRequest  = 1
	MsgTypeEchoResponse = 2
	MsgTypeErrorInd     = 26
	MsgTypeEndMarker    = 254 // 0xFE — sent on the old tunnel after handover path switch (TS 29.281 §7.3)
	MsgTypeGPDU         = 255 // 0xFF — user data
)

// ExtHeaderTypePDUSessionContainer is the extension-header type carrying the
// PDU Session Container with the QFI (TS 38.415; type value per TS 29.281
// §5.2.1). 5G requires it on N3 G-PDUs and it must be the FIRST extension
// header (TS 29.281 §5.2.1, from v15.3.0).
const ExtHeaderTypePDUSessionContainer = 0x85

// Header sizes. Carrying any extension header requires the E flag, and
// setting E (or S or PN) forces the whole 4-byte optional field
// (sequence number + N-PDU number + next-extension-header type) to be
// present (TS 29.281 §5.1). A 5G N3 G-PDU header is therefore 12 bytes
// before the PDU Session Container — never 8.
const (
	HeaderSizeMandatory = 8  // flags, type, length, TEID
	HeaderSizeN3        = 12 // + seq/N-PDU/next-ext forced by the E flag
)
