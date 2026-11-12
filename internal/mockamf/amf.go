package mockamf

import (
	"context"
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
	"github.com/free5gc/nas/nasType"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ngap"
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
	// Request) and report sessions, path switches and N2 handovers to it. Nil keeps the
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
	// AcceptUnknownPLMN makes the mock accept an NG Setup whose Supported
	// TA List names none of its PLMN, where by default it rejects it with
	// NG SETUP FAILURE, cause Misc unknown-PLMN (TS 38.413 8.7.1.4). It
	// stands in for a core that does not check, so the conformance check's
	// FAIL path can be exercised.
	AcceptUnknownPLMN bool
	// ExtraPLMNs are further PLMNs the mock serves alongside MCC/MNC: NG
	// Setup accepts them and the NG Setup Response lists them in its PLMN
	// Support List. UEs still register under MCC/MNC.
	ExtraPLMNs []PLMN
	// Log receives the mock's own errors that do not end an association,
	// such as a downlink PDU that fails to encode. Nil discards them.
	Log *slog.Logger
}

// PLMN is a PLMN identity, MCC and MNC as digit strings.
type PLMN struct{ MCC, MNC string }

// AMF is an in-process mock AMF for sim-capacity measurement.
type AMF struct {
	cfg     Config
	plmn    [3]byte
	served  [][3]byte // plmn, then ExtraPLMNs
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
	// deregs counts UE-originating Deregistration Requests received.
	deregs atomic.Int64
	// releaseCompletes counts UE CONTEXT RELEASE COMPLETEs received;
	// completeIDs (under mu) are the NGAP ID pairs they named, and
	// deregReleases the pairs of the releases sent for a switch-off.
	releaseCompletes atomic.Int64
	completeIDs      [][2]int64
	deregReleases    [][2]int64
	// strayCompletes counts Completes that came before any switch-off
	// release, release on request or release of a cancelled handover's
	// target named their UE.
	strayCompletes atomic.Int64
	// setups are the gNBs that sent an NG Setup Request, in order (Setups).
	setups []Setup
	log    *slog.Logger

	// Options (nastimers.go): T3560 retransmission and release, a crash
	// after the release, an Authentication Reject, no challenge, T3550
	// retransmission, a 5G-GUTI, and a crash after the T3550 abort.
	t3560             time.Duration
	t3560Retries      int
	t3560NoRelease    bool
	t3560Reject       bool
	t3560ReleaseAfter time.Duration
	crashAfterRelease bool
	authReject        bool
	noAuthRequest     bool
	t3550             time.Duration
	t3550Retries      int
	assignGUTI        bool
	crashAfterT3550   bool
	t3550Protection   T3550Protection
	acceptCount       uint32
	acceptCountSet    bool
	crashed           atomic.Bool // set once a crashAfterRelease release, a crashAfterT3550 abort or a CrashOnUndecodable PDU happened

	// Security Mode Command options (smc.go).
	integrityOrder  []uint8
	noCommon        NoCommonIntegrity
	forceIntegrity  bool
	forcedIntegrity uint8
	tamperCaps      bool
	skipSMC         bool
	cipheringOrder  []uint8
	forceCiphering  bool
	forcedCiphering uint8

	// Accept options (accept.go).
	undecodableAccept bool

	// Deregistration options (release.go): no Deregistration Accept for a
	// normal deregistration, and how many were sent.
	noDeregAccept bool
	deregAccepts  atomic.Int64

	// UE context release options (release.go).
	noReleaseOnDereg     bool
	silent               bool // answers NG Setup and nothing else (WithSilentAfterNGSetup)
	deregReleaseIDs      ReleaseIDs
	noiseBeforeRelease   bool
	releaseDelay         time.Duration
	staleRelease         time.Duration
	ignoreReleaseRequest bool
	// crashOnReleaseRequest is WithCrashOnUEContextReleaseRequest
	// (drop.go).
	crashOnReleaseRequest bool
	// releaseRequests counts the UE CONTEXT RELEASE REQUESTs received,
	// requestReleases (under mu) are the NGAP ID pairs of the releases sent
	// to answer them, and idleDeregs counts the Deregistration Requests
	// that came in an Initial UE Message.
	releaseRequests atomic.Int64
	requestReleases [][2]int64
	idleDeregs      atomic.Int64

	// PDU session release options and counters (pdurelease.go).
	pduReleaseAnswer        PDUReleaseAnswer
	pduReleaseKeepsUP       bool
	pduReleaseReqs          atomic.Int64
	pduReleaseCompletes     atomic.Int64
	pduResourceReleaseResps atomic.Int64
	// CM-IDLE options and state (idle.go).
	noPaging        bool
	pagingTMSIXor   uint32
	serviceAnswer   ServiceAnswer
	serviceCause    uint8
	pagings         atomic.Int64
	serviceRequests atomic.Int64

	// Unknown-DNN options (dnn.go).
	unknownDNN string
	dnnAnswer  DNNAnswer
	dnnCause   uint8

	// Unserved-NSSAI answer (nssai.go).
	nssaiAnswer NSSAIAnswer

	// Registration Reject cause (registration.go); zero accepts.
	registrationReject uint8

	// Path switch answer (pathswitch.go).
	pathSwitchAnswer PathSwitchAnswer

	// assocs are the live associations that completed NG Setup, guarded by
	// mu: paging goes out on each (idle.go), and an N2 handover finds its
	// target among them by gNB ID (handover.go). assocSeq orders them.
	assocs   map[*pipe]association
	assocSeq uint64

	// N2 handover (handover.go). handovers are those in preparation, by
	// AMF-UE-NGAP-ID, guarded by mu.
	handoverAnswer HandoverAnswer
	handovers      map[int64]*handover
	handoversDone  atomic.Int64
	// Uplink NAS security options (uplinksec.go). badMACs counts protected
	// uplinks that failed the integrity check, replays those that repeated
	// an accepted NAS COUNT, smReleases the PDU Session Release Requests
	// received.
	badMAC   UplinkAction
	badMACs  atomic.Int64
	replayed UplinkAction
	replays  atomic.Int64
	// badMACReg, countBeforeMAC, laxEstimate and the PDU session answer
	// limit are WithBadMACRegistration, WithUplinkCountBeforeMAC,
	// WithLaxUplinkCountEstimate and WithPDUSessionAnswers.
	badMACReg      BadMACRegistration
	countBeforeMAC bool
	laxEstimate    bool
	smAnswers      int
	smLimited      bool
	smReleases     atomic.Int64
	// noCancelAck withholds the HANDOVER CANCEL ACKNOWLEDGE and
	// crashOnCancel stops the mock on a HANDOVER CANCEL; handoverCancels
	// counts the HANDOVER CANCELs received; handoverReleases (under mu) are
	// the NGAP ID pairs of the releases sent to a cancelled handover's
	// target.
	noCancelAck      bool
	crashOnCancel    bool
	handoverCancels  atomic.Int64
	handoverReleases [][2]int64

	// Initial NAS options (initialnas.go): unprotectedWins,
	// missingContainer and missingIMEISV are
	// WithUnprotectedRegistrationWins, WithMissingContainer and
	// WithMissingIMEISV; smcsSent counts the Security Mode Commands sent.
	unprotectedWins  bool
	missingContainer MissingContainerAnswer
	missingIMEISV    MissingIMEISVAnswer
	smcsSent         atomic.Int64

	// regReqs records every Registration Request received, guarded by mu
	// (RegistrationRequests); initialRegReqs the same requests as the
	// Initial UE Messages carried them (InitialRegistrationRequests).
	regReqs        [][]byte
	initialRegReqs [][]byte
	// regSUPIs are those requests' SUPIs, guarded by mu (RegistrationSUPIs).
	regSUPIs []string

	// RAN Configuration Update (ranconfig.go): the answer, and the updates
	// received, guarded by mu.
	ranConfigAnswer  RANConfigUpdateAnswer
	ranConfigUpdates []RANConfigUpdate
	// dropTAs and dropTAsCause are WithRANConfigUpdateDroppingTAs;
	// droppedTAs, guarded by mu, holds the associations it has applied to.
	dropTAs      bool
	dropTAsCause uint8
	droppedTAs   map[*pipe]bool
	// duplicateRANNode is WithDuplicateRANNodeID; assocs holds the gNB ID
	// each association that completed NG Setup presented.
	duplicateRANNode DuplicateRANNodeAnswer

	// undecodable is the reaction to an uplink PDU that does not decode
	// (undecodable.go).
	undecodable UndecodableAnswer

	// Initial NAS message options (initialnas.go).
	rejectNonCleartext bool

	// Configuration update option and what it drew, and the identities of
	// the Deregistration Requests, guarded by mu (configupdate.go).
	configUpdate          *ConfigurationUpdate
	configUpdateCompletes atomic.Int64
	deregIdentities       [][]byte

	// Path switch UE security capability check (pathswitchcaps.go).
	capsInAck      bool
	capsEchoed     bool
	capsMismatches atomic.Int64

	// Registration after release (reregistration.go): the window in which
	// a released SUPI's next Registration Request is dropped, and the one
	// in which any Registration Request is after any release; when each
	// SUPI's UE context, and any UE context, was last released (guarded by
	// mu), how many requests were dropped, and whether a dropped one
	// crashes the mock.
	dropAfterRelease    time.Duration
	dropAfterAnyRelease time.Duration
	crashOnDrop         bool
	releasedAt          map[string]time.Time
	lastRelease         time.Time
	pendingReleases     int // deregistration releases sent whose RELEASE COMPLETE has not come
	// sessionsUpCh is closed when a PDU Session Resource Setup Response is
	// recorded, waking a path switch waiting for one (awaitSessionsUp);
	// guarded by mu, nil while no one waits.
	sessionsUpCh chan struct{}
	droppedRegs  atomic.Int64

	// Identification options and what the UEs answered, the answers
	// guarded by mu (identity.go).
	requestIMEISV     bool
	identityAfterSMC  bool
	identityType      uint8
	plainIdentity     bool
	plainIdentityType uint8
	smcIMEISVs        [][]byte
	identityResponses [][]byte
}

// errCrashed ends every association of a mock that crashed: after a release
// (WithCrashAfterRelease), a T3550 abort (WithCrashAfterT3550Abort), an
// undecodable PDU (CrashOnUndecodable) or a HANDOVER CANCEL
// (WithCrashOnHandoverCancel), among others.
var errCrashed = errors.New("mockamf: crashed")

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
	served := [][3]byte{plmn}
	for _, p := range cfg.ExtraPLMNs {
		extra, err := ngap.EncodePLMN(p.MCC, p.MNC)
		if err != nil {
			return nil, fmt.Errorf("extra PLMN: %w", err)
		}
		served = append(served, extra)
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
	a := &AMF{cfg: cfg, plmn: plmn, served: served, sdBytes: sd, snn: auth.ServingNetworkName(cfg.MCC, cfg.MNC), amfSeq: make(chan int64),
		ues: make(map[int64]*ueState), releasedAt: make(map[string]time.Time), log: log, droppedTAs: make(map[*pipe]bool), assocs: make(map[*pipe]association), handovers: make(map[int64]*handover)}
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
	defer func() {
		for _, st := range states {
			st.t3560.stop()
			st.t3550.stop()
		}
		a.forgetAssociation(p)
	}()
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
			return errCrashed // the core is gone
		}
		pdu, err := ngap.Decode(raw)
		if err != nil {
			if a.silent {
				continue
			}
			if err := a.onUndecodable(p, fmt.Errorf("mock amf decode: %w", err)); err != nil {
				return err
			}
			continue
		}
		if err := a.dispatch(p, states, pdu); err != nil {
			return err
		}
	}
}

// dispatch routes one gNB→AMF PDU: NG Setup is answered once, a RAN
// Configuration Update as configured; UE-associated messages go to their
// per-RAN-UE-NGAP-ID state.
func (a *AMF) dispatch(p *pipe, states map[int64]*ueState, pdu *ngapType.NGAPPDU) error {
	if pdu.Present == ngapType.NGAPPDUPresentInitiatingMessage &&
		pdu.InitiatingMessage.Value.Present == ngapType.InitiatingMessagePresentNGSetupRequest {
		setup := setupOf(pdu.InitiatingMessage.Value.NGSetupRequest)
		a.mu.Lock()
		a.setups = append(a.setups, setup)
		a.mu.Unlock()
		if !a.cfg.AcceptUnknownPLMN && !servesAnyTAPLMN(pdu.InitiatingMessage.Value.NGSetupRequest, a.served) {
			return a.send(p, buildNGSetupFailureUnknownPLMN())
		}
		if answered, err := a.onRANNode(p, setup.GNBID); answered {
			return err
		}
		resp, err := buildNGSetupResponse(a.served, a.cfg.SST, a.sdBytes)
		if err != nil {
			return err
		}
		a.mu.Lock()
		a.assocSeq++
		a.assocs[p] = association{gnbID: setup.GNBID, tac: firstTAC(pdu.InitiatingMessage.Value.NGSetupRequest), seq: a.assocSeq}
		a.mu.Unlock()
		return a.send(p, resp)
	}
	if a.silent {
		return nil
	}
	if pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome &&
		pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentInitialContextSetupResponse {
		a.onInitialContextSetupResponse(pdu.SuccessfulOutcome.Value.InitialContextSetupResponse)
		return nil
	}
	if pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome &&
		pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentHandoverRequestAcknowledge {
		return a.onHandoverRequestAcknowledge(p, pdu.SuccessfulOutcome.Value.HandoverRequestAcknowledge)
	}
	if pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome &&
		pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentPDUSessionResourceSetupResponse {
		return a.onPDUSessionSetupResponse(pdu.SuccessfulOutcome.Value.PDUSessionResourceSetupResponse)
	}
	if pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome &&
		pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentUEContextReleaseComplete {
		ids := releaseCompleteIDs(pdu.SuccessfulOutcome.Value.UEContextReleaseComplete)
		a.mu.Lock()
		a.completeIDs = append(a.completeIDs, ids)
		answered := false
		for _, r := range a.deregReleases {
			answered = answered || r == ids
		}
		for _, r := range a.requestReleases {
			answered = answered || r == ids
		}
		for _, r := range a.handoverReleases {
			answered = answered || r == ids
		}
		a.mu.Unlock()
		if !answered {
			a.strayCompletes.Add(1)
		}
		a.releaseCompletes.Add(1)
		a.noteReleased(ids[0])
		a.onANReleaseComplete(states, ids)
		return nil
	}
	if pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome &&
		pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentPDUSessionResourceReleaseResponse {
		a.pduResourceReleaseResps.Add(1)
		return nil
	}
	if pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage {
		return nil // Initial Context Setup Response etc. — no action
	}
	switch pdu.InitiatingMessage.Value.Present {
	case ngapType.InitiatingMessagePresentPathSwitchRequest:
		return a.onPathSwitchRequest(p, states, pdu.InitiatingMessage.Value.PathSwitchRequest)
	case ngapType.InitiatingMessagePresentHandoverRequired:
		return a.onHandoverRequired(p, pdu.InitiatingMessage.Value.HandoverRequired)
	case ngapType.InitiatingMessagePresentHandoverNotify:
		return a.onHandoverNotify(p, states, pdu.InitiatingMessage.Value.HandoverNotify)
	case ngapType.InitiatingMessagePresentHandoverCancel:
		return a.onHandoverCancel(p, pdu.InitiatingMessage.Value.HandoverCancel)
	}
	if pdu.InitiatingMessage.Value.Present == ngapType.InitiatingMessagePresentRANConfigurationUpdate {
		return a.onRANConfigUpdate(p, pdu.InitiatingMessage.Value.RANConfigurationUpdate)
	}
	up, err := parseUplink(pdu)
	if err != nil {
		return err
	}
	if up.procedure == ngapType.ProcedureCodeInitialUEMessage && len(up.nasPDU) >= 2 &&
		f5nas.GetSecurityHeaderType(up.nasPDU) != f5nas.SecurityHeaderTypePlainNas {
		// A protected initial NAS message: a CM-IDLE UE coming back.
		return a.onIdleInitialUE(p, states, up)
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
	case ngapType.ProcedureCodeUEContextReleaseRequest:
		return a.onUEContextReleaseRequest(p, st)
	default:
		return nil
	}
}

// ueState is one UE's state, reached from the source association's
// goroutine and, after a path switch, the target's. ranID and sessions are
// guarded by AMF.mu, sec by secMu (see the fields).
type ueState struct {
	// amfID and supi are set before the UE is published in AMF.ues, and
	// never change. ranID is guarded by AMF.mu: a path switch moves the
	// UE to its target's association while the source's goroutine may
	// still answer for it (AMF.ranIDOf).
	ranID, amfID int64
	supi         string
	// sec is the NAS security context; secMu guards it and its counts,
	// since after a path switch two associations hold the UE. Never held
	// across a send.
	secMu    sync.Mutex
	sec      *nas.SecurityContext
	ngksi    uint8
	sessions map[uint8]*pduSession        // guarded by AMF.mu
	t3560    *retransmitter               // running T3560 (WithT3560), nil otherwise
	t3550    *retransmitter               // running T3550 (WithT3550), nil otherwise
	ueCaps   nasType.UESecurityCapability // from the Registration Request, replayed in the SMC
	kamf     []byte                       // KAMF from authentication, for the SMC's keys
	// releasing is set once the mock answered the UE's UE CONTEXT RELEASE
	// REQUEST, and idle once that release completed (CM-IDLE); both are
	// guarded by AMF.mu.
	releasing, idle bool
	requested       []byte // the Requested NSSAI IE's value, nil when absent
	muted           bool   // MuteOnUplink: every NAS message is dropped
	smSeen          int    // PDU Session Establishment Requests received (WithPDUSessionAnswers)
	regIdx          int    // the UE's entry in AMF.regReqs
	containerAsked  bool   // a second Security Mode Command went out for a missing container
	// awaitingIdentity marks a WithIdentityRequest request not yet answered.
	awaitingIdentity bool
	// ranCaps is the UE Security Capabilities IE the mock last gave the RAN
	// for the UE, guarded by AMF.mu; nil before the first.
	ranCaps *ngapType.UESecurityCapabilities
}

func (a *AMF) onInitialUE(p *pipe, st *ueState, up *uplinkMessage) error {
	supi, err := supiFromRegistration(up.nasPDU)
	if err != nil {
		return err
	}
	if a.dropsRegistration(supi) {
		if a.crashOnDrop {
			a.crashed.Store(true)
			return errCrashed
		}
		return nil
	}
	// Every field another association reads is set before the UE is
	// published in a.ues.
	st.ranID = up.ranID
	st.amfID = <-a.amfSeq
	st.supi = supi
	st.ueCaps = ueCapsFromRegistration(up.nasPDU)
	st.requested = requestedNSSAIFromRegistration(up.nasPDU)
	a.mu.Lock()
	a.ues[st.amfID] = st
	st.regIdx = len(a.regReqs)
	a.regReqs = append(a.regReqs, append([]byte(nil), up.nasPDU...))
	a.initialRegReqs = append(a.initialRegReqs, append([]byte(nil), up.nasPDU...))
	a.regSUPIs = append(a.regSUPIs, supi)
	a.mu.Unlock()
	if a.tasDropped(p) {
		return a.rejectRegistration(p, st, a.dropTAsCause)
	}
	if a.rejectNonCleartext {
		if _, found, err := nas.NonCleartextRegistrationIE(up.nasPDU); err != nil || found {
			return a.rejectInitialNAS(p, st)
		}
	}

	// Network-side 5G-AKA: fresh RAND, AUTN over the shared credential.
	randv, autn, err := a.challenge()
	if err != nil {
		return err
	}

	// Derive the same key hierarchy the UE will, for later SMC/RegAccept.
	sub := auth.Subscription{SUPI: supi, Ki: a.cfg.Ki, OPc: a.cfg.OPc}
	vec, err := sub.DeriveFromChallenge(randv[:], autn[:], a.snn, nas.NIA2, nas.NEA0)
	if err != nil {
		return fmt.Errorf("mock amf key derivation: %w", err)
	}
	// The 128-NIA2/NEA0 context protects what precedes or replaces the
	// Security Mode Command; the command derives the selected pair's.
	st.setSec(&nas.SecurityContext{IntegrityAlg: nas.NIA2, CipheringAlg: nas.NEA0, KNASint: vec.KNASint, KNASenc: vec.KNASenc, Network: true, RejectReplay: true})
	st.kamf = vec.KAmf

	if a.noAuthRequest {
		return nil
	}
	if a.authReject {
		rej, err := buildAuthenticationReject()
		if err != nil {
			return err
		}
		return a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), rej))
	}
	if a.plainIdentity {
		if err := a.sendIdentityRequest(p, st, a.plainIdentityType, false); err != nil {
			return err
		}
	}
	arBytes, err := buildAuthenticationRequest(randv, autn, st.ngksi)
	if err != nil {
		return err
	}
	if err := a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), arBytes)); err != nil {
		return err
	}
	if a.t3560 > 0 {
		st.t3560.stop() // a repeated Initial UE Message: never orphan a running timer
		st.t3560 = a.startT3560(p, st, arBytes)
	}
	return nil
}

func (a *AMF) onUplinkNAS(p *pipe, st *ueState, nasPDU []byte) (bool, error) {
	if len(nasPDU) < 2 {
		return false, fmt.Errorf("uplink NAS too short")
	}
	if st.muted {
		return false, nil
	}
	var msg *f5nas.Message
	var err error
	if f5nas.GetSecurityHeaderType(nasPDU) == f5nas.SecurityHeaderTypePlainNas {
		msg, err = nas.DecodePlain(nasPDU)
	} else {
		msg, err = a.decodeUplink(p, st, nasPDU)
	}
	if err != nil {
		return false, fmt.Errorf("mock amf uplink NAS decode: %w", err)
	}
	if msg == nil {
		return false, nil // dropped (decodeUplink)
	}

	switch msg.GmmHeader.GetMessageType() {
	case f5nas.MsgTypeAuthenticationResponse:
		st.t3560.stop()
		// Accept without checking RES* (sim only), send Security Mode Command.
		return false, a.onAuthenticated(p, st)
	case f5nas.MsgTypeSecurityModeComplete, f5nas.MsgTypeIdentityResponse:
		if due, err := a.beforeAccept(p, st, msg); err != nil || !due {
			return false, err
		}
		if a.registrationReject != 0 {
			return false, a.rejectProtected(p, st, a.registrationReject)
		}
		if a.nssaiAnswer != AcceptAnyNSSAI && !a.servesRequested(st) {
			return false, a.answerUnservedNSSAI(p, st)
		}
		if a.undecodableAccept {
			return false, a.sendUndecodableAccept(p, st)
		}
		// Send Initial Context Setup Request carrying Registration Accept.
		acc, err := a.registrationAccept(st)
		if err != nil {
			return false, err
		}
		if a.acceptCountSet {
			st.forceTxCount(a.acceptCount)
		}
		wrapped, err := st.encodeSecure(acc, nas.SecHdrIntegrityCiphered)
		if err != nil {
			return false, err
		}
		if err := a.send(p, a.initialContextSetup(st, wrapped)); err != nil {
			return false, err
		}
		if a.t3550 > 0 && a.assignGUTI {
			st.t3550.stop() // a repeated Security Mode Complete: never orphan a running timer
			if st.t3550, err = a.startT3550(p, st, wrapped); err != nil {
				return false, err
			}
		}
		return false, nil
	case f5nas.MsgTypeRegistrationComplete:
		st.t3550.stop()
		if a.staleRelease > 0 {
			a.sendStaleRelease(p, st)
		}
		if a.configUpdate != nil {
			return true, a.sendConfigurationUpdate(p, st)
		}
		return true, nil // UE is REGISTERED
	case f5nas.MsgTypeConfigurationUpdateComplete:
		a.configUpdateCompletes.Add(1)
		return false, nil
	case f5nas.MsgTypeULNASTransport:
		return false, a.onPDUSessionRequest(p, st, msg)
	case f5nas.MsgTypeDeregistrationRequestUEOriginatingDeregistration:
		a.deregs.Add(1)
		a.recordDeregistration(msg.DeregistrationRequestUEOriginatingDeregistration)
		return false, a.onDeregistration(p, st, msg)
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

// Deregistrations reports how many UE-originating Deregistration Requests
// the mock has received.
func (a *AMF) Deregistrations() int64 { return a.deregs.Load() }

// ReleaseCompletes reports how many UE CONTEXT RELEASE COMPLETEs the mock
// has received: a release it sent on an association the gNB had already
// closed is never completed.
func (a *AMF) ReleaseCompletes() int64 { return a.releaseCompletes.Load() }

// StrayReleaseCompletes reports how many UE CONTEXT RELEASE COMPLETEs
// arrived before any release the mock sent named their UE (for a
// switch-off, on the gNB's request, or of a cancelled handover's target):
// a gNB that took some other PDU, or an older release, for the answer.
func (a *AMF) StrayReleaseCompletes() int64 { return a.strayCompletes.Load() }

// ReleaseCompleteIDs reports the (AMF, RAN) UE NGAP ID pair each UE
// CONTEXT RELEASE COMPLETE named, in the order they arrived.
func (a *AMF) ReleaseCompleteIDs() [][2]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][2]int64(nil), a.completeIDs...)
}

// DeregistrationReleaseIDs reports the (AMF, RAN) UE NGAP ID pair of each
// UE CONTEXT RELEASE COMMAND the mock sent to answer a switch-off.
func (a *AMF) DeregistrationReleaseIDs() [][2]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][2]int64(nil), a.deregReleases...)
}

// Setup is a gNB's NG Setup Request: its gNB ID and RAN node name.
type Setup struct {
	GNBID uint32
	Name  string
}

// Setups are the NG Setup Requests received, in order.
func (a *AMF) Setups() []Setup {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Setup(nil), a.setups...)
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

// ranIDOf is st's RAN-UE-NGAP-ID now: a path switch may change it from
// another association.
func (a *AMF) ranIDOf(st *ueState) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return st.ranID
}

// setSec, encodeSecure, encodeSecureRaw and decodeSecure use st's NAS
// security context under secMu, held only for the call.
func (st *ueState) setSec(sec *nas.SecurityContext) {
	st.secMu.Lock()
	defer st.secMu.Unlock()
	st.sec = sec
}

func (st *ueState) encodeSecure(msg *f5nas.Message, secHdr uint8) ([]byte, error) {
	st.secMu.Lock()
	defer st.secMu.Unlock()
	return st.sec.EncodeSecure(msg, secHdr)
}

func (st *ueState) encodeSecureRaw(plain []byte, secHdr uint8) ([]byte, error) {
	st.secMu.Lock()
	defer st.secMu.Unlock()
	return st.sec.EncodeSecureRaw(plain, secHdr)
}

func (st *ueState) decodeSecure(data []byte) (*f5nas.Message, error) {
	st.secMu.Lock()
	defer st.secMu.Unlock()
	return st.sec.DecodeSecure(data)
}
