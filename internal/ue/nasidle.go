package ue

import (
	"fmt"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"
)

// Service types of a Service Request (TS 24.501 9.11.3.50).
const (
	ServiceTypeSignalling       = nasMessage.ServiceTypeSignalling
	ServiceTypeData             = nasMessage.ServiceTypeData
	ServiceTypeMobileTerminated = nasMessage.ServiceTypeMobileTerminatedServices
)

// ServiceRequestParams describes a Service Request (TS 24.501 8.2.16).
type ServiceRequestParams struct {
	// GUTI is the UE's 5G-GUTI as the 11-octet 5GS mobile identity value;
	// the request carries its 5G-S-TMSI.
	GUTI []byte
	// NgKSI is the key set identifier of the UE's NAS security context.
	NgKSI uint8
	// ServiceType is one of the ServiceType constants.
	ServiceType uint8
	// UplinkDataStatus has bit n set for each PDU session ID n with uplink
	// data pending; zero omits the IE.
	UplinkDataStatus uint16
	// PDUSessionStatus has bit n set for each PDU session ID n the UE
	// holds; zero omits the IE.
	PDUSessionStatus uint16
}

// HasNonCleartext reports whether the request carries IEs that may not be
// sent in the clear in an initial NAS message (TS 24.501 4.4.6), so the
// whole message travels in a NAS message container.
func (p ServiceRequestParams) HasNonCleartext() bool {
	return p.UplinkDataStatus != 0 || p.PDUSessionStatus != 0
}

// BuildServiceRequest builds the complete plain Service Request, with every
// IE of p. Sent as it is (integrity-protected) when it has cleartext IEs
// only; otherwise it is the content of the NAS message container of the
// request BuildServiceRequestWithContainer builds.
func BuildServiceRequest(p ServiceRequestParams) ([]byte, error) {
	m, req, err := serviceRequest(p)
	if err != nil {
		return nil, err
	}
	if p.UplinkDataStatus != 0 {
		req.UplinkDataStatus = &nasType.UplinkDataStatus{Iei: nasMessage.ServiceRequestUplinkDataStatusType, Len: 2,
			Buffer: []uint8{uint8(p.UplinkDataStatus), uint8(p.UplinkDataStatus >> 8)}}
	}
	if p.PDUSessionStatus != 0 {
		req.PDUSessionStatus = &nasType.PDUSessionStatus{Iei: nasMessage.ServiceRequestPDUSessionStatusType, Len: 2,
			Buffer: []uint8{uint8(p.PDUSessionStatus), uint8(p.PDUSessionStatus >> 8)}}
	}
	return m.PlainNasEncode()
}

// BuildServiceRequestWithContainer builds the plain Service Request that
// carries the cleartext IEs only (ngKSI, service type, 5G-S-TMSI) and
// container as its NAS message container: the complete request, ciphered
// by the caller under the NAS security context (TS 24.501 4.4.6).
func BuildServiceRequestWithContainer(p ServiceRequestParams, container []byte) ([]byte, error) {
	m, req, err := serviceRequest(p)
	if err != nil {
		return nil, err
	}
	req.NASMessageContainer = &nasType.NASMessageContainer{Iei: nasMessage.ServiceRequestNASMessageContainerType,
		Len: uint16(len(container)), Buffer: container}
	return m.PlainNasEncode()
}

// serviceRequest builds a Service Request with its mandatory IEs.
func serviceRequest(p ServiceRequestParams) (*f5nas.Message, *nasMessage.ServiceRequest, error) {
	if len(p.GUTI) != 11 {
		return nil, nil, fmt.Errorf("service request requires the assigned 5G-GUTI")
	}
	m, gmm := newGmm(f5nas.MsgTypeServiceRequest)
	req := nasMessage.NewServiceRequest(f5nas.MsgTypeServiceRequest)
	setGmmHeader(&req.ExtendedProtocolDiscriminator, &req.SpareHalfOctetAndSecurityHeaderType,
		&req.ServiceRequestMessageIdentity, f5nas.MsgTypeServiceRequest)
	req.SetTSC(nasMessage.TypeOfSecurityContextFlagNative)
	req.SetNasKeySetIdentifiler(p.NgKSI)
	req.SetServiceTypeValue(p.ServiceType)
	// The 5G-S-TMSI mobile identity (TS 24.501 9.11.3.4): the type octet,
	// then the 5G-GUTI's AMF Set ID, AMF Pointer and 5G-TMSI.
	req.TMSI5GS.Len = 7
	req.TMSI5GS.Octet[0] = 0xf0 | nasMessage.MobileIdentity5GSType5gSTmsi
	copy(req.TMSI5GS.Octet[1:], p.GUTI[5:11])
	gmm.ServiceRequest = req
	return m, req, nil
}

// ServiceAccept is what a Service Accept (TS 24.501 8.2.17) reports.
type ServiceAccept struct {
	// PDUSessionStatus has bit n set for each PDU session ID n the network
	// holds; HasPDUSessionStatus is false when the IE is absent.
	PDUSessionStatus    uint16
	HasPDUSessionStatus bool
	// ReactivationResult has bit n set for each PDU session ID n whose
	// user-plane reactivation the UE asked for and the network failed
	// (TS 24.501 9.11.3.42); HasReactivationResult is false when the IE is
	// absent.
	ReactivationResult    uint16
	HasReactivationResult bool
	// ReactivationErrors is the PDU session reactivation result error
	// cause IE's value (pairs of PDU session ID and 5GMM cause), nil when
	// absent.
	ReactivationErrors []byte
}

// ParseServiceAccept reads a Service Accept.
func ParseServiceAccept(m *f5nas.Message) (*ServiceAccept, error) {
	if m == nil || m.GmmMessage == nil || m.ServiceAccept == nil {
		return nil, fmt.Errorf("not a Service Accept")
	}
	a := &ServiceAccept{}
	if s := m.ServiceAccept.PDUSessionStatus; s != nil {
		a.PDUSessionStatus, a.HasPDUSessionStatus = bitmap16(s.Buffer), true
	}
	if r := m.ServiceAccept.PDUSessionReactivationResult; r != nil {
		a.ReactivationResult, a.HasReactivationResult = bitmap16(r.Buffer), true
	}
	if e := m.ServiceAccept.PDUSessionReactivationResultErrorCause; e != nil {
		a.ReactivationErrors = append([]byte(nil), e.Buffer...)
	}
	return a, nil
}

// ParseServiceReject returns the 5GMM cause of a Service Reject (TS 24.501
// 8.2.18).
func ParseServiceReject(m *f5nas.Message) (uint8, error) {
	if m == nil || m.GmmMessage == nil || m.ServiceReject == nil {
		return 0, fmt.Errorf("not a Service Reject")
	}
	return m.ServiceReject.Cause5GMM.Octet, nil
}

// bitmap16 reads a PDU session bitmap IE's value: bit n of the result is
// PDU session ID n.
func bitmap16(b []uint8) uint16 {
	var v uint16
	if len(b) > 0 {
		v = uint16(b[0])
	}
	if len(b) > 1 {
		v |= uint16(b[1]) << 8
	}
	return v
}

// RegistrationInfo is what a Registration Accept says about the UE's idle
// behaviour (TS 24.501 8.2.7).
type RegistrationInfo struct {
	// T3512 is the periodic registration timer; HasT3512 is false when the
	// IE is absent, and T3512Deactivated true when it says the timer is
	// deactivated.
	T3512            time.Duration
	HasT3512         bool
	T3512Deactivated bool
	// MICO is true when the Accept carries a MICO indication: the UE is
	// not paged while CM-IDLE.
	MICO bool
	// ServiceAreaList and TAIList are those IEs' values, nil when absent.
	ServiceAreaList []byte
	TAIList         []byte
}

// ParseRegistrationInfo reads the idle-mode IEs of a Registration Accept.
func ParseRegistrationInfo(m *f5nas.Message) (RegistrationInfo, error) {
	if m == nil || m.GmmMessage == nil || m.RegistrationAccept == nil {
		return RegistrationInfo{}, fmt.Errorf("not a Registration Accept")
	}
	acc := m.RegistrationAccept
	var info RegistrationInfo
	if t := acc.T3512Value; t != nil {
		info.HasT3512 = true
		info.T3512, info.T3512Deactivated = gprsTimer3(t.Octet)
	}
	info.MICO = acc.MICOIndication != nil
	if s := acc.ServiceAreaList; s != nil {
		info.ServiceAreaList = append([]byte(nil), s.Buffer...)
	}
	if l := acc.TAIList; l != nil {
		info.TAIList = append([]byte(nil), l.Buffer...)
	}
	return info, nil
}

// gprsTimer3 decodes a GPRS timer 3 octet (TS 24.008 10.5.7.4a): the unit
// in bits 8 to 6 and the value in bits 5 to 1; unit 7 is "deactivated".
func gprsTimer3(o uint8) (d time.Duration, deactivated bool) {
	units := [...]time.Duration{10 * time.Minute, time.Hour, 10 * time.Hour, 2 * time.Second,
		30 * time.Second, time.Minute, 320 * time.Hour}
	u := o >> 5
	if u == 7 {
		return 0, true
	}
	return time.Duration(o&0x1f) * units[u], false
}
