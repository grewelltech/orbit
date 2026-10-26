package engine

import (
	"errors"
	"fmt"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// ErrNoSecurityContext is the decode error of a protected downlink that
// arrives before any Security Mode Command installed a context. AttachProbe.OnNAS
// receives it with the PDU's type-only read.
var ErrNoSecurityContext = errors.New("received protected NAS before security context established")

// ErrUnsupportedAlgorithms is the error Attach returns when a Security Mode
// Command selects NIA0 or an algorithm the UE did not advertise. It ends
// the attach even with AttachProbe.OnNAS set, since no context can follow.
var ErrUnsupportedAlgorithms = errors.New("AMF selected NAS algorithms the UE does not support")

// securityModeContext handles a downlink with the "integrity protected with
// new 5G NAS security context" header, which only a Security Mode Command
// carries (TS 24.501 4.4.6). The command is not ciphered, so its plain
// message, after the 7-octet security header, is read first for the
// selected algorithms. Both must be ones the UE advertised, and NIA0 is
// refused whatever was advertised (the UE never registers for emergency).
// KNASint and KNASenc are then derived from KAMF for that pair (TS 33.501
// Annex A.8), and the MAC is verified with the new context, which is
// installed only then. This holds with the Security Mode Complete withheld
// too (AttachProbe): only the reply is withheld.
func (s *attachState) securityModeContext(raw []byte) (*f5nas.Message, error) {
	if len(raw) < 8 {
		return nil, fmt.Errorf("security mode command too short: %d bytes", len(raw))
	}
	m, err := nas.DecodePlain(raw[7:])
	if err != nil {
		return nil, fmt.Errorf("decode Security Mode Command: %w", err)
	}
	sel, err := ue.ParseSecurityModeCommand(m)
	if err != nil {
		return nil, err
	}
	if sel.Integrity == nas.NIA0 || !advertises(s.caps, sel.Integrity, sel.Ciphering) {
		return nil, fmt.Errorf("%w: 128-NIA%d and NEA%d selected, UE advertised %x", ErrUnsupportedAlgorithms, sel.Integrity, sel.Ciphering, s.caps.Buffer)
	}
	kInt, kEnc, err := auth.DeriveNASKeys(s.vec.KAmf, sel.Integrity, sel.Ciphering)
	if err != nil {
		return nil, fmt.Errorf("derive NAS keys: %w", err)
	}
	sec := &nas.SecurityContext{IntegrityAlg: sel.Integrity, CipheringAlg: sel.Ciphering, KNASint: kInt, KNASenc: kEnc}
	verified, err := sec.DecodeSecure(raw)
	if err != nil {
		return nil, fmt.Errorf("verify the security mode command under 128-NIA%d: %w", sel.Integrity, err)
	}
	s.sec = sec
	return verified, nil
}

// advertises reports whether caps offers integrity algorithm ia and
// ciphering algorithm ea (TS 24.501 9.11.3.54: the first value octet holds
// 5G-EA0..7, the second 5G-IA0..7, each from bit 8 down).
func advertises(caps nasType.UESecurityCapability, ia, ea uint8) bool {
	n := int(caps.GetLen())
	if n > len(caps.Buffer) {
		n = len(caps.Buffer)
	}
	return ia < 8 && ea < 8 && n > 1 && caps.Buffer[1]&(0x80>>ia) != 0 && caps.Buffer[0]&(0x80>>ea) != 0
}

// readableCipher reports whether a ciphered downlink's header can be read
// without the key: under NEA0, or before any Security Mode Command, when no
// cipher has been agreed and a "ciphered" PDU can only be NEA0. A probe that
// needs the type under another cipher uses NASEvent.Msg.
//
// Before an SMC the read is type-only and unverified: there is no context,
// so no MAC is checked, and nothing but the message type may be taken from
// the PDU. A core that really ciphers before an SMC yields a meaningless
// type byte. The read never installs or advances a context (the old
// pre-derived NEA0 context read the same byte the same way).
func (s *attachState) readableCipher() bool {
	return s.sec == nil || s.sec.CipheringAlg == nas.NEA0
}
