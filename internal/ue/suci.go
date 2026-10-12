// Package ue implements ORBIT's UE role: identity, the NAS-MM/5GSM state
// machines, and the per-UE data-path handle. This file covers identity —
// SUPI parsing and the UE-side null-scheme SUCI encoder.
//
// SUCI is the concealed subscriber identity the UE sends instead of its
// permanent SUPI/IMSI (TS 33.501 §6.12, TS 23.003 §2.2B). free5gc ships
// only the network-side decoder, so the encoder is ours (DESIGN §4).
// Phase-1a implements scheme 0 (null) only — the ATB-01 core accepts it
// (D-8); ciphered Profile A/B is a Phase-3 differentiator (§5f).
package ue

import (
	"fmt"
	"strings"

	"github.com/bgrewell/orbit/internal/ngap"
)

// Identity codes in the first octet of a 5GS mobile identity
// (TS 24.501 §9.11.3.4).
const (
	idTypeSUCI      = 0x01 // type of identity = SUCI (bits 1-3)
	supiFormatIMSI  = 0x00 // SUPI format = IMSI  (bits 5-7)
	protSchemeNull  = 0x00 // null protection scheme (TS 24.501 Table 9.11.3.4.1)
	publicKeyIDNull = 0x00 // home network public key id, 0 for null scheme
)

// Identity holds a UE's permanent identity, split into its parts.
type Identity struct {
	SUPI string // "208930100007500" (no "imsi-" prefix)
	MCC  string // "208"
	MNC  string // "93"
	MSIN string // "0100007500" — the SUPI digits after MCC+MNC
	// RoutingIndicator selects a UDM/AUSF instance (TS 23.003 §2.10);
	// 1-4 decimal digits. "0" is the common default and what the ATB-01
	// core accepts.
	RoutingIndicator string
}

// SUPI length bounds per TS 23.003 §2.2: an IMSI is MCC (3) + MNC (2 or 3)
// + MSIN, at most 15 digits; short test IMSIs down to 5 digits are legal.
const (
	minSUPIDigits = 5
	maxSUPIDigits = 15
)

// supiNAIPrefix is the NAI spelling of an IMSI-based SUPI ("imsi-<digits>",
// TS 23.003 §28.7.2); lower-case only, as the spec writes it.
const supiNAIPrefix = "imsi-"

// NormalizeSUPI returns the digits-only SUPI for either accepted spelling:
// the bare IMSI ("208930100007500") or the NAI form ("imsi-208930100007500").
// Anything else (empty, another or upper-case prefix, non-digits, fewer than
// 5 or more than 15 digits) is an error naming the two accepted forms.
//
// It is the one place a SUPI is normalised: every producer of an
// auth.Subscription or a session key calls it, so SUCI encoding, the KAMF
// derivation (TS 33.501 A.7 takes the IMSI digits) and lookups all see the
// same digits whichever form the operator typed.
func NormalizeSUPI(s string) (string, error) {
	digits := strings.TrimPrefix(s, supiNAIPrefix)
	if digits == "" || !isDigits(digits) || len(digits) < minSUPIDigits || len(digits) > maxSUPIDigits {
		return "", fmt.Errorf("invalid SUPI %q: want 5-15 digits or imsi-<digits>", s)
	}
	return digits, nil
}

// ParseIdentity splits a SUPI into MCC/MNC/MSIN. It accepts either the bare
// IMSI ("208930100007500") or the NAI form ("imsi-208930100007500"), via
// NormalizeSUPI. mnc must be given explicitly because MNC length (2 or 3) is
// not recoverable from the digit string alone.
func ParseIdentity(supi, mcc, mnc, routingIndicator string) (Identity, error) {
	supi, err := NormalizeSUPI(supi)
	if err != nil {
		return Identity{}, err
	}
	if len(mcc) != 3 {
		return Identity{}, fmt.Errorf("mcc %q must be 3 digits", mcc)
	}
	if len(mnc) != 2 && len(mnc) != 3 {
		return Identity{}, fmt.Errorf("mnc %q must be 2 or 3 digits", mnc)
	}
	prefix := mcc + mnc
	if !strings.HasPrefix(supi, prefix) {
		return Identity{}, fmt.Errorf("supi %q does not start with mcc+mnc %q", supi, prefix)
	}
	if !isDigits(supi) {
		return Identity{}, fmt.Errorf("supi %q is not all digits", supi)
	}
	rid := routingIndicator
	if rid == "" {
		rid = "0"
	}
	if len(rid) < 1 || len(rid) > 4 || !isDigits(rid) {
		return Identity{}, fmt.Errorf("routing indicator %q must be 1-4 digits", rid)
	}
	return Identity{
		SUPI:             supi,
		MCC:              mcc,
		MNC:              mnc,
		MSIN:             supi[len(prefix):],
		RoutingIndicator: rid,
	}, nil
}

// EncodeNullSUCI builds the 5GS mobile identity value carrying this UE's
// SUCI under the null scheme (TS 24.501 §9.11.3.4). Layout:
//
//	octet 1      : SUPI format (IMSI) | type (SUCI)          = 0x01
//	octet 2-4    : PLMN (packed BCD, TS 23.003)
//	octet 5-6    : routing indicator (BCD, 0xF-filled)
//	octet 7      : protection scheme id (0x00 null)
//	octet 8      : home network public key id (0x00)
//	octet 9..    : scheme output = MSIN in swapped-nibble BCD
//
// Byte-verified against gnbsim's field-used encoder (works against this same
// SD-Core) and round-tripped through free5gc's network-side decoder in tests.
func (id Identity) EncodeNullSUCI() ([]byte, error) {
	plmn, err := ngap.EncodePLMN(id.MCC, id.MNC)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 13)
	out = append(out, supiFormatIMSI<<4|idTypeSUCI)
	out = append(out, plmn[:]...)
	out = append(out, encodeRoutingIndicator(id.RoutingIndicator)...)
	out = append(out, protSchemeNull, publicKeyIDNull)
	out = append(out, encodeBCDSwapped(id.MSIN)...)
	return out, nil
}

// encodeRoutingIndicator packs 1-4 decimal digits into 2 octets, first digit
// in the low nibble of octet 1, unused nibbles set to 0xF (TS 24.501
// §9.11.3.4). "0" → {0xF0, 0xFF}.
func encodeRoutingIndicator(rid string) []byte {
	n := [4]byte{0xF, 0xF, 0xF, 0xF}
	for i := 0; i < len(rid) && i < 4; i++ {
		n[i] = rid[i] - '0'
	}
	return []byte{n[1]<<4 | n[0], n[3]<<4 | n[2]}
}

// encodeBCDSwapped encodes decimal digits in telephony BCD: digit 1 in the
// low nibble, digit 2 in the high nibble, an odd trailing digit padded with
// 0xF (TS 24.008 §10.5.1.4).
func encodeBCDSwapped(digits string) []byte {
	out := make([]byte, (len(digits)+1)/2)
	for i := 0; i < len(digits); i += 2 {
		lo := digits[i] - '0'
		hi := byte(0xF)
		if i+1 < len(digits) {
			hi = digits[i+1] - '0'
		}
		out[i/2] = hi<<4 | lo
	}
	return out
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}
