package gnb

import "github.com/free5gc/ngap/ngapType"

// BuildUEContextReleaseRequest asks the AMF to release a UE's context
// (TS 38.413 §8.3.2, §9.2.2.4), cause radio network "release due to NG-RAN
// generated reason".
func BuildUEContextReleaseRequest(amfUENGAPID, ranUENGAPID int64) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeUEContextReleaseRequest
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentIgnore
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentUEContextReleaseRequest
	pdu.InitiatingMessage.Value.UEContextReleaseRequest = new(ngapType.UEContextReleaseRequest)
	list := &pdu.InitiatingMessage.Value.UEContextReleaseRequest.ProtocolIEs.List

	{
		ie := ngapType.UEContextReleaseRequestIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDAMFUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.UEContextReleaseRequestIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfUENGAPID}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.UEContextReleaseRequestIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDRANUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.UEContextReleaseRequestIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranUENGAPID}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.UEContextReleaseRequestIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDCause
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.UEContextReleaseRequestIEsPresentCause
		ie.Value.Cause = &ngapType.Cause{
			Present:      ngapType.CausePresentRadioNetwork,
			RadioNetwork: &ngapType.CauseRadioNetwork{Value: ngapType.CauseRadioNetworkPresentReleaseDueToNgranGeneratedReason},
		}
		*list = append(*list, ie)
	}
	return pdu
}

// BuildUEContextReleaseComplete answers a UE CONTEXT RELEASE COMMAND
// (TS 38.413 §8.3.3, §9.2.2.6).
func BuildUEContextReleaseComplete(amfUENGAPID, ranUENGAPID int64) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeUEContextRelease
	pdu.SuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentUEContextReleaseComplete
	pdu.SuccessfulOutcome.Value.UEContextReleaseComplete = new(ngapType.UEContextReleaseComplete)
	list := &pdu.SuccessfulOutcome.Value.UEContextReleaseComplete.ProtocolIEs.List

	{
		ie := ngapType.UEContextReleaseCompleteIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDAMFUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.UEContextReleaseCompleteIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfUENGAPID}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.UEContextReleaseCompleteIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDRANUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.UEContextReleaseCompleteIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranUENGAPID}
		*list = append(*list, ie)
	}
	return pdu
}
