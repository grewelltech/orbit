package gnb

import (
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"
)

// PDU Session Resource Release (TS 38.413 §8.2.2). The AMF sends the
// command when the SMF releases a session's NG-RAN resources; it lists the
// sessions to release and may carry the 5GSM PDU SESSION RELEASE COMMAND as
// its NAS PDU. The gNB answers with the sessions it released.

// PDUSessionResourceRelease is a parsed PDU SESSION RESOURCE RELEASE
// COMMAND (TS 38.413 §9.2.1.3).
type PDUSessionResourceRelease struct {
	AMFUENGAPID, RANUENGAPID int64
	// PDUSessionIDs are the sessions in the PDU Session Resource to Release
	// List, in order.
	PDUSessionIDs []int64
	// NASPDU is the optional NAS-PDU IE, nil when absent.
	NASPDU []byte
}

// ParsePDUSessionResourceReleaseCommand reads an AMF→gNB PDU SESSION
// RESOURCE RELEASE COMMAND; any other PDU is an error.
func ParsePDUSessionResourceReleaseCommand(pdu *ngapType.NGAPPDU) (*PDUSessionResourceRelease, error) {
	if pdu == nil || pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage || pdu.InitiatingMessage == nil ||
		pdu.InitiatingMessage.Value.Present != ngapType.InitiatingMessagePresentPDUSessionResourceReleaseCommand ||
		pdu.InitiatingMessage.Value.PDUSessionResourceReleaseCommand == nil {
		return nil, fmt.Errorf("not a PDU Session Resource Release Command")
	}
	out := &PDUSessionResourceRelease{}
	for _, ie := range pdu.InitiatingMessage.Value.PDUSessionResourceReleaseCommand.ProtocolIEs.List {
		switch {
		case ie.Id.Value == ngapType.ProtocolIEIDAMFUENGAPID && ie.Value.AMFUENGAPID != nil:
			out.AMFUENGAPID = ie.Value.AMFUENGAPID.Value
		case ie.Id.Value == ngapType.ProtocolIEIDRANUENGAPID && ie.Value.RANUENGAPID != nil:
			out.RANUENGAPID = ie.Value.RANUENGAPID.Value
		case ie.Id.Value == ngapType.ProtocolIEIDNASPDU && ie.Value.NASPDU != nil:
			out.NASPDU = ie.Value.NASPDU.Value
		case ie.Id.Value == ngapType.ProtocolIEIDPDUSessionResourceToReleaseListRelCmd && ie.Value.PDUSessionResourceToReleaseListRelCmd != nil:
			for _, item := range ie.Value.PDUSessionResourceToReleaseListRelCmd.List {
				out.PDUSessionIDs = append(out.PDUSessionIDs, item.PDUSessionID.Value)
			}
		}
	}
	return out, nil
}

// BuildPDUSessionResourceReleaseResponse answers a PDU SESSION RESOURCE
// RELEASE COMMAND (TS 38.413 §9.2.1.4), reporting each of pduSessionIDs as
// released with an empty PDU Session Resource Release Response Transfer.
func BuildPDUSessionResourceReleaseResponse(amfID, ranID int64, pduSessionIDs []int64) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	transfer, err := aper.MarshalWithParams(ngapType.PDUSessionResourceReleaseResponseTransfer{}, "valueExt")
	if err != nil {
		return pdu, fmt.Errorf("encode PDU session release response transfer: %w", err)
	}
	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodePDUSessionResourceRelease
	pdu.SuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentPDUSessionResourceReleaseResponse
	pdu.SuccessfulOutcome.Value.PDUSessionResourceReleaseResponse = new(ngapType.PDUSessionResourceReleaseResponse)
	list := &pdu.SuccessfulOutcome.Value.PDUSessionResourceReleaseResponse.ProtocolIEs.List

	{
		ie := ngapType.PDUSessionResourceReleaseResponseIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDAMFUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.PDUSessionResourceReleaseResponseIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.PDUSessionResourceReleaseResponseIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDRANUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.PDUSessionResourceReleaseResponseIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranID}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.PDUSessionResourceReleaseResponseIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDPDUSessionResourceReleasedListRelRes
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.PDUSessionResourceReleaseResponseIEsPresentPDUSessionResourceReleasedListRelRes
		ie.Value.PDUSessionResourceReleasedListRelRes = new(ngapType.PDUSessionResourceReleasedListRelRes)
		for _, id := range pduSessionIDs {
			item := ngapType.PDUSessionResourceReleasedItemRelRes{}
			item.PDUSessionID.Value = id
			item.PDUSessionResourceReleaseResponseTransfer = transfer
			ie.Value.PDUSessionResourceReleasedListRelRes.List = append(ie.Value.PDUSessionResourceReleasedListRelRes.List, item)
		}
		*list = append(*list, ie)
	}
	return pdu, nil
}
