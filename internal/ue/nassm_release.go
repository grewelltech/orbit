package ue

import (
	"errors"
	"fmt"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"
)

// BuildPDUSessionReleaseRequest builds the 5GSM PDU SESSION RELEASE REQUEST
// (TS 24.501 §8.3.12) for session id under procedure transaction identity
// pti, with no optional IE.
func BuildPDUSessionReleaseRequest(id, pti uint8) ([]byte, error) {
	m := f5nas.NewMessage()
	m.GsmMessage = f5nas.NewGsmMessage()
	m.GsmHeader.SetMessageType(f5nas.MsgTypePDUSessionReleaseRequest)
	req := nasMessage.NewPDUSessionReleaseRequest(f5nas.MsgTypePDUSessionReleaseRequest)
	req.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSSessionManagementMessage)
	req.SetPDUSessionID(id)
	req.SetPTI(pti)
	req.SetMessageType(f5nas.MsgTypePDUSessionReleaseRequest)
	m.PDUSessionReleaseRequest = req
	return m.PlainNasEncode()
}

// BuildPDUSessionReleaseComplete builds the 5GSM PDU SESSION RELEASE
// COMPLETE (TS 24.501 §8.3.15) that acknowledges a PDU SESSION RELEASE
// COMMAND for session id, echoing the command's pti.
func BuildPDUSessionReleaseComplete(id, pti uint8) ([]byte, error) {
	m := f5nas.NewMessage()
	m.GsmMessage = f5nas.NewGsmMessage()
	m.GsmHeader.SetMessageType(f5nas.MsgTypePDUSessionReleaseComplete)
	c := nasMessage.NewPDUSessionReleaseComplete(f5nas.MsgTypePDUSessionReleaseComplete)
	c.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSSessionManagementMessage)
	c.SetPDUSessionID(id)
	c.SetPTI(pti)
	c.SetMessageType(f5nas.MsgTypePDUSessionReleaseComplete)
	m.PDUSessionReleaseComplete = c
	return m.PlainNasEncode()
}

// BuildULNASTransportForSM wraps a 5GSM message for an established PDU
// session in a 5GMM UL NAS Transport (TS 24.501 §8.2.10): payload container
// type N1 SM information and the PDU session ID, with no request type, DNN
// or S-NSSAI, which only a request for a new session carries. Returns the
// plain 5GMM message for the caller to wrap in the NAS security context.
func BuildULNASTransportForSM(sm []byte, id uint8) (*f5nas.Message, error) {
	if len(sm) == 0 {
		return nil, fmt.Errorf("empty 5GSM payload")
	}
	m, gmm := newGmm(f5nas.MsgTypeULNASTransport)
	t := nasMessage.NewULNASTransport(f5nas.MsgTypeULNASTransport)
	t.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSMobilityManagementMessage)
	t.SetSecurityHeaderType(f5nas.SecurityHeaderTypePlainNas)
	t.SetSpareHalfOctet(0)
	t.SetMessageType(f5nas.MsgTypeULNASTransport)
	t.SetPayloadContainerType(nasMessage.PayloadContainerTypeN1SMInfo)
	t.PayloadContainer.SetLen(uint16(len(sm)))
	t.SetPayloadContainerContents(sm)
	pid := nasType.NewPduSessionID2Value(nasMessage.ULNASTransportPduSessionID2ValueType)
	pid.SetPduSessionID2Value(id)
	t.PduSessionID2Value = pid
	gmm.ULNASTransport = t
	return m, nil
}

// SMHeader is the fixed header every 5GSM message starts with (TS 24.501
// §9.1.1), and the 5GSM cause of the release messages that carry one.
type SMHeader struct {
	PDUSessionID uint8
	PTI          uint8
	MessageType  uint8
	// Cause is the 5GSM cause of a PDU SESSION RELEASE COMMAND or REJECT;
	// zero for any other message.
	Cause uint8
}

// ErrNot5GSM reports a payload that is not a 5GSM message.
var ErrNot5GSM = errors.New("not a 5GSM message")

// ParseSMHeader reads the header of a plain 5GSM message, and the 5GSM
// cause of a PDU SESSION RELEASE COMMAND or REJECT (the octet after the
// header). A release message too short to hold its cause is an error.
func ParseSMHeader(sm []byte) (SMHeader, error) {
	if len(sm) < 4 || sm[0] != nasMessage.Epd5GSSessionManagementMessage {
		return SMHeader{}, ErrNot5GSM
	}
	h := SMHeader{PDUSessionID: sm[1], PTI: sm[2], MessageType: sm[3]}
	if h.MessageType == f5nas.MsgTypePDUSessionReleaseCommand || h.MessageType == f5nas.MsgTypePDUSessionReleaseReject {
		if len(sm) < 5 {
			return SMHeader{}, fmt.Errorf("5GSM message type %#x without its 5GSM cause", h.MessageType)
		}
		h.Cause = sm[4]
	}
	return h, nil
}
