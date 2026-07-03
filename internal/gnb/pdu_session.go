package gnb

import (
	"encoding/binary"
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapConvert"
	"github.com/free5gc/ngap/ngapType"
)

// PDU Session Resource Setup (TS 38.413 §8.2.1). The AMF sends this when the
// SMF has set up a session; it carries the 5GSM PDU Session Establishment
// Accept (with the allocated UE IP) as the item's NAS PDU, plus a
// PDU-Session-Resource-Setup-Request-Transfer with the UPF's uplink N3
// tunnel (address + TEID) and the QoS flows. The gNB answers with its own
// downlink N3 tunnel. Phase 1a records the tunnel endpoints (no GTP-U yet);
// Phase 1b wires the data path.

// PDUSessionResource is one parsed item from a PDU Session Resource Setup
// Request.
type PDUSessionResource struct {
	PDUSessionID int64
	NASPDU       []byte // the 5GSM PDU Session Establishment Accept
	UPFAddress   string // UPF N3 IPv4 (uplink tunnel endpoint)
	UPFTEID      uint32 // UPF-allocated uplink TEID
	QFIs         []int64
}

// ParsePDUSessionResourceSetupRequest extracts the resources from an
// AMF→gNB PDU Session Resource Setup Request. Returns the AMF/RAN UE NGAP
// IDs and one entry per PDU session.
func ParsePDUSessionResourceSetupRequest(pdu *ngapType.NGAPPDU) (amfID, ranID int64, res []PDUSessionResource, err error) {
	if pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage ||
		pdu.InitiatingMessage.Value.Present != ngapType.InitiatingMessagePresentPDUSessionResourceSetupRequest {
		return 0, 0, nil, fmt.Errorf("not a PDU Session Resource Setup Request")
	}
	req := pdu.InitiatingMessage.Value.PDUSessionResourceSetupRequest
	for _, ie := range req.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDAMFUENGAPID:
			amfID = ie.Value.AMFUENGAPID.Value
		case ngapType.ProtocolIEIDRANUENGAPID:
			ranID = ie.Value.RANUENGAPID.Value
		case ngapType.ProtocolIEIDPDUSessionResourceSetupListSUReq:
			for _, item := range ie.Value.PDUSessionResourceSetupListSUReq.List {
				r := PDUSessionResource{PDUSessionID: item.PDUSessionID.Value}
				if item.PDUSessionNASPDU != nil {
					r.NASPDU = item.PDUSessionNASPDU.Value
				}
				if err := decodeSetupRequestTransfer(item.PDUSessionResourceSetupRequestTransfer, &r); err != nil {
					return 0, 0, nil, err
				}
				res = append(res, r)
			}
		}
	}
	return amfID, ranID, res, nil
}

func decodeSetupRequestTransfer(b aper.OctetString, r *PDUSessionResource) error {
	var t ngapType.PDUSessionResourceSetupRequestTransfer
	if err := aper.UnmarshalWithParams(b, &t, "valueExt"); err != nil {
		return fmt.Errorf("decode PDU session setup request transfer: %w", err)
	}
	for _, ie := range t.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDULNGUUPTNLInformation:
			if gt := ie.Value.ULNGUUPTNLInformation.GTPTunnel; gt != nil {
				v4, _ := ngapConvert.IPAddressToString(gt.TransportLayerAddress)
				r.UPFAddress = v4
				r.UPFTEID = binary.BigEndian.Uint32(gt.GTPTEID.Value)
			}
		case ngapType.ProtocolIEIDQosFlowSetupRequestList:
			for _, q := range ie.Value.QosFlowSetupRequestList.List {
				r.QFIs = append(r.QFIs, q.QosFlowIdentifier.Value)
			}
		}
	}
	return nil
}

// GNBTunnel is the gNB-side downlink N3 endpoint reported in the response.
type GNBTunnel struct {
	Address string // gNB N3 IPv4
	TEID    uint32 // gNB-allocated downlink TEID
}

// BuildPDUSessionResourceSetupResponse acknowledges the sessions, reporting
// the gNB downlink tunnel per session (TS 38.413 §8.2.1, §9.2.1.2). The
// per-session response transfer is APER-encoded with the "valueExt" param
// as an NGAP OCTET STRING, matching gnbsim's field-used encoding.
func BuildPDUSessionResourceSetupResponse(amfID, ranID int64, res []PDUSessionResource, tun GNBTunnel) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodePDUSessionResourceSetup
	pdu.SuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentPDUSessionResourceSetupResponse
	pdu.SuccessfulOutcome.Value.PDUSessionResourceSetupResponse = new(ngapType.PDUSessionResourceSetupResponse)
	list := &pdu.SuccessfulOutcome.Value.PDUSessionResourceSetupResponse.ProtocolIEs.List

	{
		ie := ngapType.PDUSessionResourceSetupResponseIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDAMFUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.PDUSessionResourceSetupResponseIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.PDUSessionResourceSetupResponseIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDRANUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.PDUSessionResourceSetupResponseIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranID}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.PDUSessionResourceSetupResponseIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDPDUSessionResourceSetupListSURes
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.PDUSessionResourceSetupResponseIEsPresentPDUSessionResourceSetupListSURes
		ie.Value.PDUSessionResourceSetupListSURes = new(ngapType.PDUSessionResourceSetupListSURes)
		for _, r := range res {
			transfer, err := encodeSetupResponseTransfer(tun, r.QFIs)
			if err != nil {
				return pdu, err
			}
			item := ngapType.PDUSessionResourceSetupItemSURes{}
			item.PDUSessionID.Value = r.PDUSessionID
			item.PDUSessionResourceSetupResponseTransfer = transfer
			ie.Value.PDUSessionResourceSetupListSURes.List = append(ie.Value.PDUSessionResourceSetupListSURes.List, item)
		}
		*list = append(*list, ie)
	}
	return pdu, nil
}

func encodeSetupResponseTransfer(tun GNBTunnel, qfis []int64) (aper.OctetString, error) {
	var t ngapType.PDUSessionResourceSetupResponseTransfer
	up := &t.DLQosFlowPerTNLInformation.UPTransportLayerInformation
	up.Present = ngapType.UPTransportLayerInformationPresentGTPTunnel
	up.GTPTunnel = new(ngapType.GTPTunnel)
	teid := make([]byte, 4)
	binary.BigEndian.PutUint32(teid, tun.TEID)
	up.GTPTunnel.GTPTEID.Value = teid
	up.GTPTunnel.TransportLayerAddress = ngapConvert.IPAddressToNgap(tun.Address, "")
	for _, qfi := range qfis {
		item := ngapType.AssociatedQosFlowItem{}
		item.QosFlowIdentifier.Value = qfi
		t.DLQosFlowPerTNLInformation.AssociatedQosFlowList.List =
			append(t.DLQosFlowPerTNLInformation.AssociatedQosFlowList.List, item)
	}
	b, err := aper.MarshalWithParams(t, "valueExt")
	if err != nil {
		return nil, fmt.Errorf("encode PDU session setup response transfer: %w", err)
	}
	return b, nil
}
