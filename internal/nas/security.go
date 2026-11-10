package nas

import (
	"errors"
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

	// RejectReplay makes DecodeSecure refuse, with ErrReplay, a message
	// whose NAS COUNT the context has already accepted (TS 24.501 4.4.3.2).
	// The mock AMF sets it for uplinks; a UE context leaves it unset and
	// accepts a downlink repeated at the stored COUNT.
	RejectReplay bool

	txCount uint32 // NAS COUNT of the next message to send
	rxCount uint32 // NAS COUNT tracking received messages
	// rxAccepted reports whether rxCount is an accepted COUNT, rather than
	// the zero a fresh context starts at.
	rxAccepted bool
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
	NIA1 = security.AlgIntegrity128NIA1
	NIA2 = security.AlgIntegrity128NIA2
	NIA3 = security.AlgIntegrity128NIA3
	NEA0 = security.AlgCiphering128NEA0
	NEA1 = security.AlgCiphering128NEA1
	NEA2 = security.AlgCiphering128NEA2
	NEA3 = security.AlgCiphering128NEA3
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
	plain, err := msg.PlainNasEncode()
	if err != nil {
		return nil, fmt.Errorf("nas plain encode: %w", err)
	}
	return s.EncodeSecureRaw(plain, secHdr)
}

// EncodeSecureRaw is EncodeSecure for an already-encoded plain NAS message,
// which need not decode (a mock sending a malformed message uses it). It
// advances the uplink COUNT as EncodeSecure does.
func (s *SecurityContext) EncodeSecureRaw(plain []byte, secHdr uint8) ([]byte, error) {
	if secHdr == SecHdrPlain {
		return nil, fmt.Errorf("EncodeSecureRaw called with plain header; use EncodePlain")
	}
	// A "new 5G NAS security context" header activates the fresh context:
	// both NAS COUNTs restart at 0 (TS 33.501 §6.4.3.1). Matches gnbsim.
	if secHdr == SecHdrIntegrityNewContext || secHdr == SecHdrIntegrityCipheredNewCtx {
		s.txCount, s.rxCount, s.rxAccepted = 0, 0, false
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

// ErrIntegrity is the error DecodeSecure wraps when a message's MAC does
// not verify.
var ErrIntegrity = errors.New("NAS integrity check failed")

// ErrReplay is the error DecodeSecure wraps, under RejectReplay, for a
// message whose MAC verifies at a NAS COUNT the context already accepted.
// The stored COUNT is left as it was.
var ErrReplay = errors.New("NAS COUNT already accepted")

// replayed reports whether a verified message at count repeats an accepted
// COUNT under RejectReplay: "a given NAS COUNT value shall be accepted at
// most one time and only if message integrity verifies correctly" (TS
// 24.501 4.4.3.2). A message under a new-context header starts a context
// and is exempt, and so is every message under 5G-IA0 ("Replay protection
// is not applicable when 5G-IA0 is used"). The first COUNT a context
// accepts is never a replay, COUNT 0 included.
func (s *SecurityContext) replayed(secHdr uint8, count uint32) bool {
	if !s.RejectReplay || s.IntegrityAlg == NIA0 || !s.rxAccepted {
		return false
	}
	if secHdr == SecHdrIntegrityNewContext || secHdr == SecHdrIntegrityCipheredNewCtx {
		return false
	}
	return count <= s.rxCount
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
	m, _, err := s.DecodeSecurePlain(data)
	return m, err
}

// DecodeSecurePlain is DecodeSecure that also returns the plain NAS message
// inside the security header: set once the MAC verified and deciphering
// succeeded, even when the codec then refuses the message, so a caller
// can read IEs the codec does not know. It is nil when the PDU did not
// verify or decipher.
func (s *SecurityContext) DecodeSecurePlain(data []byte) (*f5nas.Message, []byte, error) {
	count, err := s.estimateRx(data)
	if err != nil {
		return nil, nil, err
	}
	if err := s.verifyAt(data, count); err != nil {
		return nil, nil, err
	}
	if s.replayed(data[1], count) {
		return nil, nil, fmt.Errorf("%w: NAS COUNT %d", ErrReplay, count)
	}
	// The MAC proved the COUNT: track the last-received sequence number as
	// the rx COUNT state, even if deciphering or decoding fails below.
	s.rxCount, s.rxAccepted = count, true
	return s.unwrapAt(data, count)
}

// DecodeUnverified deciphers and decodes a security-protected NAS message
// at the COUNT DecodeSecure would estimate for it, without checking its MAC
// and without storing the COUNT. It stands in for a receiver that does not
// enforce integrity (a mock's non-conformant option); nothing else may use
// it.
func (s *SecurityContext) DecodeUnverified(data []byte) (*f5nas.Message, error) {
	count, err := s.estimateRx(data)
	if err != nil {
		return nil, err
	}
	m, _, err := s.unwrapAt(data, count)
	return m, err
}

// estimateRx is the NAS COUNT a received protected PDU is checked at. A
// "new context" message restarts the COUNT (TS 33.501 §6.4.3.1). Otherwise
// the 24-bit COUNT is recovered from the received sequence byte, bumping the
// overflow counter on wrap (TS 24.501 §4.4.3.1). Matches gnbsim. The
// estimate is a candidate until the MAC verifies, and only then is stored,
// since "The value of the downlink NAS COUNT that is stored or read out of
// the USIM or non-volatile memory as described in annex C, is the largest
// downlink NAS COUNT used in a successfully integrity checked NAS message."
// (TS 24.501 4.4.3.1, V15.0.0 onward). A message that fails the check
// leaves the stored COUNT as it was.
func (s *SecurityContext) estimateRx(data []byte) (uint32, error) {
	if len(data) < 7 {
		return 0, fmt.Errorf("secured NAS too short: %d bytes", len(data))
	}
	if data[0] != epd5GMM {
		return 0, fmt.Errorf("unexpected EPD %#x", data[0])
	}
	seq := data[6]
	est := s.rxCount
	if secHdr := data[1]; secHdr == SecHdrIntegrityNewContext || secHdr == SecHdrIntegrityCipheredNewCtx {
		est = 0
	}
	if byte(est&0xFF) > seq {
		est += 1 << 8 // overflow++
	}
	return est&0xFFFFFF00 | uint32(seq), nil
}

// verifyAt checks a protected PDU's MAC over seq‖payload at count. A
// mismatch wraps ErrIntegrity.
func (s *SecurityContext) verifyAt(data []byte, count uint32) error {
	wantMAC, err := security.NASMacCalculate(s.IntegrityAlg, s.KNASint, count,
		security.Bearer3GPP, s.rxDir(), data[6:])
	if err != nil {
		return fmt.Errorf("nas mac calculate: %w", err)
	}
	if recvMAC := data[2:6]; !equalMAC(recvMAC, wantMAC) {
		return fmt.Errorf("%w: mac %x != %x", ErrIntegrity, recvMAC, wantMAC)
	}
	return nil
}

// unwrapAt deciphers a protected PDU at count and decodes its plain NAS
// message. The plain bytes come back even when the codec refuses them.
func (s *SecurityContext) unwrapAt(data []byte, count uint32) (*f5nas.Message, []byte, error) {
	secHdr := data[1]
	payload := append([]byte(nil), data[7:]...)
	if secHdr == SecHdrIntegrityCiphered || secHdr == SecHdrIntegrityCipheredNewCtx {
		if err := security.NASEncrypt(s.CipheringAlg, s.KNASenc, count,
			security.Bearer3GPP, s.rxDir(), payload); err != nil {
			return nil, nil, fmt.Errorf("nas decrypt: %w", err)
		}
	}
	plain := append([]byte(nil), payload...)
	m := f5nas.NewMessage()
	if err := m.PlainNasDecode(&payload); err != nil {
		return nil, plain, fmt.Errorf("nas plain decode of unwrapped payload: %w", err)
	}
	return m, plain, nil
}

// ResetCounts zeroes the NAS COUNTs, as when a fresh security context is
// activated by Security Mode Command (TS 33.501 §6.4.3.1).
func (s *SecurityContext) ResetCounts() {
	s.txCount = 0
	s.rxCount = 0
	s.rxAccepted = false
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
