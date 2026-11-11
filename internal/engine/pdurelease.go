package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
)

const (
	// DefaultPDUReleaseWait is how long ReleasePDUSession waits for the
	// network's answer when the caller gives no bound: 16 s, the default of
	// the UE's T3582 (TS 24.501 Table 10.3.1).
	DefaultPDUReleaseWait = 16 * time.Second
	// DefaultPDUReleaseSettle is how long, after the first of the NAS and
	// NGAP release commands, ReleasePDUSession waits for the other.
	DefaultPDUReleaseSettle = 2 * time.Second
	// pduReleasePTI is the procedure transaction identity of the release
	// request. The session's establishment used PTI 0, and no other 5GSM
	// transaction of the UE is open, so 1 is not in use.
	pduReleasePTI = 1
)

// PDUReleaseOption adjusts Manager.ReleasePDUSession.
type PDUReleaseOption func(*pduReleaseOptions)

type pduReleaseOptions struct {
	wait, settle time.Duration
}

// WithPDUReleaseWait bounds the wait for the network's answer to the PDU
// SESSION RELEASE REQUEST at d; zero or less keeps DefaultPDUReleaseWait.
func WithPDUReleaseWait(d time.Duration) PDUReleaseOption {
	return func(o *pduReleaseOptions) { o.wait = d }
}

// WithPDUReleaseSettle bounds, at d, the wait for the second of the NAS and
// NGAP release commands once the first has come; zero or less keeps
// DefaultPDUReleaseSettle.
func WithPDUReleaseSettle(d time.Duration) PDUReleaseOption {
	return func(o *pduReleaseOptions) { o.settle = d }
}

// PDUReleaseResult reports what the network answered to a UE-requested PDU
// session release.
type PDUReleaseResult struct {
	// PDUSessionID and PTI identify the request; DLTEID is the gNB's
	// downlink TEID of the session as it was established.
	PDUSessionID, PTI uint8
	DLTEID            uint32
	// SentAt is when the PDU SESSION RELEASE REQUEST was sent.
	SentAt time.Time

	// NASCommand reports a PDU SESSION RELEASE COMMAND for the session,
	// read through the UE's NAS security context at NASCommandAt, with the
	// PTI and 5GSM cause it carried. NASInResourceRelease says it came as
	// the NAS-PDU of the PDU SESSION RESOURCE RELEASE COMMAND rather than
	// in a DOWNLINK NAS TRANSPORT. CompleteSent reports that the PDU
	// SESSION RELEASE COMPLETE went out in answer.
	NASCommand           bool
	NASCommandAt         time.Time
	CommandPTI           uint8
	CommandCause         uint8
	NASInResourceRelease bool
	CompleteSent         bool

	// Rejected reports a PDU SESSION RELEASE REJECT for the session, with
	// its 5GSM cause; it ends the procedure.
	Rejected    bool
	RejectCause uint8

	// ResourceRelease reports a PDU SESSION RESOURCE RELEASE COMMAND that
	// lists the session, at ResourceReleaseAt, answered with a PDU SESSION
	// RESOURCE RELEASE RESPONSE.
	ResourceRelease   bool
	ResourceReleaseAt time.Time

	// DecodeErr is the first downlink NAS PDU of the wait that did not
	// decode, if any; such a PDU is skipped.
	DecodeErr error
}

// Released reports that the network released the session on both N1 and
// N2: the NAS command and the NGAP command both came.
func (r *PDUReleaseResult) Released() bool { return r.NASCommand && r.ResourceRelease }

// ReleasePDUSession runs the UE-requested PDU session release procedure
// (TS 24.501 6.4.3) for the UE's first PDU session: it sends a PDU SESSION
// RELEASE REQUEST and waits for the PDU SESSION RELEASE COMMAND (answered
// with a PDU SESSION RELEASE COMPLETE) and the PDU SESSION RESOURCE RELEASE
// COMMAND (answered with a PDU SESSION RESOURCE RELEASE RESPONSE), in
// either order, or for a PDU SESSION RELEASE REJECT. A wait that runs out,
// or ctx ending, returns what was seen with a nil error; the error is for a
// request that could not be sent and for the association failing.
//
// It is signalling only. The session's entry in the UE's AttachResult and
// its N3 data path are left as they are until Deregister, so a caller can
// watch whether the core still sends downlink on the released tunnel. It
// holds the UE's procedure lock, like a handover or a deregistration, and
// expects the UE on an association of its own, as Register gives it. A
// CM-IDLE UE is refused before anything is sent: it has no NAS signalling
// connection to carry the request, and ServiceRequest restores one.
func (m *Manager) ReleasePDUSession(ctx context.Context, supi string, opts ...PDUReleaseOption) (*PDUReleaseResult, error) {
	o := pduReleaseOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.wait <= 0 {
		o.wait = DefaultPDUReleaseWait
	}
	if o.settle <= 0 {
		o.settle = DefaultPDUReleaseSettle
	}
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("UE %s is not registered", supi)
	}
	release, err := sess.beginProcedure(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if sess.Idle() {
		return nil, fmt.Errorf("UE %s is CM-IDLE: a UE-requested PDU session release needs a connected UE", supi)
	}
	return sess.releasePDUSession(ctx, o)
}

// releasePDUSession is ReleasePDUSession on a session whose procedure lock
// the caller holds.
func (s *Session) releasePDUSession(ctx context.Context, o pduReleaseOptions) (*PDUReleaseResult, error) {
	if s.Result == nil || len(s.Result.Sessions) == 0 {
		return nil, fmt.Errorf("UE %s has no PDU session", s.SUPI)
	}
	if s.sec == nil {
		return nil, fmt.Errorf("no security context")
	}
	sr := s.Result.Sessions[0]
	res := &PDUReleaseResult{PDUSessionID: sr.PDUSessionID, PTI: pduReleasePTI, DLTEID: sr.DLTEID}

	// A downlink PDU already waiting predates the request and cannot
	// answer it.
	if d, ok := s.conn.(interface{ Drain() int }); ok {
		d.Drain()
	}
	s.dropHeld()
	sm, err := ue.BuildPDUSessionReleaseRequest(res.PDUSessionID, res.PTI)
	if err != nil {
		return nil, err
	}
	if err := s.sendSM(sm, res.PDUSessionID); err != nil {
		return nil, fmt.Errorf("send PDU Session Release Request: %w", err)
	}
	res.SentAt = time.Now()

	deadline := res.SentAt.Add(o.wait)
	settling := false
	for !res.Rejected && !res.Released() {
		rctx, cancel := context.WithDeadline(ctx, deadline)
		pdu, err := s.readPDU(rctx)
		timedOut := rctx.Err() != nil
		cancel()
		switch {
		case err == nil:
		case timedOut:
			return res, nil
		default:
			return res, fmt.Errorf("waiting for the PDU session release: %w", err)
		}
		if err := s.onReleaseDownlink(pdu, res); err != nil {
			return res, err
		}
		if !settling && (res.NASCommand || res.ResourceRelease) {
			settling = true
			if d := time.Now().Add(o.settle); d.Before(deadline) {
				deadline = d
			}
		}
	}
	return res, nil
}

// sendSM sends a 5GSM message of an established session in a secured UL
// NAS Transport.
func (s *Session) sendSM(sm []byte, id uint8) error {
	transport, err := ue.BuildULNASTransportForSM(sm, id)
	if err != nil {
		return err
	}
	wrapped, err := s.sec.EncodeSecure(transport, nas.SecHdrIntegrityCiphered)
	if err != nil {
		return fmt.Errorf("wrap UL NAS Transport: %w", err)
	}
	pdu, err := gnb.BuildUplinkNASTransport(s.ServingGNB(), s.amfID, s.ranID, wrapped)
	if err != nil {
		return err
	}
	return gnb.SendPDU(s.conn, ueStream, pdu)
}

// onReleaseDownlink handles one downlink PDU of a PDU session release: the
// NGAP release command is answered and recorded, with its NAS PDU if it has
// one; a Downlink NAS Transport's NAS PDU is read; anything else is
// skipped. The error is a send that failed.
func (s *Session) onReleaseDownlink(pdu *ngapType.NGAPPDU, res *PDUReleaseResult) error {
	if cmd, err := gnb.ParsePDUSessionResourceReleaseCommand(pdu); err == nil {
		at := time.Now()
		resp, err := gnb.BuildPDUSessionResourceReleaseResponse(s.amfID, s.ranID, cmd.PDUSessionIDs)
		if err != nil {
			return err
		}
		if err := gnb.SendPDU(s.conn, ueStream, resp); err != nil {
			return fmt.Errorf("send PDU Session Resource Release Response: %w", err)
		}
		for _, id := range cmd.PDUSessionIDs {
			if id == int64(res.PDUSessionID) && !res.ResourceRelease {
				res.ResourceRelease, res.ResourceReleaseAt = true, at
			}
		}
		if cmd.NASPDU == nil {
			return nil
		}
		return s.onReleaseNAS(cmd.NASPDU, true, res)
	}
	dl, err := gnb.ParseDownlink(pdu)
	if err != nil || dl.Procedure != ngapType.ProcedureCodeDownlinkNASTransport || len(dl.NASPDU) == 0 {
		return nil
	}
	return s.onReleaseNAS(dl.NASPDU, false, res)
}

// onReleaseNAS reads one downlink NAS PDU of a PDU session release. A DL
// NAS Transport carrying the session's PDU SESSION RELEASE COMMAND is
// recorded and answered with the COMPLETE; its PDU SESSION RELEASE REJECT
// is recorded; a PDU that does not decode is recorded in res.DecodeErr; any
// other is skipped. The error is a send that failed.
func (s *Session) onReleaseNAS(raw []byte, inNGAP bool, res *PDUReleaseResult) error {
	at := time.Now()
	sm, err := s.n1SMOf(raw)
	if err != nil {
		if res.DecodeErr == nil {
			res.DecodeErr = err
		}
		return nil
	}
	h, err := ue.ParseSMHeader(sm)
	if err != nil {
		if sm != nil && res.DecodeErr == nil && !errors.Is(err, ue.ErrNot5GSM) {
			res.DecodeErr = err
		}
		return nil
	}
	if h.PDUSessionID != res.PDUSessionID {
		return nil
	}
	switch h.MessageType {
	case f5nas.MsgTypePDUSessionReleaseReject:
		res.Rejected, res.RejectCause = true, h.Cause
	case f5nas.MsgTypePDUSessionReleaseCommand:
		if res.NASCommand {
			return nil
		}
		res.NASCommand, res.NASCommandAt, res.CommandPTI, res.CommandCause, res.NASInResourceRelease = true, at, h.PTI, h.Cause, inNGAP
		complete, err := ue.BuildPDUSessionReleaseComplete(h.PDUSessionID, h.PTI)
		if err != nil {
			return err
		}
		if err := s.sendSM(complete, h.PDUSessionID); err != nil {
			return fmt.Errorf("send PDU Session Release Complete: %w", err)
		}
		res.CompleteSent = true
	}
	return nil
}

// n1SMOf decodes a downlink NAS PDU, through the session's security
// context when it is protected, and returns the N1 SM payload of a DL NAS
// Transport: nil, with no error, for any other message.
func (s *Session) n1SMOf(raw []byte) ([]byte, error) {
	if len(raw) < 2 {
		return nil, fmt.Errorf("downlink NAS too short: %d bytes", len(raw))
	}
	var msg *f5nas.Message
	var err error
	if f5nas.GetSecurityHeaderType(raw) == f5nas.SecurityHeaderTypePlainNas {
		msg, err = nas.DecodePlain(raw)
	} else {
		msg, err = s.sec.DecodeSecure(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("downlink NAS did not decode: %w", err)
	}
	if msg.GmmMessage == nil || msg.DLNASTransport == nil ||
		msg.DLNASTransport.GetPayloadContainerType() != nasMessage.PayloadContainerTypeN1SMInfo {
		return nil, nil
	}
	return msg.DLNASTransport.GetPayloadContainerContents(), nil
}
