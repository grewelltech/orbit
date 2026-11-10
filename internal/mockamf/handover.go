package mockamf

import (
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"
)

// N2 handover, network side (TS 38.413 §8.4.1 to §8.4.3). The source gNB's
// HANDOVER REQUIRED names the target gNB; the mock finds that gNB's
// association by the gNB ID of its NG Setup, sends it a HANDOVER REQUEST,
// and answers the source with a HANDOVER COMMAND once the target
// acknowledges. The target's HANDOVER NOTIFY moves the UE and its sessions
// to the target, tells the UPF, and releases the UE's context at the
// source.

// HandoverAnswer selects how the mock answers a HANDOVER REQUIRED that
// names a known UE with a PDU session and a connected target gNB
// (WithHandoverAnswer).
type HandoverAnswer int

const (
	// CommandHandover prepares the target and answers with a HANDOVER
	// COMMAND whose PDU Session Resource Handover List carries a Handover
	// Command Transfer per session: the default.
	CommandHandover HandoverAnswer = iota
	// CommandWithoutList answers with a HANDOVER COMMAND that has no PDU
	// Session Resource Handover List.
	CommandWithoutList
	// CommandWithoutTransfer lists each session with an empty Handover
	// Command Transfer.
	CommandWithoutTransfer
	// CommandWithBadTransfer lists each session with a Handover Command
	// Transfer that does not decode.
	CommandWithBadTransfer
	// FailHandoverPreparation answers with a HANDOVER PREPARATION FAILURE
	// and never contacts the target.
	FailHandoverPreparation
	// IgnoreHandoverRequired leaves the request unanswered.
	IgnoreHandoverRequired
)

// WithHandoverAnswer makes the mock answer a HANDOVER REQUIRED as ans says.
// The default is CommandHandover.
func WithHandoverAnswer(ans HandoverAnswer) Option {
	return func(a *AMF) { a.handoverAnswer = ans }
}

// handover is one N2 handover between HANDOVER REQUIRED and HANDOVER
// NOTIFY, guarded by AMF.mu.
type handover struct {
	st       *ueState
	src, tgt *pipe
	srcRanID int64
	sessions []uint8
	// tgtRanID and newDL are what the target's acknowledgement gave: its
	// RAN-UE-NGAP-ID and each admitted session's downlink tunnel. acked is
	// set once it arrived.
	tgtRanID int64
	newDL    map[uint8]Tunnel
	acked    bool
}

// HandoversCompleted reports how many N2 handovers reached HANDOVER NOTIFY.
func (a *AMF) HandoversCompleted() int64 { return a.handoversDone.Load() }

// sendTo sends pdu on another association than the one being served. That
// association closing is not an error of this one, so a failed write is
// logged and dropped.
func (a *AMF) sendTo(p *pipe, pdu ngapType.NGAPPDU) {
	if err := a.send(p, pdu); err != nil {
		a.log.Debug("mock amf: a PDU for another association was not sent", "err", err)
	}
}

// forgetAssociation removes p from the gNB associations and drops the
// handovers it was the source or the target of.
func (a *AMF) forgetAssociation(p *pipe) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, q := range a.assocs {
		if q == p {
			delete(a.assocs, id)
		}
	}
	for id, h := range a.handovers {
		if h.src == p || h.tgt == p {
			delete(a.handovers, id)
		}
	}
}

// gnbIDOf reads a gNB ID bit string.
func gnbIDOf(bs *aper.BitString) uint32 {
	var id uint64
	for _, b := range bs.Bytes {
		id = id<<8 | uint64(b)
	}
	return uint32(id >> (uint64(len(bs.Bytes))*8 - bs.BitLength))
}

// onHandoverRequired answers a source gNB's HANDOVER REQUIRED: a HANDOVER
// REQUEST to the target named in it, or a HANDOVER PREPARATION FAILURE to
// the source when the UE, any of its listed sessions or the target's
// association is unknown (always so without a UPF).
func (a *AMF) onHandoverRequired(p *pipe, req *ngapType.HandoverRequired) error {
	var amfID, ranID int64
	var target uint32
	var haveTarget bool
	var requested []uint8
	var container aper.OctetString
	for _, ie := range req.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDAMFUENGAPID:
			amfID = ie.Value.AMFUENGAPID.Value
		case ngapType.ProtocolIEIDRANUENGAPID:
			ranID = ie.Value.RANUENGAPID.Value
		case ngapType.ProtocolIEIDTargetID:
			if t := ie.Value.TargetID; t != nil && t.TargetRANNodeID != nil {
				if g := t.TargetRANNodeID.GlobalRANNodeID.GlobalGNBID; g != nil && g.GNBID.GNBID != nil {
					target, haveTarget = gnbIDOf(g.GNBID.GNBID), true
				}
			}
		case ngapType.ProtocolIEIDPDUSessionResourceListHORqd:
			for _, it := range ie.Value.PDUSessionResourceListHORqd.List {
				requested = append(requested, uint8(it.PDUSessionID.Value))
			}
		case ngapType.ProtocolIEIDSourceToTargetTransparentContainer:
			container = ie.Value.SourceToTargetTransparentContainer.Value
		}
	}
	if a.handoverAnswer == IgnoreHandoverRequired {
		return nil
	}

	type setup struct {
		id uint8
		ul Tunnel
	}
	var sessions []setup
	h := &handover{src: p, srcRanID: ranID}
	a.mu.Lock()
	h.st = a.ues[amfID]
	if haveTarget {
		h.tgt = a.assocs[target]
	}
	if h.st != nil {
		for _, id := range requested {
			if s := h.st.sessions[id]; s != nil {
				sessions = append(sessions, setup{id: id, ul: s.ul})
				h.sessions = append(h.sessions, id)
			}
		}
	}
	ok := h.st != nil && h.tgt != nil && h.tgt != p && len(sessions) > 0 && a.handoverAnswer != FailHandoverPreparation
	if ok {
		a.handovers[amfID] = h
	}
	a.mu.Unlock()
	if !ok {
		return a.send(p, buildHandoverPreparationFailure(amfID, ranID))
	}

	var items []ngapType.PDUSessionResourceSetupItemHOReq
	for _, s := range sessions {
		tr, err := encodeSetupRequestTransfer(s.ul)
		if err != nil {
			return err
		}
		item := ngapType.PDUSessionResourceSetupItemHOReq{}
		item.PDUSessionID.Value = int64(s.id)
		item.SNSSAI = snssai(a.cfg.SST, a.sdBytes)
		item.HandoverRequestTransfer = tr
		items = append(items, item)
	}
	a.sendTo(h.tgt, buildHandoverRequest(amfID, items, container, a.plmn, a.cfg.SST, a.sdBytes))
	return nil
}

// onHandoverRequestAcknowledge takes the target's HANDOVER REQUEST
// ACKNOWLEDGE, records the downlink tunnels it admitted, and sends the
// source its HANDOVER COMMAND, shaped as WithHandoverAnswer says. An
// acknowledgement for no handover in preparation is ignored.
func (a *AMF) onHandoverRequestAcknowledge(p *pipe, ack *ngapType.HandoverRequestAcknowledge) error {
	var amfID, ranID int64
	newDL := map[uint8]Tunnel{}
	var container aper.OctetString
	for _, ie := range ack.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDAMFUENGAPID:
			amfID = ie.Value.AMFUENGAPID.Value
		case ngapType.ProtocolIEIDRANUENGAPID:
			ranID = ie.Value.RANUENGAPID.Value
		case ngapType.ProtocolIEIDPDUSessionResourceAdmittedList:
			for _, it := range ie.Value.PDUSessionResourceAdmittedList.List {
				var tr ngapType.HandoverRequestAcknowledgeTransfer
				if err := aper.UnmarshalWithParams(it.HandoverRequestAcknowledgeTransfer, &tr, "valueExt"); err != nil {
					return fmt.Errorf("mock amf decode handover request acknowledge transfer: %w", err)
				}
				dl, err := tunnelFrom(tr.DLNGUUPTNLInformation)
				if err != nil {
					return err
				}
				newDL[uint8(it.PDUSessionID.Value)] = dl
			}
		case ngapType.ProtocolIEIDTargetToSourceTransparentContainer:
			container = ie.Value.TargetToSourceTransparentContainer.Value
		}
	}
	a.mu.Lock()
	h := a.handovers[amfID]
	if h == nil || h.tgt != p {
		a.mu.Unlock()
		return nil
	}
	h.tgtRanID, h.newDL, h.acked = ranID, newDL, true
	src, srcRanID := h.src, h.srcRanID
	sessions := append([]uint8(nil), h.sessions...)
	a.mu.Unlock()

	cmd, err := buildHandoverCommand(amfID, srcRanID, sessions, container, a.handoverAnswer)
	if err != nil {
		return err
	}
	a.sendTo(src, cmd)
	return nil
}

// onHandoverNotify completes a prepared handover on the target's HANDOVER
// NOTIFY: the UE and its admitted sessions move to the target association,
// the UPF is told of each session's new downlink tunnel, and the source is
// sent a UE CONTEXT RELEASE COMMAND. A notify for no acknowledged handover
// is ignored.
func (a *AMF) onHandoverNotify(p *pipe, states map[int64]*ueState, n *ngapType.HandoverNotify) error {
	var amfID int64
	for _, ie := range n.ProtocolIEs.List {
		if ie.Id.Value == ngapType.ProtocolIEIDAMFUENGAPID {
			amfID = ie.Value.AMFUENGAPID.Value
		}
	}
	var switches []PathSwitch
	a.mu.Lock()
	h := a.handovers[amfID]
	if h == nil || h.tgt != p || !h.acked {
		a.mu.Unlock()
		return nil
	}
	delete(a.handovers, amfID)
	for _, id := range h.sessions {
		s, dl := h.st.sessions[id], h.newDL[id]
		if s == nil || dl == (Tunnel{}) {
			continue
		}
		switches = append(switches, PathSwitch{SUPI: h.st.supi, PDUSessionID: id, OldUL: s.ul, NewUL: s.ul, OldDL: s.dl, NewDL: dl})
		s.dl = dl
	}
	h.st.ranID = h.tgtRanID
	a.mu.Unlock()
	states[h.tgtRanID] = h.st
	a.handoversDone.Add(1)

	if a.cfg.UPF != nil {
		for _, ps := range switches {
			a.cfg.UPF.PathSwitched(ps)
		}
	}
	a.sendTo(h.src, buildUEContextReleaseCommandCause(amfID, h.srcRanID, ngapType.CauseNasPresentNormalRelease))
	return nil
}

// buildHandoverPreparationFailure builds the AMF's HANDOVER PREPARATION
// FAILURE (TS 38.413 §9.2.3.3), cause radio network
// "ho-failure-in-target-5GC-ngran-node-or-target-system".
func buildHandoverPreparationFailure(amfID, ranID int64) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentUnsuccessfulOutcome
	pdu.UnsuccessfulOutcome = new(ngapType.UnsuccessfulOutcome)
	pdu.UnsuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeHandoverPreparation
	pdu.UnsuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.UnsuccessfulOutcome.Value.Present = ngapType.UnsuccessfulOutcomePresentHandoverPreparationFailure
	pdu.UnsuccessfulOutcome.Value.HandoverPreparationFailure = new(ngapType.HandoverPreparationFailure)
	list := &pdu.UnsuccessfulOutcome.Value.HandoverPreparationFailure.ProtocolIEs.List
	add := func(ieID int64, fill func(*ngapType.HandoverPreparationFailureIEs)) {
		ie := ngapType.HandoverPreparationFailureIEs{}
		ie.Id.Value = ieID
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		fill(&ie)
		*list = append(*list, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, func(ie *ngapType.HandoverPreparationFailureIEs) {
		ie.Value.Present = ngapType.HandoverPreparationFailureIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, func(ie *ngapType.HandoverPreparationFailureIEs) {
		ie.Value.Present = ngapType.HandoverPreparationFailureIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranID}
	})
	add(ngapType.ProtocolIEIDCause, func(ie *ngapType.HandoverPreparationFailureIEs) {
		ie.Value.Present = ngapType.HandoverPreparationFailureIEsPresentCause
		ie.Value.Cause = &ngapType.Cause{Present: ngapType.CausePresentRadioNetwork, RadioNetwork: &ngapType.CauseRadioNetwork{
			Value: ngapType.CauseRadioNetworkPresentHoFailureInTarget5GCNgranNodeOrTargetSystem}}
	})
	return pdu
}

// buildHandoverRequest builds the AMF's HANDOVER REQUEST to the target gNB
// (TS 38.413 §9.2.3.4) for an intra-5GS handover: the UE's AMF-UE-NGAP-ID,
// a UE AMBR, the NR security capabilities, a security context (NCC 2, a
// zero NH: the mock does no key refresh), the sessions to set up, the
// allowed NSSAI, the source's transparent container and the GUAMI.
func buildHandoverRequest(amfID int64, items []ngapType.PDUSessionResourceSetupItemHOReq, container aper.OctetString, plmn [3]byte, sst uint8, sd []byte) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeHandoverResourceAllocation
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentHandoverRequest
	pdu.InitiatingMessage.Value.HandoverRequest = new(ngapType.HandoverRequest)
	list := &pdu.InitiatingMessage.Value.HandoverRequest.ProtocolIEs.List
	add := func(ieID int64, crit aper.Enumerated, fill func(*ngapType.HandoverRequestIEs)) {
		ie := ngapType.HandoverRequestIEs{}
		ie.Id.Value = ieID
		ie.Criticality.Value = crit
		fill(&ie)
		*list = append(*list, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequestIEs) {
		ie.Value.Present = ngapType.HandoverRequestIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	})
	add(ngapType.ProtocolIEIDHandoverType, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequestIEs) {
		ie.Value.Present = ngapType.HandoverRequestIEsPresentHandoverType
		ie.Value.HandoverType = &ngapType.HandoverType{Value: ngapType.HandoverTypePresentIntra5gs}
	})
	add(ngapType.ProtocolIEIDCause, ngapType.CriticalityPresentIgnore, func(ie *ngapType.HandoverRequestIEs) {
		ie.Value.Present = ngapType.HandoverRequestIEsPresentCause
		ie.Value.Cause = &ngapType.Cause{Present: ngapType.CausePresentRadioNetwork, RadioNetwork: &ngapType.CauseRadioNetwork{
			Value: ngapType.CauseRadioNetworkPresentHandoverDesirableForRadioReason}}
	})
	add(ngapType.ProtocolIEIDUEAggregateMaximumBitRate, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequestIEs) {
		ie.Value.Present = ngapType.HandoverRequestIEsPresentUEAggregateMaximumBitRate
		ie.Value.UEAggregateMaximumBitRate = &ngapType.UEAggregateMaximumBitRate{
			UEAggregateMaximumBitRateDL: ngapType.BitRate{Value: 100_000_000},
			UEAggregateMaximumBitRateUL: ngapType.BitRate{Value: 100_000_000},
		}
	})
	add(ngapType.ProtocolIEIDUESecurityCapabilities, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequestIEs) {
		ie.Value.Present = ngapType.HandoverRequestIEsPresentUESecurityCapabilities
		nr := aper.BitString{Bytes: []byte{0xE0, 0x00}, BitLength: 16} // algorithms 1 to 3
		none := aper.BitString{Bytes: []byte{0x00, 0x00}, BitLength: 16}
		c := &ngapType.UESecurityCapabilities{}
		c.NRencryptionAlgorithms.Value = nr
		c.NRintegrityProtectionAlgorithms.Value = nr
		c.EUTRAencryptionAlgorithms.Value = none
		c.EUTRAintegrityProtectionAlgorithms.Value = none
		ie.Value.UESecurityCapabilities = c
	})
	add(ngapType.ProtocolIEIDSecurityContext, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequestIEs) {
		ie.Value.Present = ngapType.HandoverRequestIEsPresentSecurityContext
		ie.Value.SecurityContext = &ngapType.SecurityContext{
			NextHopChainingCount: ngapType.NextHopChainingCount{Value: 2},
			NextHopNH:            ngapType.SecurityKey{Value: aper.BitString{Bytes: make([]byte, 32), BitLength: 256}},
		}
	})
	add(ngapType.ProtocolIEIDPDUSessionResourceSetupListHOReq, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequestIEs) {
		ie.Value.Present = ngapType.HandoverRequestIEsPresentPDUSessionResourceSetupListHOReq
		ie.Value.PDUSessionResourceSetupListHOReq = &ngapType.PDUSessionResourceSetupListHOReq{List: items}
	})
	add(ngapType.ProtocolIEIDAllowedNSSAI, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequestIEs) {
		ie.Value.Present = ngapType.HandoverRequestIEsPresentAllowedNSSAI
		ie.Value.AllowedNSSAI = &ngapType.AllowedNSSAI{List: []ngapType.AllowedNSSAIItem{{SNSSAI: snssai(sst, sd)}}}
	})
	add(ngapType.ProtocolIEIDSourceToTargetTransparentContainer, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequestIEs) {
		ie.Value.Present = ngapType.HandoverRequestIEsPresentSourceToTargetTransparentContainer
		ie.Value.SourceToTargetTransparentContainer = &ngapType.SourceToTargetTransparentContainer{Value: container}
	})
	add(ngapType.ProtocolIEIDGUAMI, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverRequestIEs) {
		ie.Value.Present = ngapType.HandoverRequestIEsPresentGUAMI
		g := guami(plmn)
		ie.Value.GUAMI = &g
	})
	return pdu
}

// buildHandoverCommand builds the AMF's HANDOVER COMMAND to the source gNB
// (TS 38.413 §9.2.3.2): the UE's NGAP IDs, the handover type, the PDU
// Session Resource Handover List with a Handover Command Transfer per
// session (every IE of the transfer is optional, and the mock sets none)
// and the target's transparent container. ans leaves the list out, or
// replaces each transfer with an empty or an undecodable one.
func buildHandoverCommand(amfID, ranID int64, sessions []uint8, container aper.OctetString, ans HandoverAnswer) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	transfer, err := aper.MarshalWithParams(ngapType.HandoverCommandTransfer{}, "valueExt")
	if err != nil {
		return pdu, fmt.Errorf("mock amf encode handover command transfer: %w", err)
	}
	switch ans {
	case CommandWithoutTransfer:
		transfer = aper.OctetString{}
	case CommandWithBadTransfer:
		// The extension bit and all three optional IEs flagged present, and
		// no octet after them.
		transfer = aper.OctetString{0xf8}
	}
	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeHandoverPreparation
	pdu.SuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentHandoverCommand
	pdu.SuccessfulOutcome.Value.HandoverCommand = new(ngapType.HandoverCommand)
	list := &pdu.SuccessfulOutcome.Value.HandoverCommand.ProtocolIEs.List
	add := func(ieID int64, crit aper.Enumerated, fill func(*ngapType.HandoverCommandIEs)) {
		ie := ngapType.HandoverCommandIEs{}
		ie.Id.Value = ieID
		ie.Criticality.Value = crit
		fill(&ie)
		*list = append(*list, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverCommandIEs) {
		ie.Value.Present = ngapType.HandoverCommandIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverCommandIEs) {
		ie.Value.Present = ngapType.HandoverCommandIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranID}
	})
	add(ngapType.ProtocolIEIDHandoverType, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverCommandIEs) {
		ie.Value.Present = ngapType.HandoverCommandIEsPresentHandoverType
		ie.Value.HandoverType = &ngapType.HandoverType{Value: ngapType.HandoverTypePresentIntra5gs}
	})
	if ans != CommandWithoutList && len(sessions) > 0 {
		var items []ngapType.PDUSessionResourceHandoverItem
		for _, id := range sessions {
			item := ngapType.PDUSessionResourceHandoverItem{}
			item.PDUSessionID.Value = int64(id)
			item.HandoverCommandTransfer = transfer
			items = append(items, item)
		}
		add(ngapType.ProtocolIEIDPDUSessionResourceHandoverList, ngapType.CriticalityPresentIgnore, func(ie *ngapType.HandoverCommandIEs) {
			ie.Value.Present = ngapType.HandoverCommandIEsPresentPDUSessionResourceHandoverList
			ie.Value.PDUSessionResourceHandoverList = &ngapType.PDUSessionResourceHandoverList{List: items}
		})
	}
	add(ngapType.ProtocolIEIDTargetToSourceTransparentContainer, ngapType.CriticalityPresentReject, func(ie *ngapType.HandoverCommandIEs) {
		ie.Value.Present = ngapType.HandoverCommandIEsPresentTargetToSourceTransparentContainer
		ie.Value.TargetToSourceTransparentContainer = &ngapType.TargetToSourceTransparentContainer{Value: container}
	})
	return pdu, nil
}
