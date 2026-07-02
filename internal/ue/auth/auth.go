// Package auth implements the UE side of 5G-AKA authentication and the NAS
// key hierarchy (TS 33.501 §6.1.3.2 and Annex A).
//
// Reuse boundary (DESIGN §4, reuse-first): the cryptographic primitives are
// reused from free5gc/util — Milenage f1–f5 (util/milenage, verified against
// the 3GPP conformance test set) and the TS 33.501 Annex A KDF
// (util/ueauth). Those are standard, non-differentiating crypto; clean-
// rooming them would only add bug surface to the one layer that rejects you
// silently when wrong. What ORBIT builds is the UE-side *orchestration*:
// turning a network RAND/AUTN challenge into RES* and the KNASint/KNASenc
// pair the rest of the stack needs. This supersedes the design's original
// "BUILD ~400 LoC" note for Milenage/KDF now that the reuse surface is
// confirmed Apache-2.0.
package auth

import (
	"encoding/hex"
	"fmt"

	"github.com/free5gc/util/milenage"
	"github.com/free5gc/util/ueauth"
)

// Algorithm-type distinguishers for the algorithm-key derivation
// (TS 33.501 Annex A.8, Table A.8-1).
const (
	algTypeNASEnc = 0x01 // N-NAS-enc-alg
	algTypeNASInt = 0x02 // N-NAS-int-alg
)

// NAS algorithm identities (TS 33.501 §5.9, D.1). Phase-1a targets the
// ATB-01 core's offered set: ciphering NEA0, integrity NIA2.
const (
	NEA0 = 0x00 // null ciphering
	NIA2 = 0x02 // 128-NIA2 (AES-CMAC)
)

// Subscription is the UE's secret credentials plus identity, mirroring what
// the core's UDM provisions. Ki and OPc are long-term secrets; they enter
// via config and are redacted in logs (see internal/observability).
type Subscription struct {
	// SUPI in IMSI form, e.g. "208930100007500" (MCC+MNC+MSIN digits).
	SUPI string
	// Ki is the 128-bit subscriber key K (16 bytes).
	Ki []byte
	// OPc is the 128-bit operator variant key (16 bytes).
	OPc []byte
}

// Vector is the output of a successful UE-side 5G-AKA run: the response to
// return to the network plus the derived NAS keys.
type Vector struct {
	// ResStar is RES* (TS 33.501 Annex A.4), sent in Authentication
	// Response and checked by the AUSF against XRES*.
	ResStar []byte
	KAusf   []byte
	KSeaf   []byte
	KAmf    []byte
	// KNASint / KNASenc are the 128-bit NAS integrity and ciphering keys
	// selected for the algorithms in Security Mode Command.
	KNASint [16]byte
	KNASenc [16]byte
}

// MACFailure reports that the network's AUTN did not authenticate: the
// computed XMAC differs from the MAC in AUTN (TS 33.501 §6.1.3.2). The UE
// must answer with Authentication Failure (cause "MAC failure"), never
// proceed. Wraps the underlying milenage error.
type MACFailure struct{ err error }

func (e *MACFailure) Error() string { return "5G-AKA MAC failure: " + e.err.Error() }
func (e *MACFailure) Unwrap() error { return e.err }

// ServingNetworkName builds the SNN used as a KDF input for every 5G key
// (TS 24.501 §9.11.3.15, TS 33.501 §6.1.1.4): "5G:mnc<MNC>.mcc<MCC>.
// 3gppnetwork.org" with the MNC zero-padded to three digits.
func ServingNetworkName(mcc, mnc string) string {
	if len(mnc) == 2 {
		mnc = "0" + mnc
	}
	return fmt.Sprintf("5G:mnc%s.mcc%s.3gppnetwork.org", mnc, mcc)
}

// DeriveFromChallenge runs the UE side of 5G-AKA for one Authentication
// Request. It verifies the network via AUTN's MAC, derives CK/IK/RES, and
// produces RES* and the full key hierarchy down to the NAS keys for the
// given NAS algorithms. rand and autn are the 16-byte challenge values;
// snn is ServingNetworkName; intAlg/encAlg are the NIA/NEA identities.
//
// Note: sequence-number resynchronisation (AUTS) is out of scope for the
// happy-path keystone; an out-of-range SQN surfaces here as a MAC/param
// error and is handled when resync is implemented.
func (s Subscription) DeriveFromChallenge(rand, autn []byte, snn string, intAlg, encAlg uint8) (*Vector, error) {
	if len(s.Ki) != 16 || len(s.OPc) != 16 {
		return nil, fmt.Errorf("subscription %s: Ki and OPc must be 16 bytes (got %d/%d)", s.SUPI, len(s.Ki), len(s.OPc))
	}
	if len(rand) != 16 || len(autn) != 16 {
		return nil, fmt.Errorf("rand and autn must be 16 bytes (got %d/%d)", len(rand), len(autn))
	}

	// UE-side Milenage: verifies AUTN's MAC internally and returns CK/IK/RES.
	_, _, ik, ck, res, err := milenage.GenerateKeysWithAUTN(s.OPc, s.Ki, rand, autn)
	if err != nil {
		if _, ok := err.(*milenage.MACFailureError); ok {
			return nil, &MACFailure{err: err}
		}
		return nil, fmt.Errorf("milenage UE derivation: %w", err)
	}

	ckik := append(append([]byte{}, ck...), ik...)
	snb := []byte(snn)
	sqnXorAK := autn[:6] // SQN⊕AK is the concealed field of AUTN

	// RES* = KDF(CK‖IK, FC=6B, SNN, RAND, RES), least-significant 128 bits.
	resStarFull, err := ueauth.GetKDFValue(ckik, ueauth.FC_FOR_RES_STAR_XRES_STAR_DERIVATION,
		snb, ueauth.KDFLen(snb), rand, ueauth.KDFLen(rand), res, ueauth.KDFLen(res))
	if err != nil {
		return nil, fmt.Errorf("derive RES*: %w", err)
	}

	// KAUSF = KDF(CK‖IK, FC=6A, SNN, SQN⊕AK).
	kAusf, err := ueauth.GetKDFValue(ckik, ueauth.FC_FOR_KAUSF_DERIVATION,
		snb, ueauth.KDFLen(snb), sqnXorAK, ueauth.KDFLen(sqnXorAK))
	if err != nil {
		return nil, fmt.Errorf("derive KAUSF: %w", err)
	}

	// KSEAF = KDF(KAUSF, FC=6C, SNN).
	kSeaf, err := ueauth.GetKDFValue(kAusf, ueauth.FC_FOR_KSEAF_DERIVATION, snb, ueauth.KDFLen(snb))
	if err != nil {
		return nil, fmt.Errorf("derive KSEAF: %w", err)
	}

	// KAMF = KDF(KSEAF, FC=6D, SUPI, ABBA). ABBA defaults to 0x0000
	// (TS 33.501 §6.1.3.1, Annex A.7.1).
	supi := []byte(s.SUPI)
	abba := []byte{0x00, 0x00}
	kAmf, err := ueauth.GetKDFValue(kSeaf, ueauth.FC_FOR_KAMF_DERIVATION,
		supi, ueauth.KDFLen(supi), abba, ueauth.KDFLen(abba))
	if err != nil {
		return nil, fmt.Errorf("derive KAMF: %w", err)
	}

	kNasInt, err := deriveAlgKey(kAmf, algTypeNASInt, intAlg)
	if err != nil {
		return nil, fmt.Errorf("derive KNASint: %w", err)
	}
	kNasEnc, err := deriveAlgKey(kAmf, algTypeNASEnc, encAlg)
	if err != nil {
		return nil, fmt.Errorf("derive KNASenc: %w", err)
	}

	return &Vector{
		ResStar: resStarFull[16:],
		KAusf:   kAusf,
		KSeaf:   kSeaf,
		KAmf:    kAmf,
		KNASint: kNasInt,
		KNASenc: kNasEnc,
	}, nil
}

// deriveAlgKey computes a 128-bit NAS algorithm key: the least-significant
// 128 bits of KDF(KAMF, FC=69, algType, algID) (TS 33.501 Annex A.8).
func deriveAlgKey(kAmf []byte, algType, algID uint8) ([16]byte, error) {
	var out [16]byte
	full, err := ueauth.GetKDFValue(kAmf, ueauth.FC_FOR_ALGORITHM_KEY_DERIVATION,
		[]byte{algType}, ueauth.KDFLen([]byte{algType}),
		[]byte{algID}, ueauth.KDFLen([]byte{algID}))
	if err != nil {
		return out, err
	}
	copy(out[:], full[16:])
	return out, nil
}

// ParseHexKey decodes a 32-hex-digit (16-byte) key from config.
func ParseHexKey(name, s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%s: not valid hex: %w", name, err)
	}
	if len(b) != 16 {
		return nil, fmt.Errorf("%s: must be 16 bytes / 32 hex digits, got %d", name, len(b))
	}
	return b, nil
}
