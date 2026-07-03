package mockamf

import (
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

// Network-side (AMF→gNB) NGAP builders and gNB→AMF parsers for the mock. Only
// what the attach needs: NG Setup Response, Downlink NAS Transport, Initial
// Context Setup Request; and extracting the RAN UE NGAP ID + NAS PDU from
// Initial UE Message / Uplink NAS Transport.

func bits(v uint64, n int) aper.BitString {
	nbytes := (n + 7) / 8
	b := make([]byte, nbytes)
	shifted := v << (uint(nbytes*8) - uint(n))
	for i := nbytes - 1; i >= 0; i-- {
		b[i] = byte(shifted)
		shifted >>= 8
	}
	return aper.BitString{Bytes: b, BitLength: uint64(n)}
}

// buildNGSetupResponse builds a minimal but well-formed NG Setup Response
// (TS 38.413 §9.2.6.2): AMF name, one served GUAMI, relative capacity, and
// one PLMN support entry for the served slice.
func buildNGSetupResponse(plmn [3]byte, sst uint8, sd []byte) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeNGSetup
	pdu.SuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentNGSetupResponse
	pdu.SuccessfulOutcome.Value.NGSetupResponse = new(ngapType.NGSetupResponse)
	list := &pdu.SuccessfulOutcome.Value.NGSetupResponse.ProtocolIEs.List

	add := func(id int64, crit aper.Enumerated, fill func(*ngapType.NGSetupResponseIEs)) {
		ie := ngapType.NGSetupResponseIEs{}
		ie.Id.Value = id
		ie.Criticality.Value = crit
		fill(&ie)
		*list = append(*list, ie)
	}

	add(ngapType.ProtocolIEIDAMFName, ngapType.CriticalityPresentReject, func(ie *ngapType.NGSetupResponseIEs) {
		ie.Value.Present = ngapType.NGSetupResponseIEsPresentAMFName
		ie.Value.AMFName = &ngapType.AMFName{Value: "orbit-mock-amf"}
	})
	add(ngapType.ProtocolIEIDServedGUAMIList, ngapType.CriticalityPresentReject, func(ie *ngapType.NGSetupResponseIEs) {
		ie.Value.Present = ngapType.NGSetupResponseIEsPresentServedGUAMIList
		ie.Value.ServedGUAMIList = new(ngapType.ServedGUAMIList)
		item := ngapType.ServedGUAMIItem{}
		item.GUAMI = guami(plmn)
		ie.Value.ServedGUAMIList.List = append(ie.Value.ServedGUAMIList.List, item)
	})
	add(ngapType.ProtocolIEIDRelativeAMFCapacity, ngapType.CriticalityPresentIgnore, func(ie *ngapType.NGSetupResponseIEs) {
		ie.Value.Present = ngapType.NGSetupResponseIEsPresentRelativeAMFCapacity
		ie.Value.RelativeAMFCapacity = &ngapType.RelativeAMFCapacity{Value: 255}
	})
	add(ngapType.ProtocolIEIDPLMNSupportList, ngapType.CriticalityPresentReject, func(ie *ngapType.NGSetupResponseIEs) {
		ie.Value.Present = ngapType.NGSetupResponseIEsPresentPLMNSupportList
		ie.Value.PLMNSupportList = new(ngapType.PLMNSupportList)
		item := ngapType.PLMNSupportItem{}
		item.PLMNIdentity.Value = aper.OctetString(plmn[:])
		si := ngapType.SliceSupportItem{}
		si.SNSSAI.SST.Value = aper.OctetString{sst}
		if len(sd) == 3 {
			si.SNSSAI.SD = &ngapType.SD{Value: aper.OctetString(sd)}
		}
		item.SliceSupportList.List = append(item.SliceSupportList.List, si)
		ie.Value.PLMNSupportList.List = append(ie.Value.PLMNSupportList.List, item)
	})
	return pdu, nil
}

func guami(plmn [3]byte) ngapType.GUAMI {
	var g ngapType.GUAMI
	g.PLMNIdentity.Value = aper.OctetString(plmn[:])
	g.AMFRegionID.Value = bits(1, 8)
	g.AMFSetID.Value = bits(1, 10)
	g.AMFPointer.Value = bits(0, 6)
	return g
}

// buildDownlinkNASTransport wraps an AMF→UE NAS message (TS 38.413 §9.2.5.2).
func buildDownlinkNASTransport(amfID, ranID int64, nasPDU []byte) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeDownlinkNASTransport
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentIgnore
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentDownlinkNASTransport
	pdu.InitiatingMessage.Value.DownlinkNASTransport = new(ngapType.DownlinkNASTransport)
	list := &pdu.InitiatingMessage.Value.DownlinkNASTransport.ProtocolIEs.List

	add := func(id int64, fill func(*ngapType.DownlinkNASTransportIEs)) {
		ie := ngapType.DownlinkNASTransportIEs{}
		ie.Id.Value = id
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		fill(&ie)
		*list = append(*list, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, func(ie *ngapType.DownlinkNASTransportIEs) {
		ie.Value.Present = ngapType.DownlinkNASTransportIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, func(ie *ngapType.DownlinkNASTransportIEs) {
		ie.Value.Present = ngapType.DownlinkNASTransportIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranID}
	})
	add(ngapType.ProtocolIEIDNASPDU, func(ie *ngapType.DownlinkNASTransportIEs) {
		ie.Value.Present = ngapType.DownlinkNASTransportIEsPresentNASPDU
		ie.Value.NASPDU = &ngapType.NASPDU{Value: nasPDU}
	})
	return pdu
}

// buildInitialContextSetupRequest carries the Registration Accept plus the UE
// context (TS 38.413 §9.2.2.1). The UE acknowledges with a response and
// processes the embedded NAS PDU; it does not validate the security key or
// capabilities here, so those carry placeholder values.
func buildInitialContextSetupRequest(amfID, ranID int64, nasPDU []byte, plmn [3]byte, sst uint8, sd []byte) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeInitialContextSetup
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentInitialContextSetupRequest
	pdu.InitiatingMessage.Value.InitialContextSetupRequest = new(ngapType.InitialContextSetupRequest)
	list := &pdu.InitiatingMessage.Value.InitialContextSetupRequest.ProtocolIEs.List

	add := func(id int64, crit aper.Enumerated, fill func(*ngapType.InitialContextSetupRequestIEs)) {
		ie := ngapType.InitialContextSetupRequestIEs{}
		ie.Id.Value = id
		ie.Criticality.Value = crit
		fill(&ie)
		*list = append(*list, ie)
	}
	add(ngapType.ProtocolIEIDAMFUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.InitialContextSetupRequestIEs) {
		ie.Value.Present = ngapType.InitialContextSetupRequestIEsPresentAMFUENGAPID
		ie.Value.AMFUENGAPID = &ngapType.AMFUENGAPID{Value: amfID}
	})
	add(ngapType.ProtocolIEIDRANUENGAPID, ngapType.CriticalityPresentReject, func(ie *ngapType.InitialContextSetupRequestIEs) {
		ie.Value.Present = ngapType.InitialContextSetupRequestIEsPresentRANUENGAPID
		ie.Value.RANUENGAPID = &ngapType.RANUENGAPID{Value: ranID}
	})
	add(ngapType.ProtocolIEIDGUAMI, ngapType.CriticalityPresentReject, func(ie *ngapType.InitialContextSetupRequestIEs) {
		ie.Value.Present = ngapType.InitialContextSetupRequestIEsPresentGUAMI
		g := guami(plmn)
		ie.Value.GUAMI = &g
	})
	add(ngapType.ProtocolIEIDAllowedNSSAI, ngapType.CriticalityPresentReject, func(ie *ngapType.InitialContextSetupRequestIEs) {
		ie.Value.Present = ngapType.InitialContextSetupRequestIEsPresentAllowedNSSAI
		ie.Value.AllowedNSSAI = new(ngapType.AllowedNSSAI)
		item := ngapType.AllowedNSSAIItem{}
		item.SNSSAI.SST.Value = aper.OctetString{sst}
		if len(sd) == 3 {
			item.SNSSAI.SD = &ngapType.SD{Value: aper.OctetString(sd)}
		}
		ie.Value.AllowedNSSAI.List = append(ie.Value.AllowedNSSAI.List, item)
	})
	add(ngapType.ProtocolIEIDUESecurityCapabilities, ngapType.CriticalityPresentReject, func(ie *ngapType.InitialContextSetupRequestIEs) {
		ie.Value.Present = ngapType.InitialContextSetupRequestIEsPresentUESecurityCapabilities
		ie.Value.UESecurityCapabilities = &ngapType.UESecurityCapabilities{
			NRencryptionAlgorithms:             ngapType.NRencryptionAlgorithms{Value: bits(0, 16)},
			NRintegrityProtectionAlgorithms:    ngapType.NRintegrityProtectionAlgorithms{Value: bits(0x2000, 16)}, // NIA2 bit
			EUTRAencryptionAlgorithms:          ngapType.EUTRAencryptionAlgorithms{Value: bits(0, 16)},
			EUTRAintegrityProtectionAlgorithms: ngapType.EUTRAintegrityProtectionAlgorithms{Value: bits(0, 16)},
		}
	})
	add(ngapType.ProtocolIEIDSecurityKey, ngapType.CriticalityPresentReject, func(ie *ngapType.InitialContextSetupRequestIEs) {
		ie.Value.Present = ngapType.InitialContextSetupRequestIEsPresentSecurityKey
		ie.Value.SecurityKey = &ngapType.SecurityKey{Value: bits(0, 256)}
	})
	add(ngapType.ProtocolIEIDNASPDU, ngapType.CriticalityPresentIgnore, func(ie *ngapType.InitialContextSetupRequestIEs) {
		ie.Value.Present = ngapType.InitialContextSetupRequestIEsPresentNASPDU
		ie.Value.NASPDU = &ngapType.NASPDU{Value: nasPDU}
	})
	return pdu
}

// uplinkMessage is the gNB→AMF content the handler needs.
type uplinkMessage struct {
	procedure int64
	ranID     int64
	nasPDU    []byte
}

// parseUplink extracts the RAN UE NGAP ID and NAS PDU from an Initial UE
// Message or Uplink NAS Transport (the two the mock reacts to).
func parseUplink(pdu *ngapType.NGAPPDU) (*uplinkMessage, error) {
	if pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage {
		return nil, fmt.Errorf("unexpected uplink PDU present=%d", pdu.Present)
	}
	im := pdu.InitiatingMessage
	out := &uplinkMessage{procedure: im.ProcedureCode.Value}
	switch im.Value.Present {
	case ngapType.InitiatingMessagePresentInitialUEMessage:
		for _, ie := range im.Value.InitialUEMessage.ProtocolIEs.List {
			switch ie.Id.Value {
			case ngapType.ProtocolIEIDRANUENGAPID:
				out.ranID = ie.Value.RANUENGAPID.Value
			case ngapType.ProtocolIEIDNASPDU:
				out.nasPDU = ie.Value.NASPDU.Value
			}
		}
	case ngapType.InitiatingMessagePresentUplinkNASTransport:
		for _, ie := range im.Value.UplinkNASTransport.ProtocolIEs.List {
			switch ie.Id.Value {
			case ngapType.ProtocolIEIDRANUENGAPID:
				out.ranID = ie.Value.RANUENGAPID.Value
			case ngapType.ProtocolIEIDNASPDU:
				out.nasPDU = ie.Value.NASPDU.Value
			}
		}
	}
	return out, nil
}

func encode(pdu ngapType.NGAPPDU) ([]byte, error) { return ngap.Encode(pdu) }
