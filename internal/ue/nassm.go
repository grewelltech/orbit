package ue

import (
	"fmt"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"
)

// This file builds the 5GSM (session management) PDU Session Establishment
// Request and the 5GMM UL NAS Transport that carries it (TS 24.501 §8.3.1,
// §8.2.10). A 5GSM message rides inside a 5GMM NAS Transport as an "N1 SM
// container"; the AMF forwards it to the SMF.

// PDUSessionParams describes the session the UE requests.
type PDUSessionParams struct {
	PDUSessionID uint8  // 1-15 (TS 24.501 §9.4)
	SST          uint8  // slice/service type
	SD           string // 6 hex digits, optional
	DNN          string // data network name, e.g. "internet"
}

// BuildPDUSessionEstablishmentRequest builds the 5GSM PDU Session
// Establishment Request (TS 24.501 §8.3.1). PTI is 0 for a UE-requested
// session per §9.6. The Integrity Protection Maximum Data Rate IE is
// mandatory; PDU session type IPv4 and SSC mode 1 are the common defaults.
func BuildPDUSessionEstablishmentRequest(p PDUSessionParams) ([]byte, error) {
	m := f5nas.NewMessage()
	m.GsmMessage = f5nas.NewGsmMessage()
	m.GsmHeader.SetMessageType(f5nas.MsgTypePDUSessionEstablishmentRequest)

	req := nasMessage.NewPDUSessionEstablishmentRequest(f5nas.MsgTypePDUSessionEstablishmentRequest)
	req.ExtendedProtocolDiscriminator.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSSessionManagementMessage)
	req.PDUSessionID.SetPDUSessionID(p.PDUSessionID)
	req.PTI.SetPTI(0x00)
	req.PDUSESSIONESTABLISHMENTREQUESTMessageIdentity.SetMessageType(f5nas.MsgTypePDUSessionEstablishmentRequest)
	// Integrity Protection Maximum Data Rate: "full data rate" both ways
	// (0xFF) — the conventional value (TS 24.501 §9.11.4.7).
	req.IntegrityProtectionMaximumDataRate.SetMaximumDataRatePerUEForUserPlaneIntegrityProtectionForUpLink(0xFF)
	req.IntegrityProtectionMaximumDataRate.SetMaximumDataRatePerUEForUserPlaneIntegrityProtectionForDownLink(0xFF)

	pt := nasType.NewPDUSessionType(nasMessage.PDUSessionEstablishmentRequestPDUSessionTypeType)
	pt.SetPDUSessionTypeValue(nasMessage.PDUSessionTypeIPv4)
	req.PDUSessionType = pt

	ssc := nasType.NewSSCMode(nasMessage.PDUSessionEstablishmentRequestSSCModeType)
	ssc.SetSSCMode(0x01) // SSC mode 1 (TS 24.501 §9.11.4.16)
	req.SSCMode = ssc

	m.GsmMessage.PDUSessionEstablishmentRequest = req
	return m.PlainNasEncode()
}

// BuildULNASTransportForPDUSession wraps a 5GSM payload in a 5GMM UL NAS
// Transport (TS 24.501 §8.2.10): payload container type N1 SM information,
// plus PDU session ID, request type, S-NSSAI, and DNN so the AMF can route
// to the right SMF/slice. Returns the plain 5GMM message for the caller to
// wrap in the NAS security context.
func BuildULNASTransportForPDUSession(sm []byte, p PDUSessionParams) (*f5nas.Message, error) {
	if len(sm) == 0 {
		return nil, fmt.Errorf("empty 5GSM payload")
	}
	m, gmm := newGmm(f5nas.MsgTypeULNASTransport)
	t := nasMessage.NewULNASTransport(f5nas.MsgTypeULNASTransport)
	t.ExtendedProtocolDiscriminator.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSMobilityManagementMessage)
	t.SpareHalfOctetAndSecurityHeaderType.SetSecurityHeaderType(f5nas.SecurityHeaderTypePlainNas)
	t.SpareHalfOctetAndSecurityHeaderType.SetSpareHalfOctet(0)
	t.ULNASTRANSPORTMessageIdentity.SetMessageType(f5nas.MsgTypeULNASTransport)

	t.SpareHalfOctetAndPayloadContainerType.SetPayloadContainerType(nasMessage.PayloadContainerTypeN1SMInfo)
	t.PayloadContainer.SetLen(uint16(len(sm)))
	t.PayloadContainer.SetPayloadContainerContents(sm)

	pid := nasType.NewPduSessionID2Value(nasMessage.ULNASTransportPduSessionID2ValueType)
	pid.SetPduSessionID2Value(p.PDUSessionID)
	t.PduSessionID2Value = pid

	rt := nasType.NewRequestType(nasMessage.ULNASTransportRequestTypeType)
	rt.SetRequestTypeValue(nasMessage.ULNASTransportRequestTypeInitialRequest)
	t.RequestType = rt

	sn := nasType.NewSNSSAI(nasMessage.ULNASTransportSNSSAIType)
	if p.SD != "" {
		sd, err := parseSD(p.SD)
		if err != nil {
			return nil, err
		}
		sn.SetLen(4)
		sn.SetSST(p.SST)
		sn.SetSD(sd)
	} else {
		sn.SetLen(1)
		sn.SetSST(p.SST)
	}
	t.SNSSAI = sn

	dnn := nasType.NewDNN(nasMessage.ULNASTransportDNNType)
	dnn.SetDNN(p.DNN)
	t.DNN = dnn

	gmm.ULNASTransport = t
	return m, nil
}

// PDUSessionEstablishmentAcceptResult is what the UE learns from the SMF's
// PDU Session Establishment Accept (TS 24.501 §8.3.2).
type PDUSessionEstablishmentAcceptResult struct {
	PDUSessionID uint8
	IPv4         string // allocated UE IPv4 address, if PDU session type IPv4
}

// ExtractN1SMContainer pulls the 5GSM payload out of a decoded 5GMM DL NAS
// Transport (TS 24.501 §8.2.11). The AMF wraps SMF-originated 5GSM messages
// (e.g. PDU Session Establishment Accept) in a DL NAS Transport whose
// payload container carries the N1 SM information.
func ExtractN1SMContainer(m *f5nas.Message) ([]byte, error) {
	if m.GmmMessage == nil || m.DLNASTransport == nil {
		return nil, fmt.Errorf("not a DL NAS Transport")
	}
	return m.DLNASTransport.PayloadContainer.GetPayloadContainerContents(), nil
}

// ParsePDUSessionEstablishmentAccept decodes a bare 5GSM PDU Session
// Establishment Accept (the N1 SM container extracted from a DL NAS
// Transport) and returns the allocated UE IP.
func ParsePDUSessionEstablishmentAccept(sm []byte) (*PDUSessionEstablishmentAcceptResult, error) {
	m := f5nas.NewMessage()
	buf := append([]byte(nil), sm...)
	if err := m.PlainNasDecode(&buf); err != nil {
		return nil, fmt.Errorf("decode PDU Session Establishment Accept: %w", err)
	}
	if m.GsmMessage == nil || m.PDUSessionEstablishmentAccept == nil {
		return nil, fmt.Errorf("N1 SM container is not a PDU Session Establishment Accept")
	}
	acc := m.PDUSessionEstablishmentAccept
	out := &PDUSessionEstablishmentAcceptResult{PDUSessionID: acc.PDUSessionID.GetPDUSessionID()}
	if acc.PDUAddress != nil && acc.PDUAddress.GetPDUSessionTypeValue() == nasMessage.PDUSessionTypeIPv4 {
		ip := acc.PDUAddress.GetPDUAddressInformation()
		out.IPv4 = fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], ip[3])
	}
	return out, nil
}

func parseSD(sd string) ([3]byte, error) {
	var out [3]byte
	if len(sd) != 6 {
		return out, fmt.Errorf("S-NSSAI SD %q must be 6 hex digits", sd)
	}
	for i := 0; i < 3; i++ {
		var b byte
		if _, err := fmt.Sscanf(sd[i*2:i*2+2], "%02x", &b); err != nil {
			return out, fmt.Errorf("S-NSSAI SD %q: %w", sd, err)
		}
		out[i] = b
	}
	return out, nil
}
