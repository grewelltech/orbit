package gnb

import (
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

// N2 (AMF-mediated) handover, source side (TS 38.413 §8.4.1). The source gNB
// sends HandoverRequired; the AMF drives HandoverRequest to the target and
// returns HandoverCommand (or HandoverPreparationFailure). This is the
// mobility path ORBIT validates against the real core — the RRC container is
// opaque to the AMF (D-3), so the source-to-target transparent container
// carries a placeholder RRC blob inside a well-formed NGAP container.

// HandoverParams describes one N2 handover to prepare.
type HandoverParams struct {
	Source        Config  // source gNB
	Target        Config  // target gNB (target cell + TargetID)
	AMFUENGAPID   int64   // AMF's UE ID (from the attach)
	RANUENGAPID   int64   // source gNB's UE ID
	PDUSessionIDs []int64 // sessions to hand over
}

// BuildHandoverRequired constructs the source-side HandoverRequired PDU
// (TS 38.413 §9.2.3.1) for an intra-5GS handover.
func BuildHandoverRequired(p HandoverParams) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	srcNRCGI, err := nrCGI(p.Source)
	if err != nil {
		return pdu, err
	}
	tgtNRCGI, err := nrCGI(p.Target)
	if err != nil {
		return pdu, err
	}
	container, err := encodeSourceToTargetContainer(srcNRCGI, tgtNRCGI)
	if err != nil {
		return pdu, err
	}
	hoRqdTransfer, err := aper.MarshalWithParams(ngapType.HandoverRequiredTransfer{}, "valueExt")
	if err != nil {
		return pdu, fmt.Errorf("encode HandoverRequiredTransfer: %w", err)
	}
	tgtPLMN, err := ngap.EncodePLMN(p.Target.MCC, p.Target.MNC)
	if err != nil {
		return pdu, err
	}

	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeHandoverPreparation
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentHandoverRequired
	pdu.InitiatingMessage.Value.HandoverRequired = new(ngapType.HandoverRequired)
	list := &pdu.InitiatingMessage.Value.HandoverRequired.ProtocolIEs.List

	add := func(id int64, crit aper.Enumerated, fill func(*ngapType.HandoverRequiredIEs)) {
		ie := ngapType.HandoverRequiredIEs{}
		ie.Id.Value = id
		ie.Criticality.Value = crit
		fill(&ie)
		*list = append(*list, ie)
	}

	add(ngapType.ProtocolIEIDAMFUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequiredIEs) {
		ie.Value.Present = ngapType.HandoverRequiredIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: p.AMFUENGAPID}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequiredIEs) {
		ie.Value.Present = ngapType.HandoverRequiredIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: p.RANUENGAPID}
	})
	add(ngapType.ProtocolIEIDHandoverType, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequiredIEs) {
		ie.Value.Present = ngapType.HandoverRequiredIEsPresentHandoverType
		ie.Value.HandoverType = &ngapType.HandoverType{Value: ngapType.HandoverTypePresentIntra5gs}
	})
	add(ngapType.ProtocolIEIDCause, ngapType.CriticalityPresentIgnore, func(ie *ngapType.HandoverRequiredIEs) {
		ie.Value.Present = ngapType.HandoverRequiredIEsPresentCause
		ie.Value.Cause = &ngapType.Cause{
			Present:      ngapType.CausePresentRadioNetwork,
			RadioNetwork: &ngapType.CauseRadioNetwork{Value: ngapType.CauseRadioNetworkPresentHandoverDesirableForRadioReason},
		}
	})
	add(ngapType.ProtocolIEIDTargetID, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequiredIEs) {
		ie.Value.Present = ngapType.HandoverRequiredIEsPresentTargetID
		t := &ngapType.TargetID{Present: ngapType.TargetIDPresentTargetRANNodeID, TargetRANNodeID: new(ngapType.TargetRANNodeID)}
		t.TargetRANNodeID.GlobalRANNodeID = globalGNBID(p.Target, tgtPLMN)
		t.TargetRANNodeID.SelectedTAI.PLMNIdentity.Value = aper.OctetString(tgtPLMN[:])
		t.TargetRANNodeID.SelectedTAI.TAC.Value = aper.OctetString{byte(p.Target.TAC >> 16), byte(p.Target.TAC >> 8), byte(p.Target.TAC)}
		ie.Value.TargetID = t
	})
	add(ngapType.ProtocolIEIDPDUSessionResourceListHORqd, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequiredIEs) {
		ie.Value.Present = ngapType.HandoverRequiredIEsPresentPDUSessionResourceListHORqd
		ie.Value.PDUSessionResourceListHORqd = new(ngapType.PDUSessionResourceListHORqd)
		for _, id := range p.PDUSessionIDs {
			item := ngapType.PDUSessionResourceItemHORqd{}
			item.PDUSessionID.Value = id
			item.HandoverRequiredTransfer = hoRqdTransfer
			ie.Value.PDUSessionResourceListHORqd.List = append(ie.Value.PDUSessionResourceListHORqd.List, item)
		}
	})
	add(ngapType.ProtocolIEIDSourceToTargetTransparentContainer, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequiredIEs) {
		ie.Value.Present = ngapType.HandoverRequiredIEsPresentSourceToTargetTransparentContainer
		ie.Value.SourceToTargetTransparentContainer = &ngapType.SourceToTargetTransparentContainer{Value: container}
	})
	return pdu, nil
}

func nrCGI(cfg Config) (ngapType.NRCGI, error) {
	var c ngapType.NRCGI
	plmn, err := ngap.EncodePLMN(cfg.MCC, cfg.MNC)
	if err != nil {
		return c, err
	}
	id := uint64(cfg.ID) << 4 // one cell per gNB in the sim
	c.PLMNIdentity.Value = aper.OctetString(plmn[:])
	c.NRCellIdentity.Value = aper.BitString{
		Bytes:     []byte{byte(id >> 32), byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id << 4)},
		BitLength: 36,
	}
	return c, nil
}

func globalGNBID(cfg Config, plmn [3]byte) ngapType.GlobalRANNodeID {
	g := ngapType.GlobalRANNodeID{Present: ngapType.GlobalRANNodeIDPresentGlobalGNBID, GlobalGNBID: new(ngapType.GlobalGNBID)}
	g.GlobalGNBID.PLMNIdentity.Value = aper.OctetString(plmn[:])
	g.GlobalGNBID.GNBID.Present = ngapType.GNBIDPresentGNBID
	g.GlobalGNBID.GNBID.GNBID = new(aper.BitString)
	*g.GlobalGNBID.GNBID.GNBID = gnbIDBitString(cfg.ID, cfg.IDBits)
	return g
}

// encodeSourceToTargetContainer builds the NGAP source-to-target transparent
// container (TS 38.413 §9.3.1.20): a placeholder RRC blob (opaque to the AMF —
// D-3), the target cell, and a minimal UE history. APER-encoded to the OCTET
// STRING the HandoverRequired carries.
func encodeSourceToTargetContainer(srcNRCGI, tgtNRCGI ngapType.NRCGI) (aper.OctetString, error) {
	var c ngapType.SourceNGRANNodeToTargetNGRANNodeTransparentContainer
	c.RRCContainer.Value = aper.OctetString{0x00, 0x00, 0x00, 0x00} // placeholder RRC (D-3)
	c.TargetCellID.Present = ngapType.NGRANCGIPresentNRCGI
	c.TargetCellID.NRCGI = &tgtNRCGI

	item := ngapType.LastVisitedCellItem{}
	item.LastVisitedCellInformation.Present = ngapType.LastVisitedCellInformationPresentNGRANCell
	item.LastVisitedCellInformation.NGRANCell = new(ngapType.LastVisitedNGRANCellInformation)
	nr := item.LastVisitedCellInformation.NGRANCell
	nr.GlobalCellID.Present = ngapType.NGRANCGIPresentNRCGI
	nr.GlobalCellID.NRCGI = &srcNRCGI
	nr.CellType.CellSize.Value = ngapType.CellSizePresentMedium
	nr.TimeUEStayedInCell.Value = 0
	c.UEHistoryInformation.List = append(c.UEHistoryInformation.List, item)

	b, err := aper.MarshalWithParams(c, "valueExt")
	if err != nil {
		return nil, fmt.Errorf("encode source-to-target transparent container: %w", err)
	}
	return b, nil
}

// HandoverOutcome classifies the AMF's response to HandoverRequired.
type HandoverOutcome int

const (
	HandoverOther             HandoverOutcome = iota
	HandoverCommandReceived                   // AMF drove it — supported
	HandoverPreparationFailed                 // AMF rejected preparation
	HandoverRequestAtTarget                   // AMF forwarded to the target gNB
)

// ClassifyHandover inspects a downlink PDU for the handover-procedure
// outcomes relevant to the D-1 probe.
func ClassifyHandover(pdu *ngapType.NGAPPDU) HandoverOutcome {
	switch pdu.Present {
	case ngapType.NGAPPDUPresentSuccessfulOutcome:
		if pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentHandoverCommand {
			return HandoverCommandReceived
		}
	case ngapType.NGAPPDUPresentUnsuccessfulOutcome:
		if pdu.UnsuccessfulOutcome.Value.Present == ngapType.UnsuccessfulOutcomePresentHandoverPreparationFailure {
			return HandoverPreparationFailed
		}
	case ngapType.NGAPPDUPresentInitiatingMessage:
		if pdu.InitiatingMessage.Value.Present == ngapType.InitiatingMessagePresentHandoverRequest {
			return HandoverRequestAtTarget
		}
	}
	return HandoverOther
}
