package mockamf

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasConvert"
	"github.com/free5gc/ngap/ngapType"
	"github.com/free5gc/util/milenage"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// Config parameters the mock AMF. All simulated UEs share one Milenage
// credential (the ATB-01 simapp shape); the per-UE SUPI comes from the SUCI
// in each Registration Request.
type Config struct {
	Ki, OPc  []byte // 16 bytes each
	SQN, AMF []byte // 6-byte sequence number, 2-byte auth management field
	MCC, MNC string
	SST      uint8
	SD       string // 6 hex digits, optional
}

// AMF is an in-process mock AMF for sim-capacity measurement.
type AMF struct {
	cfg     Config
	plmn    [3]byte
	sdBytes []byte
	snn     string
	amfSeq  chan int64 // hands out AMF-UE-NGAP-IDs
}

// New validates config and returns a mock AMF.
func New(cfg Config) (*AMF, error) {
	if len(cfg.Ki) != 16 || len(cfg.OPc) != 16 {
		return nil, fmt.Errorf("Ki and OPc must be 16 bytes")
	}
	if len(cfg.SQN) != 6 || len(cfg.AMF) != 2 {
		return nil, fmt.Errorf("SQN must be 6 bytes and AMF 2 bytes")
	}
	plmn, err := ngap.EncodePLMN(cfg.MCC, cfg.MNC)
	if err != nil {
		return nil, err
	}
	var sd []byte
	if cfg.SD != "" {
		if sd, err = hex.DecodeString(cfg.SD); err != nil || len(sd) != 3 {
			return nil, fmt.Errorf("SD %q must be 6 hex digits", cfg.SD)
		}
	}
	a := &AMF{cfg: cfg, plmn: plmn, sdBytes: sd, snn: auth.ServingNetworkName(cfg.MCC, cfg.MNC), amfSeq: make(chan int64)}
	go func() {
		id := int64(1)
		for {
			a.amfSeq <- id
			id++
		}
	}()
	return a, nil
}

// Connect returns a UE-side transport for a single UE (one pipe per UE), used
// by the D-6 sim-capacity harness to isolate the actor-model cost. The
// handler stops when ctx is done or the pipe closes.
func (a *AMF) Connect(ctx context.Context) gnb.Transport {
	return a.connect(ctx)
}

// ConnectShared returns a UE-side transport meant to carry MANY UEs
// multiplexed over one association (as a gnb.Session does), demultiplexed by
// RAN-UE-NGAP-ID — the real gNB pattern, for testing the actor model's
// muxing in-process.
func (a *AMF) ConnectShared(ctx context.Context) gnb.Transport {
	return a.connect(ctx)
}

func (a *AMF) connect(ctx context.Context) gnb.Transport {
	uePipe, amfPipe := newPipePair()
	go func() {
		_ = a.serve(ctx, amfPipe)
		amfPipe.Close()
	}()
	return uePipe
}

// serve handles one association from NG Setup onward, tracking per-UE state
// keyed by RAN-UE-NGAP-ID so a single pipe can carry many UEs.
func (a *AMF) serve(ctx context.Context, p *pipe) error {
	states := make(map[int64]*ueState)
	buf := make([]byte, 65536)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw, _, _, err := p.ReadMsg(buf)
		if err != nil {
			return err
		}
		pdu, err := ngap.Decode(raw)
		if err != nil {
			return fmt.Errorf("mock amf decode: %w", err)
		}
		if err := a.dispatch(p, states, pdu); err != nil {
			return err
		}
	}
}

// dispatch routes one gNB→AMF PDU: NG Setup is answered once; UE-associated
// messages go to their per-RAN-UE-NGAP-ID state.
func (a *AMF) dispatch(p *pipe, states map[int64]*ueState, pdu *ngapType.NGAPPDU) error {
	if pdu.Present == ngapType.NGAPPDUPresentInitiatingMessage &&
		pdu.InitiatingMessage.Value.Present == ngapType.InitiatingMessagePresentNGSetupRequest {
		resp, err := buildNGSetupResponse(a.plmn, a.cfg.SST, a.sdBytes)
		if err != nil {
			return err
		}
		return a.send(p, resp)
	}
	if pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage {
		return nil // Initial Context Setup Response etc. — no action
	}
	up, err := parseUplink(pdu)
	if err != nil {
		return err
	}
	st := states[up.ranID]
	if st == nil {
		st = &ueState{ngksi: 1}
		states[up.ranID] = st
	}
	switch up.procedure {
	case ngapType.ProcedureCodeInitialUEMessage:
		return a.onInitialUE(p, st, up)
	case ngapType.ProcedureCodeUplinkNASTransport:
		_, err := a.onUplinkNAS(p, st, up.nasPDU)
		return err
	default:
		return nil
	}
}

type ueState struct {
	ranID, amfID int64
	supi         string
	sec          *nas.SecurityContext
	ngksi        uint8
}

func (a *AMF) onInitialUE(p *pipe, st *ueState, up *uplinkMessage) error {
	st.ranID = up.ranID
	st.amfID = <-a.amfSeq

	supi, err := supiFromRegistration(up.nasPDU)
	if err != nil {
		return err
	}
	st.supi = supi

	// Network-side 5G-AKA: fresh RAND, AUTN over the shared credential.
	var randv [16]byte
	if _, err := rand.Read(randv[:]); err != nil {
		return err
	}
	_, _, _, autnSlice, err := milenage.GenerateAKAParameters(a.cfg.OPc, a.cfg.Ki, randv[:], a.cfg.SQN, a.cfg.AMF)
	if err != nil {
		return fmt.Errorf("mock amf AKA: %w", err)
	}
	var autn [16]byte
	copy(autn[:], autnSlice)

	// Derive the same key hierarchy the UE will, for later SMC/RegAccept.
	sub := auth.Subscription{SUPI: supi, Ki: a.cfg.Ki, OPc: a.cfg.OPc}
	vec, err := sub.DeriveFromChallenge(randv[:], autn[:], a.snn, nas.NIA2, nas.NEA0)
	if err != nil {
		return fmt.Errorf("mock amf key derivation: %w", err)
	}
	st.sec = &nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA0, KNASint: vec.KNASint, KNASenc: vec.KNASenc, Network: true}

	arBytes, err := buildAuthenticationRequest(randv, autn, st.ngksi)
	if err != nil {
		return err
	}
	return a.send(p, buildDownlinkNASTransport(st.amfID, st.ranID, arBytes))
}

func (a *AMF) onUplinkNAS(p *pipe, st *ueState, nasPDU []byte) (bool, error) {
	if len(nasPDU) < 2 {
		return false, fmt.Errorf("uplink NAS too short")
	}
	var msg *f5nas.Message
	var err error
	if f5nas.GetSecurityHeaderType(nasPDU) == f5nas.SecurityHeaderTypePlainNas {
		msg, err = nas.DecodePlain(nasPDU)
	} else {
		msg, err = st.sec.DecodeSecure(nasPDU)
	}
	if err != nil {
		return false, fmt.Errorf("mock amf uplink NAS decode: %w", err)
	}

	switch msg.GmmHeader.GetMessageType() {
	case f5nas.MsgTypeAuthenticationResponse:
		// Accept without checking RES* (sim only), send Security Mode Command.
		smc, err := buildSecurityModeCommand(nas.NIA2, nas.NEA0, st.ngksi, ue.SecurityCapability())
		if err != nil {
			return false, err
		}
		wrapped, err := st.sec.EncodeSecure(smc, nas.SecHdrIntegrityNewContext)
		if err != nil {
			return false, err
		}
		return false, a.send(p, buildDownlinkNASTransport(st.amfID, st.ranID, wrapped))
	case f5nas.MsgTypeSecurityModeComplete:
		// Send Initial Context Setup Request carrying Registration Accept.
		acc, err := buildRegistrationAccept()
		if err != nil {
			return false, err
		}
		wrapped, err := st.sec.EncodeSecure(acc, nas.SecHdrIntegrityCiphered)
		if err != nil {
			return false, err
		}
		return false, a.send(p, buildInitialContextSetupRequest(st.amfID, st.ranID, wrapped, a.plmn, a.cfg.SST, a.sdBytes))
	case f5nas.MsgTypeRegistrationComplete:
		return true, nil // UE is REGISTERED
	default:
		return false, nil
	}
}

func (a *AMF) send(p *pipe, pdu ngapType.NGAPPDU) error {
	b, err := encode(pdu)
	if err != nil {
		return err
	}
	return p.WriteNGAP(1, b)
}

// supiFromRegistration decodes the SUCI in a Registration Request and returns
// the SUPI in IMSI form (MCC+MNC+MSIN).
func supiFromRegistration(regReq []byte) (string, error) {
	m, err := nas.DecodePlain(regReq)
	if err != nil {
		return "", fmt.Errorf("decode Registration Request: %w", err)
	}
	if m.GmmMessage == nil || m.RegistrationRequest == nil {
		return "", fmt.Errorf("not a Registration Request")
	}
	suci, plmnID, err := nasConvert.SuciToStringWithError(m.RegistrationRequest.MobileIdentity5GS.Buffer)
	if err != nil {
		return "", fmt.Errorf("decode SUCI: %w", err)
	}
	// "suci-0-<mcc>-<mnc>-<rid>-<scheme>-<keyid>-<msin>"
	parts := strings.Split(suci, "-")
	if len(parts) < 8 {
		return "", fmt.Errorf("unexpected SUCI form %q", suci)
	}
	return plmnID + parts[len(parts)-1], nil
}
