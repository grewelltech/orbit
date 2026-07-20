package gnb

import (
	"encoding/binary"
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapConvert"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/coreprofile"
)

// N2 handover completion (TS 38.413 §8.4). After the source sends
// HandoverRequired (see handover.go), the AMF drives:
//
//	AMF → target: HandoverRequest        (parsed here)
//	target → AMF: HandoverRequestAcknowledge (built here — target DL N3 tunnel)
//	AMF → source: HandoverCommand        (parsed here)
//	target → AMF: HandoverNotify         (built here — UE arrived at target)
//
// then the AMF drives the SMF/UPF N4 path switch to the target's tunnel. RRC
// is stubbed: the target-to-source transparent container carries a placeholder
// RRC blob (opaque to the AMF, D-3).

// HandoverRequestInfo is the parsed AMF→target HandoverRequest.
type HandoverRequestInfo struct {
	AMFUENGAPID   int64
	PDUSessionIDs []int64
	// ULTunnels maps a PDU session ID to the UPF's uplink N3 endpoint from
	// that session's Handover Request Transfer, when it decoded. The core
	// may re-anchor the UL F-TEID across an N2 handover — the target gNB
	// must send subsequent uplink there. Sessions absent from the map keep
	// their previous UL tunnel (transfer missing or undecodable; free5gc-
	// lineage cores encode the transfer as a PDU Session Resource Setup
	// Request Transfer, which is what this parser understands).
	ULTunnels map[int64]GNBTunnel
}

// ParseHandoverRequest extracts what the target needs to acknowledge.
func ParseHandoverRequest(pdu *ngapType.NGAPPDU) (*HandoverRequestInfo, error) {
	if pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage ||
		pdu.InitiatingMessage.Value.Present != ngapType.InitiatingMessagePresentHandoverRequest {
		return nil, fmt.Errorf("not a HandoverRequest")
	}
	req := pdu.InitiatingMessage.Value.HandoverRequest
	out := &HandoverRequestInfo{}
	for _, ie := range req.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDAMFUENGAPID:
			out.AMFUENGAPID = ie.Value.AMFUENGAPID.Value
		case ngapType.ProtocolIEIDPDUSessionResourceSetupListHOReq:
			for _, item := range ie.Value.PDUSessionResourceSetupListHOReq.List {
				out.PDUSessionIDs = append(out.PDUSessionIDs, item.PDUSessionID.Value)
				// Best-effort UL NG-U tunnel extraction; a failed decode
				// leaves the session's previous UL tunnel in force.
				var r PDUSessionResource
				if err := decodeSetupRequestTransfer(item.HandoverRequestTransfer, &r); err == nil &&
					r.UPFAddress != "" && r.UPFTEID != 0 {
					if out.ULTunnels == nil {
						out.ULTunnels = make(map[int64]GNBTunnel)
					}
					out.ULTunnels[item.PDUSessionID.Value] = GNBTunnel{Address: r.UPFAddress, TEID: r.UPFTEID}
				}
			}
		}
	}
	return out, nil
}

// AdmittedSession is one PDU session the target admits, with its downlink
// N3 tunnel (where the UPF will send after the path switch).
type AdmittedSession struct {
	PDUSessionID int64
	GNBTunnel    GNBTunnel
	QFIs         []int64
}

// BuildHandoverRequestAcknowledge builds the target's ack (TS 38.413
// §9.2.3.3): the admitted sessions with the target's DL tunnels, plus the
// target-to-source transparent container (RRC stub).
func BuildHandoverRequestAcknowledge(amfID, targetRANID int64, admitted []AdmittedSession, quirks coreprofile.Quirks) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	container, err := encodeTargetToSourceContainer()
	if err != nil {
		return pdu, err
	}

	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeHandoverResourceAllocation
	pdu.SuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentHandoverRequestAcknowledge
	pdu.SuccessfulOutcome.Value.HandoverRequestAcknowledge = new(ngapType.HandoverRequestAcknowledge)
	list := &pdu.SuccessfulOutcome.Value.HandoverRequestAcknowledge.ProtocolIEs.List

	add := func(id int64, crit aper.Enumerated, fill func(*ngapType.HandoverRequestAcknowledgeIEs)) {
		ie := ngapType.HandoverRequestAcknowledgeIEs{}
		ie.Id.Value = id
		ie.Criticality.Value = crit
		fill(&ie)
		*list = append(*list, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, ngapType.CriticalityPresentIgnore, func(ie *ngapType.HandoverRequestAcknowledgeIEs) {
		ie.Value.Present = ngapType.HandoverRequestAcknowledgeIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, ngapType.CriticalityPresentIgnore, func(ie *ngapType.HandoverRequestAcknowledgeIEs) {
		ie.Value.Present = ngapType.HandoverRequestAcknowledgeIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: targetRANID}
	})
	add(ngapType.ProtocolIEIDPDUSessionResourceAdmittedList, ngapType.CriticalityPresentIgnore, func(ie *ngapType.HandoverRequestAcknowledgeIEs) {
		ie.Value.Present = ngapType.HandoverRequestAcknowledgeIEsPresentPDUSessionResourceAdmittedList
		ie.Value.PDUSessionResourceAdmittedList = new(ngapType.PDUSessionResourceAdmittedList)
		for _, a := range admitted {
			transfer, err := encodeHandoverAckTransfer(a.GNBTunnel, a.QFIs, quirks.HandoverAckForwardingMandatory)
			if err != nil {
				continue
			}
			item := ngapType.PDUSessionResourceAdmittedItem{}
			item.PDUSessionID.Value = a.PDUSessionID
			item.HandoverRequestAcknowledgeTransfer = transfer
			ie.Value.PDUSessionResourceAdmittedList.List = append(ie.Value.PDUSessionResourceAdmittedList.List, item)
		}
	})
	add(ngapType.ProtocolIEIDTargetToSourceTransparentContainer, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequestAcknowledgeIEs) {
		ie.Value.Present = ngapType.HandoverRequestAcknowledgeIEsPresentTargetToSourceTransparentContainer
		ie.Value.TargetToSourceTransparentContainer = &ngapType.TargetToSourceTransparentContainer{Value: container}
	})
	return pdu, nil
}

func gtpTunnelUP(tun GNBTunnel) ngapType.UPTransportLayerInformation {
	up := ngapType.UPTransportLayerInformation{Present: ngapType.UPTransportLayerInformationPresentGTPTunnel, GTPTunnel: new(ngapType.GTPTunnel)}
	teid := make([]byte, 4)
	binary.BigEndian.PutUint32(teid, tun.TEID)
	up.GTPTunnel.GTPTEID.Value = teid
	up.GTPTunnel.TransportLayerAddress = ngapConvert.IPAddressToNgap(tun.Address, "")
	return up
}

func qosFlowSetupResponseList(qfis []int64) ngapType.QosFlowListWithDataForwarding {
	if len(qfis) == 0 {
		qfis = []int64{1} // default flow
	}
	var l ngapType.QosFlowListWithDataForwarding
	for _, qfi := range qfis {
		l.List = append(l.List, ngapType.QosFlowItemWithDataForwarding{QosFlowIdentifier: ngapType.QosFlowIdentifier{Value: qfi}})
	}
	return l
}

// sdcoreHOAckTransfer mirrors ngapType.HandoverRequestAcknowledgeTransfer but
// with dLForwardingUP-TNLInformation as a MANDATORY value (no `optional`),
// matching SD-Core / omec-project ngap v2.x's non-conformant schema (3GPP TS
// 38.413 defines it OPTIONAL). Encoded with ORBIT's own conformant aper, this
// is byte-identical to omec's own v2.1.0 encoder — so omec's decoder accepts
// it. Only used under the HandoverAckForwardingMandatory quirk; a conformant
// core would reject these bytes. See docs/interop/sdcore.md.
type sdcoreHOAckTransfer struct {
	DLNGUUPTNLInformation         ngapType.UPTransportLayerInformation `aper:"valueLB:0,valueUB:1"`
	DLForwardingUPTNLInformation  ngapType.UPTransportLayerInformation `aper:"valueLB:0,valueUB:1"`
	SecurityResult                *ngapType.SecurityResult             `aper:"valueExt,optional"`
	QosFlowSetupResponseList      ngapType.QosFlowListWithDataForwarding
	QosFlowFailedToSetupList      *ngapType.QosFlowListWithCause                                               `aper:"optional"`
	DataForwardingResponseDRBList *ngapType.DataForwardingResponseDRBList                                      `aper:"optional"`
	IEExtensions                  *ngapType.ProtocolExtensionContainerHandoverRequestAcknowledgeTransferExtIEs `aper:"optional"`
}

// encodeHandoverAckTransfer builds the per-session Handover Request
// Acknowledge Transfer with the target's downlink tunnel. With
// forwardingMandatory set (SD-Core quirk), it also emits
// dLForwardingUP-TNLInformation (as the same tunnel) so omec's decoder, which
// wrongly requires it, accepts the message.
func encodeHandoverAckTransfer(tun GNBTunnel, qfis []int64, forwardingMandatory bool) (aper.OctetString, error) {
	if forwardingMandatory {
		t := sdcoreHOAckTransfer{
			DLNGUUPTNLInformation:        gtpTunnelUP(tun),
			DLForwardingUPTNLInformation: gtpTunnelUP(tun),
			QosFlowSetupResponseList:     qosFlowSetupResponseList(qfis),
		}
		return aper.MarshalWithParams(t, "valueExt")
	}
	t := ngapType.HandoverRequestAcknowledgeTransfer{
		DLNGUUPTNLInformation:    gtpTunnelUP(tun),
		QosFlowSetupResponseList: qosFlowSetupResponseList(qfis),
	}
	return aper.MarshalWithParams(t, "valueExt")
}

func encodeTargetToSourceContainer() (aper.OctetString, error) {
	var c ngapType.TargetNGRANNodeToSourceNGRANNodeTransparentContainer
	c.RRCContainer.Value = aper.OctetString{0x00, 0x00, 0x00, 0x00} // placeholder RRC (D-3)
	return aper.MarshalWithParams(c, "valueExt")
}

// ParseHandoverCommand confirms the AMF authorised the handover (source side,
// TS 38.413 §9.2.3.2), returning the AMF UE NGAP ID.
func ParseHandoverCommand(pdu *ngapType.NGAPPDU) (int64, error) {
	if pdu.Present != ngapType.NGAPPDUPresentSuccessfulOutcome ||
		pdu.SuccessfulOutcome.Value.Present != ngapType.SuccessfulOutcomePresentHandoverCommand {
		return 0, fmt.Errorf("not a HandoverCommand")
	}
	for _, ie := range pdu.SuccessfulOutcome.Value.HandoverCommand.ProtocolIEs.List {
		if ie.Id.Value == ngapType.ProtocolIEIDAMFUENGAPID {
			return ie.Value.AMFUENGAPID.Value, nil
		}
	}
	return 0, fmt.Errorf("HandoverCommand missing AMF UE NGAP ID")
}

// BuildHandoverNotify tells the AMF the UE arrived at the target so it drives
// the N4/N3 path switch (TS 38.413 §9.2.3.6).
func BuildHandoverNotify(cfg Config, amfID, targetRANID int64) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	loc, err := locationFrom(cfg)
	if err != nil {
		return pdu, err
	}
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeHandoverNotification
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentIgnore
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentHandoverNotify
	pdu.InitiatingMessage.Value.HandoverNotify = new(ngapType.HandoverNotify)
	list := &pdu.InitiatingMessage.Value.HandoverNotify.ProtocolIEs.List

	add := func(id int64, fill func(*ngapType.HandoverNotifyIEs)) {
		ie := ngapType.HandoverNotifyIEs{}
		ie.Id.Value = id
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		fill(&ie)
		*list = append(*list, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, func(ie *ngapType.HandoverNotifyIEs) {
		ie.Value.Present = ngapType.HandoverNotifyIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, func(ie *ngapType.HandoverNotifyIEs) {
		ie.Value.Present = ngapType.HandoverNotifyIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: targetRANID}
	})
	add(ngapType.ProtocolIEIDUserLocationInformation, func(ie *ngapType.HandoverNotifyIEs) {
		ie.Value.Present = ngapType.HandoverNotifyIEsPresentUserLocationInformation
		ie.Value.UserLocationInformation = loc.userLocationInformation()
	})
	return pdu, nil
}
