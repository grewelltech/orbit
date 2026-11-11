package ue

import (
	"fmt"
	"strings"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"
)

// This file covers the UE's equipment identity (PEI) and the identification
// procedure (TS 24.501 §5.4.3): the IMEISV the Security Mode Complete
// carries on request, and the Identity Response.

// Types of identity of the 5GS mobile identity IE (TS 24.501 §9.11.3.4).
const (
	IdentityTypeNone    = nasMessage.MobileIdentity5GSTypeNoIdentity
	IdentityTypeSUCI    = nasMessage.MobileIdentity5GSTypeSuci
	IdentityTypeGUTI    = nasMessage.MobileIdentity5GSType5gGuti
	IdentityTypeIMEI    = nasMessage.MobileIdentity5GSTypeImei
	IdentityTypeSTMSI   = nasMessage.MobileIdentity5GSType5gSTmsi
	IdentityTypeIMEISV  = nasMessage.MobileIdentity5GSTypeImeisv
	identityTypeMask    = 0x07
	imeisvDigits        = 16
	imeiDigits          = 15
	gutiIdentityOctets  = 11 // a 5G-GUTI's 5GS mobile identity value
	gutiSTMSIOffset     = 5  // AMF Set ID, AMF Pointer and 5G-TMSI follow the PLMN and AMF Region ID
	stmsiIdentityHeader = 0xF0 | IdentityTypeSTMSI
)

// defaultIMEISVPrefix starts every derived IMEISV: a Type Allocation Code
// under reporting body identifier 00, which no production equipment holds.
const defaultIMEISVPrefix = "0010"

// DefaultIMEISV derives a UE's IMEISV from its SUPI digits, so every UE of
// a run has its own and keeps it from run to run:
//
//	"0010" | the last 10 SUPI digits, left-padded with zeros | "00"
//
// which reads as an 8-digit TAC ("0010" and four SUPI digits), a 6-digit
// serial number (the last six SUPI digits) and software version 00. Two
// SUPIs of one PLMN differ in their MSIN, at most 10 digits, so their
// IMEISVs differ.
func DefaultIMEISV(supi string) string {
	const n = 10
	tail := supi
	if len(tail) > n {
		tail = tail[len(tail)-n:]
	}
	return defaultIMEISVPrefix + strings.Repeat("0", n-len(tail)) + tail + "00"
}

// EquipmentIMEISV is the UE's IMEISV, 16 decimal digits: id.IMEISV when
// set, else DefaultIMEISV of the SUPI.
func (id Identity) EquipmentIMEISV() (string, error) {
	v := id.IMEISV
	if v == "" {
		if !isDigits(id.SUPI) || id.SUPI == "" {
			return "", fmt.Errorf("no IMEISV: SUPI %q is not all digits", id.SUPI)
		}
		v = DefaultIMEISV(id.SUPI)
	}
	if len(v) != imeisvDigits || !isDigits(v) {
		return "", fmt.Errorf("IMEISV %q must be %d digits", v, imeisvDigits)
	}
	return v, nil
}

// EquipmentIMEI is the UE's IMEI, 15 decimal digits: the IMEISV's TAC and
// serial number followed by their Luhn check digit.
func (id Identity) EquipmentIMEI() (string, error) {
	sv, err := id.EquipmentIMEISV()
	if err != nil {
		return "", err
	}
	body := sv[:imeiDigits-1]
	return body + string(rune('0'+luhnCheckDigit(body))), nil
}

// luhnCheckDigit is the digit that makes digits followed by it pass the
// Luhn check.
func luhnCheckDigit(digits string) int {
	sum := 0
	for i := 0; i < len(digits); i++ {
		d := int(digits[len(digits)-1-i] - '0')
		if i%2 == 0 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return (10 - sum%10) % 10
}

// encodeIdentityDigits codes an IMEI or IMEISV as a 5GS mobile identity
// value (TS 24.501 §9.11.3.4): the first digit, the odd/even indication and
// the type of identity in the first octet, then two BCD digits an octet,
// low nibble first, with a "1111" end mark after an even number of digits.
func encodeIdentityDigits(digits string, idType uint8) []byte {
	out := []byte{(digits[0]-'0')<<4 | idType}
	if len(digits)%2 == 1 {
		out[0] |= 0x08
	}
	for i := 1; i < len(digits); i += 2 {
		hi := byte(0x0F)
		if i+1 < len(digits) {
			hi = digits[i+1] - '0'
		}
		out = append(out, hi<<4|(digits[i]-'0'))
	}
	return out
}

// MobileIdentity builds the 5GS mobile identity value answering an Identity
// Request for idType (TS 24.501 §5.4.3.3): the null-scheme SUCI, the IMEISV
// or the IMEI, and, from guti (the assigned 5G-GUTI's mobile identity
// value, nil when none is held), the 5G-GUTI or the 5G-S-TMSI. An identity
// the UE cannot give (no 5G-GUTI held, an EUI-64, a MAC address) is coded
// "No identity" (§5.4.3.5 b).
func (id Identity) MobileIdentity(idType uint8, guti []byte) ([]byte, error) {
	switch idType & identityTypeMask {
	case IdentityTypeSUCI:
		return id.EncodeNullSUCI()
	case IdentityTypeIMEISV:
		v, err := id.EquipmentIMEISV()
		if err != nil {
			return nil, err
		}
		return encodeIdentityDigits(v, IdentityTypeIMEISV), nil
	case IdentityTypeIMEI:
		v, err := id.EquipmentIMEI()
		if err != nil {
			return nil, err
		}
		return encodeIdentityDigits(v, IdentityTypeIMEI), nil
	case IdentityTypeGUTI:
		if len(guti) == gutiIdentityOctets {
			return append([]byte(nil), guti...), nil
		}
	case IdentityTypeSTMSI:
		if len(guti) == gutiIdentityOctets {
			return append([]byte{stmsiIdentityHeader}, guti[gutiSTMSIOffset:]...), nil
		}
	}
	return []byte{IdentityTypeNone}, nil
}

// ParseIdentityRequest extracts the requested type of identity from a
// decoded Identity Request (TS 24.501 §8.2.21).
func ParseIdentityRequest(m *f5nas.Message) (uint8, error) {
	if m.GmmMessage == nil || m.IdentityRequest == nil {
		return 0, fmt.Errorf("not an Identity Request")
	}
	return m.IdentityRequest.GetTypeOfIdentity(), nil
}

// BuildIdentityResponse builds the Identity Response carrying
// mobileIdentity, a 5GS mobile identity value (TS 24.501 §8.2.22). Returns
// the plain message: the caller wraps it once a security context exists.
func BuildIdentityResponse(mobileIdentity []byte) (*f5nas.Message, error) {
	if len(mobileIdentity) == 0 {
		return nil, fmt.Errorf("empty mobile identity")
	}
	m, gmm := newGmm(f5nas.MsgTypeIdentityResponse)
	r := nasMessage.NewIdentityResponse(f5nas.MsgTypeIdentityResponse)
	setGmmHeader(&r.ExtendedProtocolDiscriminator, &r.SpareHalfOctetAndSecurityHeaderType,
		&r.IdentityResponseMessageIdentity, f5nas.MsgTypeIdentityResponse)
	r.SetLen(uint16(len(mobileIdentity)))
	r.Buffer = append([]byte(nil), mobileIdentity...)
	gmm.IdentityResponse = r
	return m, nil
}

// SecurityModeCompleteOption adjusts the Security Mode Complete
// BuildSecurityModeComplete builds.
type SecurityModeCompleteOption func(*nasMessage.SecurityModeComplete) error

// WithIMEISV adds the IMEISV IE, which the UE includes when the Security
// Mode Command requested the IMEISV (TS 24.501 §5.4.2.3). imeisv is 16
// decimal digits.
func WithIMEISV(imeisv string) SecurityModeCompleteOption {
	return func(c *nasMessage.SecurityModeComplete) error {
		if len(imeisv) != imeisvDigits || !isDigits(imeisv) {
			return fmt.Errorf("IMEISV %q must be %d digits", imeisv, imeisvDigits)
		}
		v := encodeIdentityDigits(imeisv, IdentityTypeIMEISV)
		ie := nasType.NewIMEISV(nasMessage.SecurityModeCompleteIMEISVType)
		ie.SetLen(uint16(len(v)))
		copy(ie.Octet[:], v)
		c.IMEISV = ie
		return nil
	}
}
