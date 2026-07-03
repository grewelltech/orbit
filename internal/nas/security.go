package nas

import (
	"fmt"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/security"
)

// Security-header types (TS 24.501 §9.3, re-exported from free5gc so callers
// don't import two nas packages).
const (
	SecHdrPlain                   = f5nas.SecurityHeaderTypePlainNas                                      // 0x00
	SecHdrIntegrity               = f5nas.SecurityHeaderTypeIntegrityProtected                            // 0x01
	SecHdrIntegrityCiphered       = f5nas.SecurityHeaderTypeIntegrityProtectedAndCiphered                 // 0x02
	SecHdrIntegrityNewContext     = f5nas.SecurityHeaderTypeIntegrityProtectedWithNew5gNasSecurityContext // 0x03
	SecHdrIntegrityCipheredNewCtx = f5nas.SecurityHeaderTypeIntegrityProtectedAndCipheredWithNew5gNasSecurityContext
)

// SecurityContext holds the NAS security state established after the
// Security Mode Command exchange (TS 33.501 §6.4, TS 24.501 §4.4). The NAS
// COUNTs are 24-bit (8-bit sequence number + 16-bit overflow), carried in a
// uint32; only the low sequence byte appears in the message header.
type SecurityContext struct {
	IntegrityAlg uint8 // security.AlgIntegrity128NIAx
	CipheringAlg uint8 // security.AlgCiphering128NEAx
	KNASint      [16]byte
	KNASenc      [16]byte

	// Network flips the role: a UE (default, false) sends uplink and
	// receives downlink; an AMF/mock (true) sends downlink and receives
	// uplink. Only the direction label differs — the tx/rx COUNTs and
	// framing are identical, so one implementation serves both.
	Network bool

	txCount uint32 // NAS COUNT of the next message to send
	rxCount uint32 // NAS COUNT tracking received messages
}

func (s *SecurityContext) txDir() uint8 {
	if s.Network {
		return security.DirectionDownlink
	}
	return security.DirectionUplink
}

func (s *SecurityContext) rxDir() uint8 {
	if s.Network {
		return security.DirectionUplink
	}
	return security.DirectionDownlink
}

// NIA/NEA identities, re-exported.
const (
	NIA0 = security.AlgIntegrity128NIA0
	NIA2 = security.AlgIntegrity128NIA2
	NEA0 = security.AlgCiphering128NEA0
	NEA2 = security.AlgCiphering128NEA2
)

// epd5GMM is the extended protocol discriminator for 5GMM (TS 24.501 §9.2).
const epd5GMM = 0x7E

// EncodeSecure wraps a plain NAS message in the security header
// (TS 24.501 §9.1.1): [EPD | sec-header-type | MAC(4) | seq | payload],
// where payload is ciphered (NEA0 = passthrough) and the MAC covers
// seq‖payload with the uplink NAS COUNT. It advances the uplink COUNT.
//
// secHdr selects integrity-only vs integrity+ciphered (and the "new
// context" variants used for the first message under a fresh context). A
// plain message must be sent with PlainEncode, not this.
func (s *SecurityContext) EncodeSecure(msg *f5nas.Message, secHdr uint8) ([]byte, error) {
	if secHdr == SecHdrPlain {
		return nil, fmt.Errorf("EncodeSecure called with plain header; use EncodePlain")
	}
	// A "new 5G NAS security context" header activates the fresh context:
	// both NAS COUNTs restart at 0 (TS 33.501 §6.4.3.1). Matches gnbsim.
	if secHdr == SecHdrIntegrityNewContext || secHdr == SecHdrIntegrityCipheredNewCtx {
		s.txCount, s.rxCount = 0, 0
	}
	plain, err := msg.PlainNasEncode()
	if err != nil {
		return nil, fmt.Errorf("nas plain encode: %w", err)
	}

	seq := byte(s.txCount & 0xFF)
	payload := append([]byte(nil), plain...)

	ciphered := secHdr == SecHdrIntegrityCiphered || secHdr == SecHdrIntegrityCipheredNewCtx
	if ciphered {
		if err := security.NASEncrypt(s.CipheringAlg, s.KNASenc, s.txCount,
			security.Bearer3GPP, s.txDir(), payload); err != nil {
			return nil, fmt.Errorf("nas encrypt: %w", err)
		}
	}

	macInput := append([]byte{seq}, payload...)
	mac, err := security.NASMacCalculate(s.IntegrityAlg, s.KNASint, s.txCount,
		security.Bearer3GPP, s.txDir(), macInput)
	if err != nil {
		return nil, fmt.Errorf("nas mac calculate: %w", err)
	}

	out := make([]byte, 0, 7+len(payload))
	out = append(out, epd5GMM, secHdr)
	out = append(out, mac...)
	out = append(out, seq)
	out = append(out, payload...)

	s.txCount++
	return out, nil
}

// DecodeSecure verifies and unwraps a security-protected downlink NAS
// message, returning the decoded plain message. It checks the received MAC
// against a recomputation over seq‖payload with the downlink NAS COUNT,
// deciphers if needed, and advances the downlink COUNT.
//
// Note: the downlink COUNT is taken from the message's own sequence number
// (with the tracked overflow) rather than blindly from local state, matching
// how the NAS layer resynchronises on the sequence byte (TS 24.501 §4.4.3.1).
func (s *SecurityContext) DecodeSecure(data []byte) (*f5nas.Message, error) {
	if len(data) < 7 {
		return nil, fmt.Errorf("secured NAS too short: %d bytes", len(data))
	}
	if data[0] != epd5GMM {
		return nil, fmt.Errorf("unexpected EPD %#x", data[0])
	}
	secHdr := data[1]
	recvMAC := data[2:6]
	seq := data[6]
	payload := append([]byte(nil), data[7:]...)

	// A "new context" downlink message restarts the downlink COUNT
	// (TS 33.501 §6.4.3.1). Otherwise, recover the 24-bit COUNT from the
	// received sequence byte, bumping the overflow counter on wrap
	// (TS 24.501 §4.4.3.1). Matches gnbsim.
	if secHdr == SecHdrIntegrityNewContext || secHdr == SecHdrIntegrityCipheredNewCtx {
		s.rxCount = 0
	}
	if byte(s.rxCount&0xFF) > seq {
		s.rxCount += 1 << 8 // overflow++
	}
	count := s.rxCount&0xFFFFFF00 | uint32(seq)

	macInput := append([]byte{seq}, payload...)
	wantMAC, err := security.NASMacCalculate(s.IntegrityAlg, s.KNASint, count,
		security.Bearer3GPP, s.rxDir(), macInput)
	if err != nil {
		return nil, fmt.Errorf("nas mac calculate: %w", err)
	}
	if !equalMAC(recvMAC, wantMAC) {
		return nil, fmt.Errorf("NAS integrity check failed: mac %x != %x", recvMAC, wantMAC)
	}

	ciphered := secHdr == SecHdrIntegrityCiphered || secHdr == SecHdrIntegrityCipheredNewCtx
	if ciphered {
		if err := security.NASEncrypt(s.CipheringAlg, s.KNASenc, count,
			security.Bearer3GPP, s.rxDir(), payload); err != nil {
			return nil, fmt.Errorf("nas decrypt: %w", err)
		}
	}

	// Track the last-received sequence number as the rx COUNT state.
	s.rxCount = count

	m := f5nas.NewMessage()
	if err := m.PlainNasDecode(&payload); err != nil {
		return nil, fmt.Errorf("nas plain decode of unwrapped payload: %w", err)
	}
	return m, nil
}

// ResetCounts zeroes the NAS COUNTs, as when a fresh security context is
// activated by Security Mode Command (TS 33.501 §6.4.3.1).
func (s *SecurityContext) ResetCounts() {
	s.txCount = 0
	s.rxCount = 0
}

// UplinkCount reports the next uplink NAS COUNT (for observability/tests).
func (s *SecurityContext) UplinkCount() uint32 { return s.txCount }

func equalMAC(a, b []byte) bool {
	if len(a) != 4 || len(b) != 4 {
		return false
	}
	var diff byte
	for i := 0; i < 4; i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
