// Package engine orchestrates ORBIT's UE and gNB roles into end-to-end
// procedures. attach.go drives the Phase-1a control-plane attach of a single
// UE against a live core: Registration → Authentication (5G-AKA) → Security
// Mode → Initial Context Setup → REGISTERED (TS 23.502 §4.2.2.2).
package engine

import (
	"context"
	"fmt"
	"log/slog"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// ueStream is the SCTP stream used for the single UE's associated signalling
// (non-UE-associated NG Setup used stream 0).
const ueStream uint16 = 1

// UEConfig is the subscription and identity ORBIT registers.
type UEConfig struct {
	Identity ue.Identity
	Sub      auth.Subscription
}

// AttachResult reports the outcome of a control-plane attach.
type AttachResult struct {
	Registered  bool
	SUPI        string
	AMFUENGAPID int64
	RANUENGAPID int64
}

// Attach registers one UE over an already-NG-Setup association. It runs the
// NAS-MM and NGAP procedure sequence to REGISTERED, wrapping NAS in the
// security context once Security Mode Command activates it. The PDU-session
// establishment (data plane) is Phase 1b and layered on top of this.
func Attach(ctx context.Context, conn *sctp.Conn, gnbCfg gnb.Config, ueCfg UEConfig, log *slog.Logger) (*AttachResult, error) {
	ranID := int64(1)
	snn := auth.ServingNetworkName(gnbCfg.MCC, gnbCfg.MNC)

	suci, err := ueCfg.Identity.EncodeNullSUCI()
	if err != nil {
		return nil, fmt.Errorf("encode SUCI: %w", err)
	}
	regReq, err := ue.BuildRegistrationRequest(suci, ue.SecurityCapability(), nil)
	if err != nil {
		return nil, fmt.Errorf("build Registration Request: %w", err)
	}

	initial, err := gnb.BuildInitialUEMessage(gnbCfg, ranID, regReq)
	if err != nil {
		return nil, err
	}
	if err := gnb.SendPDU(conn, ueStream, initial); err != nil {
		return nil, fmt.Errorf("send Initial UE Message: %w", err)
	}
	log.InfoContext(ctx, "sent Initial UE Message with Registration Request", "supi", ueCfg.Sub.SUPI)

	st := &attachState{
		ctx: ctx, conn: conn, gnbCfg: gnbCfg, ueCfg: ueCfg,
		snn: snn, ranID: ranID, regReq: regReq, log: log,
	}

	for !st.registered {
		pdu, err := gnb.ReadPDU(ctx, conn)
		if err != nil {
			return nil, err
		}
		dl, err := gnb.ParseDownlink(pdu)
		if err != nil {
			return nil, err
		}
		if dl.HasAMFID {
			st.amfID = dl.AMFUENGAPID
		}
		if err := st.handle(dl); err != nil {
			return nil, err
		}
	}

	return &AttachResult{
		Registered:  true,
		SUPI:        ueCfg.Sub.SUPI,
		AMFUENGAPID: st.amfID,
		RANUENGAPID: ranID,
	}, nil
}

type attachState struct {
	ctx    context.Context
	conn   *sctp.Conn
	gnbCfg gnb.Config
	ueCfg  UEConfig
	snn    string
	ranID  int64
	regReq []byte
	log    *slog.Logger

	amfID      int64
	vec        *auth.Vector
	sec        *nas.SecurityContext
	registered bool
}

// handle dispatches one AMF→gNB UE-associated message.
func (s *attachState) handle(dl *gnb.DownlinkMessage) error {
	switch dl.Procedure {
	case ngapProcDownlinkNASTransport:
		return s.handleNAS(dl.NASPDU, false)
	case ngapProcInitialContextSetup:
		// Carries Registration Accept as its NAS PDU; acknowledge the
		// context, then process the NAS and complete registration.
		resp := gnb.BuildInitialContextSetupResponse(s.amfID, s.ranID)
		if err := gnb.SendPDU(s.conn, ueStream, resp); err != nil {
			return fmt.Errorf("send Initial Context Setup Response: %w", err)
		}
		s.log.InfoContext(s.ctx, "sent Initial Context Setup Response")
		if len(dl.NASPDU) > 0 {
			return s.handleNAS(dl.NASPDU, true)
		}
		return nil
	default:
		s.log.WarnContext(s.ctx, "unhandled downlink procedure", "procedure", dl.Procedure)
		return nil
	}
}

// handleNAS decodes one downlink NAS PDU (plain or security-protected) and
// runs the matching UE reaction. fromICS marks NAS carried in Initial
// Context Setup (the Registration Accept path).
func (s *attachState) handleNAS(raw []byte, fromICS bool) error {
	if len(raw) < 2 {
		return fmt.Errorf("downlink NAS too short: %d bytes", len(raw))
	}
	var msg *f5nas.Message
	var err error
	if f5nas.GetSecurityHeaderType(raw) == f5nas.SecurityHeaderTypePlainNas {
		msg, err = nas.DecodePlain(raw)
	} else {
		if s.sec == nil {
			return fmt.Errorf("received protected NAS before security context established")
		}
		msg, err = s.sec.DecodeSecure(raw)
	}
	if err != nil {
		return fmt.Errorf("decode downlink NAS: %w", err)
	}

	switch mt := msg.GmmHeader.GetMessageType(); mt {
	case f5nas.MsgTypeAuthenticationRequest:
		return s.onAuthRequest(msg)
	case f5nas.MsgTypeSecurityModeCommand:
		return s.onSecurityModeCommand(msg)
	case f5nas.MsgTypeRegistrationAccept:
		return s.onRegistrationAccept(msg)
	case f5nas.MsgTypeIdentityRequest:
		return fmt.Errorf("AMF requested an Identity (not expected with null-SUCI in Initial UE Message); Identity Response not yet implemented")
	case f5nas.MsgTypeAuthenticationReject:
		return fmt.Errorf("AMF rejected authentication")
	case f5nas.MsgTypeRegistrationReject:
		return fmt.Errorf("AMF rejected registration")
	default:
		s.log.WarnContext(s.ctx, "unhandled downlink NAS message", "type", mt)
		return nil
	}
}

func (s *attachState) onAuthRequest(msg *f5nas.Message) error {
	ch, err := ue.ParseAuthenticationRequest(msg)
	if err != nil {
		return err
	}
	// Derive with the algorithms the UE advertised (NIA2/NEA0); the AMF must
	// select from them. KNASint/KNASenc are re-confirmed at Security Mode
	// Command.
	vec, err := s.ueCfg.Sub.DeriveFromChallenge(ch.RAND[:], ch.AUTN[:], s.snn, nas.NIA2, nas.NEA0)
	if err != nil {
		return fmt.Errorf("5G-AKA derivation: %w", err)
	}
	s.vec = vec

	resp, err := ue.BuildAuthenticationResponse(vec.ResStar)
	if err != nil {
		return err
	}
	if err := s.sendUL(resp); err != nil {
		return err
	}
	s.log.InfoContext(s.ctx, "authenticated network, sent Authentication Response")
	return nil
}

func (s *attachState) onSecurityModeCommand(msg *f5nas.Message) error {
	sel, err := ue.ParseSecurityModeCommand(msg)
	if err != nil {
		return err
	}
	if sel.Integrity != nas.NIA2 || sel.Ciphering != nas.NEA0 {
		return fmt.Errorf("AMF selected unsupported NAS algorithms: integrity %d, ciphering %d", sel.Integrity, sel.Ciphering)
	}

	// Security Mode Complete echoes the full initial Registration Request in
	// the NAS Message Container (TS 24.501 §4.4.6), sent integrity-protected
	// and ciphered under the new context.
	complete, err := ue.BuildSecurityModeComplete(s.regReq)
	if err != nil {
		return err
	}
	wrapped, err := s.sec.EncodeSecure(complete, nas.SecHdrIntegrityCipheredNewCtx)
	if err != nil {
		return fmt.Errorf("wrap Security Mode Complete: %w", err)
	}
	if err := s.sendUL(wrapped); err != nil {
		return err
	}
	s.log.InfoContext(s.ctx, "verified Security Mode Command, sent Security Mode Complete",
		"integrity", sel.Integrity, "ciphering", sel.Ciphering)
	return nil
}

func (s *attachState) onRegistrationAccept(_ *f5nas.Message) error {
	complete, err := ue.BuildRegistrationComplete()
	if err != nil {
		return err
	}
	wrapped, err := s.sec.EncodeSecure(complete, nas.SecHdrIntegrityCiphered)
	if err != nil {
		return fmt.Errorf("wrap Registration Complete: %w", err)
	}
	if err := s.sendUL(wrapped); err != nil {
		return err
	}
	s.registered = true
	s.log.InfoContext(s.ctx, "registration accepted; sent Registration Complete — UE is 5GMM-REGISTERED",
		"supi", s.ueCfg.Sub.SUPI)
	return nil
}

// sendUL wraps a (plain or already-secured) NAS PDU in an Uplink NAS
// Transport and sends it. Before the security context exists it also
// pre-derives that context from KAMF so the first protected downlink
// (Security Mode Command) can be verified.
func (s *attachState) sendUL(nasPDU []byte) error {
	if s.sec == nil && s.vec != nil {
		kInt, kEnc, err := auth.DeriveNASKeys(s.vec.KAmf, nas.NIA2, nas.NEA0)
		if err != nil {
			return fmt.Errorf("derive NAS keys: %w", err)
		}
		s.sec = &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA0, KNASint: kInt, KNASenc: kEnc}
	}
	pdu, err := gnb.BuildUplinkNASTransport(s.gnbCfg, s.amfID, s.ranID, nasPDU)
	if err != nil {
		return err
	}
	return gnb.SendPDU(s.conn, ueStream, pdu)
}

// NGAP procedure codes the FSM branches on (TS 38.413), aliased locally so
// attach.go does not import ngapType directly.
const (
	ngapProcDownlinkNASTransport = 4
	ngapProcInitialContextSetup  = 14
)
