// Package ngap adapts the free5gc NGAP codec (github.com/free5gc/ngap
// v1.1.3, Apache-2.0) for ORBIT. Wire encode/decode goes through this
// package so the conformance-decode path can later swap to the
// omec-project fork per discovery spike D-11 without touching callers.
//
// The adapter deliberately does not re-export the ngapType message structs —
// procedure code imports github.com/free5gc/ngap/ngapType directly for IE
// population. What is isolated here is the wire boundary (Encoder/Decoder)
// and the small encoding helpers whose correctness is grounded against
// captures rather than the type system.
package ngap

import (
	"fmt"

	f5ngap "github.com/free5gc/ngap"
	"github.com/free5gc/ngap/ngapType"
)

// Encode serializes an NGAP PDU to aligned PER bytes (TS 38.413 uses APER).
func Encode(pdu ngapType.NGAPPDU) ([]byte, error) {
	b, err := f5ngap.Encoder(pdu)
	if err != nil {
		return nil, fmt.Errorf("ngap encode: %w", err)
	}
	return b, nil
}

// Decode parses aligned-PER bytes into an NGAP PDU.
func Decode(b []byte) (*ngapType.NGAPPDU, error) {
	pdu, err := f5ngap.Decoder(b)
	if err != nil {
		return nil, fmt.Errorf("ngap decode (%d bytes): %w", len(b), err)
	}
	return pdu, nil
}

// EncodePLMN packs an MCC/MNC digit pair into the 3-octet BCD PLMN identity
// used across NGAP and NAS (TS 38.413 §9.3.3.5 → TS 23.003 §12.1):
//
//	octet 1: MCC digit 2 | MCC digit 1
//	octet 2: MNC digit 3 (0xF filler for 2-digit MNC) | MCC digit 3
//	octet 3: MNC digit 2 | MNC digit 1
//
// Verified against gnbsim's field-used bytes for PLMN 208/93: 02 f8 39
// (omec-project/gnbsim util/ngapTestpacket/build.go).
//
// Warning: for 3-digit MNCs this follows the TS 24.008 §10.5.1.3 layout
// (octet 2 high nibble = MNC digit 3). free5gc's ngapConvert.PlmnIdToNgap
// instead places MNC digit 1 there — the two disagree for 3-digit MNCs and
// only the 2-digit case (ATB-01's 208/93) is capture-verified. Ground
// against a live capture before targeting any 3-digit-MNC core.
func EncodePLMN(mcc, mnc string) ([3]byte, error) {
	var out [3]byte
	if len(mcc) != 3 {
		return out, fmt.Errorf("mcc %q: must be 3 digits", mcc)
	}
	if len(mnc) != 2 && len(mnc) != 3 {
		return out, fmt.Errorf("mnc %q: must be 2 or 3 digits", mnc)
	}
	d := func(c byte) (byte, error) {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit %q in PLMN", c)
		}
		return c - '0', nil
	}
	var digits [6]byte
	for i := 0; i < 3; i++ {
		v, err := d(mcc[i])
		if err != nil {
			return out, err
		}
		digits[i] = v
	}
	mnc3 := byte(0xF)
	if len(mnc) == 3 {
		v, err := d(mnc[2])
		if err != nil {
			return out, err
		}
		mnc3 = v
	}
	for i := 0; i < 2; i++ {
		v, err := d(mnc[i])
		if err != nil {
			return out, err
		}
		digits[3+i] = v
	}
	out[0] = digits[1]<<4 | digits[0]
	out[1] = mnc3<<4 | digits[2]
	out[2] = digits[4]<<4 | digits[3]
	return out, nil
}

// DecodePLMN unpacks the 3-octet BCD PLMN identity back to MCC/MNC strings.
func DecodePLMN(b [3]byte) (mcc, mnc string) {
	dig := func(v byte) byte { return v + '0' }
	mcc = string([]byte{dig(b[0] & 0xF), dig(b[0] >> 4), dig(b[1] & 0xF)})
	mnc = string([]byte{dig(b[2] & 0xF), dig(b[2] >> 4)})
	if b[1]>>4 != 0xF {
		mnc += string([]byte{dig(b[1] >> 4)})
	}
	return mcc, mnc
}
