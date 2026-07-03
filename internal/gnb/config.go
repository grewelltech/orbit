// Package gnb implements ORBIT's gNB role. Phase-0 scope: configuration and
// the NG Setup procedure (TS 38.413 §8.7.1) — the "hello, I'm a base
// station" exchange that registers the gNB with the AMF. The full NGAP
// procedure FSM (Initial UE Message, Initial Context Setup, PDU Session
// Resource Setup, handover) grows here from Phase 1a onward.
package gnb

import (
	"encoding/hex"
	"fmt"
)

// SNSSAI is one supported network slice: a 1-octet Slice/Service Type and an
// optional 3-octet Slice Differentiator (TS 38.413 §9.3.1.24).
type SNSSAI struct {
	SST uint8
	SD  string // 6 hex digits (e.g. "010203"); empty means no SD
}

func (s SNSSAI) sdBytes() ([]byte, error) {
	if s.SD == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(s.SD)
	if err != nil || len(b) != 3 {
		return nil, fmt.Errorf("snssai sd %q: must be 6 hex digits", s.SD)
	}
	return b, nil
}

// Config identifies one simulated gNB toward the AMF.
type Config struct {
	// ID is the gNB identifier, carried as a BIT STRING of IDBits bits
	// inside the Global RAN Node ID (TS 38.413 §9.3.1.6: 22–32 bits).
	ID     uint32
	IDBits int // 0 defaults to 24

	// Name is the human-readable RANNodeName (optional IE, criticality
	// ignore). The ATB-01 core is shared, so give ORBIT gNBs distinct
	// identities to avoid collisions with other tenants.
	Name string

	MCC string // 3 digits
	MNC string // 2 or 3 digits

	// TAC is the 24-bit tracking area code (TS 38.413 §9.3.3.10, 3 octets).
	TAC uint32

	// Slices are broadcast in the Supported TA List; must cover what the
	// core expects for the PLMN (ATB-01: SST 1, SD 010203).
	Slices []SNSSAI
}

func (c *Config) validate() error {
	bits := c.IDBits
	if bits == 0 {
		bits = 24
	}
	if bits < 22 || bits > 32 {
		return fmt.Errorf("gnb id bits %d: TS 38.413 §9.3.1.6 allows 22-32", bits)
	}
	if bits < 32 && c.ID >= 1<<bits {
		return fmt.Errorf("gnb id %#x does not fit in %d bits", c.ID, bits)
	}
	if c.TAC > 0xFFFFFF {
		return fmt.Errorf("tac %#x exceeds 24 bits", c.TAC)
	}
	if len(c.Slices) == 0 {
		return fmt.Errorf("at least one supported S-NSSAI is required")
	}
	for _, s := range c.Slices {
		if _, err := s.sdBytes(); err != nil {
			return err
		}
	}
	return nil
}
