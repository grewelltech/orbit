package gnb

import "github.com/free5gc/ngap/ngapType"

// BuildHandoverCancel builds the source gNB's HANDOVER CANCEL for a handover
// in preparation or prepared (TS 38.413 8.4.5), cause radio network
// "handover cancelled".
func BuildHandoverCancel(amfUENGAPID, ranUENGAPID int64) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeHandoverCancel
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentHandoverCancel
	pdu.InitiatingMessage.Value.HandoverCancel = new(ngapType.HandoverCancel)
	list := &pdu.InitiatingMessage.Value.HandoverCancel.ProtocolIEs.List

	amf := ngapType.HandoverCancelIEs{}
	amf.Id.Value = ngapType.ProtocolIEIDAMFUENGAPID
	amf.Criticality.Value = ngapType.CriticalityPresentReject
	amf.Value.Present = ngapType.HandoverCancelIEsPresentAMFUENGAPID
	amf.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfUENGAPID}
	ran := ngapType.HandoverCancelIEs{}
	ran.Id.Value = ngapType.ProtocolIEIDRANUENGAPID
	ran.Criticality.Value = ngapType.CriticalityPresentReject
	ran.Value.Present = ngapType.HandoverCancelIEsPresentRANUENGAPID
	ran.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranUENGAPID}
	cause := ngapType.HandoverCancelIEs{}
	cause.Id.Value = ngapType.ProtocolIEIDCause
	cause.Criticality.Value = ngapType.CriticalityPresentIgnore
	cause.Value.Present = ngapType.HandoverCancelIEsPresentCause
	cause.Value.Cause = &ngapType.Cause{
		Present:      ngapType.CausePresentRadioNetwork,
		RadioNetwork: &ngapType.CauseRadioNetwork{Value: ngapType.CauseRadioNetworkPresentHandoverCancelled},
	}
	*list = append(*list, amf, ran, cause)
	return pdu
}

// IsHandoverCancelAcknowledge reports whether pdu is a HANDOVER CANCEL
// ACKNOWLEDGE (TS 38.413 9.2.3.12).
func IsHandoverCancelAcknowledge(pdu *ngapType.NGAPPDU) bool {
	return pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome &&
		pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentHandoverCancelAcknowledge
}
