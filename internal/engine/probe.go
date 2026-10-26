package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
)

// ErrReleasedByCore is the error Attach returns, with a probe set, when the
// core sends a UE CONTEXT RELEASE COMMAND for the UE during the attach.
// No UE CONTEXT RELEASE COMPLETE is sent: the caller closes the association.
var ErrReleasedByCore = errors.New("the core released the UE context during the attach")

// AttachProbe lets a conformance check withhold the UE's reply to chosen
// downlink NAS messages and watch what the core sends during an attach.
//
// A withheld reply is withheld at the reaction: the UE does nothing else in
// reaction to that downlink either (no state event, no key derivation, no
// REGISTERED, no PDU session request), so a retransmitted downlink finds the
// attach where the first one left it and is withheld again.
type AttachProbe struct {
	// Withhold names the uplink NAS message types the UE never sends (for
	// example f5nas.MsgTypeAuthenticationResponse). Only the replies the
	// attach makes are affected: Authentication Response, Security Mode
	// Complete and Registration Complete.
	Withhold map[uint8]bool
	// OnDownlink is called for every downlink NGAP message of the UE, with
	// the NAS message type when it carries one. It runs on the attach's
	// goroutine and must not block.
	OnDownlink func(DownlinkEvent)
	// OnSMRequest is called with each PDU Session Establishment Request
	// the UE sends, as the plain 5GSM message that goes in the UL NAS
	// Transport's payload container, just before it is sent. It runs on
	// the attach's goroutine and must not block.
	OnSMRequest func(sm []byte)
	// OnNAS is called for every downlink NAS PDU the attach decodes, with
	// the message as the UE's NAS security context unwrapped it (MAC
	// verified, deciphered), or the decode error: errors.Is(err,
	// nas.ErrIntegrity) for a MAC that does not verify. With OnNAS set, a
	// PDU that does not decode is skipped instead of failing the attach;
	// the probe decides whether to close Stop. It runs on the attach's
	// goroutine and must not block.
	OnNAS func(NASEvent)
	// Stop, once closed, ends the attach where it would next wait for the
	// core. A REGISTERED UE's attach returns its Session without the PDU
	// sessions not yet established, for the caller to deregister; an
	// earlier one fails with ErrProbeStopped. A read in flight when Stop
	// closes is abandoned and the PDU it later takes is lost, so the next
	// step should be the deregistration.
	Stop <-chan struct{}
}

// NASEvent is one downlink NAS PDU the attach decoded (see
// AttachProbe.OnNAS).
type NASEvent struct {
	// Raw is the NAS PDU as received.
	Raw []byte
	// Type is the 5GMM message type read from the PDU's header without
	// decoding it (see DownlinkEvent.NASType), 0 when unreadable.
	Type uint8
	// Msg is the decoded plain message, nil when Err is set.
	Msg *f5nas.Message
	Err error
}

// ErrProbeStopped is the error Attach returns when AttachProbe.Stop ends
// it before the UE is REGISTERED.
var ErrProbeStopped = errors.New("the probe stopped the attach before registration")

// DownlinkEvent is one downlink NGAP message an AttachProbe saw.
type DownlinkEvent struct {
	At time.Time
	// Procedure names the NGAP procedure (procedureName), e.g.
	// "DownlinkNASTransport" or "UEContextRelease".
	Procedure string
	// NASType is the 5GMM message type of the NAS PDU the message carries
	// at its top level, 0 when there is none or it cannot be read.
	NASType uint8
	// NAS is that raw NAS PDU, nil when there is none. A PDU Session
	// Resource Setup Request's per-session NAS is not included.
	NAS []byte
}

// stopped reports whether the probe's Stop is closed.
func (s *attachState) stopped() bool {
	p := s.ueCfg.Probe
	if p == nil || p.Stop == nil {
		return false
	}
	select {
	case <-p.Stop:
		return true
	default:
		return false
	}
}

// readContext is ctx, cancelled too when the probe's Stop closes, for the
// attach's reads. The returned func releases it.
func (s *attachState) readContext(ctx context.Context) (context.Context, context.CancelFunc) {
	p := s.ueCfg.Probe
	if p == nil || p.Stop == nil {
		return ctx, func() {}
	}
	rctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-p.Stop:
			cancel()
		case <-rctx.Done():
		}
	}()
	return rctx, cancel
}

// withheld reports whether the probe withholds the uplink NAS message type.
func (s *attachState) withheld(uplink uint8) bool {
	return s.ueCfg.Probe != nil && s.ueCfg.Probe.Withhold[uplink]
}

// observe reports one downlink PDU to the probe and returns
// ErrReleasedByCore for a UE CONTEXT RELEASE COMMAND. A nil probe observes
// nothing.
func (s *attachState) observe(pdu *ngapType.NGAPPDU) error {
	p := s.ueCfg.Probe
	if p == nil {
		return nil
	}
	code, ok := procedureCode(pdu)
	ev := DownlinkEvent{At: time.Now(), Procedure: "unknown"}
	if ok {
		ev.Procedure = procedureName(code)
	}
	if dl, err := gnb.ParseDownlink(pdu); err == nil && len(dl.NASPDU) > 0 {
		ev.NAS = dl.NASPDU
		ev.NASType = nasMessageType(dl.NASPDU, s.readableCipher())
	}
	if p.OnDownlink != nil {
		p.OnDownlink(ev)
	}
	if ok && code == ngapType.ProcedureCodeUEContextRelease && pdu.Present == ngapType.NGAPPDUPresentInitiatingMessage {
		return ErrReleasedByCore
	}
	return nil
}

// procedureCode is the NGAP procedure code of pdu.
func procedureCode(pdu *ngapType.NGAPPDU) (int64, bool) {
	switch {
	case pdu.Present == ngapType.NGAPPDUPresentInitiatingMessage && pdu.InitiatingMessage != nil:
		return pdu.InitiatingMessage.ProcedureCode.Value, true
	case pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome && pdu.SuccessfulOutcome != nil:
		return pdu.SuccessfulOutcome.ProcedureCode.Value, true
	case pdu.Present == ngapType.NGAPPDUPresentUnsuccessfulOutcome && pdu.UnsuccessfulOutcome != nil:
		return pdu.UnsuccessfulOutcome.ProcedureCode.Value, true
	}
	return 0, false
}

// procedureName names the NGAP procedures an attach can see; others are
// "procedure N".
func procedureName(code int64) string {
	switch code {
	case ngapType.ProcedureCodeDownlinkNASTransport:
		return "DownlinkNASTransport"
	case ngapType.ProcedureCodeInitialContextSetup:
		return "InitialContextSetup"
	case ngapType.ProcedureCodeUEContextRelease:
		return "UEContextRelease"
	case ngapType.ProcedureCodePDUSessionResourceSetup:
		return "PDUSessionResourceSetup"
	case ngapType.ProcedureCodeErrorIndication:
		return "ErrorIndication"
	}
	return fmt.Sprintf("procedure %d", code)
}

// nasMessageType reads the 5GMM message type of a downlink NAS PDU without
// decoding it, so the security context's downlink count is not consumed
// twice. A plain message (security header type 0) has its type at offset 2.
// A security-protected one carries the inner message from offset 7, its
// type at offset 9: readable when only integrity-protected (types 1 and 3),
// or when ciphered (types 2 and 4) with NEA0 selected (nea0). A ciphered
// message under another algorithm, a short or malformed PDU, or a non-5GMM
// one is 0; a probe that needs the type under another cipher reads
// NASEvent.Msg, which the security context deciphered.
func nasMessageType(raw []byte, nea0 bool) uint8 {
	const epd5GMM = 0x7e
	if len(raw) < 3 || raw[0] != epd5GMM {
		return 0
	}
	switch raw[1] & 0x0f {
	case nas.SecHdrPlain:
		return raw[2]
	case nas.SecHdrIntegrity, nas.SecHdrIntegrityNewContext:
	case nas.SecHdrIntegrityCiphered, nas.SecHdrIntegrityCipheredNewCtx:
		if !nea0 {
			return 0
		}
	default:
		return 0
	}
	if len(raw) < 10 || raw[7] != epd5GMM || raw[8]&0x0f != nas.SecHdrPlain {
		return 0
	}
	return raw[9]
}
