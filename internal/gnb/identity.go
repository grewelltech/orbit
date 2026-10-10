package gnb

import (
	"encoding/hex"
	"fmt"
)

// MaxTAC is the largest 24-bit tracking area code (TS 38.413 §9.3.3.10).
const MaxTAC = 0xFFFFFF

// ValidatePLMN checks a PLMN as the operator writes it: MCC is exactly 3
// decimal digits, MNC is 2 or 3. Both stay strings so a leading zero
// survives ("01" and "001" are different MNCs; an integer would lose that).
func ValidatePLMN(mcc, mnc string) error {
	if len(mcc) != 3 || !allDigits(mcc) {
		return fmt.Errorf("mcc %q: must be exactly 3 digits", mcc)
	}
	if (len(mnc) != 2 && len(mnc) != 3) || !allDigits(mnc) {
		return fmt.Errorf("mnc %q: must be 2 or 3 digits (keep a leading zero: 01 and 001 differ)", mnc)
	}
	return nil
}

// ValidateTAC checks a tracking area code: 1 to MaxTAC. Zero is rejected
// because it is what an unset field looks like, and a TAC the core does not
// serve reads like a core finding rather than a missing flag.
func ValidateTAC(tac uint32) error {
	if tac == 0 || tac > MaxTAC {
		return fmt.Errorf("tac %d: must be 1-%d (24 bits)", tac, MaxTAC)
	}
	return nil
}

// ValidateSNSSAI checks a slice: SST fits one octet (0-255) and SD is empty
// or exactly 6 hex digits (TS 38.413 §9.3.1.24).
func ValidateSNSSAI(sst uint32, sd string) error {
	if sst > 0xFF {
		return fmt.Errorf("sst %d: must be 0-255", sst)
	}
	if sd == "" {
		return nil
	}
	if b, err := hex.DecodeString(sd); err != nil || len(b) != 3 {
		return fmt.Errorf("sd %q: must be empty or exactly 6 hex digits (e.g. 010203)", sd)
	}
	return nil
}

// ValidateIdentity checks the network identity a gNB presents to the core:
// PLMN, TAC and every supported slice. It is the one rule the CLI, the API
// server and the scenario loader all apply, so each surface rejects the same
// input with the same message. Cell identity (ID, IDBits, Name) is checked
// at encode time instead.
func (c Config) ValidateIdentity() error {
	if err := ValidatePLMN(c.MCC, c.MNC); err != nil {
		return err
	}
	if err := ValidateTAC(c.TAC); err != nil {
		return err
	}
	if len(c.Slices) == 0 {
		return fmt.Errorf("at least one supported S-NSSAI is required")
	}
	for _, s := range c.Slices {
		if err := ValidateSNSSAI(uint32(s.SST), s.SD); err != nil {
			return err
		}
	}
	return nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}
