package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
)

// ErrReleasedByCore is the error Attach returns, with a probe set, when the
// core sends a UE CONTEXT RELEASE COMMAND for the UE during the attach.
// No UE CONTEXT RELEASE COMPLETE is sent: the caller closes the association.
var ErrReleasedByCore = errors.New("the core released the UE context during the attach")

// ErrRegistrationRejected is the error Attach returns when the core answers
// with a REGISTRATION REJECT that decoded (and, when protected, verified).
var ErrRegistrationRejected = errors.New("AMF rejected registration")

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
	// CleanUpPlainAccept, when positive, ends the attach at a Registration
	// Accept sent without NAS security (security header type 0) with a
	// *PlainAcceptError, after cleaning up what it can: the UE sends a
	// plain switch-off Deregistration Request, waits up to
	// CleanUpPlainAccept for a UE CONTEXT RELEASE COMMAND, and without one
	// sends a UE CONTEXT RELEASE REQUEST and waits as long again. A command
	// is answered with UE CONTEXT RELEASE COMPLETE. Zero leaves a plain
	// Registration Accept to the attach.
	CleanUpPlainAccept time.Duration
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

// ErrDownlinkNASDecode wraps the error Attach returns when a downlink NAS
// PDU does not decode (and no probe asked to skip it).
var ErrDownlinkNASDecode = errors.New("decode downlink NAS")

// PlainAcceptError is the error Attach returns when
// AttachProbe.CleanUpPlainAccept ended it at a Registration Accept sent
// without NAS security.
type PlainAcceptError struct {
	// Steps is what the cleanup did, in order, for example "plain
	// switch-off Deregistration Request sent".
	Steps []string
}

func (e *PlainAcceptError) Error() string {
	return "Registration Accept without NAS security; cleanup: " + strings.Join(e.Steps, "; ")
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
		ev.NASType = nasMessageType(dl.NASPDU, s.sec != nil && s.sec.CipheringAlg == nas.NEA0)
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
// one is 0.
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

// cleanUpPlainAccept answers a Registration Accept sent without NAS
// security under AttachProbe.CleanUpPlainAccept (see there) and returns the
// *PlainAcceptError that ends the attach. With no security context the
// Deregistration Request goes in the clear, which TS 24.501 4.4.4.3 allows
// (item f) and NOTE 2), and names the UE by the 5G-GUTI
// the Accept assigned, else by its SUCI, with ngKSI "no key is available".
// The last read of the cleanup ends when the caller closes the association,
// which it does when the attach fails.
func (s *attachState) cleanUpPlainAccept(acc *f5nas.Message) error {
	e := &PlainAcceptError{}
	wait := s.ueCfg.Probe.CleanUpPlainAccept
	done := make(chan struct{})
	defer close(done)
	pdus := s.readPDUs(done)
	// The cleanup runs to its own bound even if the attach's context ends
	// as the Accept arrives (a caller's wait running out), so the
	// deregistration and the release request still go out.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 2*wait+time.Second)
	defer cancel()
	id, err := s.ueCfg.Identity.EncodeNullSUCI()
	if g, gerr := ue.ParseRegistrationAccept(acc); gerr == nil && g != nil {
		id, err = g.Raw, nil
	}
	if err == nil {
		err = s.sendPlainDeregistration(id)
	}
	if err != nil {
		e.Steps = append(e.Steps, "Deregistration Request not sent: "+err.Error())
	} else {
		e.Steps = append(e.Steps, "plain switch-off Deregistration Request sent")
		released, rerr := s.awaitRelease(ctx, pdus, wait)
		if rerr != nil {
			e.Steps = append(e.Steps, rerr.Error())
			return e
		}
		if released {
			e.Steps = append(e.Steps, "the core released the UE context (UE Context Release Complete sent)")
			return e
		}
		e.Steps = append(e.Steps, fmt.Sprintf("no UE Context Release Command within %s", wait))
	}
	if err := gnb.SendPDU(s.conn, ueStream, gnb.BuildUEContextReleaseRequest(s.amfID, s.ranID)); err != nil {
		e.Steps = append(e.Steps, "UE Context Release Request not sent: "+err.Error())
		return e
	}
	e.Steps = append(e.Steps, "UE Context Release Request sent")
	released, err := s.awaitRelease(ctx, pdus, wait)
	switch {
	case err != nil:
		e.Steps = append(e.Steps, err.Error())
	case released:
		e.Steps = append(e.Steps, "the core released the UE context (UE Context Release Complete sent)")
	default:
		e.Steps = append(e.Steps, fmt.Sprintf("no UE Context Release Command within %s; the association is closed with the UE context still held", wait))
	}
	return e
}

// sendPlainDeregistration sends a switch-off Deregistration Request without
// NAS security, naming the UE by id (a 5GS mobile identity).
func (s *attachState) sendPlainDeregistration(id []byte) error {
	m, err := ue.BuildDeregistrationRequest(id, true)
	if err != nil {
		return err
	}
	req := m.DeregistrationRequestUEOriginatingDeregistration
	req.SetNasKeySetIdentifiler(noKeyAvailable)
	raw, err := m.PlainNasEncode()
	if err != nil {
		return err
	}
	pdu, err := gnb.BuildUplinkNASTransport(s.gnbCfg, s.amfID, s.ranID, raw)
	if err != nil {
		return err
	}
	return gnb.SendPDU(s.conn, ueStream, pdu)
}

// noKeyAvailable is the ngKSI value "no key is available" (TS 24.501
// 9.11.3.32).
const noKeyAvailable = 7

// pduResult is one read from the association.
type pduResult struct {
	pdu *ngapType.NGAPPDU
	err error
}

// readPDUs reads downlink PDUs on one goroutine until a read fails or done
// closes, so a wait that runs out never leaves a read behind to swallow the
// next PDU.
func (s *attachState) readPDUs(done <-chan struct{}) <-chan pduResult {
	ch := make(chan pduResult)
	go func() {
		for {
			pdu, err := gnb.ReadPDU(context.Background(), s.conn)
			select {
			case ch <- pduResult{pdu, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// awaitRelease takes downlink PDUs from pdus, each reported to the probe,
// for up to wait, or until ctx ends. It answers a UE CONTEXT RELEASE COMMAND with UE CONTEXT
// RELEASE COMPLETE and reports true; the wait running out is false with no
// error.
func (s *attachState) awaitRelease(ctx context.Context, pdus <-chan pduResult, wait time.Duration) (bool, error) {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("waiting for a UE Context Release Command: %w", ctx.Err())
		case <-t.C:
			return false, nil
		case r := <-pdus:
			if r.err != nil {
				return false, fmt.Errorf("reading for a UE Context Release Command: %w", r.err)
			}
			if !errors.Is(s.observe(r.pdu), ErrReleasedByCore) {
				continue
			}
			if err := gnb.SendPDU(s.conn, ueStream, gnb.BuildUEContextReleaseComplete(s.amfID, s.ranID)); err != nil {
				return true, fmt.Errorf("UE Context Release Complete not sent: %w", err)
			}
			return true, nil
		}
	}
}
