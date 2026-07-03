package gnb

import (
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

// This file builds and parses the NGAP UE-associated procedures used to
// attach one UE (TS 38.413 §8.2, §8.3, §8.6). The gNB relays opaque NAS
// blobs: it wraps a UE's NAS bytes in Initial UE Message / Uplink NAS
// Transport and unwraps DL NAS Transport / Initial Context Setup. IE sets
// are grounded against gnbsim's field-used builders (which attach to this
// same SD-Core) and TS 38.413.

// UELocation identifies the cell a UE is camped on for the User Location
// Information IE (TS 38.413 §9.3.1.16). The NR Cell Identity is 36 bits.
type UELocation struct {
	PLMN           [3]byte
	TAC            uint32
	NRCellIdentity uint64 // low 36 bits significant
}

func locationFrom(cfg Config) (UELocation, error) {
	plmn, err := ngap.EncodePLMN(cfg.MCC, cfg.MNC)
	if err != nil {
		return UELocation{}, err
	}
	// Derive a stable 36-bit NR Cell Identity from the gNB ID (gNB ID in the
	// high bits, cell id 0 in the low bits) — one cell per gNB in Phase 1a.
	return UELocation{PLMN: plmn, TAC: cfg.TAC, NRCellIdentity: uint64(cfg.ID) << 4}, nil
}

func (l UELocation) userLocationInformation() *ngapType.UserLocationInformation {
	uli := &ngapType.UserLocationInformation{
		Present:                   ngapType.UserLocationInformationPresentUserLocationInformationNR,
		UserLocationInformationNR: new(ngapType.UserLocationInformationNR),
	}
	nr := uli.UserLocationInformationNR
	nr.NRCGI.PLMNIdentity.Value = aper.OctetString(l.PLMN[:])
	nr.NRCGI.NRCellIdentity.Value = aper.BitString{
		Bytes: []byte{
			byte(l.NRCellIdentity >> 32), byte(l.NRCellIdentity >> 24),
			byte(l.NRCellIdentity >> 16), byte(l.NRCellIdentity >> 8),
			byte(l.NRCellIdentity << 4),
		},
		BitLength: 36,
	}
	nr.TAI.PLMNIdentity.Value = aper.OctetString(l.PLMN[:])
	nr.TAI.TAC.Value = aper.OctetString{byte(l.TAC >> 16), byte(l.TAC >> 8), byte(l.TAC)}
	return uli
}

// BuildInitialUEMessage wraps a UE's first NAS message (the Registration
// Request) for the AMF (TS 38.413 §8.6.1, §9.2.5.1). It requests a UE
// context so the AMF runs Initial Context Setup.
func BuildInitialUEMessage(cfg Config, ranUENGAPID int64, nasPDU []byte) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	loc, err := locationFrom(cfg)
	if err != nil {
		return pdu, err
	}

	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeInitialUEMessage
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentIgnore
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentInitialUEMessage
	pdu.InitiatingMessage.Value.InitialUEMessage = new(ngapType.InitialUEMessage)
	list := &pdu.InitiatingMessage.Value.InitialUEMessage.ProtocolIEs.List

	{
		ie := ngapType.InitialUEMessageIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDRANUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.InitialUEMessageIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranUENGAPID}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.InitialUEMessageIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDNASPDU
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.InitialUEMessageIEsPresentNASPDU
		ie.Value.NASPDU = &ngapType.NASPDU{Value: nasPDU}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.InitialUEMessageIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDUserLocationInformation
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.InitialUEMessageIEsPresentUserLocationInformation
		ie.Value.UserLocationInformation = loc.userLocationInformation()
		*list = append(*list, ie)
	}
	{
		ie := ngapType.InitialUEMessageIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDRRCEstablishmentCause
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.InitialUEMessageIEsPresentRRCEstablishmentCause
		ie.Value.RRCEstablishmentCause = &ngapType.RRCEstablishmentCause{Value: ngapType.RRCEstablishmentCausePresentMoSignalling}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.InitialUEMessageIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDUEContextRequest
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.InitialUEMessageIEsPresentUEContextRequest
		ie.Value.UEContextRequest = &ngapType.UEContextRequest{Value: ngapType.UEContextRequestPresentRequested}
		*list = append(*list, ie)
	}
	return pdu, nil
}

// BuildUplinkNASTransport wraps a subsequent UE NAS message (TS 38.413
// §8.6.3, §9.2.5.3).
func BuildUplinkNASTransport(cfg Config, amfUENGAPID, ranUENGAPID int64, nasPDU []byte) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	loc, err := locationFrom(cfg)
	if err != nil {
		return pdu, err
	}
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeUplinkNASTransport
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentIgnore
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentUplinkNASTransport
	pdu.InitiatingMessage.Value.UplinkNASTransport = new(ngapType.UplinkNASTransport)
	list := &pdu.InitiatingMessage.Value.UplinkNASTransport.ProtocolIEs.List

	{
		ie := ngapType.UplinkNASTransportIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDAMFUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.UplinkNASTransportIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfUENGAPID}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.UplinkNASTransportIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDRANUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.UplinkNASTransportIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranUENGAPID}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.UplinkNASTransportIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDNASPDU
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.UplinkNASTransportIEsPresentNASPDU
		ie.Value.NASPDU = &ngapType.NASPDU{Value: nasPDU}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.UplinkNASTransportIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDUserLocationInformation
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.UplinkNASTransportIEsPresentUserLocationInformation
		ie.Value.UserLocationInformation = loc.userLocationInformation()
		*list = append(*list, ie)
	}
	return pdu, nil
}

// BuildInitialContextSetupResponse acknowledges Initial Context Setup with
// no PDU sessions (control-plane attach; TS 38.413 §8.3.1, §9.2.2.2).
func BuildInitialContextSetupResponse(amfUENGAPID, ranUENGAPID int64) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeInitialContextSetup
	pdu.SuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentInitialContextSetupResponse
	pdu.SuccessfulOutcome.Value.InitialContextSetupResponse = new(ngapType.InitialContextSetupResponse)
	list := &pdu.SuccessfulOutcome.Value.InitialContextSetupResponse.ProtocolIEs.List

	{
		ie := ngapType.InitialContextSetupResponseIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDAMFUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.InitialContextSetupResponseIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfUENGAPID}
		*list = append(*list, ie)
	}
	{
		ie := ngapType.InitialContextSetupResponseIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDRANUENGAPID
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.InitialContextSetupResponseIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranUENGAPID}
		*list = append(*list, ie)
	}
	return pdu
}

// DownlinkMessage is a parsed AMF→gNB UE-associated message: the procedure,
// the two NGAP UE IDs, and any embedded NAS PDU.
type DownlinkMessage struct {
	Procedure   int64 // ngapType.ProcedureCode*
	AMFUENGAPID int64
	RANUENGAPID int64
	HasAMFID    bool
	NASPDU      []byte
}

// ParseDownlink extracts the fields ORBIT's attach FSM needs from a decoded
// AMF→gNB PDU. It recognises DownlinkNASTransport and InitialContextSetup
// (the two that carry NAS during attach); other procedures return their
// procedure code with whatever IEs are present.
func ParseDownlink(pdu *ngapType.NGAPPDU) (*DownlinkMessage, error) {
	switch pdu.Present {
	case ngapType.NGAPPDUPresentInitiatingMessage:
		im := pdu.InitiatingMessage
		switch im.Value.Present {
		case ngapType.InitiatingMessagePresentDownlinkNASTransport:
			return parseUEIEs(ngapType.ProcedureCodeDownlinkNASTransport, dlNASIEs(im.Value.DownlinkNASTransport)), nil
		case ngapType.InitiatingMessagePresentInitialContextSetupRequest:
			return parseUEIEs(ngapType.ProcedureCodeInitialContextSetup, icsIEs(im.Value.InitialContextSetupRequest)), nil
		default:
			return &DownlinkMessage{Procedure: im.ProcedureCode.Value}, nil
		}
	case ngapType.NGAPPDUPresentSuccessfulOutcome:
		return &DownlinkMessage{Procedure: pdu.SuccessfulOutcome.ProcedureCode.Value}, nil
	case ngapType.NGAPPDUPresentUnsuccessfulOutcome:
		return &DownlinkMessage{Procedure: pdu.UnsuccessfulOutcome.ProcedureCode.Value}, nil
	default:
		return nil, fmt.Errorf("unrecognised NGAP PDU present=%d", pdu.Present)
	}
}

// ueIE is the common shape of the UE-associated IE fields the FSM reads.
type ueIE struct {
	id    int64
	amfID *int64
	ranID *int64
	nas   []byte
}

func dlNASIEs(m *ngapType.DownlinkNASTransport) []ueIE {
	var out []ueIE
	for _, ie := range m.ProtocolIEs.List {
		e := ueIE{id: ie.Id.Value}
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDAMFUENGAPID:
			v := ie.Value.AMFUENGAPID.Value
			e.amfID = &v
		case ngapType.ProtocolIEIDRANUENGAPID:
			v := ie.Value.RANUENGAPID.Value
			e.ranID = &v
		case ngapType.ProtocolIEIDNASPDU:
			e.nas = ie.Value.NASPDU.Value
		}
		out = append(out, e)
	}
	return out
}

func icsIEs(m *ngapType.InitialContextSetupRequest) []ueIE {
	var out []ueIE
	for _, ie := range m.ProtocolIEs.List {
		e := ueIE{id: ie.Id.Value}
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDAMFUENGAPID:
			v := ie.Value.AMFUENGAPID.Value
			e.amfID = &v
		case ngapType.ProtocolIEIDRANUENGAPID:
			v := ie.Value.RANUENGAPID.Value
			e.ranID = &v
		case ngapType.ProtocolIEIDNASPDU:
			e.nas = ie.Value.NASPDU.Value
		}
		out = append(out, e)
	}
	return out
}

func parseUEIEs(proc int64, ies []ueIE) *DownlinkMessage {
	out := &DownlinkMessage{Procedure: proc}
	for _, e := range ies {
		if e.amfID != nil {
			out.AMFUENGAPID, out.HasAMFID = *e.amfID, true
		}
		if e.ranID != nil {
			out.RANUENGAPID = *e.ranID
		}
		if e.nas != nil {
			out.NASPDU = e.nas
		}
	}
	return out
}
