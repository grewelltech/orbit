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
// NAS message's, and it replaces that message in RegistrationRequests. A
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
	st.requested = requestedNSSAIFromRegistration(complete)
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
