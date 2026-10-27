package mockamf

import (
	"fmt"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// NoCommonIntegrity is what the mock does when the UE offers none of the
// integrity algorithms in its order (WithNoCommonIntegrity).
type NoCommonIntegrity int

const (
	// RejectNoCommonIntegrity answers with a Registration Reject, 5GMM
	// cause #23 UE security capabilities mismatch (the default).
	RejectNoCommonIntegrity NoCommonIntegrity = iota
	// ReleaseNoCommonIntegrity releases the UE with a UE CONTEXT RELEASE
	// COMMAND.
	ReleaseNoCommonIntegrity
	// NIA0NoCommonIntegrity selects NIA0 anyway: an AMF whose selection
	// falls back to the null algorithm.
	NIA0NoCommonIntegrity
	// SkipNoCommonIntegrity sends no Security Mode Command: a plain
	// Registration Accept (security header type 0) follows the
	// Authentication Response, so registration proceeds without NAS
	// security.
	SkipNoCommonIntegrity
)

// defaultIntegrityOrder is the mock's integrity preference: 128-NIA2, then
// 128-NIA1, then 128-NIA3. NIA0 is never chosen from the order.
var defaultIntegrityOrder = []uint8{nas.NIA2, nas.NIA1, nas.NIA3}

// WithIntegrityOrder sets the integrity algorithms the mock selects from, in
// preference order (default 128-NIA2, 128-NIA1, 128-NIA3).
func WithIntegrityOrder(algs ...uint8) Option {
	return func(a *AMF) { a.integrityOrder = append([]uint8(nil), algs...) }
}

// WithNoCommonIntegrity sets what the mock does when the UE offers none of
// its integrity algorithms.
func WithNoCommonIntegrity(n NoCommonIntegrity) Option { return func(a *AMF) { a.noCommon = n } }

// WithSMCIntegrity makes the Security Mode Command select alg whatever the
// UE offers (for example NIA0).
func WithSMCIntegrity(alg uint8) Option {
	return func(a *AMF) { a.forceIntegrity, a.forcedIntegrity = true, alg }
}

// defaultCipheringOrder is the mock's ciphering preference: NEA0 only.
var defaultCipheringOrder = []uint8{nas.NEA0}

// WithCipheringOrder sets the ciphering algorithms the mock selects from,
// in preference order (default NEA0 only).
func WithCipheringOrder(algs ...uint8) Option {
	return func(a *AMF) { a.cipheringOrder = append([]uint8(nil), algs...) }
}

// WithSMCCiphering makes the Security Mode Command select alg whatever the
// UE offers.
func WithSMCCiphering(alg uint8) Option {
	return func(a *AMF) { a.forceCiphering, a.forcedCiphering = true, alg }
}

// selectCiphering picks the ciphering algorithm for the Security Mode
// Command: the forced one, else the first of the order the UE offers, else
// the first of the order (an AMF that ignores the UE's capabilities).
func (a *AMF) selectCiphering(caps nasType.UESecurityCapability) uint8 {
	if a.forceCiphering {
		return a.forcedCiphering
	}
	order := a.cipheringOrder
	if len(order) == 0 {
		order = defaultCipheringOrder
	}
	for _, alg := range order {
		if alg < 8 && len(caps.Buffer) > 0 && caps.Buffer[0]&(0x80>>alg) != 0 {
			return alg
		}
	}
	return order[0]
}

// WithSMCTamperCaps makes the Security Mode Command replay UE security
// capabilities that differ from those the UE sent (5G-EA7 flipped).
func WithSMCTamperCaps() Option { return func(a *AMF) { a.tamperCaps = true } }

// WithSkipSMC skips the Security Mode Command: the Registration Accept
// follows the Authentication Response directly.
func WithSkipSMC() Option { return func(a *AMF) { a.skipSMC = true } }

// ueCapsFromRegistration reads the UE security capability from a plain
// Registration Request, falling back to ue.SecurityCapability when absent.
func ueCapsFromRegistration(regReq []byte) nasType.UESecurityCapability {
	m, err := nas.DecodePlain(regReq)
	if err != nil || m.GmmMessage == nil {
		return ue.SecurityCapability()
	}
	gm := m.GmmMessage
	if gm.RegistrationRequest == nil {
		return ue.SecurityCapability()
	}
	rr := gm.RegistrationRequest
	if rr.UESecurityCapability == nil || len(rr.UESecurityCapability.Buffer) < 2 {
		return ue.SecurityCapability()
	}
	c := *rr.UESecurityCapability
	c.Buffer = append([]uint8(nil), c.Buffer...)
	return c
}

// selectIntegrity picks the integrity algorithm for st's Security Mode
// Command: the forced one, else the first of the order the UE offers.
func (a *AMF) selectIntegrity(caps nasType.UESecurityCapability) (uint8, bool) {
	if a.forceIntegrity {
		return a.forcedIntegrity, true
	}
	order := a.integrityOrder
	if order == nil {
		order = defaultIntegrityOrder
	}
	for _, alg := range order {
		if alg < 8 && len(caps.Buffer) > 1 && caps.Buffer[1]&(0x80>>alg) != 0 {
			return alg, true
		}
	}
	return 0, false
}

// onAuthenticated answers an Authentication Response: the Security Mode
// Command (or, per the options, a reject, a release, or straight to the
// Registration Accept).
func (a *AMF) onAuthenticated(p *pipe, st *ueState) error {
	if a.skipSMC {
		acc, err := a.registrationAccept(st)
		if err != nil {
			return err
		}
		wrapped, err := st.encodeSecure(acc, nas.SecHdrIntegrityCiphered)
		if err != nil {
			return err
		}
		return a.send(p, buildInitialContextSetupRequest(st.amfID, a.ranIDOf(st), wrapped, a.plmn, a.cfg.SST, a.sdBytes))
	}
	alg, ok := a.selectIntegrity(st.ueCaps)
	if !ok {
		switch a.noCommon {
		case ReleaseNoCommonIntegrity:
			err := a.send(p, buildUEContextReleaseCommand(st.amfID, a.ranIDOf(st)))
			if a.crashAfterRelease {
				a.crashed.Store(true) // WithCrashAfterRelease covers this release too
			}
			return err
		case NIA0NoCommonIntegrity:
			alg = nas.NIA0
		case SkipNoCommonIntegrity:
			acc, err := a.registrationAccept(st)
			if err != nil {
				return err
			}
			plain, err := acc.PlainNasEncode()
			if err != nil {
				return err
			}
			return a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), plain))
		default:
			rej, err := buildRegistrationReject(nasMessage.Cause5GMMUESecurityCapabilitiesMismatch)
			if err != nil {
				return err
			}
			return a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), rej))
		}
	}
	replayed := st.ueCaps
	replayed.Buffer = append([]uint8(nil), st.ueCaps.Buffer...)
	if a.tamperCaps && len(replayed.Buffer) > 0 {
		replayed.Buffer[0] ^= 0x01
	}
	enc := a.selectCiphering(st.ueCaps)
	if !a.forceIntegrity && alg != nas.NIA0 {
		// The new context is the selected pair's, from KAMF (TS 33.501
		// Annex A.8), and it protects the command itself. A forced or NIA0
		// selection keeps the 128-NIA2 context from authentication, so
		// those commands stay as they were on the wire.
		kInt, kEnc, err := auth.DeriveNASKeys(st.kamf, alg, enc)
		if err != nil {
			return fmt.Errorf("mock amf NAS key derivation: %w", err)
		}
		st.setSec(&nas.SecurityContext{IntegrityAlg: alg, CipheringAlg: enc, KNASint: kInt, KNASenc: kEnc, Network: true})
	}
	smc, err := buildSecurityModeCommand(alg, enc, st.ngksi, replayed)
	if err != nil {
		return err
	}
	wrapped, err := st.encodeSecure(smc, nas.SecHdrIntegrityNewContext)
	if err != nil {
		return err
	}
	return a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), wrapped))
}

// buildRegistrationReject builds a plain Registration Reject with 5GMM
// cause (TS 24.501 8.2.9).
func buildRegistrationReject(cause uint8) ([]byte, error) {
	m, gmm := plainGmm(f5nas.MsgTypeRegistrationReject)
	rej := nasMessage.NewRegistrationReject(f5nas.MsgTypeRegistrationReject)
	setPlainHeader(&rej.ExtendedProtocolDiscriminator, &rej.SpareHalfOctetAndSecurityHeaderType,
		&rej.RegistrationRejectMessageIdentity, f5nas.MsgTypeRegistrationReject)
	rej.SetCauseValue(cause)
	gmm.RegistrationReject = rej
	return m.PlainNasEncode()
}
