// Package engine orchestrates ORBIT's UE and gNB roles into end-to-end
// procedures. attach.go drives the Phase-1a control-plane attach of a single
// UE against a live core: Registration → Authentication (5G-AKA) → Security
// Mode → Initial Context Setup → REGISTERED (TS 23.502 §4.2.2.2).
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// ueStream is the SCTP stream used for the single UE's associated signalling
// (non-UE-associated NG Setup used stream 0).
const ueStream uint16 = 1

// UEConfig is the subscription and identity ORBIT registers. When
// PDUSession is non-nil, the attach continues past REGISTERED to establish
// one PDU session (control-plane signalling and IP allocation; no user-plane
// bytes until Phase 1b).
type UEConfig struct {
	Identity ue.Identity
	Sub      auth.Subscription
	// PDUSession is a single session to establish (the API / single-session
	// path). PDUSessions, if non-empty, takes precedence and establishes
	// several sessions — each with its own DNN and S-NSSAI.
	PDUSession  *ue.PDUSessionParams
	PDUSessions []ue.PDUSessionParams
	// GNBN3Addr is the gNB N3 transport address reported to the AMF in the
	// PDU Session Resource Setup Response and bound for the data path.
	// Must be reachable from the UPF access-net; empty disables the data
	// path (control-plane only).
	GNBN3Addr string
	// RANUENGAPID is the gNB's identifier for this UE, unique per gNB
	// association (TS 38.413 §9.3.3.2). It keys the demux when many UEs
	// share one association; 0 defaults to 1 (single-UE case).
	RANUENGAPID int64
	// RequestedNSSAI is the set of slices the UE requests at registration
	// (TS 24.501 §9.11.3.37). If empty, it defaults to the PDU session's
	// slice so a slice-aware core routes correctly; if there is no session
	// either, none is sent and the AMF assigns the subscribed slice.
	RequestedNSSAI []ue.SNSSAI
}

// AttachResult reports the outcome of a control-plane attach.
type AttachResult struct {
	Registered  bool
	SUPI        string
	AMFUENGAPID int64
	RANUENGAPID int64
	// SessionActive is true when at least one PDU session was established.
	SessionActive bool
	// Sessions holds one entry per established PDU session.
	Sessions []SessionResult
	// The following mirror Sessions[0] for the single-session callers
	// (Ping, the API). PDUAddress is the allocated UE IP; UPFAddress/UPFTEID
	// the UPF uplink N3 endpoint; DLTEID the gNB downlink TEID; QFI the flow.
	PDUAddress string
	UPFAddress string
	UPFTEID    uint32
	DLTEID     uint32
	QFI        uint8
}

// SessionResult is one established PDU session's endpoints.
type SessionResult struct {
	PDUSessionID uint8
	DNN          string
	Slice        ue.SNSSAI
	IPv4         string
	UPFAddress   string
	UPFTEID      uint32
	DLTEID       uint32
	QFI          uint8
}

// Attach registers one UE over an already-NG-Setup association. It runs the
// NAS-MM and NGAP procedure sequence to REGISTERED, wrapping NAS in the
// security context once Security Mode Command activates it, then (if
// requested) establishes one PDU session. emit, if non-nil, receives a
// StateEvent at each transition. The returned Session holds the live
// association and security context for later Status/Deregister.
func Attach(ctx context.Context, conn gnb.Transport, gnbCfg gnb.Config, ueCfg UEConfig, log *slog.Logger, emit func(StateEvent)) (*Session, error) {
	ranID := ueCfg.RANUENGAPID
	if ranID == 0 {
		ranID = 1
	}
	snn := auth.ServingNetworkName(gnbCfg.MCC, gnbCfg.MNC)

	suci, err := ueCfg.Identity.EncodeNullSUCI()
	if err != nil {
		return nil, fmt.Errorf("encode SUCI: %w", err)
	}
	requested, err := ue.BuildRequestedNSSAI(requestedSlices(ueCfg))
	if err != nil {
		return nil, fmt.Errorf("build Requested NSSAI: %w", err)
	}
	regReq, err := ue.BuildRegistrationRequest(suci, ue.SecurityCapability(), requested)
	if err != nil {
		return nil, fmt.Errorf("build Registration Request: %w", err)
	}

	st := &attachState{
		ctx: ctx, conn: conn, gnbCfg: gnbCfg, ueCfg: ueCfg,
		snn: snn, ranID: ranID, regReq: regReq, log: log, emit: emit,
		wanted: sessionsToEstablish(ueCfg), sessions: map[uint8]SessionResult{},
	}

	initial, err := gnb.BuildInitialUEMessage(gnbCfg, ranID, regReq)
	if err != nil {
		return nil, err
	}
	if err := gnb.SendPDU(conn, ueStream, initial); err != nil {
		return nil, fmt.Errorf("send Initial UE Message: %w", err)
	}
	log.InfoContext(ctx, "sent Initial UE Message with Registration Request", "supi", ueCfg.Sub.SUPI)
	st.event(StateRegistering, "sent Registration Request")

	// The loop runs until the target state: REGISTERED, or (if a PDU session
	// is requested) session-active. The PDU Session Establishment Request is
	// triggered once REGISTERED (see onRegistrationAccept).
	for !st.done() {
		pdu, err := gnb.ReadPDU(ctx, conn)
		if err != nil {
			return nil, err
		}
		if handled, err := st.handlePDUSession(pdu); err != nil {
			return nil, err
		} else if handled {
			continue
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

	return &Session{
		SUPI:   ueCfg.Sub.SUPI,
		gnbCfg: gnbCfg,
		conn:   conn,
		sec:    st.sec,
		amfID:  st.amfID,
		ranID:  ranID,
		guti:   st.guti,
		Result: st.result(ueCfg.Sub.SUPI, ranID),
	}, nil
}

// result assembles the AttachResult, mirroring the first session into the
// single-session fields for backward-compatible callers.
func (s *attachState) result(supi string, ranID int64) *AttachResult {
	r := &AttachResult{
		Registered:    true,
		SUPI:          supi,
		AMFUENGAPID:   s.amfID,
		RANUENGAPID:   ranID,
		SessionActive: len(s.sessions) > 0,
	}
	for _, w := range s.wanted { // stable order = requested order
		if sr, ok := s.sessions[w.PDUSessionID]; ok {
			r.Sessions = append(r.Sessions, sr)
		}
	}
	if len(r.Sessions) > 0 {
		f := r.Sessions[0]
		r.PDUAddress, r.UPFAddress, r.UPFTEID, r.DLTEID, r.QFI = f.IPv4, f.UPFAddress, f.UPFTEID, f.DLTEID, f.QFI
	}
	return r
}

type attachState struct {
	ctx    context.Context
	conn   gnb.Transport
	gnbCfg gnb.Config
	ueCfg  UEConfig
	snn    string
	ranID  int64
	regReq []byte
	log    *slog.Logger
	emit   func(StateEvent)

	amfID      int64
	vec        *auth.Vector
	sec        *nas.SecurityContext
	guti       []byte
	registered bool

	wanted   []ue.PDUSessionParams   // sessions to establish
	sessions map[uint8]SessionResult // established, by PDU session ID
}

// event publishes a state transition to the emitter, if any.
func (s *attachState) event(state, detail string) {
	if s.emit != nil {
		s.emit(StateEvent{SUPI: s.ueCfg.Sub.SUPI, State: state, Detail: detail})
	}
}

// done reports whether the attach has reached its target state: all
// requested PDU sessions established, or just REGISTERED if none requested.
func (s *attachState) done() bool {
	if len(s.wanted) > 0 {
		return len(s.sessions) >= len(s.wanted)
	}
	return s.registered
}

// handlePDUSession processes a PDU Session Resource Setup Request: it assigns
// each session a distinct gNB downlink TEID, acks with the response, extracts
// the allocated UE IP from the embedded Establishment Accept, and records the
// UPF endpoint. One message may carry several sessions. Returns true if it
// consumed the PDU.
func (s *attachState) handlePDUSession(pdu *ngapType.NGAPPDU) (bool, error) {
	amfID, ranID, res, err := gnb.ParsePDUSessionResourceSetupRequest(pdu)
	if err != nil {
		return false, nil // not a PDU Session Resource Setup Request
	}
	s.amfID = amfID

	// Assign a downlink TEID per session (stable if the same session recurs
	// within the message), drawn from the process-wide allocator so every UE
	// on a gNB gets a distinct demux key.
	teids := map[int64]uint32{}
	teidFor := func(id int64) uint32 {
		if t, ok := teids[id]; ok {
			return t
		}
		t := allocDLTEID()
		teids[id] = t
		return t
	}
	resp, err := gnb.BuildPDUSessionResourceSetupResponse(amfID, ranID, res, s.gnbN3Addr(), teidFor)
	if err != nil {
		return true, err
	}
	if err := gnb.SendPDU(s.conn, ueStream, resp); err != nil {
		return true, fmt.Errorf("send PDU Session Resource Setup Response: %w", err)
	}

	for _, r := range res {
		sr := SessionResult{
			PDUSessionID: uint8(r.PDUSessionID),
			UPFAddress:   r.UPFAddress,
			UPFTEID:      r.UPFTEID,
			DLTEID:       teids[r.PDUSessionID],
		}
		if p := s.wantedByID(uint8(r.PDUSessionID)); p != nil {
			sr.DNN, sr.Slice = p.DNN, p.Slice()
		}
		if len(r.QFIs) > 0 {
			sr.QFI = uint8(r.QFIs[0])
		}
		if len(r.NASPDU) > 0 {
			if ip, err := s.extractPDUAddress(r.NASPDU); err != nil {
				s.log.WarnContext(s.ctx, "could not extract UE IP from PDU Session Establishment Accept", "err", err)
			} else {
				sr.IPv4 = ip
			}
		}
		s.sessions[sr.PDUSessionID] = sr
		s.log.InfoContext(s.ctx, "PDU session established",
			"pdu_session_id", sr.PDUSessionID, "ue_ip", sr.IPv4, "dnn", sr.DNN, "upf", sr.UPFAddress)
		s.event(StateSessionActive, fmt.Sprintf("PDU session %d active, UE IP %s", sr.PDUSessionID, sr.IPv4))
	}
	return true, nil
}

func (s *attachState) wantedByID(id uint8) *ue.PDUSessionParams {
	for i := range s.wanted {
		if s.wanted[i].PDUSessionID == id {
			return &s.wanted[i]
		}
	}
	return nil
}

// extractPDUAddress decrypts the security-protected DL NAS Transport that
// the AMF puts in the PDU session's NAS PDU, pulls out the N1 SM container
// (the 5GSM Establishment Accept), and returns the allocated UE IP.
func (s *attachState) extractPDUAddress(nasPDU []byte) (string, error) {
	if s.sec == nil {
		return "", fmt.Errorf("no security context")
	}
	msg, err := s.sec.DecodeSecure(nasPDU)
	if err != nil {
		return "", fmt.Errorf("decode secured DL NAS Transport: %w", err)
	}
	sm, err := ue.ExtractN1SMContainer(msg)
	if err != nil {
		return "", err
	}
	acc, err := ue.ParsePDUSessionEstablishmentAccept(sm)
	if err != nil {
		return "", err
	}
	return acc.IPv4, nil
}

func (s *attachState) gnbN3Addr() string {
	if s.ueCfg.GNBN3Addr != "" {
		return s.ueCfg.GNBN3Addr
	}
	return "127.0.0.1" // Phase 1a records the endpoint; Phase 1b wires it
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
	s.event(StateAuthenticated, "network authenticated, sent Authentication Response")
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
	s.event(StateSecurityEstablished, "NAS security context established")
	return nil
}

func (s *attachState) onRegistrationAccept(msg *f5nas.Message) error {
	if guti, err := ue.ParseRegistrationAccept(msg); err != nil {
		s.log.WarnContext(s.ctx, "could not parse Registration Accept", "err", err)
	} else if guti != nil {
		s.guti = guti.Raw
	}

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
	s.event(StateRegistered, "UE is 5GMM-REGISTERED")

	// With PDU sessions requested, kick off establishment now that the UE is
	// REGISTERED (TS 23.502 §4.3.2.2) — one request per session.
	for _, p := range s.wanted {
		if err := s.requestPDUSession(p); err != nil {
			return err
		}
	}
	return nil
}

// requestPDUSession sends the 5GSM PDU Session Establishment Request wrapped
// in a secured 5GMM UL NAS Transport (TS 24.501 §8.2.10, §8.3.1).
func (s *attachState) requestPDUSession(p ue.PDUSessionParams) error {
	sm, err := ue.BuildPDUSessionEstablishmentRequest(p)
	if err != nil {
		return err
	}
	transport, err := ue.BuildULNASTransportForPDUSession(sm, p)
	if err != nil {
		return err
	}
	wrapped, err := s.sec.EncodeSecure(transport, nas.SecHdrIntegrityCiphered)
	if err != nil {
		return fmt.Errorf("wrap PDU session UL NAS Transport: %w", err)
	}
	if err := s.sendUL(wrapped); err != nil {
		return err
	}
	s.log.InfoContext(s.ctx, "sent PDU Session Establishment Request",
		"pdu_session_id", p.PDUSessionID, "dnn", p.DNN)
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

// dlTEIDSeq hands out gNB downlink TEIDs. It is process-wide (one counter
// across every attach, handover, and fleet path switch), because the DL TEID
// is the key of the per-gNB shared Demux (design §6): two UEs on one gNB —
// or two UEs handed over to one target gNB — with the same DL TEID would
// collide on the demux AND be indistinguishable to the UPF on the wire. A
// per-attach sequence (the old shape: every UE started at 1) had exactly
// that defect. Process-wide uniqueness is stronger than the per-gNB minimum,
// and trivially safe.
var dlTEIDSeq atomic.Uint32

// allocDLTEID returns the next gNB downlink TEID (starting at 1; 0 is the
// reserved "no TEID" value in GTP-U signalling here).
func allocDLTEID() uint32 { return dlTEIDSeq.Add(1) }

// sessionsToEstablish is the list of PDU sessions to bring up: the explicit
// multi-session list if set, else the single PDUSession (API/single-session
// callers), else none.
func sessionsToEstablish(cfg UEConfig) []ue.PDUSessionParams {
	if len(cfg.PDUSessions) > 0 {
		return cfg.PDUSessions
	}
	if cfg.PDUSession != nil {
		return []ue.PDUSessionParams{*cfg.PDUSession}
	}
	return nil
}

// requestedSlices returns the slices to advertise in the Registration
// Request: the explicit set if given, else the union of the sessions' slices.
func requestedSlices(cfg UEConfig) []ue.SNSSAI {
	if len(cfg.RequestedNSSAI) > 0 {
		return cfg.RequestedNSSAI
	}
	var out []ue.SNSSAI
	seen := map[ue.SNSSAI]bool{}
	for _, p := range sessionsToEstablish(cfg) {
		if sl := p.Slice(); !seen[sl] {
			seen[sl] = true
			out = append(out, sl)
		}
	}
	return out
}
