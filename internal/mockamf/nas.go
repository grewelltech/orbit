package mockamf

import (
	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"
)

// Network-side (AMF→UE) 5GMM message builders — the mirror of the UE builders
// in internal/ue. Only what the attach needs: Authentication Request,
// Security Mode Command, Registration Accept. Message structs and codec come
// from free5gc/nas; the mock populates the IEs.

// registrationResult3GPPAccess is the 5GS registration result value for
// 3GPP access (TS 24.501 §9.11.3.6).
const registrationResult3GPPAccess = 0x01

func plainGmm(msgType uint8) (*f5nas.Message, *f5nas.GmmMessage) {
	m := f5nas.NewMessage()
	m.GmmMessage = f5nas.NewGmmMessage()
	m.SecurityHeader = f5nas.SecurityHeader{
		ProtocolDiscriminator: nasMessage.Epd5GSMobilityManagementMessage,
		SecurityHeaderType:    f5nas.SecurityHeaderTypePlainNas,
	}
	m.GmmHeader.SetMessageType(msgType)
	return m, m.GmmMessage
}

func setPlainHeader(epd interface {
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

// buildAuthenticationRequest builds a plain Authentication Request carrying
// the RAND/AUTN challenge (TS 24.501 §8.2.1). ABBA defaults to 0x0000.
func buildAuthenticationRequest(rand, autn [16]byte, ngksi uint8) ([]byte, error) {
	m, gmm := plainGmm(f5nas.MsgTypeAuthenticationRequest)
	req := nasMessage.NewAuthenticationRequest(f5nas.MsgTypeAuthenticationRequest)
	setPlainHeader(&req.ExtendedProtocolDiscriminator, &req.SpareHalfOctetAndSecurityHeaderType,
		&req.AuthenticationRequestMessageIdentity, f5nas.MsgTypeAuthenticationRequest)

	req.SpareHalfOctetAndNgksi.SetTSC(nasMessage.TypeOfSecurityContextFlagNative)
	req.SpareHalfOctetAndNgksi.SetNasKeySetIdentifiler(ngksi)
	req.ABBA.SetLen(2)
	req.ABBA.SetABBAContents([]uint8{0x00, 0x00})

	r := nasType.NewAuthenticationParameterRAND(nasMessage.AuthenticationRequestAuthenticationParameterRANDType)
	r.SetRANDValue(rand)
	req.AuthenticationParameterRAND = r

	a := nasType.NewAuthenticationParameterAUTN(nasMessage.AuthenticationRequestAuthenticationParameterAUTNType)
	a.SetLen(16)
	a.SetAUTN(autn)
	req.AuthenticationParameterAUTN = a

	gmm.AuthenticationRequest = req
	return m.PlainNasEncode()
}

// buildSecurityModeCommand builds a plain Security Mode Command selecting the
// NAS algorithms and echoing the UE security capabilities (TS 24.501
// §8.2.25). Returned for the caller to wrap under the new context.
func buildSecurityModeCommand(intAlg, encAlg, ngksi uint8, replayedCap nasType.UESecurityCapability) (*f5nas.Message, error) {
	m, gmm := plainGmm(f5nas.MsgTypeSecurityModeCommand)
	smc := nasMessage.NewSecurityModeCommand(f5nas.MsgTypeSecurityModeCommand)
	setPlainHeader(&smc.ExtendedProtocolDiscriminator, &smc.SpareHalfOctetAndSecurityHeaderType,
		&smc.SecurityModeCommandMessageIdentity, f5nas.MsgTypeSecurityModeCommand)

	smc.SelectedNASSecurityAlgorithms.SetTypeOfCipheringAlgorithm(encAlg)
	smc.SelectedNASSecurityAlgorithms.SetTypeOfIntegrityProtectionAlgorithm(intAlg)
	smc.SpareHalfOctetAndNgksi.SetTSC(nasMessage.TypeOfSecurityContextFlagNative)
	smc.SpareHalfOctetAndNgksi.SetNasKeySetIdentifiler(ngksi)

	// Replay the UE security capabilities verbatim (same layout as
	// UESecurityCapability).
	smc.ReplayedUESecurityCapabilities.SetLen(replayedCap.GetLen())
	smc.ReplayedUESecurityCapabilities.Buffer = append([]uint8(nil), replayedCap.Buffer...)

	gmm.SecurityModeCommand = smc
	return m, nil
}

// buildRegistrationAccept builds a plain Registration Accept (TS 24.501
// §8.2.7). Returned for the caller to wrap (integrity + ciphered).
func buildRegistrationAccept() (*f5nas.Message, error) {
	m, gmm := plainGmm(f5nas.MsgTypeRegistrationAccept)
	acc := nasMessage.NewRegistrationAccept(f5nas.MsgTypeRegistrationAccept)
	setPlainHeader(&acc.ExtendedProtocolDiscriminator, &acc.SpareHalfOctetAndSecurityHeaderType,
		&acc.RegistrationAcceptMessageIdentity, f5nas.MsgTypeRegistrationAccept)

	acc.RegistrationResult5GS.SetLen(1)
	acc.RegistrationResult5GS.SetRegistrationResultValue5GS(registrationResult3GPPAccess)

	gmm.RegistrationAccept = acc
	return m, nil
}
