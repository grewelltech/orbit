package mockamf

import (
	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
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
		acc, err := buildRegistrationAccept()
		if err != nil {
			return err
		}
		wrapped, err := st.sec.EncodeSecure(acc, nas.SecHdrIntegrityCiphered)
		if err != nil {
			return err
		}
		return a.send(p, buildInitialContextSetupRequest(st.amfID, st.ranID, wrapped, a.plmn, a.cfg.SST, a.sdBytes))
	}
	alg, ok := a.selectIntegrity(st.ueCaps)
	if !ok {
		switch a.noCommon {
		case ReleaseNoCommonIntegrity:
			err := a.send(p, buildUEContextReleaseCommand(st.amfID, st.ranID))
			if a.crashAfterRelease {
				a.crashed.Store(true) // WithCrashAfterRelease covers this release too
			}
			return err
		case NIA0NoCommonIntegrity:
			alg = nas.NIA0
		default:
			rej, err := buildRegistrationReject(nasMessage.Cause5GMMUESecurityCapabilitiesMismatch)
			if err != nil {
				return err
			}
			return a.send(p, buildDownlinkNASTransport(st.amfID, st.ranID, rej))
		}
	}
	replayed := st.ueCaps
	replayed.Buffer = append([]uint8(nil), st.ueCaps.Buffer...)
	if a.tamperCaps && len(replayed.Buffer) > 0 {
		replayed.Buffer[0] ^= 0x01
	}
	// The selected algorithm is what the command says; the mock keeps
	// protecting with 128-NIA2, which only a UE that goes on to complete
	// the procedure would notice, and such a UE offers 128-NIA2.
	smc, err := buildSecurityModeCommand(alg, nas.NEA0, st.ngksi, replayed)
	if err != nil {
		return err
	}
	wrapped, err := st.sec.EncodeSecure(smc, nas.SecHdrIntegrityNewContext)
	if err != nil {
		return err
	}
	return a.send(p, buildDownlinkNASTransport(st.amfID, st.ranID, wrapped))
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
