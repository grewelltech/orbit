package gnb

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapConvert"
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
// ueSecCaps is the UE's 5G security capabilities as the RAN holds them, from
// the AMF (Initial Context Setup Request, Handover Request, or a Path Switch
// Request Acknowledge that corrected them): the target sends the AMF what
// the source handed it, and the AMF checks it against its own copy
// (TS 33.501 §6.7.3.1). A UE without them cannot be switched.
func BuildPathSwitchRequest(cfg Config, sourceAMFUENGAPID, targetRANID int64, ueSecCaps *ngapType.UESecurityCapabilities, sessions []AdmittedSession) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	if ueSecCaps == nil {
		return pdu, fmt.Errorf("path switch: no UE security capabilities held for the UE")
	}
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
		ie.Value.UESecurityCapabilities = CloneUESecurityCapabilities(ueSecCaps)
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
	up, err := gtpTunnelUP(tun)
	if err != nil {
		return nil, err
	}
	t.DLNGUUPTNLInformation = up
	if len(qfis) == 0 {
		qfis = []int64{1}
	}
	for _, qfi := range qfis {
		t.QosFlowAcceptedList.List = append(t.QosFlowAcceptedList.List,
			ngapType.QosFlowAcceptedItem{QosFlowIdentifier: ngapType.QosFlowIdentifier{Value: qfi}})
	}
	return aper.MarshalWithParams(t, "valueExt")
}

// NewUESecurityCapabilities builds the NGAP UE Security Capabilities IE from
// its four 16-bit algorithm bitmaps, each with the first algorithm after the
// null one (NEA1, NIA1, EEA1, EIA1) in the most significant bit.
func NewUESecurityCapabilities(nrEnc, nrInt, eutraEnc, eutraInt uint16) *ngapType.UESecurityCapabilities {
	bitmap := func(v uint16) aper.BitString {
		return aper.BitString{Bytes: []byte{byte(v >> 8), byte(v)}, BitLength: 16}
	}
	c := &ngapType.UESecurityCapabilities{}
	c.NRencryptionAlgorithms.Value = bitmap(nrEnc)
	c.NRintegrityProtectionAlgorithms.Value = bitmap(nrInt)
	c.EUTRAencryptionAlgorithms.Value = bitmap(eutraEnc)
	c.EUTRAintegrityProtectionAlgorithms.Value = bitmap(eutraInt)
	return c
}

// CloneUESecurityCapabilities copies c with its bitmaps, nil for nil.
func CloneUESecurityCapabilities(c *ngapType.UESecurityCapabilities) *ngapType.UESecurityCapabilities {
	if c == nil {
		return nil
	}
	clone := func(b aper.BitString) aper.BitString {
		return aper.BitString{Bytes: append([]byte(nil), b.Bytes...), BitLength: b.BitLength}
	}
	out := &ngapType.UESecurityCapabilities{}
	out.NRencryptionAlgorithms.Value = clone(c.NRencryptionAlgorithms.Value)
	out.NRintegrityProtectionAlgorithms.Value = clone(c.NRintegrityProtectionAlgorithms.Value)
	out.EUTRAencryptionAlgorithms.Value = clone(c.EUTRAencryptionAlgorithms.Value)
	out.EUTRAintegrityProtectionAlgorithms.Value = clone(c.EUTRAintegrityProtectionAlgorithms.Value)
	return out
}

// SameUESecurityCapabilities reports whether a and b hold the same four
// bitmaps; a nil is equal only to a nil.
func SameUESecurityCapabilities(a, b *ngapType.UESecurityCapabilities) bool {
	if a == nil || b == nil {
		return a == b
	}
	same := func(x, y aper.BitString) bool {
		return x.BitLength == y.BitLength && bytes.Equal(x.Bytes, y.Bytes)
	}
	return same(a.NRencryptionAlgorithms.Value, b.NRencryptionAlgorithms.Value) &&
		same(a.NRintegrityProtectionAlgorithms.Value, b.NRintegrityProtectionAlgorithms.Value) &&
		same(a.EUTRAencryptionAlgorithms.Value, b.EUTRAencryptionAlgorithms.Value) &&
		same(a.EUTRAintegrityProtectionAlgorithms.Value, b.EUTRAintegrityProtectionAlgorithms.Value)
}

// PathSwitchAcknowledgeUESecurityCapabilities returns the UE Security
// Capabilities IE of a PathSwitchRequestAcknowledge, nil when absent: the
// AMF's stored copy, sent when the request's differed from it (TS 33.501
// §6.7.3.1), which the target gNB takes over.
func PathSwitchAcknowledgeUESecurityCapabilities(pdu *ngapType.NGAPPDU) *ngapType.UESecurityCapabilities {
	if pdu.Present != ngapType.NGAPPDUPresentSuccessfulOutcome ||
		pdu.SuccessfulOutcome.Value.Present != ngapType.SuccessfulOutcomePresentPathSwitchRequestAcknowledge {
		return nil
	}
	for _, ie := range pdu.SuccessfulOutcome.Value.PathSwitchRequestAcknowledge.ProtocolIEs.List {
		if ie.Id.Value == ngapType.ProtocolIEIDUESecurityCapabilities {
			return CloneUESecurityCapabilities(ie.Value.UESecurityCapabilities)
		}
	}
	return nil
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

// SwitchedSession is one entry of the PathSwitchRequestAcknowledge's PDU
// Session Resource Switched List whose transfer carried UL NG-U UP TNL
// information — the core reallocated the UPF's uplink F-TEID at path switch
// (TS 38.413 §9.3.4.14), and the gNB MUST send subsequent uplink there.
type SwitchedSession struct {
	PDUSessionID int64
	UPFAddress   string // new UPF N3 IPv4
	UPFTEID      uint32 // new UPF-allocated uplink TEID
}

// ParsePathSwitchAcknowledgeSwitched extracts the switched list's new UL
// NG-U tunnels from a PathSwitchRequestAcknowledge. Sessions whose transfer
// omits the (optional) UL NG-U UP TNL information — the common "UPF kept the
// tunnel" case — are absent from the result. Transfers that fail to decode
// are skipped: a missing UL update degrades to the pre-parse behaviour
// (keep sending to the old F-TEID), never a failed handover.
func ParsePathSwitchAcknowledgeSwitched(pdu *ngapType.NGAPPDU) []SwitchedSession {
	if pdu.Present != ngapType.NGAPPDUPresentSuccessfulOutcome ||
		pdu.SuccessfulOutcome.Value.Present != ngapType.SuccessfulOutcomePresentPathSwitchRequestAcknowledge {
		return nil
	}
	var out []SwitchedSession
	for _, ie := range pdu.SuccessfulOutcome.Value.PathSwitchRequestAcknowledge.ProtocolIEs.List {
		if ie.Id.Value != ngapType.ProtocolIEIDPDUSessionResourceSwitchedList ||
			ie.Value.PDUSessionResourceSwitchedList == nil {
			continue
		}
		for _, item := range ie.Value.PDUSessionResourceSwitchedList.List {
			var t ngapType.PathSwitchRequestAcknowledgeTransfer
			if err := aper.UnmarshalWithParams(item.PathSwitchRequestAcknowledgeTransfer, &t, "valueExt"); err != nil {
				continue
			}
			if t.ULNGUUPTNLInformation == nil || t.ULNGUUPTNLInformation.GTPTunnel == nil {
				continue
			}
			gt := t.ULNGUUPTNLInformation.GTPTunnel
			if len(gt.GTPTEID.Value) != 4 {
				continue
			}
			v4, _ := ngapConvert.IPAddressToString(gt.TransportLayerAddress)
			out = append(out, SwitchedSession{
				PDUSessionID: item.PDUSessionID.Value,
				UPFAddress:   v4,
				UPFTEID:      binary.BigEndian.Uint32(gt.GTPTEID.Value),
			})
		}
	}
	return out
}

// PathSwitchAckItem is one entry of a PATH SWITCH REQUEST ACKNOWLEDGE's PDU
// Session Resource Switched List, as received (TS 38.413 9.2.3.9).
type PathSwitchAckItem struct {
	PDUSessionID int64
	// TransferLen is the length in octets of the item's Path Switch Request
	// Acknowledge Transfer (TS 38.413 9.3.4.9); zero means the item carries
	// none.
	TransferLen int
	// TransferErr is why the transfer does not decode, "" when it does.
	TransferErr string
	// HasULTNL reports whether the transfer carries the optional UL NG-U UP
	// TNL Information.
	HasULTNL bool
}

// HasTransfer reports whether the item carries a Path Switch Request
// Acknowledge Transfer that decodes.
func (i PathSwitchAckItem) HasTransfer() bool { return i.TransferLen > 0 && i.TransferErr == "" }

// PathSwitchAck is the PDU Session Resource Switched List of a PATH SWITCH
// REQUEST ACKNOWLEDGE.
type PathSwitchAck struct {
	// ListPresent reports whether the message carries the list at all.
	ListPresent bool
	Items       []PathSwitchAckItem
}

// ParsePathSwitchAcknowledgeItems reads the PDU Session Resource Switched
// List of a PATH SWITCH REQUEST ACKNOWLEDGE item by item, keeping the items
// whose transfer is empty or does not decode. Any other PDU yields the zero
// value.
func ParsePathSwitchAcknowledgeItems(pdu *ngapType.NGAPPDU) PathSwitchAck {
	var out PathSwitchAck
	if pdu.Present != ngapType.NGAPPDUPresentSuccessfulOutcome ||
		pdu.SuccessfulOutcome.Value.Present != ngapType.SuccessfulOutcomePresentPathSwitchRequestAcknowledge {
		return out
	}
	for _, ie := range pdu.SuccessfulOutcome.Value.PathSwitchRequestAcknowledge.ProtocolIEs.List {
		if ie.Id.Value != ngapType.ProtocolIEIDPDUSessionResourceSwitchedList ||
			ie.Value.PDUSessionResourceSwitchedList == nil {
			continue
		}
		out.ListPresent = true
		for _, item := range ie.Value.PDUSessionResourceSwitchedList.List {
			it := PathSwitchAckItem{PDUSessionID: item.PDUSessionID.Value, TransferLen: len(item.PathSwitchRequestAcknowledgeTransfer)}
			if it.TransferLen > 0 {
				var t ngapType.PathSwitchRequestAcknowledgeTransfer
				if err := aper.UnmarshalWithParams(item.PathSwitchRequestAcknowledgeTransfer, &t, "valueExt"); err != nil {
					it.TransferErr = err.Error()
				} else {
					it.HasULTNL = t.ULNGUUPTNLInformation != nil
				}
			}
			out.Items = append(out.Items, it)
		}
	}
	return out
}

// DescribePathSwitchAnswer names the PDU that answered a PATH SWITCH REQUEST
// when it is not an Acknowledge.
func DescribePathSwitchAnswer(pdu *ngapType.NGAPPDU) string {
	switch ClassifyPathSwitch(pdu) {
	case PathSwitchAcknowledged:
		return "PATH SWITCH REQUEST ACKNOWLEDGE"
	case PathSwitchFailed:
		return "PATH SWITCH REQUEST FAILURE"
	}
	switch pdu.Present {
	case ngapType.NGAPPDUPresentInitiatingMessage:
		return fmt.Sprintf("an initiating message, procedure code %d", pdu.InitiatingMessage.ProcedureCode.Value)
	case ngapType.NGAPPDUPresentSuccessfulOutcome:
		return fmt.Sprintf("a successful outcome, procedure code %d", pdu.SuccessfulOutcome.ProcedureCode.Value)
	case ngapType.NGAPPDUPresentUnsuccessfulOutcome:
		return fmt.Sprintf("an unsuccessful outcome, procedure code %d", pdu.UnsuccessfulOutcome.ProcedureCode.Value)
	}
	return "an undecodable PDU"
}
