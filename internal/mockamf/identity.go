package mockamf

import (
	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/nas"
)

// WithSMCIMEISVRequest makes the Security Mode Command carry the IMEISV
// request IE set to "IMEISV requested" (TS 24.501 5.4.2.2); the IMEISV of
// each Security Mode Complete is reported by SecurityModeCompleteIMEISVs.
func WithSMCIMEISVRequest() Option { return func(a *AMF) { a.requestIMEISV = true } }

// WithIdentityRequest makes the mock run the identification procedure
// (TS 24.501 5.4.3) after the Security Mode Complete: a protected Identity
// Request for idType (a type of identity of TS 24.501 9.11.3.3), and the
// Registration Accept only once the Identity Response arrives.
func WithIdentityRequest(idType uint8) Option {
	return func(a *AMF) { a.identityAfterSMC, a.identityType = true, idType }
}

// WithPlainIdentityRequest makes the mock send an unprotected Identity
// Request for idType just before the Authentication Request, which it does
// not hold back for the answer: a UE answers only a request for the SUCI
// before NAS security (TS 24.501 4.4.4.2).
func WithPlainIdentityRequest(idType uint8) Option {
	return func(a *AMF) { a.plainIdentity, a.plainIdentityType = true, idType }
}

// buildIdentityRequest builds an Identity Request for idType (TS 24.501
// 8.2.21).
func buildIdentityRequest(idType uint8) *f5nas.Message {
	m, gmm := plainGmm(f5nas.MsgTypeIdentityRequest)
	req := nasMessage.NewIdentityRequest(f5nas.MsgTypeIdentityRequest)
	setPlainHeader(&req.ExtendedProtocolDiscriminator, &req.SpareHalfOctetAndSecurityHeaderType,
		&req.IdentityRequestMessageIdentity, f5nas.MsgTypeIdentityRequest)
	req.SetTypeOfIdentity(idType)
	gmm.IdentityRequest = req
	return m
}

// sendIdentityRequest sends st an Identity Request for idType, integrity
// protected and ciphered when protected is set and plain otherwise.
func (a *AMF) sendIdentityRequest(p *pipe, st *ueState, idType uint8, protected bool) error {
	req := buildIdentityRequest(idType)
	var out []byte
	var err error
	if protected {
		out, err = st.encodeSecure(req, nas.SecHdrIntegrityCiphered)
	} else {
		out, err = req.PlainNasEncode()
	}
	if err != nil {
		return err
	}
	return a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), out))
}

// imeisvRequest is the IMEISV request IE set to "IMEISV requested".
func imeisvRequest() *nasType.IMEISVRequest {
	ie := nasType.NewIMEISVRequest(nasMessage.SecurityModeCommandIMEISVRequestType)
	ie.SetIei(nasMessage.SecurityModeCommandIMEISVRequestType)
	ie.SetIMEISVRequestValue(nasMessage.IMEISVRequested)
	return ie
}

// beforeAccept handles a Security Mode Complete or an Identity Response and
// reports whether the Registration Accept is now due. A Security Mode
// Complete has its NAS message container adopted and its IMEISV recorded;
// one without a container is answered per WithMissingContainer and one
// without a requested IMEISV per WithMissingIMEISV; and with
// WithIdentityRequest it draws the Identity Request in place of the Accept. An Identity Response is recorded, and makes the Accept due when
// it answers that request.
func (a *AMF) beforeAccept(p *pipe, st *ueState, msg *f5nas.Message) (bool, error) {
	if r := msg.IdentityResponse; r != nil {
		id := r.MobileIdentity
		a.mu.Lock()
		a.identityResponses = append(a.identityResponses, append([]byte(nil), id.Buffer[:min(int(id.GetLen()), len(id.Buffer))]...))
		a.mu.Unlock()
		due := st.awaitingIdentity
		st.awaitingIdentity = false
		return due, nil
	}
	smc := msg.SecurityModeComplete
	a.adoptCompleteRegistration(st, smc)
	var imeisv []byte
	if smc != nil && smc.IMEISV != nil {
		ie := smc.IMEISV
		imeisv = append([]byte{}, ie.Octet[:min(int(ie.GetLen()), len(ie.Octet))]...)
	}
	a.mu.Lock()
	a.smcIMEISVs = append(a.smcIMEISVs, imeisv)
	a.mu.Unlock()
	if smc != nil && smc.NASMessageContainer == nil && !st.containerAsked {
		switch a.missingContainer {
		case RequestInitialNAS, RepeatSecurityModeCommand:
			st.containerAsked = true
			return false, a.onAuthenticated(p, st)
		case RejectMissingContainer:
			return false, a.rejectProtected(p, st, nasMessage.Cause5GMMProtocolErrorUnspecified)
		}
	}
	if a.requestIMEISV && imeisv == nil {
		switch a.missingIMEISV {
		case IdentifyMissingIMEISV:
			st.awaitingIdentity = true
			return false, a.sendIdentityRequest(p, st, nasMessage.MobileIdentity5GSTypeImeisv, true)
		case RejectMissingIMEISV:
			return false, a.rejectProtected(p, st, nasMessage.Cause5GMMPEINotAccepted)
		}
	}
	if a.identityAfterSMC {
		st.awaitingIdentity = true
		return false, a.sendIdentityRequest(p, st, a.identityType, true)
	}
	return true, nil
}

// SecurityModeCompleteIMEISVs returns, for each Security Mode Complete
// received, oldest first, its IMEISV IE's value (a 5GS mobile identity
// value), nil when the Complete carried none.
func (a *AMF) SecurityModeCompleteIMEISVs() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneAll(a.smcIMEISVs)
}

// IdentityResponses returns the 5GS mobile identity value of each Identity
// Response received, oldest first.
func (a *AMF) IdentityResponses() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneAll(a.identityResponses)
}

// cloneAll copies a list of byte strings, keeping nil entries nil.
func cloneAll(in [][]byte) [][]byte {
	out := make([][]byte, len(in))
	for i, b := range in {
		if b != nil {
			out[i] = append([]byte{}, b...)
		}
	}
	return out
}
