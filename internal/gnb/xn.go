package gnb

import (
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"
)

// Xn handover, core-facing completion (TS 38.413 §8.4.4 / TS 38.423). In an Xn
// handover the source and target gNBs coordinate directly over Xn (XnAP
// Handover Request/Ack + RRC), which ORBIT stubs in-process; the only
// core-facing message is the target gNB's NGAP PathSwitchRequest, which asks
// the AMF/SMF to switch the downlink to the target's new N3 tunnel. This file
// builds that request and classifies the response.

// BuildPathSwitchRequest builds the target gNB's PathSwitchRequest (TS 38.413
// §9.2.3.10): it names the UE by its source AMF-UE-NGAP-ID, assigns a target
// RAN-UE-NGAP-ID, and carries the target's new downlink tunnel per session.
func BuildPathSwitchRequest(cfg Config, sourceAMFUENGAPID, targetRANID int64, sessions []AdmittedSession) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	loc, err := locationFrom(cfg)
	if err != nil {
		return pdu, err
	}

	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodePathSwitchRequest
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentPathSwitchRequest
	pdu.InitiatingMessage.Value.PathSwitchRequest = new(ngapType.PathSwitchRequest)
	list := &pdu.InitiatingMessage.Value.PathSwitchRequest.ProtocolIEs.List

	add := func(id int64, crit aper.Enumerated, fill func(*ngapType.PathSwitchRequestIEs)) {
		ie := ngapType.PathSwitchRequestIEs{}
		ie.Id.Value = id
		ie.Criticality.Value = crit
		fill(&ie)
		*list = append(*list, ie)
	}
	add(ngapType.ProtocolIEIDRANUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.PathSwitchRequestIEs) {
		ie.Value.Present = ngapType.PathSwitchRequestIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: targetRANID}
	})
	add(ngapType.ProtocolIEIDSourceAMFUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.PathSwitchRequestIEs) {
		ie.Value.Present = ngapType.PathSwitchRequestIEsPresentSourceAMFUENGAPID
		ie.Value.SourceAMFUENGAPID = &ngapType.AMFUENGAPID{Value: sourceAMFUENGAPID}
	})
	add(ngapType.ProtocolIEIDUserLocationInformation, ngapType.CriticalityPresentIgnore, func(ie *ngapType.PathSwitchRequestIEs) {
		ie.Value.Present = ngapType.PathSwitchRequestIEsPresentUserLocationInformation
		ie.Value.UserLocationInformation = loc.userLocationInformation()
	})
	add(ngapType.ProtocolIEIDUESecurityCapabilities, ngapType.CriticalityPresentIgnore, func(ie *ngapType.PathSwitchRequestIEs) {
		ie.Value.Present = ngapType.PathSwitchRequestIEsPresentUESecurityCapabilities
		ie.Value.UESecurityCapabilities = ueSecurityCapabilities()
	})
	add(ngapType.ProtocolIEIDPDUSessionResourceToBeSwitchedDLList, ngapType.CriticalityPresentReject, func(ie *ngapType.PathSwitchRequestIEs) {
		ie.Value.Present = ngapType.PathSwitchRequestIEsPresentPDUSessionResourceToBeSwitchedDLList
		ie.Value.PDUSessionResourceToBeSwitchedDLList = new(ngapType.PDUSessionResourceToBeSwitchedDLList)
		for _, s := range sessions {
			transfer, err := encodePathSwitchTransfer(s.GNBTunnel, s.QFIs)
			if err != nil {
				continue
			}
			item := ngapType.PDUSessionResourceToBeSwitchedDLItem{}
			item.PDUSessionID.Value = s.PDUSessionID
			item.PathSwitchRequestTransfer = transfer
			ie.Value.PDUSessionResourceToBeSwitchedDLList.List = append(ie.Value.PDUSessionResourceToBeSwitchedDLList.List, item)
		}
	})
	return pdu, nil
}

// encodePathSwitchTransfer builds the per-session PathSwitchRequestTransfer
// (TS 38.413 §9.3.4.14): the target's new downlink N3 tunnel and accepted QoS
// flows.
func encodePathSwitchTransfer(tun GNBTunnel, qfis []int64) (aper.OctetString, error) {
	var t ngapType.PathSwitchRequestTransfer
	t.DLNGUUPTNLInformation = gtpTunnelUP(tun)
	if len(qfis) == 0 {
		qfis = []int64{1}
	}
	for _, qfi := range qfis {
		t.QosFlowAcceptedList.List = append(t.QosFlowAcceptedList.List,
			ngapType.QosFlowAcceptedItem{QosFlowIdentifier: ngapType.QosFlowIdentifier{Value: qfi}})
	}
	return aper.MarshalWithParams(t, "valueExt")
}

// ueSecurityCapabilities reports the UE's NR algorithms (NEA1-3 / NIA1-3), a
// static capability advertised at path switch.
func ueSecurityCapabilities() *ngapType.UESecurityCapabilities {
	nr := aper.BitString{Bytes: []byte{0xE0, 0x00}, BitLength: 16} // NEA1/2/3, NIA1/2/3
	eutra := aper.BitString{Bytes: []byte{0x00, 0x00}, BitLength: 16}
	c := &ngapType.UESecurityCapabilities{}
	c.NRencryptionAlgorithms.Value = nr
	c.NRintegrityProtectionAlgorithms.Value = nr
	c.EUTRAencryptionAlgorithms.Value = eutra
	c.EUTRAintegrityProtectionAlgorithms.Value = eutra
	return c
}

// PathSwitchOutcome classifies the AMF's response to a PathSwitchRequest.
type PathSwitchOutcome int

const (
	PathSwitchOther        PathSwitchOutcome = iota
	PathSwitchAcknowledged                   // AMF switched the path — Xn supported
	PathSwitchFailed                         // AMF rejected the path switch
)

// ClassifyPathSwitch inspects a downlink PDU for the PathSwitch outcome.
func ClassifyPathSwitch(pdu *ngapType.NGAPPDU) PathSwitchOutcome {
	switch pdu.Present {
	case ngapType.NGAPPDUPresentSuccessfulOutcome:
		if pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentPathSwitchRequestAcknowledge {
			return PathSwitchAcknowledged
		}
	case ngapType.NGAPPDUPresentUnsuccessfulOutcome:
		if pdu.UnsuccessfulOutcome.Value.Present == ngapType.UnsuccessfulOutcomePresentPathSwitchRequestFailure {
			return PathSwitchFailed
		}
	}
	return PathSwitchOther
}

// ParsePathSwitchAcknowledge confirms the AMF acknowledged the path switch and
// returns the (new) AMF-UE-NGAP-ID it assigned.
func ParsePathSwitchAcknowledge(pdu *ngapType.NGAPPDU) (int64, error) {
	if pdu.Present != ngapType.NGAPPDUPresentSuccessfulOutcome ||
		pdu.SuccessfulOutcome.Value.Present != ngapType.SuccessfulOutcomePresentPathSwitchRequestAcknowledge {
		return 0, fmt.Errorf("not a PathSwitchRequestAcknowledge")
	}
	for _, ie := range pdu.SuccessfulOutcome.Value.PathSwitchRequestAcknowledge.ProtocolIEs.List {
		if ie.Id.Value == ngapType.ProtocolIEIDAMFUENGAPID {
			return ie.Value.AMFUENGAPID.Value, nil
		}
	}
	return 0, nil
}
