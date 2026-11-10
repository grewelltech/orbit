package mockamf

import (
	"github.com/free5gc/ngap/ngapType"
)

// UndecodableAnswer is how the mock reacts to an uplink NGAP PDU that does
// not decode, a transfer syntax error (WithUndecodablePDU).
type UndecodableAnswer int

const (
	// CloseOnUndecodable ends the association (the default); other
	// associations carry on.
	CloseOnUndecodable UndecodableAnswer = iota
	// ErrorIndicationOnUndecodable answers with an ERROR INDICATION, cause
	// Protocol transfer-syntax-error (TS 38.413 10.2), and keeps the
	// association.
	ErrorIndicationOnUndecodable
	// IgnoreUndecodable drops the PDU and keeps the association.
	IgnoreUndecodable
	// CrashOnUndecodable ends the association and every other one, and
	// answers nothing afterwards: a core the PDU brought down.
	CrashOnUndecodable
)

// WithUndecodablePDU sets how the mock reacts to an uplink NGAP PDU that
// does not decode.
func WithUndecodablePDU(answer UndecodableAnswer) Option {
	return func(a *AMF) { a.undecodable = answer }
}

// onUndecodable reacts to an uplink PDU that failed to decode with
// decodeErr. A non-nil return ends the association.
func (a *AMF) onUndecodable(p *pipe, decodeErr error) error {
	switch a.undecodable {
	case ErrorIndicationOnUndecodable:
		return a.send(p, buildErrorIndicationTransferSyntax())
	case IgnoreUndecodable:
		return nil
	case CrashOnUndecodable:
		a.crashed.Store(true)
		return decodeErr
	default:
		return decodeErr
	}
}

// buildErrorIndicationTransferSyntax builds an ERROR INDICATION (TS 38.413
// 9.2.7.1) carrying only the Cause IE, Protocol transfer-syntax-error.
func buildErrorIndicationTransferSyntax() ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeErrorIndication
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentIgnore
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentErrorIndication
	pdu.InitiatingMessage.Value.ErrorIndication = new(ngapType.ErrorIndication)
	ie := ngapType.ErrorIndicationIEs{}
	ie.Id.Value = ngapType.ProtocolIEIDCause
	ie.Criticality.Value = ngapType.CriticalityPresentIgnore
	ie.Value.Present = ngapType.ErrorIndicationIEsPresentCause
	ie.Value.Cause = &ngapType.Cause{Present: ngapType.CausePresentProtocol,
		Protocol: &ngapType.CauseProtocol{Value: ngapType.CauseProtocolPresentTransferSyntaxError}}
	pdu.InitiatingMessage.Value.ErrorIndication.ProtocolIEs.List = append(
		pdu.InitiatingMessage.Value.ErrorIndication.ProtocolIEs.List, ie)
	return pdu
}
