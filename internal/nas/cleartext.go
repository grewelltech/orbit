package nas

import (
	"fmt"

	f5nas "github.com/free5gc/nas"
)

// Information element identifiers of the REGISTRATION REQUEST's optional
// cleartext IEs (TS 24.501 4.4.6, table 8.2.6.1.1). All are full-octet
// IEIs; the two with a two-octet length are TLV-E.
const (
	ieiUESecurityCapability   = 0x2E
	ieiAdditionalGUTI         = 0x77
	ieiUEStatus               = 0x2B
	ieiEPSNASMessageContainer = 0x70
	ieiNID                    = 0x32
	ieiDisasterPLMN           = 0x16
)

// NonCleartextRegistrationIE reports the IEI of the first optional IE of a
// plain REGISTRATION REQUEST that is not one of the cleartext IEs a UE
// without a valid 5G NAS security context may send (TS 24.501 4.4.6); ok is
// false when the message carries cleartext IEs only. A type 1 IE is
// reported by its octet with the value half cleared.
func NonCleartextRegistrationIE(regReq []byte) (iei uint8, ok bool, err error) {
	// EPD, security header type, message type, 5GS registration type and
	// ngKSI, then the 5GS mobile identity (LV-E).
	if len(regReq) < 6 || f5nas.GetSecurityHeaderType(regReq) != f5nas.SecurityHeaderTypePlainNas || regReq[2] != f5nas.MsgTypeRegistrationRequest {
		return 0, false, fmt.Errorf("not a plain Registration Request")
	}
	rest := regReq[4:]
	n := 2 + (int(rest[0])<<8 | int(rest[1]))
	if n > len(rest) {
		return 0, false, fmt.Errorf("5GS mobile identity runs past the message")
	}
	rest = rest[n:]
	for len(rest) > 0 {
		var hdr int
		switch rest[0] {
		case ieiUESecurityCapability, ieiUEStatus, ieiNID, ieiDisasterPLMN:
			hdr = 2
			if len(rest) >= hdr {
				n = int(rest[1])
			}
		case ieiAdditionalGUTI, ieiEPSNASMessageContainer:
			hdr = 3
			if len(rest) >= hdr {
				n = int(rest[1])<<8 | int(rest[2])
			}
		default:
			if rest[0]&0x80 != 0 {
				return rest[0] & 0xF0, true, nil
			}
			return rest[0], true, nil
		}
		if len(rest) < hdr || hdr+n > len(rest) {
			return 0, false, fmt.Errorf("IE 0x%02X runs past the message", rest[0])
		}
		rest = rest[hdr+n:]
	}
	return 0, false, nil
}
