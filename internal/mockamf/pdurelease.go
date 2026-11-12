package mockamf

import (
	"fmt"

	"github.com/free5gc/aper"
	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/nas"
)

// UE-requested PDU session release, network side (TS 24.501 6.4.3, TS
// 23.502 4.3.4.2). The mock plays AMF and SMF: it answers a PDU SESSION
// RELEASE REQUEST with a PDU SESSION RESOURCE RELEASE COMMAND carrying the
// PDU SESSION RELEASE COMMAND, forgets the session and tells the UPF.

// PDUReleaseAnswer selects how the mock answers a PDU SESSION RELEASE
// REQUEST for a session it established.
type PDUReleaseAnswer int

const (
	// ReleaseInResourceRelease answers with a PDU SESSION RESOURCE RELEASE
	// COMMAND whose NAS-PDU is the PDU SESSION RELEASE COMMAND. The
	// default.
	ReleaseInResourceRelease PDUReleaseAnswer = iota
	// ReleaseSeparately answers with a PDU SESSION RESOURCE RELEASE COMMAND
	// without a NAS-PDU, then the PDU SESSION RELEASE COMMAND in a DOWNLINK
	// NAS TRANSPORT.
	ReleaseSeparately
	// ReleaseNASOnly sends the PDU SESSION RELEASE COMMAND in a DOWNLINK
	// NAS TRANSPORT and no PDU SESSION RESOURCE RELEASE COMMAND.
	ReleaseNASOnly
	// ReleaseNGAPOnly sends the PDU SESSION RESOURCE RELEASE COMMAND
	// without a NAS-PDU, and no PDU SESSION RELEASE COMMAND at all.
	ReleaseNGAPOnly
	// RejectPDURelease answers with a PDU SESSION RELEASE REJECT, 5GSM
	// cause #43 "invalid PDU session identity", and keeps the session.
	RejectPDURelease
	// IgnorePDURelease leaves the request unanswered and keeps the session.
	IgnorePDURelease
	// DropOnPDURelease answers with nothing and ends every association
	// (DropAssociations); the mock goes on serving new ones.
	DropOnPDURelease
	// CrashOnPDURelease answers with nothing and crashes the mock (Crash).
	CrashOnPDURelease
)

// WithPDUReleaseAnswer makes the mock answer a PDU SESSION RELEASE REQUEST
// as answer says.
func WithPDUReleaseAnswer(answer PDUReleaseAnswer) Option {
	return func(a *AMF) { a.pduReleaseAnswer = answer }
}

// WithPDUReleaseKeepingUserPlane makes the mock release a session without
// telling the UPF, as a core whose SMF leaves the N4 session in place.
func WithPDUReleaseKeepingUserPlane() Option {
	return func(a *AMF) { a.pduReleaseKeepsUP = true }
}

// SessionReleaser is a UPF that is told when a session is released, the way
// an SMF's N4 Session Release tells a real one. A UPF that does not
// implement it is not told.
type SessionReleaser interface {
	// SessionReleased is called after the release has been sent to the
	// gNB, with the session as it was. It must not block.
	SessionReleased(s SessionInfo)
}

// PDUReleaseRequests reports how many PDU SESSION RELEASE REQUESTs the mock
// has received.
func (a *AMF) PDUReleaseRequests() int64 { return a.pduReleaseReqs.Load() }

// PDUReleaseCompletes reports how many PDU SESSION RELEASE COMPLETEs the
// mock has received.
func (a *AMF) PDUReleaseCompletes() int64 { return a.pduReleaseCompletes.Load() }

// PDUResourceReleaseResponses reports how many PDU SESSION RESOURCE RELEASE
// RESPONSEs the mock has received.
func (a *AMF) PDUResourceReleaseResponses() int64 { return a.pduResourceReleaseResps.Load() }

// cause5GSMRegularDeactivation and cause5GSMInvalidPDUSessionIdentity are
// 5GSM causes #36 and #43 (TS 24.501 9.11.4.2).
const (
	cause5GSMRegularDeactivation       = 36
	cause5GSMInvalidPDUSessionIdentity = 43
)

// onPDUSessionReleaseRequest answers a PDU SESSION RELEASE REQUEST as
// WithPDUReleaseAnswer selects. A request for a session the UE does not
// have is ignored.
func (a *AMF) onPDUSessionReleaseRequest(p *pipe, st *ueState, req *nasMessage.PDUSessionReleaseRequest) error {
	a.pduReleaseReqs.Add(1)
	id, pti := req.GetPDUSessionID(), req.GetPTI()
	a.mu.Lock()
	s := st.sessions[id]
	var info SessionInfo
	if s != nil {
		info = SessionInfo{SUPI: st.supi, PDUSessionID: s.id, UEIP: s.ueIP, QFI: defaultQFI, UL: s.ul, DL: s.dl}
	}
	amfID, ranID := st.amfID, st.ranID
	a.mu.Unlock()
	switch a.pduReleaseAnswer {
	case DropOnPDURelease:
		a.DropAssociations()
		return nil
	case CrashOnPDURelease:
		a.Crash()
		return errCrashed
	}
	if s == nil || a.pduReleaseAnswer == IgnorePDURelease {
		return nil
	}

	nasPDU := func(mt, cause uint8) ([]byte, error) {
		sm, err := buildPDUSessionReleaseAnswer(mt, id, pti, cause)
		if err != nil {
			return nil, err
		}
		dl, err := buildDLNASTransportN1SM(sm, id)
		if err != nil {
			return nil, err
		}
		return st.encodeSecure(dl, nas.SecHdrIntegrityCiphered)
	}
	if a.pduReleaseAnswer == RejectPDURelease {
		rej, err := nasPDU(f5nas.MsgTypePDUSessionReleaseReject, cause5GSMInvalidPDUSessionIdentity)
		if err != nil {
			return err
		}
		return a.send(p, buildDownlinkNASTransport(amfID, ranID, rej))
	}

	var piggybacked []byte
	if a.pduReleaseAnswer == ReleaseInResourceRelease {
		var err error
		if piggybacked, err = nasPDU(f5nas.MsgTypePDUSessionReleaseCommand, cause5GSMRegularDeactivation); err != nil {
			return err
		}
	}
	if a.pduReleaseAnswer != ReleaseNASOnly {
		cmd, err := buildPDUSessionResourceReleaseCommand(amfID, ranID, id, piggybacked)
		if err != nil {
			return err
		}
		if err := a.send(p, cmd); err != nil {
			return err
		}
	}
	if a.pduReleaseAnswer == ReleaseSeparately || a.pduReleaseAnswer == ReleaseNASOnly {
		cmd, err := nasPDU(f5nas.MsgTypePDUSessionReleaseCommand, cause5GSMRegularDeactivation)
		if err != nil {
			return err
		}
		if err := a.send(p, buildDownlinkNASTransport(amfID, ranID, cmd)); err != nil {
			return err
		}
	}

	a.mu.Lock()
	delete(st.sessions, id)
	a.mu.Unlock()
	if r, ok := a.cfg.UPF.(SessionReleaser); ok && !a.pduReleaseKeepsUP {
		r.SessionReleased(info)
	}
	return nil
}

// buildPDUSessionReleaseAnswer builds a plain 5GSM PDU SESSION RELEASE
// COMMAND or REJECT (mt; TS 24.501 §8.3.14, §8.3.13) for session id,
// echoing pti, with the 5GSM cause given.
func buildPDUSessionReleaseAnswer(mt, id, pti, cause uint8) ([]byte, error) {
	m := f5nas.NewMessage()
	m.GsmMessage = f5nas.NewGsmMessage()
	m.GsmHeader.SetMessageType(mt)
	switch mt {
	case f5nas.MsgTypePDUSessionReleaseCommand:
		c := nasMessage.NewPDUSessionReleaseCommand(mt)
		c.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSSessionManagementMessage)
		c.SetPDUSessionID(id)
		c.SetPTI(pti)
		c.SetMessageType(mt)
		c.SetCauseValue(cause)
		m.PDUSessionReleaseCommand = c
	case f5nas.MsgTypePDUSessionReleaseReject:
		r := nasMessage.NewPDUSessionReleaseReject(mt)
		r.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSSessionManagementMessage)
		r.SetPDUSessionID(id)
		r.SetPTI(pti)
		r.SetMessageType(mt)
		r.SetCauseValue(cause)
		m.PDUSessionReleaseReject = r
	default:
		return nil, fmt.Errorf("mock amf: 5GSM message type %#x is not a release answer", mt)
	}
	return m.PlainNasEncode()
}

// buildPDUSessionResourceReleaseCommand builds the AMF's PDU SESSION
// RESOURCE RELEASE COMMAND (TS 38.413 §9.2.1.3) for one session, with a
// transfer of cause NAS normal-release and nasPDU as its NAS-PDU when it is
// non-nil.
func buildPDUSessionResourceReleaseCommand(amfID, ranID int64, id uint8, nasPDU []byte) (ngapType.NGAPPDU, error) {
	transfer, err := aper.MarshalWithParams(ngapType.PDUSessionResourceReleaseCommandTransfer{
		Cause: ngapType.Cause{Present: ngapType.CausePresentNas, Nas: &ngapType.CauseNas{Value: ngapType.CauseNasPresentNormalRelease}},
	}, "valueExt")
	if err != nil {
		return ngapType.NGAPPDU{}, fmt.Errorf("mock amf encode release command transfer: %w", err)
	}
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodePDUSessionResourceRelease
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentPDUSessionResourceReleaseCommand
	cmd := new(ngapType.PDUSessionResourceReleaseCommand)
	pdu.InitiatingMessage.Value.PDUSessionResourceReleaseCommand = cmd
	add := func(ieID int64, crit aper.Enumerated, fill func(*ngapType.PDUSessionResourceReleaseCommandIEs)) {
		ie := ngapType.PDUSessionResourceReleaseCommandIEs{}
		ie.Id.Value = ieID
		ie.Criticality.Value = crit
		fill(&ie)
		cmd.ProtocolIEs.List = append(cmd.ProtocolIEs.List, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.PDUSessionResourceReleaseCommandIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceReleaseCommandIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.PDUSessionResourceReleaseCommandIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceReleaseCommandIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranID}
	})
	if nasPDU != nil {
		add(ngapType.ProtocolIEIDNASPDU, ngapType.CriticalityPresentIgnore, func(ie *ngapType.PDUSessionResourceReleaseCommandIEs) {
			ie.Value.Present = ngapType.PDUSessionResourceReleaseCommandIEsPresentNASPDU
			ie.Value.NASPDU = &ngapType.NASPDU{Value: nasPDU}
		})
	}
	add(ngapType.ProtocolIEIDPDUSessionResourceToReleaseListRelCmd, ngapType.CriticalityPresentReject, func(ie *ngapType.PDUSessionResourceReleaseCommandIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceReleaseCommandIEsPresentPDUSessionResourceToReleaseListRelCmd
		item := ngapType.PDUSessionResourceToReleaseItemRelCmd{}
		item.PDUSessionID.Value = int64(id)
		item.PDUSessionResourceReleaseCommandTransfer = transfer
		ie.Value.PDUSessionResourceToReleaseListRelCmd = &ngapType.PDUSessionResourceToReleaseListRelCmd{
			List: []ngapType.PDUSessionResourceToReleaseItemRelCmd{item},
		}
	})
	return pdu, nil
}
