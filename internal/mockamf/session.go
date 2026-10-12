package mockamf

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/free5gc/aper"
	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"
	"github.com/free5gc/ngap/ngapConvert"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/nas"
)

// PDU sessions and the Xn path switch, network side (TS 23.502 §4.3.2,
// §4.9.1.2). The mock plays AMF and SMF: it answers a UE's PDU Session
// Establishment Request with a PDU Session Resource Setup Request carrying
// the Establishment Accept and the UPF's uplink tunnel, learns the gNB's
// downlink tunnel from the Setup Response, and answers a target gNB's
// PathSwitchRequest with an Acknowledge. A UPF, when configured, is told
// about each session and each path switch; it owns the user plane.

// Tunnel is one end of an N3 GTP-U tunnel as NGAP carries it: an IPv4
// transport address and a TEID. The port is the GTP-U port, 2152.
type Tunnel struct {
	Addr string
	TEID uint32
}

// SessionInfo describes one established PDU session to the UPF: the UE's
// address, the uplink tunnel the UPF terminates and the downlink tunnel at
// the serving gNB.
type SessionInfo struct {
	SUPI         string
	PDUSessionID uint8
	UEIP         net.IP
	QFI          uint8
	UL           Tunnel // UPF side: where the gNB sends uplink
	DL           Tunnel // gNB side: where the UPF sends downlink
}

// PathSwitch describes one session's Xn path switch to the UPF: the
// downlink moves from OldDL to NewDL, and the uplink from OldUL to NewUL
// (equal unless the AMF reallocated the UPF's uplink tunnel).
type PathSwitch struct {
	SUPI         string
	PDUSessionID uint8
	OldUL, NewUL Tunnel
	OldDL, NewDL Tunnel
}

// UPF is the user-plane function the mock AMF drives, the way a real core's
// SMF drives its UPF over N4. The mock does the NGAP decoding and hands the
// UPF tunnel endpoints only, so a UPF stub needs no NGAP knowledge. Calls
// come from the association goroutines and must not block.
type UPF interface {
	// SessionUp is called once the gNB has answered the PDU Session
	// Resource Setup with its downlink tunnel.
	SessionUp(s SessionInfo)
	// PathSwitched is called after the PathSwitchRequestAcknowledge has
	// been sent, once per switched session.
	PathSwitched(p PathSwitch)
}

// defaultQFI is the one QoS flow the mock sets up per session.
const defaultQFI = 1

// pduSession is the mock's record of one UE's PDU session.
type pduSession struct {
	id   uint8
	ueIP net.IP
	ul   Tunnel
	dl   Tunnel
}

// onPDUSessionRequest answers a PDU Session Establishment Request carried in
// a UL NAS Transport: it allocates the UE address and the UPF uplink TEID,
// and sends a PDU Session Resource Setup Request carrying the secured DL NAS
// Transport with the Establishment Accept. Without a UPF the request is
// ignored, which is the mock's behaviour from before sessions existed.
func (a *AMF) onPDUSessionRequest(p *pipe, st *ueState, msg *f5nas.Message) error {
	if a.cfg.UPF == nil || msg.ULNASTransport == nil {
		return nil
	}
	t := msg.ULNASTransport
	if t.GetPayloadContainerType() != nasMessage.PayloadContainerTypeN1SMInfo {
		return nil
	}
	sm := f5nas.NewMessage()
	buf := append([]byte(nil), t.GetPayloadContainerContents()...)
	if err := sm.PlainNasDecode(&buf); err != nil {
		return fmt.Errorf("mock amf decode 5GSM: %w", err)
	}
	if sm.GsmMessage == nil || sm.PDUSessionEstablishmentRequest == nil {
		return nil
	}
	req := sm.PDUSessionEstablishmentRequest
	id := req.GetPDUSessionID()
	pti := req.GetPTI()
	var dnn string
	if t.DNN != nil {
		dnn = t.GetDNN()
	}

	s := &pduSession{id: id, ueIP: a.nextUEIP(), ul: Tunnel{Addr: a.cfg.UPFAddress, TEID: a.nextULTEID()}}
	a.mu.Lock()
	if st.sessions == nil {
		st.sessions = map[uint8]*pduSession{}
	}
	st.sessions[id] = s
	amfID, ranID := st.amfID, st.ranID
	a.mu.Unlock()

	acc, err := buildPDUSessionEstablishmentAccept(id, pti, s.ueIP, a.cfg.SST, a.sdBytes, dnn)
	if err != nil {
		return err
	}
	dl, err := buildDLNASTransportN1SM(acc, id)
	if err != nil {
		return err
	}
	wrapped, err := st.sec.EncodeSecure(dl, nas.SecHdrIntegrityCiphered)
	if err != nil {
		return err
	}
	setup, err := buildPDUSessionResourceSetupRequest(amfID, ranID, id, wrapped, s.ul, a.cfg.SST, a.sdBytes)
	if err != nil {
		return err
	}
	return a.send(p, setup)
}

// onPDUSessionSetupResponse records the gNB's downlink tunnel for each
// session in a PDU Session Resource Setup Response and tells the UPF.
func (a *AMF) onPDUSessionSetupResponse(resp *ngapType.PDUSessionResourceSetupResponse) error {
	var amfID int64
	type item struct {
		id uint8
		dl Tunnel
	}
	var items []item
	for _, ie := range resp.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDAMFUENGAPID:
			amfID = ie.Value.AMFUENGAPID.Value
		case ngapType.ProtocolIEIDPDUSessionResourceSetupListSURes:
			for _, it := range ie.Value.PDUSessionResourceSetupListSURes.List {
				var tr ngapType.PDUSessionResourceSetupResponseTransfer
				if err := aper.UnmarshalWithParams(it.PDUSessionResourceSetupResponseTransfer, &tr, "valueExt"); err != nil {
					return fmt.Errorf("mock amf decode setup response transfer: %w", err)
				}
				dl, err := tunnelFrom(tr.DLQosFlowPerTNLInformation.UPTransportLayerInformation)
				if err != nil {
					return err
				}
				items = append(items, item{id: uint8(it.PDUSessionID.Value), dl: dl})
			}
		}
	}
	var up []SessionInfo
	a.mu.Lock()
	st := a.ues[amfID]
	if st != nil {
		for _, it := range items {
			if s := st.sessions[it.id]; s != nil {
				s.dl = it.dl
				up = append(up, SessionInfo{SUPI: st.supi, PDUSessionID: s.id, UEIP: s.ueIP, QFI: defaultQFI, UL: s.ul, DL: s.dl})
			}
		}
	}
	a.mu.Unlock()
	if a.cfg.UPF != nil {
		for _, s := range up {
			a.cfg.UPF.SessionUp(s)
		}
	}
	return nil
}

// onPathSwitchRequest answers a target gNB's PathSwitchRequest on its own
// association: it finds the UE by its source AMF-UE-NGAP-ID, moves each
// listed session's downlink to the target's tunnel (reallocating the UPF
// uplink TEID when configured), adopts the target RAN-UE-NGAP-ID on this
// association, acknowledges, and then tells the UPF. Unknown UEs are
// answered with nothing, as the engine treats a missing ack as a failure.
func (a *AMF) onPathSwitchRequest(p *pipe, states map[int64]*ueState, req *ngapType.PathSwitchRequest) error {
	var ranID, srcAMFID int64
	newDL := map[uint8]Tunnel{}
	for _, ie := range req.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDRANUENGAPID:
			ranID = ie.Value.RANUENGAPID.Value
		case ngapType.ProtocolIEIDSourceAMFUENGAPID:
			srcAMFID = ie.Value.SourceAMFUENGAPID.Value
		case ngapType.ProtocolIEIDPDUSessionResourceToBeSwitchedDLList:
			for _, it := range ie.Value.PDUSessionResourceToBeSwitchedDLList.List {
				var tr ngapType.PathSwitchRequestTransfer
				if err := aper.UnmarshalWithParams(it.PathSwitchRequestTransfer, &tr, "valueExt"); err != nil {
					return fmt.Errorf("mock amf decode path switch transfer: %w", err)
				}
				dl, err := tunnelFrom(tr.DLNGUUPTNLInformation)
				if err != nil {
					return err
				}
				newDL[uint8(it.PDUSessionID.Value)] = dl
			}
		}
	}

	var switches []PathSwitch
	var switched []switchedItem
	a.mu.Lock()
	st := a.ues[srcAMFID]
	if st == nil {
		a.mu.Unlock()
		return nil
	}
	st.ranID = ranID
	for id, dl := range newDL {
		s := st.sessions[id]
		if s == nil {
			continue
		}
		ps := PathSwitch{SUPI: st.supi, PDUSessionID: id, OldUL: s.ul, NewUL: s.ul, OldDL: s.dl, NewDL: dl}
		si := switchedItem{id: id}
		if a.cfg.ReallocateULOnPathSwitch {
			ps.NewUL = Tunnel{Addr: s.ul.Addr, TEID: a.nextULTEID()}
			ul := ps.NewUL
			si.newUL = &ul
		}
		s.dl, s.ul = ps.NewDL, ps.NewUL
		switches = append(switches, ps)
		switched = append(switched, si)
	}
	amfID := st.amfID
	a.mu.Unlock()
	states[ranID] = st

	ack, err := buildPathSwitchRequestAcknowledge(amfID, ranID, switched, a.cfg.SST, a.sdBytes)
	if err != nil {
		return err
	}
	if err := a.send(p, ack); err != nil {
		return err
	}
	if a.cfg.UPF != nil {
		for _, ps := range switches {
			a.cfg.UPF.PathSwitched(ps)
		}
	}
	return nil
}

// nextUEIP hands out UE addresses from 10.60.0.1 upwards.
func (a *AMF) nextUEIP() net.IP {
	n := a.ueIPSeq.Add(1)
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, 0x0a3c0000+n)
	return ip
}

// nextULTEID hands out UPF uplink TEIDs from 0x10001 upwards.
func (a *AMF) nextULTEID() uint32 { return 0x10000 + a.ulTEIDSeq.Add(1) }

// tunnelFrom reads an NGAP GTP tunnel.
func tunnelFrom(up ngapType.UPTransportLayerInformation) (Tunnel, error) {
	gt := up.GTPTunnel
	if gt == nil || len(gt.GTPTEID.Value) != 4 {
		return Tunnel{}, fmt.Errorf("mock amf: missing GTP tunnel")
	}
	v4, _ := ngapConvert.IPAddressToString(gt.TransportLayerAddress)
	return Tunnel{Addr: v4, TEID: binary.BigEndian.Uint32(gt.GTPTEID.Value)}, nil
}

// gtpTunnel builds an NGAP GTP tunnel.
func gtpTunnel(t Tunnel) ngapType.UPTransportLayerInformation {
	teid := make([]byte, 4)
	binary.BigEndian.PutUint32(teid, t.TEID)
	return ngapType.UPTransportLayerInformation{
		Present: ngapType.UPTransportLayerInformationPresentGTPTunnel,
		GTPTunnel: &ngapType.GTPTunnel{
			TransportLayerAddress: ngapConvert.IPAddressToNgap(t.Addr, ""),
			GTPTEID:               ngapType.GTPTEID{Value: teid},
		},
	}
}

// snssai builds an NGAP S-NSSAI.
func snssai(sst uint8, sd []byte) ngapType.SNSSAI {
	s := ngapType.SNSSAI{SST: ngapType.SST{Value: aper.OctetString{sst}}}
	if len(sd) == 3 {
		s.SD = &ngapType.SD{Value: aper.OctetString(append([]byte(nil), sd...))}
	}
	return s
}

// defaultQoSRule is one authorised QoS rule (TS 24.501 §9.11.4.13): rule 1,
// "create new QoS rule", default rule, one match-all packet filter,
// precedence 255, QFI 1.
var defaultQoSRule = []byte{0x01, 0x00, 0x06, 0x31, 0x31, 0x01, 0x01, 0xff, defaultQFI}

// buildPDUSessionEstablishmentAccept builds the 5GSM PDU Session
// Establishment Accept (TS 24.501 §8.3.2): IPv4, SSC mode 1, the default QoS
// rule, a session AMBR, the UE's address, the slice and the DNN.
func buildPDUSessionEstablishmentAccept(id, pti uint8, ueIP net.IP, sst uint8, sd []byte, dnn string) ([]byte, error) {
	m := f5nas.NewMessage()
	m.GsmMessage = f5nas.NewGsmMessage()
	m.GsmHeader.SetMessageType(f5nas.MsgTypePDUSessionEstablishmentAccept)
	acc := nasMessage.NewPDUSessionEstablishmentAccept(f5nas.MsgTypePDUSessionEstablishmentAccept)
	acc.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSSessionManagementMessage)
	acc.SetPDUSessionID(id)
	acc.SetPTI(pti)
	acc.SetMessageType(f5nas.MsgTypePDUSessionEstablishmentAccept)
	acc.SetPDUSessionType(nasMessage.PDUSessionTypeIPv4)
	acc.SetSSCMode(0x01)
	acc.AuthorizedQosRules.SetLen(uint16(len(defaultQoSRule)))
	acc.SetQosRule(append([]byte(nil), defaultQoSRule...))
	acc.SessionAMBR.SetLen(6)
	acc.SetUnitForSessionAMBRForDownlink(0x06) // 1 Mbps units
	acc.SetSessionAMBRForDownlink([2]uint8{0x00, 0x64})
	acc.SetUnitForSessionAMBRForUplink(0x06)
	acc.SetSessionAMBRForUplink([2]uint8{0x00, 0x64})

	addr := nasType.NewPDUAddress(nasMessage.PDUSessionEstablishmentAcceptPDUAddressType)
	addr.SetLen(5)
	addr.SetPDUSessionTypeValue(nasMessage.PDUSessionTypeIPv4)
	var info [12]uint8
	copy(info[:], ueIP.To4())
	addr.SetPDUAddressInformation(info)
	acc.PDUAddress = addr

	sn := nasType.NewSNSSAI(nasMessage.PDUSessionEstablishmentAcceptSNSSAIType)
	sn.SetSST(sst)
	sn.SetLen(1)
	if len(sd) == 3 {
		sn.SetLen(4)
		sn.SetSD([3]uint8{sd[0], sd[1], sd[2]})
	}
	acc.SNSSAI = sn
	if dnn != "" {
		d := nasType.NewDNN(nasMessage.PDUSessionEstablishmentAcceptDNNType)
		d.SetDNN(dnn)
		acc.DNN = d
	}
	m.PDUSessionEstablishmentAccept = acc
	return m.PlainNasEncode()
}

// buildDLNASTransportN1SM wraps a 5GSM message in a 5GMM DL NAS Transport
// (TS 24.501 §8.2.11) with payload container type N1 SM information and the
// PDU session ID, for the caller to secure.
func buildDLNASTransportN1SM(sm []byte, id uint8) (*f5nas.Message, error) {
	m, gmm := plainGmm(f5nas.MsgTypeDLNASTransport)
	t := nasMessage.NewDLNASTransport(f5nas.MsgTypeDLNASTransport)
	setPlainHeader(&t.ExtendedProtocolDiscriminator, &t.SpareHalfOctetAndSecurityHeaderType,
		&t.DLNASTRANSPORTMessageIdentity, f5nas.MsgTypeDLNASTransport)
	t.SetPayloadContainerType(nasMessage.PayloadContainerTypeN1SMInfo)
	t.PayloadContainer.SetLen(uint16(len(sm)))
	t.SetPayloadContainerContents(sm)
	pid := nasType.NewPduSessionID2Value(nasMessage.DLNASTransportPduSessionID2ValueType)
	pid.SetPduSessionID2Value(id)
	t.PduSessionID2Value = pid
	gmm.DLNASTransport = t
	return m, nil
}

// buildPDUSessionResourceSetupRequest builds the AMF's PDU Session Resource
// Setup Request (TS 38.413 §9.2.1.1) for one session: the secured NAS PDU,
// the slice, and a transfer with the session AMBR, the UPF uplink tunnel,
// PDU session type IPv4 and one QoS flow (5QI 9).
func buildPDUSessionResourceSetupRequest(amfID, ranID int64, id uint8, nasPDU []byte, ul Tunnel, sst uint8, sd []byte) (ngapType.NGAPPDU, error) {
	var tr ngapType.PDUSessionResourceSetupRequestTransfer
	addTr := func(ieID int64, crit aper.Enumerated, fill func(*ngapType.PDUSessionResourceSetupRequestTransferIEs)) {
		ie := ngapType.PDUSessionResourceSetupRequestTransferIEs{}
		ie.Id.Value = ieID
		ie.Criticality.Value = crit
		fill(&ie)
		tr.ProtocolIEs.List = append(tr.ProtocolIEs.List, ie)
	}
	addTr(ngapType.ProtocolIEIDPDUSessionAggregateMaximumBitRate, ngapType.CriticalityPresentReject, func(ie *ngapType.PDUSessionResourceSetupRequestTransferIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceSetupRequestTransferIEsPresentPDUSessionAggregateMaximumBitRate
		ie.Value.PDUSessionAggregateMaximumBitRate = &ngapType.PDUSessionAggregateMaximumBitRate{
			PDUSessionAggregateMaximumBitRateDL: ngapType.BitRate{Value: 100_000_000},
			PDUSessionAggregateMaximumBitRateUL: ngapType.BitRate{Value: 100_000_000},
		}
	})
	addTr(ngapType.ProtocolIEIDULNGUUPTNLInformation, ngapType.CriticalityPresentReject, func(ie *ngapType.PDUSessionResourceSetupRequestTransferIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceSetupRequestTransferIEsPresentULNGUUPTNLInformation
		up := gtpTunnel(ul)
		ie.Value.ULNGUUPTNLInformation = &up
	})
	addTr(ngapType.ProtocolIEIDPDUSessionType, ngapType.CriticalityPresentReject, func(ie *ngapType.PDUSessionResourceSetupRequestTransferIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceSetupRequestTransferIEsPresentPDUSessionType
		ie.Value.PDUSessionType = &ngapType.PDUSessionType{Value: ngapType.PDUSessionTypePresentIpv4}
	})
	addTr(ngapType.ProtocolIEIDQosFlowSetupRequestList, ngapType.CriticalityPresentReject, func(ie *ngapType.PDUSessionResourceSetupRequestTransferIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceSetupRequestTransferIEsPresentQosFlowSetupRequestList
		item := ngapType.QosFlowSetupRequestItem{}
		item.QosFlowIdentifier.Value = defaultQFI
		item.QosFlowLevelQosParameters.QosCharacteristics.Present = ngapType.QosCharacteristicsPresentNonDynamic5QI
		item.QosFlowLevelQosParameters.QosCharacteristics.NonDynamic5QI = &ngapType.NonDynamic5QIDescriptor{FiveQI: ngapType.FiveQI{Value: 9}}
		item.QosFlowLevelQosParameters.AllocationAndRetentionPriority.PriorityLevelARP.Value = 8
		item.QosFlowLevelQosParameters.AllocationAndRetentionPriority.PreEmptionCapability.Value = ngapType.PreEmptionCapabilityPresentShallNotTriggerPreEmption
		item.QosFlowLevelQosParameters.AllocationAndRetentionPriority.PreEmptionVulnerability.Value = ngapType.PreEmptionVulnerabilityPresentNotPreEmptable
		ie.Value.QosFlowSetupRequestList = &ngapType.QosFlowSetupRequestList{List: []ngapType.QosFlowSetupRequestItem{item}}
	})
	transfer, err := aper.MarshalWithParams(tr, "valueExt")
	if err != nil {
		return ngapType.NGAPPDU{}, fmt.Errorf("mock amf encode setup request transfer: %w", err)
	}

	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodePDUSessionResourceSetup
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentPDUSessionResourceSetupRequest
	pdu.InitiatingMessage.Value.PDUSessionResourceSetupRequest = new(ngapType.PDUSessionResourceSetupRequest)
	list := &pdu.InitiatingMessage.Value.PDUSessionResourceSetupRequest.ProtocolIEs.List
	add := func(ieID int64, crit aper.Enumerated, fill func(*ngapType.PDUSessionResourceSetupRequestIEs)) {
		ie := ngapType.PDUSessionResourceSetupRequestIEs{}
		ie.Id.Value = ieID
		ie.Criticality.Value = crit
		fill(&ie)
		*list = append(*list, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.PDUSessionResourceSetupRequestIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceSetupRequestIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.PDUSessionResourceSetupRequestIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceSetupRequestIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranID}
	})
	add(ngapType.ProtocolIEIDPDUSessionResourceSetupListSUReq, ngapType.CriticalityPresentReject, func(ie *ngapType.PDUSessionResourceSetupRequestIEs) {
		ie.Value.Present = ngapType.PDUSessionResourceSetupRequestIEsPresentPDUSessionResourceSetupListSUReq
		item := ngapType.PDUSessionResourceSetupItemSUReq{}
		item.PDUSessionID.Value = int64(id)
		item.PDUSessionNASPDU = &ngapType.NASPDU{Value: nasPDU}
		item.SNSSAI = snssai(sst, sd)
		item.PDUSessionResourceSetupRequestTransfer = transfer
		ie.Value.PDUSessionResourceSetupListSUReq = &ngapType.PDUSessionResourceSetupListSUReq{List: []ngapType.PDUSessionResourceSetupItemSUReq{item}}
	})
	return pdu, nil
}

// switchedItem is one session in a PathSwitchRequestAcknowledge; newUL is
// set when the UPF's uplink tunnel was reallocated.
type switchedItem struct {
	id    uint8
	newUL *Tunnel
}

// buildPathSwitchRequestAcknowledge builds the AMF's PathSwitchRequest
// Acknowledge (TS 38.413 §9.2.3.11): the UE's NGAP IDs, a security context
// (NCC 1, a zero NH: the mock does no key refresh), the switched sessions
// (each transfer carrying the new UL tunnel when reallocated) and the
// allowed NSSAI.
func buildPathSwitchRequestAcknowledge(amfID, ranID int64, switched []switchedItem, sst uint8, sd []byte) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodePathSwitchRequest
	pdu.SuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentPathSwitchRequestAcknowledge
	pdu.SuccessfulOutcome.Value.PathSwitchRequestAcknowledge = new(ngapType.PathSwitchRequestAcknowledge)
	list := &pdu.SuccessfulOutcome.Value.PathSwitchRequestAcknowledge.ProtocolIEs.List
	add := func(ieID int64, crit aper.Enumerated, fill func(*ngapType.PathSwitchRequestAcknowledgeIEs)) {
		ie := ngapType.PathSwitchRequestAcknowledgeIEs{}
		ie.Id.Value = ieID
		ie.Criticality.Value = crit
		fill(&ie)
		*list = append(*list, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, ngapType.CriticalityPresentIgnore, func(ie *ngapType.PathSwitchRequestAcknowledgeIEs) {
		ie.Value.Present = ngapType.PathSwitchRequestAcknowledgeIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, ngapType.CriticalityPresentIgnore, func(ie *ngapType.PathSwitchRequestAcknowledgeIEs) {
		ie.Value.Present = ngapType.PathSwitchRequestAcknowledgeIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranID}
	})
	add(ngapType.ProtocolIEIDSecurityContext, ngapType.CriticalityPresentReject, func(ie *ngapType.PathSwitchRequestAcknowledgeIEs) {
		ie.Value.Present = ngapType.PathSwitchRequestAcknowledgeIEsPresentSecurityContext
		ie.Value.SecurityContext = &ngapType.SecurityContext{
			NextHopChainingCount: ngapType.NextHopChainingCount{Value: 1},
			NextHopNH:            ngapType.SecurityKey{Value: aper.BitString{Bytes: make([]byte, 32), BitLength: 256}},
		}
	})
	var items []ngapType.PDUSessionResourceSwitchedItem
	for _, s := range switched {
		var tr ngapType.PathSwitchRequestAcknowledgeTransfer
		if s.newUL != nil {
			up := gtpTunnel(*s.newUL)
			tr.ULNGUUPTNLInformation = &up
		}
		b, err := aper.MarshalWithParams(tr, "valueExt")
		if err != nil {
			return ngapType.NGAPPDU{}, fmt.Errorf("mock amf encode path switch ack transfer: %w", err)
		}
		item := ngapType.PDUSessionResourceSwitchedItem{}
		item.PDUSessionID.Value = int64(s.id)
		item.PathSwitchRequestAcknowledgeTransfer = b
		items = append(items, item)
	}
	add(ngapType.ProtocolIEIDPDUSessionResourceSwitchedList, ngapType.CriticalityPresentIgnore, func(ie *ngapType.PathSwitchRequestAcknowledgeIEs) {
		ie.Value.Present = ngapType.PathSwitchRequestAcknowledgeIEsPresentPDUSessionResourceSwitchedList
		ie.Value.PDUSessionResourceSwitchedList = &ngapType.PDUSessionResourceSwitchedList{List: items}
	})
	add(ngapType.ProtocolIEIDAllowedNSSAI, ngapType.CriticalityPresentReject, func(ie *ngapType.PathSwitchRequestAcknowledgeIEs) {
		ie.Value.Present = ngapType.PathSwitchRequestAcknowledgeIEsPresentAllowedNSSAI
		ie.Value.AllowedNSSAI = &ngapType.AllowedNSSAI{List: []ngapType.AllowedNSSAIItem{{SNSSAI: snssai(sst, sd)}}}
	})
	return pdu, nil
}
