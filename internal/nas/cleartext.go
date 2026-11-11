package nas

import (
	"fmt"

	f5nas "github.com/free5gc/nas"
)

// ieFormat is how an optional REGISTRATION REQUEST IE is laid out on the
// wire.
type ieFormat uint8

const (
	ieTLV  ieFormat = iota // IEI, one length octet, value
	ieTLVE                 // IEI, two length octets, value
	ieTV1                  // one octet: the IEI in bits 8 to 5, the value in bits 4 to 1
	ieTV7                  // IEI and a 6-octet value
)

// registrationIE describes one optional IE of the REGISTRATION REQUEST.
type registrationIE struct {
	format ieFormat
	// cleartext marks an IE a UE without a valid 5G NAS security context
	// may send in the initial message (TS 24.501 4.4.6).
	cleartext bool
}

// registrationIEs is every optional IE of the REGISTRATION REQUEST (TS
// 24.501 table 8.2.6.1.1, V19.8.0) by IEI, a type 1 IE under its octet with
// the value half cleared. It is the one list the cleartext scan and the
// cleartext form are built from.
var registrationIEs = map[uint8]registrationIE{
	0xC0: {format: ieTV1},                   // Non-current native NAS key set identifier
	0x10: {format: ieTLV},                   // 5GMM capability
	0x2E: {format: ieTLV, cleartext: true},  // UE security capability
	0x2F: {format: ieTLV},                   // Requested NSSAI
	0x52: {format: ieTV7},                   // Last visited registered TAI
	0x17: {format: ieTLV},                   // S1 UE network capability
	0x40: {format: ieTLV},                   // Uplink data status
	0x50: {format: ieTLV},                   // PDU session status
	0xB0: {format: ieTV1},                   // MICO indication
	0x2B: {format: ieTLV, cleartext: true},  // UE status
	0x77: {format: ieTLVE, cleartext: true}, // Additional GUTI
	0x25: {format: ieTLV},                   // Allowed PDU session status
	0x18: {format: ieTLV},                   // UE's usage setting
	0x51: {format: ieTLV},                   // Requested DRX parameters
	0x70: {format: ieTLVE, cleartext: true}, // EPS NAS message container
	0x74: {format: ieTLVE},                  // LADN indication
	0x80: {format: ieTV1},                   // Payload container type
	0x7B: {format: ieTLVE},                  // Payload container
	0x90: {format: ieTV1},                   // Network slicing indication
	0x53: {format: ieTLV},                   // 5GS update type
	0x41: {format: ieTLV},                   // Mobile station classmark 2
	0x42: {format: ieTLV},                   // Supported codecs
	0x71: {format: ieTLVE},                  // NAS message container
	0x60: {format: ieTLV},                   // EPS bearer context status
	0x6E: {format: ieTLV},                   // Requested extended DRX parameters
	0x6A: {format: ieTLV},                   // T3324 value
	0x67: {format: ieTLV},                   // UE radio capability ID
	0x35: {format: ieTLV},                   // Requested mapped NSSAI
	0x48: {format: ieTLV},                   // Additional information requested
	0x1A: {format: ieTLV},                   // Requested WUS assistance information
	0xA0: {format: ieTV1},                   // N5GC indication
	0x30: {format: ieTLV},                   // Requested NB-N1 mode DRX parameters
	0x29: {format: ieTLV},                   // UE request type
	0x28: {format: ieTLV},                   // Paging restriction
	0x72: {format: ieTLVE},                  // Service-level-AA container
	0x32: {format: ieTLV, cleartext: true},  // NID
	0x16: {format: ieTLV, cleartext: true},  // UE determined PLMN with disaster condition
	0x2A: {format: ieTLV},                   // Requested PEIPS assistance information
	0x3B: {format: ieTLV},                   // Requested T3512 value
	0x3C: {format: ieTLV},                   // Unavailability information
	0x3F: {format: ieTLV},                   // Non-3GPP path switching information
	0x56: {format: ieTLV},                   // AUN3 indication
	0x64: {format: ieTLV},                   // Requested LP-WUSPS assistance information
}

// ieKey is the registrationIEs key of the IE that starts with octet b.
func ieKey(b uint8) uint8 {
	if b&0x80 != 0 {
		return b & 0xF0
	}
	return b
}

// registrationOptionalIEs returns the offset of the first optional IE of a
// plain REGISTRATION REQUEST: after the EPD, the security header type, the
// message type, the 5GS registration type and ngKSI octet, and the 5GS
// mobile identity (LV-E).
func registrationOptionalIEs(regReq []byte) (int, error) {
	if len(regReq) < 6 || f5nas.GetSecurityHeaderType(regReq) != f5nas.SecurityHeaderTypePlainNas || regReq[2] != f5nas.MsgTypeRegistrationRequest {
		return 0, fmt.Errorf("not a plain Registration Request")
	}
	end := 6 + (int(regReq[4])<<8 | int(regReq[5]))
	if end > len(regReq) {
		return 0, fmt.Errorf("5GS mobile identity runs past the message")
	}
	return end, nil
}

// ieLen is the length on the wire of the IE of format f at the start of b.
func ieLen(f ieFormat, b []byte) (int, error) {
	n := 0
	switch f {
	case ieTV1:
		n = 1
	case ieTV7:
		n = 7
	case ieTLV:
		if len(b) >= 2 {
			n = 2 + int(b[1])
		}
	case ieTLVE:
		if len(b) >= 3 {
			n = 3 + (int(b[1])<<8 | int(b[2]))
		}
	}
	if n == 0 || n > len(b) {
		return 0, fmt.Errorf("IE 0x%02X runs past the message", ieKey(b[0]))
	}
	return n, nil
}

// NonCleartextRegistrationIE reports the IEI of the first optional IE of a
// plain REGISTRATION REQUEST that is not one of the cleartext IEs a UE
// without a valid 5G NAS security context may send (TS 24.501 4.4.6); ok is
// false when the message carries cleartext IEs only. A type 1 IE is
// reported by its octet with the value half cleared, and an IEI the
// message does not define counts as not cleartext.
func NonCleartextRegistrationIE(regReq []byte) (iei uint8, ok bool, err error) {
	at, err := registrationOptionalIEs(regReq)
	if err != nil {
		return 0, false, err
	}
	for rest := regReq[at:]; len(rest) > 0; {
		key := ieKey(rest[0])
		ie := registrationIEs[key]
		if !ie.cleartext {
			return key, true, nil
		}
		n, err := ieLen(ie.format, rest)
		if err != nil {
			return 0, false, err
		}
		rest = rest[n:]
	}
	return 0, false, nil
}

// CleartextRegistrationRequest reduces a complete plain REGISTRATION REQUEST
// to the form a UE without a valid 5G NAS security context sends as the
// initial NAS message (TS 24.501 4.4.6): the mandatory IEs and every
// optional IE NonCleartextRegistrationIE accepts, byte for byte and in
// order, with every other optional IE left out. An IEI the message does not
// define cannot be stepped over and is an error.
func CleartextRegistrationRequest(complete []byte) ([]byte, error) {
	at, err := registrationOptionalIEs(complete)
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), complete[:at]...)
	for rest := complete[at:]; len(rest) > 0; {
		key := ieKey(rest[0])
		ie, known := registrationIEs[key]
		if !known {
			return nil, fmt.Errorf("IE 0x%02X is not a Registration Request IE", key)
		}
		n, err := ieLen(ie.format, rest)
		if err != nil {
			return nil, err
		}
		if ie.cleartext {
			out = append(out, rest[:n]...)
		}
		rest = rest[n:]
	}
	return out, nil
}
