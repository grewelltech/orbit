package gnb

import (
	"github.com/free5gc/ngap/ngapType"
)

// BuildUEContextReleaseRequest asks the AMF to release a UE's context
// (TS 38.413 §8.3.2, §9.2.2.4), cause radio network "release due to NG-RAN
// generated reason". sessions are the PDU sessions with an active N3 user
// plane, sent as the PDU Session Resource List: the AMF deactivates the
// user plane of the sessions the gNB lists (TS 23.502 4.2.6 steps 1 and 5).
// With none the IE is left out.
func BuildUEContextReleaseRequest(amfUENGAPID, ranUENGAPID int64, sessions ...int64) ngapType.NGAPPDU {
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
	if len(sessions) > 0 {
		ie := ngapType.UEContextReleaseRequestIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDPDUSessionResourceListCxtRelReq
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.UEContextReleaseRequestIEsPresentPDUSessionResourceListCxtRelReq
		ie.Value.PDUSessionResourceListCxtRelReq = new(ngapType.PDUSessionResourceListCxtRelReq)
		for _, id := range sessions {
			item := ngapType.PDUSessionResourceItemCxtRelReq{}
			item.PDUSessionID.Value = id
			ie.Value.PDUSessionResourceListCxtRelReq.List = append(ie.Value.PDUSessionResourceListCxtRelReq.List, item)
		}
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
// (TS 38.413 §8.3.3, §9.2.2.6). sessions are the PDU sessions that had an
// active N3 user plane, sent as the PDU Session Resource List (TS 23.502
// 4.2.6 step 4); with none the IE is left out.
func BuildUEContextReleaseComplete(amfUENGAPID, ranUENGAPID int64, sessions ...int64) ngapType.NGAPPDU {
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
	if len(sessions) > 0 {
		ie := ngapType.UEContextReleaseCompleteIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDPDUSessionResourceListCxtRelCpl
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.UEContextReleaseCompleteIEsPresentPDUSessionResourceListCxtRelCpl
		ie.Value.PDUSessionResourceListCxtRelCpl = new(ngapType.PDUSessionResourceListCxtRelCpl)
		for _, id := range sessions {
			item := ngapType.PDUSessionResourceItemCxtRelCpl{}
			item.PDUSessionID.Value = id
			ie.Value.PDUSessionResourceListCxtRelCpl.List = append(ie.Value.PDUSessionResourceListCxtRelCpl.List, item)
		}
		*list = append(*list, ie)
	}
	return pdu
}

// UEContextReleaseCommandInfo is what a UE CONTEXT RELEASE COMMAND names.
type UEContextReleaseCommandInfo struct {
	// AMFUENGAPID and RANUENGAPID are the UE NGAP ID pair; HasPair is false
	// when the command names the UE by its AMF-UE-NGAP-ID alone, and
	// RANUENGAPID is then zero.
	AMFUENGAPID, RANUENGAPID int64
	HasPair                  bool
	// Cause is the command's cause, "" when it carries none.
	Cause string
}

// ParseUEContextReleaseCommand reads a UE CONTEXT RELEASE COMMAND's UE NGAP
// IDs and cause; false for any other PDU.
func ParseUEContextReleaseCommand(pdu *ngapType.NGAPPDU) (UEContextReleaseCommandInfo, bool) {
	var out UEContextReleaseCommandInfo
	if !IsUEContextReleaseCommand(pdu) || pdu.InitiatingMessage.Value.UEContextReleaseCommand == nil {
		return out, false
	}
	for _, ie := range pdu.InitiatingMessage.Value.UEContextReleaseCommand.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDUENGAPIDs:
			ids := ie.Value.UENGAPIDs
			switch {
			case ids == nil:
			case ids.Present == ngapType.UENGAPIDsPresentUENGAPIDPair && ids.UENGAPIDPair != nil:
				out.AMFUENGAPID, out.RANUENGAPID, out.HasPair = ids.UENGAPIDPair.AMFUENGAPID.Value, ids.UENGAPIDPair.RANUENGAPID.Value, true
			case ids.Present == ngapType.UENGAPIDsPresentAMFUENGAPID && ids.AMFUENGAPID != nil:
				out.AMFUENGAPID = ids.AMFUENGAPID.Value
			}
		case ngapType.ProtocolIEIDCause:
			if ie.Value.Cause != nil {
				out.Cause = causeString(ie.Value.Cause)
			}
		}
	}
	return out, true
}

// IsUEContextReleaseCommand reports whether pdu is a UE CONTEXT RELEASE
// COMMAND (TS 38.413 8.3.3).
func IsUEContextReleaseCommand(pdu *ngapType.NGAPPDU) bool {
	return pdu.Present == ngapType.NGAPPDUPresentInitiatingMessage &&
		pdu.InitiatingMessage.Value.Present == ngapType.InitiatingMessagePresentUEContextReleaseCommand
}
