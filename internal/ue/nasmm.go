package ue

import (
	"fmt"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/nas"
)

// This file builds the UE-originated 5GMM (NAS mobility management) messages
// and parses the network-originated ones for the Phase-1a attach
// (TS 24.501 §8.2). Message structs and codec come from free5gc/nas; ORBIT
// populates the IEs. Construction is grounded against gnbsim's field-used
// builders (which attach to this same SD-Core) and the free5gc message
// tests, cited per message.

// SecurityCapability advertises the NAS algorithms this UE supports
// (TS 24.501 §9.11.3.54). ORBIT implements ciphering NEA0 and integrity
// NIA2 only, so it advertises exactly those: advertising an algorithm we
// cannot perform would let the AMF select it. With the ATB-01 AMF offering
// integrity NIA1 then NIA2 (D-8), advertising only NIA2 forces NIA2.
//
//	octet 1 (5GS enc): bit8=EA0 bit7=EA1 bit6=EA2 bit5=EA3 → 0x80 = EA0
//	octet 2 (5GS int): bit8=IA0 bit7=IA1 bit6=IA2 bit5=IA3 → 0x20 = IA2
func SecurityCapability() nasType.UESecurityCapability {
	c := nasType.NewUESecurityCapability(nasMessage.RegistrationRequestUESecurityCapabilityType)
	c.SetLen(2)
	c.SetEA0_5G(1)
	c.SetIA2_128_5G(1)
	return *c
}

// newGmm builds an empty plain 5GMM message with the header set for the
// given message type.
func newGmm(msgType uint8) (*f5nas.Message, *f5nas.GmmMessage) {
	m := f5nas.NewMessage()
	m.GmmMessage = f5nas.NewGmmMessage()
	m.SecurityHeader = f5nas.SecurityHeader{
		ProtocolDiscriminator: nasMessage.Epd5GSMobilityManagementMessage,
		SecurityHeaderType:    f5nas.SecurityHeaderTypePlainNas,
	}
	m.GmmHeader.SetMessageType(msgType)
	return m, m.GmmMessage
}

func setGmmHeader(epd interface {
	SetExtendedProtocolDiscriminator(uint8)
}, secHdr interface {
	SetSecurityHeaderType(uint8)
	SetSpareHalfOctet(uint8)
}, msgID interface{ SetMessageType(uint8) }, msgType uint8) {
	epd.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSMobilityManagementMessage)
	secHdr.SetSecurityHeaderType(f5nas.SecurityHeaderTypePlainNas)
	secHdr.SetSpareHalfOctet(0)
	msgID.SetMessageType(msgType)
}

// BuildRegistrationRequest builds an initial Registration Request carrying
// the UE's SUCI (TS 24.501 §8.2.6). ngKSI is set to "no key available" (7)
// on the first registration. Mandatory IEs plus the UE security capability
// (so the AMF can run Security Mode Command) and requested NSSAI.
func BuildRegistrationRequest(suci []byte, secCap nasType.UESecurityCapability, requestedNSSAI *nasType.RequestedNSSAI) ([]byte, error) {
	m, gmm := newGmm(f5nas.MsgTypeRegistrationRequest)
	req := nasMessage.NewRegistrationRequest(f5nas.MsgTypeRegistrationRequest)
	setGmmHeader(&req.ExtendedProtocolDiscriminator, &req.SpareHalfOctetAndSecurityHeaderType,
		&req.RegistrationRequestMessageIdentity, f5nas.MsgTypeRegistrationRequest)

	// ngKSI: no key set available yet (value 7); native key set (TSC 0).
	req.NgksiAndRegistrationType5GS.SetTSC(nasMessage.TypeOfSecurityContextFlagNative)
	req.NgksiAndRegistrationType5GS.SetNasKeySetIdentifiler(0x07)
	req.NgksiAndRegistrationType5GS.SetFOR(1)
	req.NgksiAndRegistrationType5GS.SetRegistrationType5GS(nasMessage.RegistrationType5GSInitialRegistration)

	req.MobileIdentity5GS = nasType.MobileIdentity5GS{
		Len:    uint16(len(suci)),
		Buffer: suci,
	}

	sc := secCap
	req.UESecurityCapability = &sc

	// 5GMM capability (TS 24.501 §9.11.3.1): advertise minimal S1 support.
	cap5g := nasType.NewCapability5GMM(nasMessage.RegistrationRequestCapability5GMMType)
	cap5g.SetLen(1)
	cap5g.SetS1Mode(0)
	req.Capability5GMM = cap5g

	if requestedNSSAI != nil {
		req.RequestedNSSAI = requestedNSSAI
	}

	gmm.RegistrationRequest = req
	return m.PlainNasEncode()
}

// BuildAuthenticationResponse carries RES* back to the network
// (TS 24.501 §8.2.2). resStar must be 16 bytes.
func BuildAuthenticationResponse(resStar []byte) ([]byte, error) {
	if len(resStar) != 16 {
		return nil, fmt.Errorf("RES* must be 16 bytes, got %d", len(resStar))
	}
	m, gmm := newGmm(f5nas.MsgTypeAuthenticationResponse)
	resp := nasMessage.NewAuthenticationResponse(f5nas.MsgTypeAuthenticationResponse)
	setGmmHeader(&resp.ExtendedProtocolDiscriminator, &resp.SpareHalfOctetAndSecurityHeaderType,
		&resp.AuthenticationResponseMessageIdentity, f5nas.MsgTypeAuthenticationResponse)

	p := nasType.NewAuthenticationResponseParameter(nasMessage.AuthenticationResponseAuthenticationResponseParameterType)
	p.SetLen(uint8(len(resStar)))
	var res [16]uint8
	copy(res[:], resStar)
	p.SetRES(res)
	resp.AuthenticationResponseParameter = p

	gmm.AuthenticationResponse = resp
	return m.PlainNasEncode()
}

// BuildSecurityModeComplete completes the security mode procedure
// (TS 24.501 §8.2.26). The full initial Registration Request is echoed back
// in the NAS Message Container (TS 24.501 §4.4.6): the AMF asked for the
// complete initial NAS message so it can re-evaluate it now that integrity
// is established. Returns the plain message for the caller to wrap with the
// new security context.
func BuildSecurityModeComplete(fullRegistrationRequest []byte) (*f5nas.Message, error) {
	m, gmm := newGmm(f5nas.MsgTypeSecurityModeComplete)
	c := nasMessage.NewSecurityModeComplete(f5nas.MsgTypeSecurityModeComplete)
	setGmmHeader(&c.ExtendedProtocolDiscriminator, &c.SpareHalfOctetAndSecurityHeaderType,
		&c.SecurityModeCompleteMessageIdentity, f5nas.MsgTypeSecurityModeComplete)

	if len(fullRegistrationRequest) > 0 {
		nmc := nasType.NewNASMessageContainer(nasMessage.SecurityModeCompleteNASMessageContainerType)
		nmc.SetLen(uint16(len(fullRegistrationRequest)))
		nmc.Buffer = fullRegistrationRequest
		c.NASMessageContainer = nmc
	}

	gmm.SecurityModeComplete = c
	return m, nil
}

// BuildRegistrationComplete acknowledges Registration Accept
// (TS 24.501 §8.2.8). Returns the plain message for security wrapping.
func BuildRegistrationComplete() (*f5nas.Message, error) {
	m, gmm := newGmm(f5nas.MsgTypeRegistrationComplete)
	c := nasMessage.NewRegistrationComplete(f5nas.MsgTypeRegistrationComplete)
	setGmmHeader(&c.ExtendedProtocolDiscriminator, &c.SpareHalfOctetAndSecurityHeaderType,
		&c.RegistrationCompleteMessageIdentity, f5nas.MsgTypeRegistrationComplete)
	gmm.RegistrationComplete = c
	return m, nil
}

// AuthChallenge is the RAND/AUTN pair extracted from an Authentication
// Request (TS 24.501 §8.2.1).
type AuthChallenge struct {
	RAND [16]byte
	AUTN [16]byte
}

// ParseAuthenticationRequest extracts the challenge from a decoded plain NAS
// Authentication Request.
func ParseAuthenticationRequest(m *f5nas.Message) (*AuthChallenge, error) {
	if m.GmmMessage == nil || m.AuthenticationRequest == nil {
		return nil, fmt.Errorf("not an Authentication Request")
	}
	ar := m.AuthenticationRequest
	if ar.AuthenticationParameterRAND == nil || ar.AuthenticationParameterAUTN == nil {
		return nil, fmt.Errorf("Authentication Request missing RAND/AUTN (EAP-AKA' not supported in Phase 1a)")
	}
	return &AuthChallenge{
		RAND: ar.AuthenticationParameterRAND.GetRANDValue(),
		AUTN: ar.AuthenticationParameterAUTN.GetAUTN(),
	}, nil
}

// SelectedAlgorithms are the NAS algorithms the AMF chose, from Security
// Mode Command (TS 24.501 §8.2.25).
type SelectedAlgorithms struct {
	Ciphering uint8 // NEA identity
	Integrity uint8 // NIA identity
}

// ParseSecurityModeCommand extracts the selected NAS algorithms.
func ParseSecurityModeCommand(m *f5nas.Message) (*SelectedAlgorithms, error) {
	if m.GmmMessage == nil || m.SecurityModeCommand == nil {
		return nil, fmt.Errorf("not a Security Mode Command")
	}
	smc := m.SecurityModeCommand
	return &SelectedAlgorithms{
		Ciphering: smc.SelectedNASSecurityAlgorithms.GetTypeOfCipheringAlgorithm(),
		Integrity: smc.SelectedNASSecurityAlgorithms.GetTypeOfIntegrityProtectionAlgorithm(),
	}, nil
}

// DecodePlainMM decodes a plain (unprotected) downlink 5GMM message.
func DecodePlainMM(b []byte) (*f5nas.Message, error) {
	return nas.DecodePlain(b)
}
