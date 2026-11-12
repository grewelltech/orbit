package mockamf

import (
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
)

// N2 handover, network side (TS 38.413 §8.4.1 to §8.4.3, §8.4.5). The source gNB's
// HANDOVER REQUIRED names the target gNB; the mock finds that gNB's
// association by the gNB ID of its NG Setup, sends it a HANDOVER REQUEST,
// and answers the source with a HANDOVER COMMAND once the target
// acknowledges. The target's HANDOVER NOTIFY moves the UE and its sessions
// to the target, tells the UPF, and releases the UE's context at the
// source. A HANDOVER CANCEL from the source before that drops the handover
// and releases what the target prepared.

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
	// FailHandoverAfterAcknowledge prepares the target and, on its HANDOVER
	// REQUEST ACKNOWLEDGE, answers the source with a HANDOVER PREPARATION
	// FAILURE.
	FailHandoverAfterAcknowledge
	// CrashOnHandoverRequired answers with nothing and crashes the mock
	// (Crash).
	CrashOnHandoverRequired
)

// WithHandoverAnswer makes the mock answer a HANDOVER REQUIRED as ans says.
// The default is CommandHandover.
func WithHandoverAnswer(ans HandoverAnswer) Option {
	return func(a *AMF) { a.handoverAnswer = ans }
}

// WithNoHandoverCancelAcknowledge makes the mock cancel a handover on a
// HANDOVER CANCEL without sending the HANDOVER CANCEL ACKNOWLEDGE. By
// default it acknowledges.
func WithNoHandoverCancelAcknowledge() Option { return func(a *AMF) { a.noCancelAck = true } }

// WithCrashOnHandoverCancel makes the mock stop on a HANDOVER CANCEL: the
// association ends, as does every other one, and no later NG Setup is
// answered.
func WithCrashOnHandoverCancel() Option { return func(a *AMF) { a.crashOnCancel = true } }

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

// HandoverCancels reports how many HANDOVER CANCELs the mock has received.
func (a *AMF) HandoverCancels() int64 { return a.handoverCancels.Load() }

// sendTo sends pdu on another association than the one being served. That
// association closing is not an error of this one, so a failed write is
// logged and dropped.
func (a *AMF) sendTo(p *pipe, pdu ngapType.NGAPPDU) {
	if err := a.send(p, pdu); err != nil {
		a.log.Debug("mock amf: a PDU for another association was not sent", "err", err)
	}
}

// association is one gNB association that completed NG Setup: the gNB ID
// and first supported TAC its NG Setup Request named, and its place in the
// order the setups completed.
type association struct {
	gnbID uint32
	tac   []byte
	seq   uint64
}

// assocOfGNBLocked is the association of the gNB id, the latest to set up
// when several named it; nil when none did. Callers hold mu.
func (a *AMF) assocOfGNBLocked(id uint32) *pipe {
	var found *pipe
	var seq uint64
	for p, as := range a.assocs {
		if as.gnbID == id && as.seq >= seq {
			found, seq = p, as.seq
		}
	}
	return found
}

// forgetAssociation drops what the mock holds for p's association once it
// has ended: its place among the gNB associations, its dropped tracking
// areas, and the handovers it was the source or the target of.
func (a *AMF) forgetAssociation(p *pipe) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.assocs, p)
	delete(a.droppedTAs, p)
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
	switch a.handoverAnswer {
	case IgnoreHandoverRequired:
		return nil
	case CrashOnHandoverRequired:
		a.Crash()
		return errCrashed
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
		h.tgt = a.assocOfGNBLocked(target)
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
	var caps *ngapType.UESecurityCapabilities
	if ok {
		a.handovers[amfID] = h
		// The target gNB holds the capabilities the Handover Request gives it.
		caps = ranSecurityCapabilities(h.st.ueCaps)
		h.st.ranCaps = caps
	}
	a.mu.Unlock()
	if !ok {
		return a.send(p, buildHandoverPreparationFailure(amfID, ranID))
	}

	var items []ngapType.PDUSessionResourceSetupItemHOReq
	for _, s := range sessions {
		tr, err := setupRequestTransfer(s.ul)
		if err != nil {
			return err
		}
		item := ngapType.PDUSessionResourceSetupItemHOReq{}
		item.PDUSessionID.Value = int64(s.id)
		item.SNSSAI = snssai(a.cfg.SST, a.sdBytes)
		item.HandoverRequestTransfer = tr
		items = append(items, item)
	}
	a.sendTo(h.tgt, buildHandoverRequest(amfID, items, container, a.plmn, a.cfg.SST, a.sdBytes, gnb.CloneUESecurityCapabilities(caps)))
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
	if a.handoverAnswer == FailHandoverAfterAcknowledge {
		delete(a.handovers, amfID)
		src, srcRanID := h.src, h.srcRanID
		a.mu.Unlock()
		a.sendTo(src, buildHandoverPreparationFailure(amfID, srcRanID))
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

// onHandoverCancel cancels the handover a source gNB's HANDOVER CANCEL
// names (TS 38.413 8.4.5): the handover is dropped, a target that had
// acknowledged is sent a UE CONTEXT RELEASE COMMAND for the context it
// prepared, and the source gets a HANDOVER CANCEL ACKNOWLEDGE, unless
// WithNoHandoverCancelAcknowledge. The UE stays where it was. A cancel
// naming no handover is acknowledged the same way. With
// WithCrashOnHandoverCancel the mock stops instead.
func (a *AMF) onHandoverCancel(p *pipe, c *ngapType.HandoverCancel) error {
	var amfID, ranID int64
	for _, ie := range c.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDAMFUENGAPID:
			amfID = ie.Value.AMFUENGAPID.Value
		case ngapType.ProtocolIEIDRANUENGAPID:
			ranID = ie.Value.RANUENGAPID.Value
		}
	}
	a.handoverCancels.Add(1)
	if a.crashOnCancel {
		a.crashed.Store(true)
		return errCrashed
	}
	a.mu.Lock()
	h := a.handovers[amfID]
	if h != nil && h.src != p {
		h = nil
	}
	if h != nil {
		delete(a.handovers, amfID)
		if h.acked {
			a.handoverReleases = append(a.handoverReleases, [2]int64{amfID, h.tgtRanID})
		}
	}
	a.mu.Unlock()
	if h != nil && h.acked {
		a.sendTo(h.tgt, buildUEContextReleaseCommandCause(amfID, h.tgtRanID, ngapType.CauseNasPresentNormalRelease))
	}
	if a.noCancelAck {
		return nil
	}
	return a.send(p, buildHandoverCancelAcknowledge(amfID, ranID))
}

// buildHandoverCancelAcknowledge builds the AMF's HANDOVER CANCEL
// ACKNOWLEDGE (TS 38.413 9.2.3.12): the UE's NGAP IDs.
func buildHandoverCancelAcknowledge(amfID, ranID int64) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeHandoverCancel
	pdu.SuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentHandoverCancelAcknowledge
	ack := new(ngapType.HandoverCancelAcknowledge)
	pdu.SuccessfulOutcome.Value.HandoverCancelAcknowledge = ack
	amf := ngapType.HandoverCancelAcknowledgeIEs{}
	amf.Id.Value = ngapType.ProtocolIEIDAMFUENGAPID
	amf.Criticality.Value = ngapType.CriticalityPresentIgnore
	amf.Value.Present = ngapType.HandoverCancelAcknowledgeIEsPresentAMFUENGAPID
	amf.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	ran := ngapType.HandoverCancelAcknowledgeIEs{}
	ran.Id.Value = ngapType.ProtocolIEIDRANUENGAPID
	ran.Criticality.Value = ngapType.CriticalityPresentIgnore
	ran.Value.Present = ngapType.HandoverCancelAcknowledgeIEsPresentRANUENGAPID
	ran.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranID}
	ack.ProtocolIEs.List = append(ack.ProtocolIEs.List, amf, ran)
	return pdu
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
func buildHandoverRequest(amfID int64, items []ngapType.PDUSessionResourceSetupItemHOReq, container aper.OctetString, plmn [3]byte, sst uint8, sd []byte, caps *ngapType.UESecurityCapabilities) ngapType.NGAPPDU {
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
		ie.Value.UESecurityCapabilities = caps
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
