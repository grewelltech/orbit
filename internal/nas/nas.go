// Package nas adapts the free5gc NAS codec (github.com/free5gc/nas v1.2.3,
// Apache-2.0) for ORBIT. Like internal/ngap, only the wire boundary lives
// here; NAS message structs are imported from the library directly by the
// UE state machines (Phase 1a). The adapter exists so the conformance
// decode path can swap to the omec-project fork per D-11.
//
// Phase-0 scope: plain (unprotected) decode/encode only. Security-protected
// NAS (integrity + ciphering after Security Mode) arrives with the Phase-1a
// 5G-AKA work and will extend this package, reusing free5gc/nas/security.
package nas

import (
	"fmt"

	f5nas "github.com/free5gc/nas"
)

// DecodePlain parses an unprotected NAS-5GS message (plain NAS, no security
// header). Registration Request and everything before Security Mode Complete
// travels in this form when NEA0/no protection is in effect (TS 24.501 §9.1.1).
func DecodePlain(b []byte) (*f5nas.Message, error) {
	m := f5nas.NewMessage()
	if err := m.PlainNasDecode(&b); err != nil {
		return nil, fmt.Errorf("nas plain decode (%d bytes): %w", len(b), err)
	}
	return m, nil
}

// EncodePlain serializes a plain NAS-5GS message.
func EncodePlain(m *f5nas.Message) ([]byte, error) {
	b, err := m.PlainNasEncode()
	if err != nil {
		return nil, fmt.Errorf("nas plain encode: %w", err)
	}
	return b, nil
}
