package gnb

import (
	"fmt"

	"github.com/free5gc/ngap/ngapType"
)

// PDUSessionResourceModify is a parsed PDU SESSION RESOURCE MODIFY REQUEST
// (TS 38.413 §9.2.1.5): the AMF asks the gNB to modify the NG-RAN
// resources of established sessions, each item optionally carrying a 5GSM
// message for the UE.
type PDUSessionResourceModify struct {
	AMFUENGAPID, RANUENGAPID int64
	// PDUSessionIDs are the sessions in the PDU Session Resource Modify
	// List, in order.
	PDUSessionIDs []int64
	// NASPDUs are the NAS-PDU IEs of the items that carry one, in order.
	NASPDUs [][]byte
}

// ParsePDUSessionResourceModifyRequest reads an AMF→gNB PDU SESSION
// RESOURCE MODIFY REQUEST; any other PDU is an error.
func ParsePDUSessionResourceModifyRequest(pdu *ngapType.NGAPPDU) (*PDUSessionResourceModify, error) {
	if pdu == nil || pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage || pdu.InitiatingMessage == nil ||
		pdu.InitiatingMessage.Value.Present != ngapType.InitiatingMessagePresentPDUSessionResourceModifyRequest ||
		pdu.InitiatingMessage.Value.PDUSessionResourceModifyRequest == nil {
		return nil, fmt.Errorf("not a PDU Session Resource Modify Request")
	}
	out := &PDUSessionResourceModify{}
	for _, ie := range pdu.InitiatingMessage.Value.PDUSessionResourceModifyRequest.ProtocolIEs.List {
		switch {
		case ie.Id.Value == ngapType.ProtocolIEIDAMFUENGAPID && ie.Value.AMFUENGAPID != nil:
			out.AMFUENGAPID = ie.Value.AMFUENGAPID.Value
		case ie.Id.Value == ngapType.ProtocolIEIDRANUENGAPID && ie.Value.RANUENGAPID != nil:
			out.RANUENGAPID = ie.Value.RANUENGAPID.Value
		case ie.Id.Value == ngapType.ProtocolIEIDPDUSessionResourceModifyListModReq && ie.Value.PDUSessionResourceModifyListModReq != nil:
			for _, item := range ie.Value.PDUSessionResourceModifyListModReq.List {
				out.PDUSessionIDs = append(out.PDUSessionIDs, item.PDUSessionID.Value)
				if item.NASPDU != nil && len(item.NASPDU.Value) > 0 {
					out.NASPDUs = append(out.NASPDUs, item.NASPDU.Value)
				}
			}
		}
	}
	return out, nil
}
