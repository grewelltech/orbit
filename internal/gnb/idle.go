package gnb

import (
	"encoding/binary"
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

// STMSI is a 5G-S-TMSI (TS 23.003 2.10.1): the AMF Set ID (10 bits), the
// AMF Pointer (6 bits) and the 5G-TMSI. It names a CM-IDLE UE in PAGING
// and in the Initial UE Message that brings the UE back.
type STMSI struct {
	AMFSetID   uint16
	AMFPointer uint8
	TMSI       uint32
}

// String renders the identity as set/pointer/TMSI in hex.
func (s STMSI) String() string {
	return fmt.Sprintf("%#x/%#x/%#08x", s.AMFSetID, s.AMFPointer, s.TMSI)
}

// STMSIFromGUTI takes the 5G-S-TMSI out of a 5G-GUTI given as the 11-octet
// 5GS mobile identity value (TS 24.501 9.11.3.4): the type octet, the PLMN,
// the AMF Region ID, then the AMF Set ID, AMF Pointer and 5G-TMSI.
func STMSIFromGUTI(guti []byte) (STMSI, error) {
	if len(guti) != 11 || guti[0]&0x07 != 0x02 {
		return STMSI{}, fmt.Errorf("not a 5G-GUTI mobile identity (%d octets)", len(guti))
	}
	return STMSI{
		AMFSetID:   uint16(guti[5])<<2 | uint16(guti[6])>>6,
		AMFPointer: guti[6] & 0x3f,
		TMSI:       binary.BigEndian.Uint32(guti[7:11]),
	}, nil
}

func (s STMSI) ngap() *ngapType.FiveGSTMSI {
	tmsi := make([]byte, 4)
	binary.BigEndian.PutUint32(tmsi, s.TMSI)
	return &ngapType.FiveGSTMSI{
		AMFSetID:   ngapType.AMFSetID{Value: aper.BitString{Bytes: []byte{byte(s.AMFSetID >> 2), byte(s.AMFSetID << 6)}, BitLength: 10}},
		AMFPointer: ngapType.AMFPointer{Value: aper.BitString{Bytes: []byte{s.AMFPointer << 2}, BitLength: 6}},
		FiveGTMSI:  ngapType.FiveGTMSI{Value: tmsi},
	}
}

// stmsiFromNGAP reads an NGAP 5G-S-TMSI; false when it is malformed.
func stmsiFromNGAP(v *ngapType.FiveGSTMSI) (STMSI, bool) {
	if v == nil || len(v.AMFSetID.Value.Bytes) < 2 || len(v.AMFPointer.Value.Bytes) < 1 || len(v.FiveGTMSI.Value) != 4 {
		return STMSI{}, false
	}
	set, ptr := v.AMFSetID.Value.Bytes, v.AMFPointer.Value.Bytes
	return STMSI{
		AMFSetID:   uint16(set[0])<<2 | uint16(set[1])>>6,
		AMFPointer: ptr[0] >> 2,
		TMSI:       binary.BigEndian.Uint32(v.FiveGTMSI.Value),
	}, true
}

// InitialUEOption adjusts an Initial UE Message.
type InitialUEOption func(*initialUE)

type initialUE struct {
	stmsi *STMSI
	cause aper.Enumerated
}

// WithSTMSI adds the 5G-S-TMSI IE, by which the AMF finds the context of a
// UE coming back from CM-IDLE (TS 38.413 9.2.5.1).
func WithSTMSI(s STMSI) InitialUEOption { return func(m *initialUE) { m.stmsi = &s } }

// WithMTAccess sets the RRC Establishment Cause to mt-Access, the cause of
// a UE answering paging; the default is mo-Signalling.
func WithMTAccess() InitialUEOption {
	return func(m *initialUE) { m.cause = ngapType.RRCEstablishmentCausePresentMtAccess }
}

// WithMOData sets the RRC Establishment Cause to mo-Data, the cause of a UE
// with uplink data pending.
func WithMOData() InitialUEOption {
	return func(m *initialUE) { m.cause = ngapType.RRCEstablishmentCausePresentMoData }
}

// TAI is a tracking area identity as PAGING lists it.
type TAI struct {
	MCC, MNC string
	TAC      uint32
}

// Paging is a parsed NGAP PAGING (TS 38.413 9.2.4.1).
type Paging struct {
	// STMSI is the UE Paging Identity; HasSTMSI is false when the IE is
	// absent or malformed.
	STMSI    STMSI
	HasSTMSI bool
	// TAIs is the TAI List for Paging.
	TAIs []TAI
}

// ParsePaging reads a PAGING; ok is false for any other PDU.
func ParsePaging(pdu *ngapType.NGAPPDU) (p Paging, ok bool) {
	if pdu == nil || pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage || pdu.InitiatingMessage == nil ||
		pdu.InitiatingMessage.Value.Present != ngapType.InitiatingMessagePresentPaging || pdu.InitiatingMessage.Value.Paging == nil {
		return Paging{}, false
	}
	for _, ie := range pdu.InitiatingMessage.Value.Paging.ProtocolIEs.List {
		switch ie.Id.Value {
		case ngapType.ProtocolIEIDUEPagingIdentity:
			if id := ie.Value.UEPagingIdentity; id != nil && id.Present == ngapType.UEPagingIdentityPresentFiveGSTMSI {
				p.STMSI, p.HasSTMSI = stmsiFromNGAP(id.FiveGSTMSI)
			}
		case ngapType.ProtocolIEIDTAIListForPaging:
			if ie.Value.TAIListForPaging == nil {
				continue
			}
			for _, it := range ie.Value.TAIListForPaging.List {
				t := TAI{}
				if b := it.TAI.PLMNIdentity.Value; len(b) == 3 {
					t.MCC, t.MNC = ngap.DecodePLMN([3]byte{b[0], b[1], b[2]})
				}
				for _, o := range it.TAI.TAC.Value {
					t.TAC = t.TAC<<8 | uint32(o)
				}
				p.TAIs = append(p.TAIs, t)
			}
		}
	}
	return p, true
}

// ReleaseCommandCause names the cause of a UE CONTEXT RELEASE COMMAND as
// "group/value" ("nas/0"); ok is false for any other PDU.
func ReleaseCommandCause(pdu *ngapType.NGAPPDU) (cause string, ok bool) {
	if pdu == nil || pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage || pdu.InitiatingMessage == nil ||
		pdu.InitiatingMessage.Value.Present != ngapType.InitiatingMessagePresentUEContextReleaseCommand ||
		pdu.InitiatingMessage.Value.UEContextReleaseCommand == nil {
		return "", false
	}
	for _, ie := range pdu.InitiatingMessage.Value.UEContextReleaseCommand.ProtocolIEs.List {
		if ie.Id.Value != ngapType.ProtocolIEIDCause || ie.Value.Cause == nil {
			continue
		}
		if name := causeName(ie.Value.Cause); name != "" {
			return name, true
		}
	}
	return "unknown", true
}

// causeName names an NGAP Cause as "group/value" ("radio-network/3"), ""
// for a group it does not know.
func causeName(c *ngapType.Cause) string {
	switch c.Present {
	case ngapType.CausePresentRadioNetwork:
		return fmt.Sprintf("radio-network/%d", c.RadioNetwork.Value)
	case ngapType.CausePresentTransport:
		return fmt.Sprintf("transport/%d", c.Transport.Value)
	case ngapType.CausePresentNas:
		return fmt.Sprintf("nas/%d", c.Nas.Value)
	case ngapType.CausePresentProtocol:
		return fmt.Sprintf("protocol/%d", c.Protocol.Value)
	case ngapType.CausePresentMisc:
		return fmt.Sprintf("misc/%d", c.Misc.Value)
	}
	return ""
}

// ParseInitialContextSetupSessions extracts the PDU sessions an INITIAL
// CONTEXT SETUP REQUEST asks the gNB to set up (PDU Session Resource Setup
// List, TS 38.413 9.2.2.1), as a Service Request's reactivation carries
// them. It returns none for a request without the list or any other PDU.
func ParseInitialContextSetupSessions(pdu *ngapType.NGAPPDU) ([]PDUSessionResource, error) {
	if pdu == nil || pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage || pdu.InitiatingMessage == nil ||
		pdu.InitiatingMessage.Value.Present != ngapType.InitiatingMessagePresentInitialContextSetupRequest {
		return nil, nil
	}
	var res []PDUSessionResource
	for _, ie := range pdu.InitiatingMessage.Value.InitialContextSetupRequest.ProtocolIEs.List {
		if ie.Id.Value != ngapType.ProtocolIEIDPDUSessionResourceSetupListCxtReq || ie.Value.PDUSessionResourceSetupListCxtReq == nil {
			continue
		}
		for _, item := range ie.Value.PDUSessionResourceSetupListCxtReq.List {
			r := PDUSessionResource{PDUSessionID: item.PDUSessionID.Value}
			if item.NASPDU != nil {
				r.NASPDU = item.NASPDU.Value
			}
			if err := decodeSetupRequestTransfer(item.PDUSessionResourceSetupRequestTransfer, &r); err != nil {
				return nil, err
			}
			res = append(res, r)
		}
	}
	return res, nil
}

// BuildInitialContextSetupResponseSessions acknowledges an Initial Context
// Setup that set up PDU sessions, reporting the gNB's downlink tunnel for
// each (TS 38.413 9.2.2.2). With no sessions it is
// BuildInitialContextSetupResponse.
func BuildInitialContextSetupResponseSessions(amfUENGAPID, ranUENGAPID int64, res []PDUSessionResource, gnbAddr string, teidFor func(pduSessionID int64) uint32) (ngapType.NGAPPDU, error) {
	pdu := BuildInitialContextSetupResponse(amfUENGAPID, ranUENGAPID)
	if len(res) == 0 {
		return pdu, nil
	}
	ie := ngapType.InitialContextSetupResponseIEs{}
	ie.Id.Value = ngapType.ProtocolIEIDPDUSessionResourceSetupListCxtRes
	ie.Criticality.Value = ngapType.CriticalityPresentIgnore
	ie.Value.Present = ngapType.InitialContextSetupResponseIEsPresentPDUSessionResourceSetupListCxtRes
	ie.Value.PDUSessionResourceSetupListCxtRes = new(ngapType.PDUSessionResourceSetupListCxtRes)
	for _, r := range res {
		transfer, err := encodeSetupResponseTransfer(GNBTunnel{Address: gnbAddr, TEID: teidFor(r.PDUSessionID)}, r.QFIs)
		if err != nil {
			return pdu, err
		}
		item := ngapType.PDUSessionResourceSetupItemCxtRes{}
		item.PDUSessionID.Value = r.PDUSessionID
		item.PDUSessionResourceSetupResponseTransfer = transfer
		ie.Value.PDUSessionResourceSetupListCxtRes.List = append(ie.Value.PDUSessionResourceSetupListCxtRes.List, item)
	}
	list := &pdu.SuccessfulOutcome.Value.InitialContextSetupResponse.ProtocolIEs.List
	*list = append(*list, ie)
	return pdu, nil
}
