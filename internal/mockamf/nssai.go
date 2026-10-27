package mockamf

import (
	"bytes"

	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/nas"
)

// NSSAIAnswer is how the mock answers a registration whose requested NSSAI
// holds none of its configured S-NSSAI (WithUnservedNSSAI). Each answer
// replaces the Registration Accept that follows the Security Mode Complete.
type NSSAIAnswer int

const (
	// AcceptAnyNSSAI ignores the requested NSSAI and accepts as usual (the
	// default).
	AcceptAnyNSSAI NSSAIAnswer = iota
	// Reject62NSSAI rejects with 5GMM cause #62 "No network slices
	// available", listing every requested S-NSSAI in the Rejected NSSAI IE
	// (cause "not available in the current PLMN or SNPN").
	Reject62NSSAI
	// Reject62BareNSSAI rejects with #62 and no rejected NSSAI.
	Reject62BareNSSAI
	// Reject62ExtendedNSSAI rejects with #62, listing the requested
	// S-NSSAIs only in an Extended rejected NSSAI IE.
	Reject62ExtendedNSSAI
	// Reject62UndecodableNSSAI rejects with #62 and an Extended rejected
	// NSSAI IE whose value does not decode.
	Reject62UndecodableNSSAI
	// RejectProtocolErrorNSSAI rejects with #111 "Protocol error,
	// unspecified".
	RejectProtocolErrorNSSAI
	// AcceptDefaultNSSAI accepts with the mock's S-NSSAI as the allowed
	// NSSAI and the requested ones in the Rejected NSSAI IE: a core that
	// falls back to a default S-NSSAI.
	AcceptDefaultNSSAI
	// AcceptRequestedNSSAI accepts with the requested S-NSSAIs allowed.
	AcceptRequestedNSSAI
	// IgnoreNSSAI sends nothing after the Security Mode Complete.
	IgnoreNSSAI
)

// Information element identifiers of the REGISTRATION REJECT (TS 24.501
// 8.2.9.1).
const (
	ieiRejectedNSSAI         = 0x69
	ieiExtendedRejectedNSSAI = 0x68
)

// cause5GMMNoNetworkSlicesAvailable is 5GMM cause #62 (TS 24.501 9.11.3.2),
// which the NAS library does not name.
const cause5GMMNoNetworkSlicesAvailable = 62

// WithUnservedNSSAI sets how the mock answers a registration that requests
// none of its configured S-NSSAI.
func WithUnservedNSSAI(answer NSSAIAnswer) Option { return func(a *AMF) { a.nssaiAnswer = answer } }

// requestedNSSAIFromRegistration is the Requested NSSAI IE's value in a
// plain Registration Request, nil when absent.
func requestedNSSAIFromRegistration(regReq []byte) []byte {
	m, err := nas.DecodePlain(regReq)
	if err != nil || m.GmmMessage == nil || m.RegistrationRequest == nil {
		return nil
	}
	rr := m.RegistrationRequest
	n := rr.RequestedNSSAI
	if n == nil {
		return nil
	}
	return append([]byte(nil), n.Buffer[:n.GetLen()]...)
}

// snssaiValues splits an NSSAI value (TS 24.501 9.11.2.8 entries, each a
// length octet and its contents) into the entries' contents. A truncated
// entry ends the list.
func snssaiValues(v []byte) [][]byte {
	var out [][]byte
	for len(v) > 0 {
		n := int(v[0])
		if n == 0 || 1+n > len(v) {
			break
		}
		out = append(out, v[1:1+n])
		v = v[1+n:]
	}
	return out
}

// servesRequested reports whether st requested the mock's S-NSSAI, or no
// NSSAI at all.
func (a *AMF) servesRequested(st *ueState) bool {
	if st.requested == nil {
		return true
	}
	own := append([]byte{a.cfg.SST}, a.sdBytes...)
	for _, s := range snssaiValues(st.requested) {
		if len(s) >= len(own) && bytes.Equal(s[:len(own)], own) {
			return true
		}
	}
	return false
}

// rejectedList codes the S-NSSAIs of st's requested NSSAI as rejected
// S-NSSAIs (TS 24.501 9.11.3.46): length and cause 0 in one octet, then
// the SST and SD.
func rejectedList(st *ueState) []byte {
	var out []byte
	for _, s := range snssaiValues(st.requested) {
		sd := s
		if len(sd) > 4 {
			sd = sd[:4] // SST and SD, without the mapped HPLMN S-NSSAI
		}
		out = append(out, byte(len(sd))<<4)
		out = append(out, sd...)
	}
	return out
}

// answerUnservedNSSAI sends the WithUnservedNSSAI answer, protected.
func (a *AMF) answerUnservedNSSAI(p *pipe, st *ueState) error {
	var plain []byte
	switch a.nssaiAnswer {
	case IgnoreNSSAI:
		return nil
	case AcceptDefaultNSSAI, AcceptRequestedNSSAI:
		return a.acceptWithNSSAI(p, st)
	case RejectProtocolErrorNSSAI:
		rej, err := buildRegistrationReject(nasMessage.Cause5GMMProtocolErrorUnspecified)
		if err != nil {
			return err
		}
		plain = rej
	default:
		rej, err := buildRegistrationReject(cause5GMMNoNetworkSlicesAvailable)
		if err != nil {
			return err
		}
		list := rejectedList(st)
		switch a.nssaiAnswer {
		case Reject62NSSAI:
			rej = append(rej, ieiRejectedNSSAI, byte(len(list)))
			rej = append(rej, list...)
		case Reject62ExtendedNSSAI:
			// One partial extended rejected NSSAI list of type 000 (no
			// back-off timer), its element count minus one in bits 4-1
			// (TS 24.501 9.11.3.75).
			n := len(snssaiValues(st.requested))
			v := append([]byte{byte(n - 1)}, list...)
			rej = append(rej, ieiExtendedRejectedNSSAI, byte(len(v)))
			rej = append(rej, v...)
		case Reject62UndecodableNSSAI:
			// Type of list 111 is reserved.
			rej = append(rej, ieiExtendedRejectedNSSAI, 1, 0x71)
		}
		plain = rej
	}
	wrapped, err := st.encodeSecureRaw(plain, nas.SecHdrIntegrityCiphered)
	if err != nil {
		return err
	}
	return a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), wrapped))
}

// acceptWithNSSAI accepts st with the allowed and rejected NSSAI of the
// WithUnservedNSSAI answer, in an Initial Context Setup Request.
func (a *AMF) acceptWithNSSAI(p *pipe, st *ueState) error {
	acc, err := a.registrationAccept(st)
	if err != nil {
		return err
	}
	ra := acc.RegistrationAccept
	allowed := st.requested
	if a.nssaiAnswer == AcceptDefaultNSSAI {
		own := append([]byte{a.cfg.SST}, a.sdBytes...)
		allowed = append([]byte{byte(len(own))}, own...)
		list := rejectedList(st)
		ra.RejectedNSSAI = nasType.NewRejectedNSSAI(nasMessage.RegistrationAcceptRejectedNSSAIType)
		ra.RejectedNSSAI.SetLen(uint8(len(list)))
		ra.RejectedNSSAI.Buffer = list
	}
	ra.AllowedNSSAI = nasType.NewAllowedNSSAI(nasMessage.RegistrationAcceptAllowedNSSAIType)
	ra.AllowedNSSAI.SetLen(uint8(len(allowed)))
	ra.AllowedNSSAI.Buffer = append([]byte(nil), allowed...)
	wrapped, err := st.encodeSecure(acc, nas.SecHdrIntegrityCiphered)
	if err != nil {
		return err
	}
	return a.send(p, buildInitialContextSetupRequest(st.amfID, a.ranIDOf(st), wrapped, a.plmn, a.cfg.SST, a.sdBytes))
}

// RegistrationRequests returns the plain Registration Requests the mock has
// received, oldest first, so a test can check what a UE sent.
func (a *AMF) RegistrationRequests() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([][]byte, len(a.regReqs))
	for i, r := range a.regReqs {
		out[i] = append([]byte(nil), r...)
	}
	return out
}
