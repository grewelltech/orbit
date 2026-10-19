package mockamf

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	// UPF, when set, makes the mock establish PDU sessions (answering a UE's
	// PDU Session Establishment Request with a PDU Session Resource Setup
	// Request) and report sessions and path switches to it. Nil keeps the
	// mock's registration-only behaviour: PDU session requests go
	// unanswered. A PathSwitchRequest is always answered: with an
	// Acknowledge when at least one listed session is known, else with a
	// PathSwitchRequestFailure (always the case without a UPF).
	UPF UPF
	// UPFAddress is the IPv4 the mock advertises as the UPF's uplink N3
	// address; required with UPF.
	UPFAddress string
	// ReallocateULOnPathSwitch makes the PathSwitchRequestAcknowledge carry
	// a new UPF uplink TEID for each switched session, the re-anchoring case
	// of TS 38.413 §9.3.4.14; by default the uplink tunnel is kept.
	ReallocateULOnPathSwitch bool
	// Log receives the mock's own errors that do not end an association,
	// such as a downlink PDU that fails to encode. Nil discards them.
	Log *slog.Logger
}

// AMF is an in-process mock AMF for sim-capacity measurement.
type AMF struct {
	cfg     Config
	plmn    [3]byte
	sdBytes []byte
	snn     string
	amfSeq  chan int64 // hands out AMF-UE-NGAP-IDs

	// mu guards ues and the per-UE session maps: a UE registers on one
	// association and its path switch arrives on another.
	mu        sync.Mutex
	ues       map[int64]*ueState // by AMF-UE-NGAP-ID
	ueIPSeq   atomic.Uint32
	ulTEIDSeq atomic.Uint32
	// encodeErrs counts downlink PDUs that failed to encode and were
	// dropped instead of ending the association.
	encodeErrs atomic.Int64
	log        *slog.Logger

	// Options (t3560.go): T3560 retransmission and release, a crash
	// after the release, an Authentication Reject, or no challenge.
	t3560             time.Duration
	t3560Retries      int
	t3560NoRelease    bool
	crashAfterRelease bool
	authReject        bool
	noAuthRequest     bool
	crashed           atomic.Bool // set once a crashAfterRelease release went out
}

// errCrashed ends every association of a mock that crashed after a release
// (WithCrashAfterRelease).
var errCrashed = errors.New("mockamf: crashed after the T3560 release")

// New validates config and returns a mock AMF, adjusted by opts.
func New(cfg Config, opts ...Option) (*AMF, error) {
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
	if cfg.UPF != nil {
		if ip := net.ParseIP(cfg.UPFAddress); ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("UPFAddress %q must be an IPv4 address when a UPF is configured", cfg.UPFAddress)
		}
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	a := &AMF{cfg: cfg, plmn: plmn, sdBytes: sd, snn: auth.ServingNetworkName(cfg.MCC, cfg.MNC), amfSeq: make(chan int64),
		ues: make(map[int64]*ueState), log: log}
	for _, o := range opts {
		o(a)
	}
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
		if a.crashed.Load() {
			return errCrashed // WithCrashAfterRelease: the core is gone
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
	if pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome &&
		pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentPDUSessionResourceSetupResponse {
		return a.onPDUSessionSetupResponse(pdu.SuccessfulOutcome.Value.PDUSessionResourceSetupResponse)
	}
	if pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage {
		return nil // Initial Context Setup Response etc. — no action
	}
	if pdu.InitiatingMessage.Value.Present == ngapType.InitiatingMessagePresentPathSwitchRequest {
		return a.onPathSwitchRequest(p, states, pdu.InitiatingMessage.Value.PathSwitchRequest)
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

// ueState is one UE's state. ranID and sessions are guarded by AMF.mu, since
// a path switch reaches them from the target association's goroutine. sec,
// the NAS security context, is owned by whichever association currently
// carries the UE's NAS: the source until the path switch, the target after
// it. The hand-over is ordered through the gNB (the source sends no NAS
// after it asks for the switch), so sec is never used from two goroutines at
// once; a change that lets both associations send NAS must guard it.
type ueState struct {
	ranID, amfID int64
	supi         string
	sec          *nas.SecurityContext
	ngksi        uint8
	sessions     map[uint8]*pduSession // guarded by AMF.mu
	t3560        *t3560Timer           // running T3560 (WithT3560), nil otherwise
}

func (a *AMF) onInitialUE(p *pipe, st *ueState, up *uplinkMessage) error {
	st.ranID = up.ranID
	st.amfID = <-a.amfSeq
	a.mu.Lock()
	a.ues[st.amfID] = st
	a.mu.Unlock()

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

	if a.noAuthRequest {
		return nil
	}
	if a.authReject {
		rej, err := buildAuthenticationReject()
		if err != nil {
			return err
		}
		return a.send(p, buildDownlinkNASTransport(st.amfID, st.ranID, rej))
	}
	arBytes, err := buildAuthenticationRequest(randv, autn, st.ngksi)
	if err != nil {
		return err
	}
	if err := a.send(p, buildDownlinkNASTransport(st.amfID, st.ranID, arBytes)); err != nil {
		return err
	}
	if a.t3560 > 0 {
		st.t3560 = a.startT3560(p, st, arBytes)
	}
	return nil
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
		st.t3560.stop()
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
	case f5nas.MsgTypeULNASTransport:
		return false, a.onPDUSessionRequest(p, st, msg)
	default:
		return false, nil
	}
}

// send encodes and writes one downlink PDU. A PDU that fails to encode is
// the mock's own bug for that one message: it is logged, counted and
// dropped, and the association carries on, so no single malformed answer
// can end it. A write error still ends the association (the peer is gone).
func (a *AMF) send(p *pipe, pdu ngapType.NGAPPDU) error {
	b, err := encode(pdu)
	if err != nil {
		a.encodeErrs.Add(1)
		a.log.Error("mock amf: dropping a downlink PDU that does not encode", "present", pdu.Present, "err", err)
		return nil
	}
	return p.WriteNGAP(1, b)
}

// EncodeErrors reports how many downlink PDUs failed to encode and were
// dropped; tests use it to assert the mock never produced one.
func (a *AMF) EncodeErrors() int64 { return a.encodeErrs.Load() }

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
