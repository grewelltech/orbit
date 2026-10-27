package mockamf

import (
	"fmt"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/nas"
)

// DNNAnswer is how the mock answers a PDU Session Establishment Request for
// the DNN named by WithUnknownDNN.
type DNNAnswer int

const (
	// AcceptDNN answers as for any other DNN: an Establishment Accept with
	// a UPF configured, nothing without one.
	AcceptDNN DNNAnswer = iota
	// RejectDNN answers as an SMF does (TS 24.501 6.4.1.4): a PDU Session
	// Establishment Reject in a DL NAS Transport with no 5GMM cause. The
	// 5GSM cause is #27 missing or unknown DNN unless one is given.
	RejectDNN
	// ReturnDNN answers as an AMF whose SMF selection failed (TS 24.501
	// 5.4.5.3.2 case e)): a DL NAS Transport whose payload container is the
	// un-forwarded request. The 5GMM cause is #91 DNN not supported or not
	// subscribed in the slice unless one is given.
	ReturnDNN
	// ReturnNoCauseDNN is ReturnDNN without the 5GMM cause IE.
	ReturnNoCauseDNN
	// CongestionDNN answers as an AMF applying congestion control (TS 24.501
	// 5.4.5.3.1 case f)): ReturnDNN with 5GMM cause #22 congestion unless
	// one is given, and a Back-off timer value IE of 1 minute.
	CongestionDNN
	// ReturnWrongPayloadDNN is ReturnDNN with an AMF-built PDU Session
	// Establishment Reject as the payload instead of the request, its 5GSM
	// cause set to the 5GMM cause.
	ReturnWrongPayloadDNN
	// ReturnOtherPayloadDNN is ReturnDNN with a 5GSM STATUS (cause #111
	// protocol error, unspecified) as the payload instead of the request.
	ReturnOtherPayloadDNN
	// StatusDNN answers with a 5GSM STATUS in a DL NAS Transport with no
	// 5GMM cause. The 5GSM cause is #96 invalid mandatory information
	// unless one is given.
	StatusDNN
	// PlainAcceptDNN answers with a PDU Session Establishment Accept in a
	// DL NAS Transport alone, with no PDU Session Resource Setup Request.
	PlainAcceptDNN
	// UndecodableDNN answers with a protected NAS message whose MAC
	// verifies but whose plain message is a truncated DL NAS Transport.
	UndecodableDNN
	// BadMACDNN is RejectDNN with the message's MAC corrupted.
	BadMACDNN
	// IgnoreDNN sends nothing back.
	IgnoreDNN
)

// WithUnknownDNN makes the mock answer a PDU Session Establishment Request
// for dnn with answer. cause is the 5GSM cause of RejectDNN, BadMACDNN and
// StatusDNN, or the 5GMM cause of the Return and Congestion answers; zero
// takes the default (#27, #96, #91 and, for CongestionDNN, #22). Requests for other DNNs are answered as
// before.
func WithUnknownDNN(dnn string, answer DNNAnswer, cause uint8) Option {
	return func(a *AMF) { a.unknownDNN, a.dnnAnswer, a.dnnCause = dnn, answer, cause }
}

// answerUnknownDNN sends the WithUnknownDNN answer to a request for PDU
// session id with procedure transaction pti; request is the 5GSM message
// the UE sent.
func (a *AMF) answerUnknownDNN(p *pipe, st *ueState, id, pti uint8, request []byte) error {
	var payload []byte
	var mmCause uint8
	var err error
	routing := causeOr(a.dnnCause, nasMessage.Cause5GMMDNNNotSupportedOrNotSubscribedInTheSlice)
	switch a.dnnAnswer {
	case RejectDNN, BadMACDNN:
		payload, err = buildPDUSessionEstablishmentReject(id, pti, causeOr(a.dnnCause, nasMessage.Cause5GSMMissingOrUnknownDNN))
	case ReturnDNN:
		payload, mmCause = append([]byte(nil), request...), routing
	case ReturnNoCauseDNN:
		payload = append([]byte(nil), request...)
	case CongestionDNN:
		payload, mmCause = append([]byte(nil), request...), causeOr(a.dnnCause, nasMessage.Cause5GMMCongestion)
	case ReturnWrongPayloadDNN:
		mmCause = routing
		payload, err = buildPDUSessionEstablishmentReject(id, pti, mmCause)
	case ReturnOtherPayloadDNN:
		mmCause = routing
		payload, err = buildStatus5GSM(id, pti, nasMessage.Cause5GSMProtocolErrorUnspecified)
	case StatusDNN:
		payload, err = buildStatus5GSM(id, pti, causeOr(a.dnnCause, nasMessage.Cause5GSMInvalidMandatoryInformation))
	case PlainAcceptDNN:
		payload, err = buildPDUSessionEstablishmentAccept(id, pti, a.nextUEIP(), a.cfg.SST, a.sdBytes, a.unknownDNN)
	case UndecodableDNN:
	case IgnoreDNN:
		return nil
	default:
		return fmt.Errorf("mock amf: unknown DNN answer %d", a.dnnAnswer)
	}
	if err != nil {
		return err
	}
	var wrapped []byte
	if a.dnnAnswer == UndecodableDNN {
		// A DL NAS Transport header with no payload container.
		wrapped, err = st.encodeSecureRaw([]byte{nasMessage.Epd5GSMobilityManagementMessage, 0x00, f5nas.MsgTypeDLNASTransport}, nas.SecHdrIntegrityCiphered)
	} else {
		var dl *f5nas.Message
		if dl, err = buildDLNASTransportN1SM(payload, id); err != nil {
			return err
		}
		if mmCause != 0 {
			c := nasType.NewCause5GMM(nasMessage.DLNASTransportCause5GMMType)
			c.SetCauseValue(mmCause)
			dl.DLNASTransport.Cause5GMM = c
		}
		if a.dnnAnswer == CongestionDNN {
			b := nasType.NewBackoffTimerValue(nasMessage.DLNASTransportBackoffTimerValueType)
			b.SetLen(1)
			b.SetUnitTimerValue(0x05) // 1 minute (TS 24.008 10.5.7.4a)
			b.SetTimerValue(1)
			dl.DLNASTransport.BackoffTimerValue = b
		}
		wrapped, err = st.encodeSecure(dl, nas.SecHdrIntegrityCiphered)
	}
	if err != nil {
		return err
	}
	if a.dnnAnswer == BadMACDNN {
		wrapped[2] ^= 0xff
	}
	a.mu.Lock()
	amfID, ranID := st.amfID, st.ranID
	a.mu.Unlock()
	return a.send(p, buildDownlinkNASTransport(amfID, ranID, wrapped))
}

// causeOr is c, or def when c is zero.
func causeOr(c, def uint8) uint8 {
	if c == 0 {
		return def
	}
	return c
}

// buildPDUSessionEstablishmentReject builds the 5GSM PDU Session
// Establishment Reject (TS 24.501 8.3.3) with the 5GSM cause.
func buildPDUSessionEstablishmentReject(id, pti, cause uint8) ([]byte, error) {
	m := f5nas.NewMessage()
	m.GsmMessage = f5nas.NewGsmMessage()
	m.GsmHeader.SetMessageType(f5nas.MsgTypePDUSessionEstablishmentReject)
	rej := nasMessage.NewPDUSessionEstablishmentReject(f5nas.MsgTypePDUSessionEstablishmentReject)
	rej.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSSessionManagementMessage)
	rej.SetPDUSessionID(id)
	rej.SetPTI(pti)
	rej.SetMessageType(f5nas.MsgTypePDUSessionEstablishmentReject)
	rej.SetCauseValue(cause)
	m.PDUSessionEstablishmentReject = rej
	return m.PlainNasEncode()
}

// buildStatus5GSM builds a 5GSM STATUS (TS 24.501 8.3.16) with the 5GSM
// cause.
func buildStatus5GSM(id, pti, cause uint8) ([]byte, error) {
	m := f5nas.NewMessage()
	m.GsmMessage = f5nas.NewGsmMessage()
	m.GsmHeader.SetMessageType(f5nas.MsgTypeStatus5GSM)
	st := nasMessage.NewStatus5GSM(f5nas.MsgTypeStatus5GSM)
	st.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSSessionManagementMessage)
	st.SetPDUSessionID(id)
	st.SetPTI(pti)
	st.SetMessageType(f5nas.MsgTypeStatus5GSM)
	st.SetCauseValue(cause)
	m.Status5GSM = st
	return m.PlainNasEncode()
}
