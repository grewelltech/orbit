package mockamf

import (
	"encoding/binary"

	"github.com/free5gc/aper"
	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/nas"
)

// CM-IDLE, network side (TS 23.502 4.2.6, 4.2.3.2, 4.2.3.3). A UE whose
// gNB asked for its release, and completed it, is CM-IDLE: its sessions are
// kept, and a UPF that buffers (ANReleaser) is told. DownlinkData, the
// stand-in for the SMF's N1N2MessageTransfer, pages an idle UE on every
// association (WithGUTI: paging needs a 5G-S-TMSI). A Service Request in an
// Initial UE Message is answered with an Initial Context Setup Request
// carrying the Service Accept and the sessions to reactivate; the UPF gets
// SessionUp again with the gNB's new downlink tunnel.

// ANReleaser is a UPF that reacts to an AN release: s is a session of a UE
// that has just become CM-IDLE, whose downlink tunnel is gone. It is called
// from an association goroutine and must not block.
type ANReleaser interface {
	ANReleased(s SessionInfo)
}

// ServiceAnswer is how the mock answers a Service Request.
type ServiceAnswer int

const (
	// ServiceAccept answers with Service Accept and sets up the user plane
	// of the sessions the request named or downlink data waits for.
	ServiceAccept ServiceAnswer = iota
	// ServiceAcceptNoUserPlane answers with Service Accept and sets up no
	// session.
	ServiceAcceptNoUserPlane
	// ServiceAcceptUPFNotTold answers as ServiceAccept does and sets the
	// sessions up on N2, and never tells the UPF the gNB's new downlink
	// tunnels: its downlink stays buffered.
	ServiceAcceptUPFNotTold
	// ServiceReject answers with Service Reject and releases the UE.
	ServiceReject
	// ServiceIgnore leaves the request unanswered.
	ServiceIgnore
)

// WithNoPaging makes the mock keep a CM-IDLE UE's downlink data
// notification to itself: no PAGING is sent.
func WithNoPaging() Option { return func(a *AMF) { a.noPaging = true } }

// WithWrongPagingIdentity makes the mock page with a 5G-S-TMSI that is not
// the UE's (its 5G-TMSI with every bit flipped).
func WithWrongPagingIdentity() Option { return func(a *AMF) { a.pagingTMSIXor = 0xffffffff } }

// WithServiceAnswer sets how the mock answers a Service Request; cause is
// the 5GMM cause of a ServiceReject.
func WithServiceAnswer(answer ServiceAnswer, cause uint8) Option {
	return func(a *AMF) { a.serviceAnswer, a.serviceCause = answer, cause }
}

// Pagings reports how many PAGING messages the mock has sent.
func (a *AMF) Pagings() int64 { return a.pagings.Load() }

// ServiceRequests reports how many Service Requests the mock has verified.
func (a *AMF) ServiceRequests() int64 { return a.serviceRequests.Load() }

// Idle reports whether the UE with supi is CM-IDLE.
func (a *AMF) Idle(supi string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, st := range a.ues {
		if st.supi == supi {
			return st.idle
		}
	}
	return false
}

// stmsiSetID and stmsiPointer are the AMF Set ID and AMF Pointer of the
// 5G-GUTIs guti5G builds (octets 0xfe, 0x00 after the AMF Region ID).
const (
	stmsiSetID   = 0xfe << 2
	stmsiPointer = 0
)

// firstTAC is the TAC of the first tracking area an NG Setup Request
// supports, nil without one.
func firstTAC(req *ngapType.NGSetupRequest) []byte {
	for _, ie := range req.ProtocolIEs.List {
		if ie.Id.Value == ngapType.ProtocolIEIDSupportedTAList && ie.Value.SupportedTAList != nil {
			for _, ta := range ie.Value.SupportedTAList.List {
				return append([]byte(nil), ta.TAC.Value...)
			}
		}
	}
	return nil
}

// onANReleaseComplete makes the UE a UE CONTEXT RELEASE COMPLETE names
// CM-IDLE when the release answered its gNB's request, and tells a
// buffering UPF about each of its sessions.
func (a *AMF) onANReleaseComplete(states map[int64]*ueState, ids [2]int64) {
	var released []SessionInfo
	a.mu.Lock()
	st := a.ues[ids[0]]
	if st == nil || !st.releasing {
		a.mu.Unlock()
		return
	}
	st.releasing, st.idle = false, true
	delete(states, st.ranID)
	for _, s := range st.sessions {
		released = append(released, SessionInfo{SUPI: st.supi, PDUSessionID: s.id, UEIP: s.ueIP, QFI: defaultQFI, UL: s.ul, DL: s.dl})
	}
	a.mu.Unlock()
	if r, ok := a.cfg.UPF.(ANReleaser); ok {
		for _, s := range released {
			r.ANReleased(s)
		}
	}
}

// DownlinkData tells the mock that downlink data waits for a session of a
// UE, as an SMF does with N1N2MessageTransfer after the UPF's downlink data
// notification. For a CM-IDLE UE with a 5G-GUTI it sends PAGING on every
// association (unless WithNoPaging), naming the UE by its 5G-S-TMSI with
// the association's tracking area in the TAI List for Paging; a connected
// or unknown UE gets nothing. It is safe to call from any goroutine.
func (a *AMF) DownlinkData(supi string, pduSessionID uint8) {
	type target struct {
		p   *pipe
		tac []byte
	}
	var targets []target
	var tmsi uint32
	a.mu.Lock()
	for _, st := range a.ues {
		if st.supi != supi || !st.idle {
			continue
		}
		if s := st.sessions[pduSessionID]; s != nil {
			s.pending = true
		}
		tmsi = uint32(st.amfID)
		for p, tac := range a.assocs {
			targets = append(targets, target{p, tac})
		}
	}
	a.mu.Unlock()
	if a.noPaging || !a.assignGUTI {
		return
	}
	for _, t := range targets {
		if err := a.send(t.p, buildPaging(tmsi^a.pagingTMSIXor, a.plmn, t.tac)); err == nil {
			a.pagings.Add(1)
		}
	}
}

// buildPaging builds a PAGING (TS 38.413 9.2.4.1): the UE Paging Identity
// (5G-S-TMSI) and a TAI List for Paging with one TAI.
func buildPaging(tmsi uint32, plmn [3]byte, tac []byte) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodePaging
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentIgnore
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentPaging
	m := new(ngapType.Paging)
	pdu.InitiatingMessage.Value.Paging = m

	t := make([]byte, 4)
	binary.BigEndian.PutUint32(t, tmsi)
	id := ngapType.PagingIEs{}
	id.Id.Value = ngapType.ProtocolIEIDUEPagingIdentity
	id.Criticality.Value = ngapType.CriticalityPresentIgnore
	id.Value.Present = ngapType.PagingIEsPresentUEPagingIdentity
	id.Value.UEPagingIdentity = &ngapType.UEPagingIdentity{
		Present: ngapType.UEPagingIdentityPresentFiveGSTMSI,
		FiveGSTMSI: &ngapType.FiveGSTMSI{
			AMFSetID:   ngapType.AMFSetID{Value: bits(stmsiSetID, 10)},
			AMFPointer: ngapType.AMFPointer{Value: bits(stmsiPointer, 6)},
			FiveGTMSI:  ngapType.FiveGTMSI{Value: t},
		},
	}
	tais := ngapType.PagingIEs{}
	tais.Id.Value = ngapType.ProtocolIEIDTAIListForPaging
	tais.Criticality.Value = ngapType.CriticalityPresentIgnore
	tais.Value.Present = ngapType.PagingIEsPresentTAIListForPaging
	item := ngapType.TAIListForPagingItem{}
	item.TAI.PLMNIdentity.Value = aper.OctetString(plmn[:])
	item.TAI.TAC.Value = aper.OctetString(append([]byte(nil), tac...))
	tais.Value.TAIListForPaging = &ngapType.TAIListForPaging{List: []ngapType.TAIListForPagingItem{item}}
	m.ProtocolIEs.List = append(m.ProtocolIEs.List, id, tais)
	return pdu
}

// onIdleInitialUE handles a protected initial NAS message: a CM-IDLE UE's
// Service Request or Deregistration Request, found by the Initial UE
// Message's 5G-S-TMSI. A message for no known UE, or one whose MAC does
// not verify, is ignored.
func (a *AMF) onIdleInitialUE(p *pipe, states map[int64]*ueState, up *uplinkMessage) error {
	if !up.hasTMSI {
		return nil
	}
	a.mu.Lock()
	st := a.ues[int64(up.tmsi)]
	a.mu.Unlock()
	if st == nil {
		return nil
	}
	msg, err := st.decodeSecure(up.nasPDU)
	if err != nil || msg.GmmMessage == nil {
		return nil
	}
	a.mu.Lock()
	st.ranID, st.idle = up.ranID, false
	a.mu.Unlock()
	states[up.ranID] = st

	switch msg.GmmHeader.GetMessageType() {
	case f5nas.MsgTypeServiceRequest:
		a.serviceRequests.Add(1)
		return a.onServiceRequest(p, st, msg.ServiceRequest)
	case f5nas.MsgTypeDeregistrationRequestUEOriginatingDeregistration:
		a.deregs.Add(1)
		return a.onDeregistration(p, st, msg)
	}
	return nil
}

// onServiceRequest answers a verified Service Request as WithServiceAnswer
// says. The request's own IEs are read from its NAS message container when
// it has one (TS 24.501 4.4.6), deciphered under the UE's context.
func (a *AMF) onServiceRequest(p *pipe, st *ueState, req *nasMessage.ServiceRequest) error {
	if c := req.NASMessageContainer; c != nil {
		inner := append([]byte(nil), c.Buffer...)
		st.secMu.Lock()
		err := st.sec.DecipherContainer(inner)
		st.secMu.Unlock()
		if err == nil {
			if m, derr := nas.DecodePlain(inner); derr == nil && m.GmmMessage != nil && m.ServiceRequest != nil {
				req = m.ServiceRequest
			}
		}
	}
	var uplink uint16
	if u := req.UplinkDataStatus; u != nil && len(u.Buffer) >= 2 {
		uplink = uint16(u.Buffer[0]) | uint16(u.Buffer[1])<<8
	}
	amfID, ranID := st.amfID, a.ranIDOf(st)

	switch a.serviceAnswer {
	case ServiceIgnore:
		return nil
	case ServiceReject:
		rej, err := st.encodeSecure(buildServiceReject(a.serviceCause), nas.SecHdrIntegrityCiphered)
		if err != nil {
			return err
		}
		if err := a.send(p, buildDownlinkNASTransport(amfID, ranID, rej)); err != nil {
			return err
		}
		a.mu.Lock()
		st.idle = true
		a.mu.Unlock()
		return a.send(p, buildUEContextReleaseCommandCause(amfID, ranID, ngapType.CauseNasPresentNormalRelease))
	}

	// The sessions to reactivate: those the UE named and those downlink
	// data waits for.
	type item struct {
		id uint8
		ul Tunnel
	}
	var activate []item
	var status uint16
	a.mu.Lock()
	for id, s := range st.sessions {
		status |= 1 << (id & 0x0f)
		if (a.serviceAnswer == ServiceAccept || a.serviceAnswer == ServiceAcceptUPFNotTold) && (uplink&(1<<(id&0x0f)) != 0 || s.pending) {
			activate = append(activate, item{id, s.ul})
		}
	}
	a.mu.Unlock()

	acc, err := st.encodeSecure(buildServiceAccept(status, uplink != 0), nas.SecHdrIntegrityCiphered)
	if err != nil {
		return err
	}
	ics := buildInitialContextSetupRequest(amfID, ranID, acc, a.plmn, a.cfg.SST, a.sdBytes)
	if len(activate) > 0 {
		list := &ngapType.PDUSessionResourceSetupListCxtReq{}
		for _, it := range activate {
			transfer, err := setupRequestTransfer(it.ul)
			if err != nil {
				return err
			}
			ci := ngapType.PDUSessionResourceSetupItemCxtReq{}
			ci.PDUSessionID.Value = int64(it.id)
			ci.SNSSAI = snssai(a.cfg.SST, a.sdBytes)
			ci.PDUSessionResourceSetupRequestTransfer = transfer
			list.List = append(list.List, ci)
		}
		ie := ngapType.InitialContextSetupRequestIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDPDUSessionResourceSetupListCxtReq
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.InitialContextSetupRequestIEsPresentPDUSessionResourceSetupListCxtReq
		ie.Value.PDUSessionResourceSetupListCxtReq = list
		ies := &ics.InitiatingMessage.Value.InitialContextSetupRequest.ProtocolIEs.List
		*ies = append(*ies, ie)
	}
	return a.send(p, ics)
}

// onInitialContextSetupResponse records the downlink tunnels of the
// sessions an Initial Context Setup Response reports set up.
func (a *AMF) onInitialContextSetupResponse(resp *ngapType.InitialContextSetupResponse) {
	var amfID int64
	dls := map[uint8]Tunnel{}
	for _, ie := range resp.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDAMFUENGAPID:
			amfID = ie.Value.AMFUENGAPID.Value
		case ngapType.ProtocolIEIDPDUSessionResourceSetupListCxtRes:
			for _, it := range ie.Value.PDUSessionResourceSetupListCxtRes.List {
				var tr ngapType.PDUSessionResourceSetupResponseTransfer
				if err := aper.UnmarshalWithParams(it.PDUSessionResourceSetupResponseTransfer, &tr, "valueExt"); err != nil {
					continue
				}
				if dl, err := tunnelFrom(tr.DLQosFlowPerTNLInformation.UPTransportLayerInformation); err == nil {
					dls[uint8(it.PDUSessionID.Value)] = dl
				}
			}
		}
	}
	if len(dls) > 0 && a.serviceAnswer != ServiceAcceptUPFNotTold {
		a.sessionsUp(amfID, dls)
	}
}

// buildServiceAccept builds a plain Service Accept (TS 24.501 8.2.17) with
// the PDU session status and, when the request asked for reactivation, a
// PDU session reactivation result reporting no failure.
func buildServiceAccept(status uint16, reactivation bool) *f5nas.Message {
	m, gmm := plainGmm(f5nas.MsgTypeServiceAccept)
	acc := nasMessage.NewServiceAccept(f5nas.MsgTypeServiceAccept)
	setPlainHeader(&acc.ExtendedProtocolDiscriminator, &acc.SpareHalfOctetAndSecurityHeaderType,
		&acc.ServiceAcceptMessageIdentity, f5nas.MsgTypeServiceAccept)
	acc.PDUSessionStatus = &nasType.PDUSessionStatus{Iei: nasMessage.ServiceAcceptPDUSessionStatusType, Len: 2,
		Buffer: []uint8{uint8(status), uint8(status >> 8)}}
	if reactivation {
		acc.PDUSessionReactivationResult = &nasType.PDUSessionReactivationResult{
			Iei: nasMessage.ServiceAcceptPDUSessionReactivationResultType, Len: 2, Buffer: []uint8{0, 0}}
	}
	gmm.ServiceAccept = acc
	return m
}

// buildServiceReject builds a plain Service Reject (TS 24.501 8.2.18) with
// the 5GMM cause.
func buildServiceReject(cause uint8) *f5nas.Message {
	m, gmm := plainGmm(f5nas.MsgTypeServiceReject)
	rej := nasMessage.NewServiceReject(f5nas.MsgTypeServiceReject)
	setPlainHeader(&rej.ExtendedProtocolDiscriminator, &rej.SpareHalfOctetAndSecurityHeaderType,
		&rej.ServiceRejectMessageIdentity, f5nas.MsgTypeServiceReject)
	rej.Cause5GMM.Octet = cause
	gmm.ServiceReject = rej
	return m
}
