package mockamf

import (
	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/nas"
)

// WithRejectNonCleartextIEs makes the mock answer an initial Registration
// Request that carries any IE besides the cleartext IEs of TS 24.501 4.4.6
// with a plain Registration Reject, 5GMM cause #95 "Semantically incorrect
// message", instead of authenticating the UE.
func WithRejectNonCleartextIEs() Option { return func(a *AMF) { a.rejectNonCleartext = true } }

// WithUnprotectedRegistrationWins makes the mock keep the Requested NSSAI of
// the initial NAS message when that message carried one, whatever the
// Registration Request in the Security Mode Complete's NAS message
// container says: an AMF that does not take the protected copy as the
// message that triggered the registration.
func WithUnprotectedRegistrationWins() Option { return func(a *AMF) { a.unprotectedWins = true } }

// MissingContainerAnswer is what the mock does with a Security Mode
// Complete that has no NAS message container (WithMissingContainer).
type MissingContainerAnswer int

const (
	// ProceedWithoutContainer goes on with the initial NAS message as the
	// one that triggered the registration (the default).
	ProceedWithoutContainer MissingContainerAnswer = iota
	// RequestInitialNAS sends a second Security Mode Command whose
	// Additional 5G security information IE has the RINMR bit set
	// ("Retransmission of the initial NAS message requested").
	RequestInitialNAS
	// RepeatSecurityModeCommand sends a second Security Mode Command with
	// no RINMR bit.
	RepeatSecurityModeCommand
	// RejectMissingContainer answers with a protected Registration Reject,
	// 5GMM cause #111 "Protocol error, unspecified".
	RejectMissingContainer
)

// WithMissingContainer sets what the mock does with a Security Mode
// Complete that has no NAS message container. A second Security Mode
// Command is sent once per registration; a Complete that again has no
// container is then taken as it is.
func WithMissingContainer(answer MissingContainerAnswer) Option {
	return func(a *AMF) { a.missingContainer = answer }
}

// MissingIMEISVAnswer is what the mock does with a Security Mode Complete
// that lacks the IMEISV its Security Mode Command requested
// (WithMissingIMEISV).
type MissingIMEISVAnswer int

const (
	// ProceedWithoutIMEISV goes on to the Registration Accept (the
	// default).
	ProceedWithoutIMEISV MissingIMEISVAnswer = iota
	// IdentifyMissingIMEISV sends a protected Identity Request for the
	// IMEISV and accepts once the Identity Response arrives.
	IdentifyMissingIMEISV
	// RejectMissingIMEISV answers with a protected Registration Reject,
	// 5GMM cause #5 "PEI not accepted".
	RejectMissingIMEISV
)

// WithMissingIMEISV sets what the mock does when WithSMCIMEISVRequest asked
// for the IMEISV and the Security Mode Complete does not carry it.
func WithMissingIMEISV(answer MissingIMEISVAnswer) Option {
	return func(a *AMF) { a.missingIMEISV = answer }
}

// SecurityModeCommands reports how many Security Mode Commands the mock
// has sent.
func (a *AMF) SecurityModeCommands() int64 { return a.smcsSent.Load() }

// rejectProtected sends st a Registration Reject with cause, integrity
// protected and ciphered.
func (a *AMF) rejectProtected(p *pipe, st *ueState, cause uint8) error {
	rej, err := buildRegistrationReject(cause)
	if err != nil {
		return err
	}
	m, err := nas.DecodePlain(rej)
	if err != nil {
		return err
	}
	wrapped, err := st.encodeSecure(m, nas.SecHdrIntegrityCiphered)
	if err != nil {
		return err
	}
	return a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), wrapped))
}

// rejectInitialNAS sends the WithRejectNonCleartextIEs reject.
func (a *AMF) rejectInitialNAS(p *pipe, st *ueState) error {
	rej, err := buildRegistrationReject(nasMessage.Cause5GMMSemanticallyIncorrectMessage)
	if err != nil {
		return err
	}
	return a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), rej))
}

// adoptCompleteRegistration takes the Registration Request in a Security
// Mode Complete's NAS message container as the message that triggered st's
// registration (TS 24.501 5.4.2.4): its Requested NSSAI replaces the initial
// NAS message's (unless WithUnprotectedRegistrationWins keeps that one),
// and it replaces that message in RegistrationRequests. A
// Complete without a container, or with one that holds no Registration
// Request, leaves the initial NAS message standing.
func (a *AMF) adoptCompleteRegistration(st *ueState, smc *nasMessage.SecurityModeComplete) {
	if smc == nil || smc.NASMessageContainer == nil {
		return
	}
	c := smc.NASMessageContainer
	if int(c.GetLen()) > len(c.Buffer) {
		return
	}
	complete := append([]byte(nil), c.Buffer[:c.GetLen()]...)
	m, err := nas.DecodePlain(complete)
	if err != nil || m.GmmMessage == nil || m.RegistrationRequest == nil {
		return
	}
	if !a.unprotectedWins || st.requested == nil {
		st.requested = requestedNSSAIFromRegistration(complete)
	}
	a.mu.Lock()
	a.regReqs[st.regIdx] = complete
	a.mu.Unlock()
}

// InitialRegistrationRequests returns the Registration Requests as the
// Initial UE Messages carried them, oldest first: entry i is the initial NAS
// message of the registration RegistrationRequests reports at i.
func (a *AMF) InitialRegistrationRequests() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([][]byte, len(a.initialRegReqs))
	for i, r := range a.initialRegReqs {
		out[i] = append([]byte(nil), r...)
	}
	return out
}
