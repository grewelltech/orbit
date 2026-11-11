package conformance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/free5gc/nas/nasType"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// UESpec is the subscriber a UE-level (Mobility) check registers. An empty
// SUPI means no UE is configured, and every mobility check SKIPs. Ki and OPc
// are hex and never appear in a verdict.
type UESpec struct {
	SUPI string
	Ki   string
	OPc  string
	// Subscribers is how many consecutive SUPIs, SUPI first, share Ki and
	// OPc and may be registered. On the suite's Env it is the pool the
	// checks are spread over, each taking its own block (see
	// Registry.subscriberBlock); on a check's Env it is that check's block.
	// Zero or one is the single SUPI, which every check then registers.
	Subscribers int
	// DNN is the PDU session's data network; "" lets the core choose.
	DNN string
	// SST and SD are the PDU session's slice; zero SST takes the first
	// slice of Env.GNB.
	SST uint8
	SD  string
	// GNBN3 is the local gNB N3 address (an IP reachable from the UPF);
	// both gNBs of a handover use it, each with its own TEIDs.
	GNBN3 string
	// Traffic is an optional host:port echo peer the UE sends to across the
	// handover, so downlink flows on the old path; "" sends nothing.
	Traffic string
	// NASIntegrity and NASCiphering are the 128-NIA and NEA algorithms the
	// UE advertises, as the UE security capability's octets (TS 24.501
	// 9.11.3.54: bit 8 is algorithm 0). Zero keeps the default, 128-NIA2 and
	// NEA0 (ue.SecurityCapability); a check that sets its own capabilities
	// as the stimulus overrides them.
	NASIntegrity, NASCiphering uint8
}

// Configured reports whether a UE is configured.
func (s UESpec) Configured() bool { return s.SUPI != "" }

// trafficRate and trafficPacketSize shape the optional echo flow: enough
// downlink to show the old path in use, little enough for any testbed.
const (
	trafficRate       = "1Mbps"
	trafficPacketSize = 256
)

// UEHarness drives one UE through engine.Manager for the mobility checks: it
// registers the UE with a PDU session on Env.GNB (gNB A), observes its
// downlink lane, runs an optional echo flow, hands it over by Xn or N2 to
// gNB B (Env.TargetGNBID, same PLMN, TAC and slices) and records what
// arrived on the old path. Both gNB associations come from Env.Bind, which
// must leave the port to the kernel. Close releases everything.
type UEHarness struct {
	env  Env
	m    *engine.Manager
	supi string
	id   ue.Identity
	sub  auth.Subscription
	pdu  ue.PDUSessionParams
	// caps is the advertised UE security capability, nil for the default.
	caps *nasType.UESecurityCapability

	registered bool
	// probe is the NAS probe that took the UE over (ProbeNAS), nil if none.
	probe    *engine.NASProbe
	stopObs  func()
	stopTraf context.CancelFunc
	trafDone chan error

	mu      sync.Mutex
	markers []datapath.EndMarker
	gpdus   []datapath.GPDUArrival
	obs     PathObservation
	// gpduSeen is signalled, without blocking, for each downlink G-PDU.
	gpduSeen chan struct{}
}

// PathObservation is what a harness saw on the old path around an Xn path
// switch, for a check to judge.
type PathObservation struct {
	// OldTEID and SourceN3 are the vacated downlink tunnel: the TEID gNB A
	// gave the core and gNB A's N3 socket as the engine bound it, with an
	// IPv4-mapped address unmapped (the canonical form).
	OldTEID  uint32
	SourceN3 netip.AddrPort
	// Started is taken just before the handover began, so it precedes the
	// Path Switch Request; Ack is when the engine read the Path Switch
	// Request Acknowledge (AckFromEvent false: the handover's return was
	// used, a few milliseconds later).
	Started, Ack time.Time
	AckFromEvent bool
	EndMarkers   []datapath.EndMarker
	GPDUs        []datapath.GPDUArrival
	// Traffic is the configured echo peer ("" none).
	Traffic string
	// NewTEID is the downlink TEID gNB B gave the core in the Path Switch
	// Request.
	NewTEID uint32
	// Switched is the PDU Session Resource Switched List of the Path Switch
	// Request Acknowledge, as received; nil before the switch.
	Switched *gnb.PathSwitchAck
}

// NewUEHarness validates env for a UE-level check and builds a harness. It
// opens no association: a fixed-port Env.Bind or an incomplete UE section is
// an error before anything is dialled. log may be nil.
func NewUEHarness(env Env, log *slog.Logger) (*UEHarness, error) {
	if err := ValidateBind(env.Bind); err != nil {
		return nil, err
	}
	if !env.UE.Configured() {
		return nil, errors.New("no UE configured")
	}
	if env.UE.GNBN3 == "" {
		return nil, errors.New("the UE needs a gNB N3 IP address (--gnb-n3) for the data path")
	}
	n3, err := gnb.CanonicalN3(env.UE.GNBN3)
	if err != nil {
		return nil, err
	}
	supi, err := ue.NormalizeSUPI(env.UE.SUPI)
	if err != nil {
		return nil, err
	}
	id, err := ue.ParseIdentity(supi, env.GNB.MCC, env.GNB.MNC, "0")
	if err != nil {
		return nil, err
	}
	ki, err := auth.ParseHexKey("Ki", env.UE.Ki)
	if err != nil {
		return nil, err
	}
	opc, err := auth.ParseHexKey("OPc", env.UE.OPc)
	if err != nil {
		return nil, err
	}
	pdu := ue.PDUSessionParams{PDUSessionID: 1, SST: env.UE.SST, SD: env.UE.SD, DNN: env.UE.DNN}
	if pdu.SST == 0 && len(env.GNB.Slices) > 0 {
		pdu.SST, pdu.SD = env.GNB.Slices[0].SST, env.GNB.Slices[0].SD
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	env.UE.GNBN3 = n3
	caps, err := ueCapability(env.UE)
	if err != nil {
		return nil, err
	}
	return &UEHarness{
		env: env, m: engine.NewManager(log), supi: supi, id: id,
		sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc}, pdu: pdu, caps: caps,
		gpduSeen: make(chan struct{}, 1),
	}, nil
}

// ueCapability is the UE security capability s advertises: nil (the
// default) when both bitmaps are zero; otherwise the given ones, the zero
// one taking its default, 128-NIA2 or NEA0.
func ueCapability(s UESpec) (*nasType.UESecurityCapability, error) {
	if s.NASIntegrity == 0 && s.NASCiphering == 0 {
		return nil, nil
	}
	if s.NASIntegrity&0x80 != 0 {
		return nil, errors.New("the UE never offers NIA0")
	}
	ia, ea := s.NASIntegrity, s.NASCiphering
	if ia == 0 {
		ia = 0x80 >> nas.NIA2
	}
	if ea == 0 {
		ea = 0x80 >> nas.NEA0
	}
	c := ue.SecurityCapability()
	c.Buffer = []uint8{ea, ia}
	return &c, nil
}

// TargetGNB is gNB B, the handover target.
func (h *UEHarness) TargetGNB() gnb.Config {
	t := h.env.GNB
	t.ID = h.env.targetGNBID()
	t.Name = "orbit-conf-xn-target"
	return t
}

// Register attaches the UE with a PDU session on gNB A and starts observing
// its downlink lane. An error names the step that failed.
func (h *UEHarness) Register(ctx context.Context) (*engine.AttachResult, error) {
	res, err := h.m.Register(ctx, engine.GNBSpec{Config: h.env.GNB, AMFAddr: h.env.AMFAddr, BindAddr: h.env.Bind},
		engine.UEConfig{Identity: h.id, Sub: h.sub, PDUSession: &h.pdu, GNBN3Addr: h.env.UE.GNBN3, SecurityCapabilities: h.caps})
	if err != nil {
		return nil, fmt.Errorf("registration on gNB %#x: %w", h.env.GNB.ID, err)
	}
	h.registered = true
	if !res.Registered {
		return nil, fmt.Errorf("registration on gNB %#x: not registered", h.env.GNB.ID)
	}
	if !res.SessionActive {
		return nil, fmt.Errorf("PDU session establishment: no active session")
	}
	stop, err := h.m.ObserveDownlink(h.supi, h.noteEndMarker, h.noteGPDU)
	if err != nil {
		return nil, fmt.Errorf("data path: %w", err)
	}
	h.stopObs = stop
	src, err := h.boundN3()
	if err != nil {
		return nil, fmt.Errorf("data path: %w", err)
	}
	h.mu.Lock()
	h.obs.OldTEID = res.DLTEID
	h.obs.SourceN3 = src
	h.mu.Unlock()
	return res, nil
}

// RegisterWithProbe attaches the UE on gNB A with probe set: registration
// only, with no PDU session and no downlink observers, for the NAS checks.
// The attach error comes back as it is (engine.ErrReleasedByCore, a context
// error, a reject), and the association is closed when the attach fails.
// Each mod adjusts the UE's configuration first (for example its security
// capabilities).
func (h *UEHarness) RegisterWithProbe(ctx context.Context, probe *engine.AttachProbe, mods ...func(*engine.UEConfig)) (*engine.AttachResult, error) {
	cfg := engine.UEConfig{Identity: h.id, Sub: h.sub, Probe: probe, SecurityCapabilities: h.caps}
	for _, m := range mods {
		m(&cfg)
	}
	res, err := h.m.Register(ctx, engine.GNBSpec{Config: h.env.GNB, AMFAddr: h.env.AMFAddr, BindAddr: h.env.Bind}, cfg)
	if err != nil {
		return nil, err
	}
	h.registered = true
	return res, nil
}

// ProbeNAS opens an engine.NASProbe on the registered UE, for a check that
// drives its NAS signalling connection by hand. The probe owns the UE from
// then on: its Close drops the association, and the harness's Close and
// Deregister no longer signal for the UE.
func (h *UEHarness) ProbeNAS(ctx context.Context) (*engine.NASProbe, error) {
	p, err := h.m.ProbeNAS(ctx, h.supi)
	if err != nil {
		return nil, err
	}
	h.registered = false
	h.probe = p
	return p, nil
}

// boundN3 is gNB A's N3 socket as the engine bound it, in canonical form,
// so the check never compares against the operator's spelling of --gnb-n3.
func (h *UEHarness) boundN3() (netip.AddrPort, error) {
	sess, err := h.m.Status(h.supi)
	if err != nil {
		return netip.AddrPort{}, err
	}
	ua, ok := sess.N3LocalAddr().(*net.UDPAddr)
	if !ok || ua == nil {
		return netip.AddrPort{}, errors.New("no bound N3 socket")
	}
	return canonical(ua.AddrPort()), nil
}

// canonical unmaps an IPv4-mapped address, so the two spellings of one
// IPv4 socket compare equal.
func canonical(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// sameSocket reports whether a demux-reported socket address ("host:port")
// is the socket ap, compared as values.
func sameSocket(ap netip.AddrPort, reported string) bool {
	r, err := netip.ParseAddrPort(reported)
	return err == nil && canonical(r) == ap
}

func (h *UEHarness) noteEndMarker(em datapath.EndMarker) {
	h.mu.Lock()
	h.markers = append(h.markers, em)
	h.mu.Unlock()
}

func (h *UEHarness) noteGPDU(g datapath.GPDUArrival) {
	h.mu.Lock()
	h.gpdus = append(h.gpdus, g)
	h.mu.Unlock()
	select {
	case h.gpduSeen <- struct{}{}:
	default:
	}
}

// StartTraffic starts the optional echo flow to UESpec.Traffic in the
// background; it runs until Close. A no-op when no peer is configured.
func (h *UEHarness) StartTraffic(ctx context.Context) {
	if h.env.UE.Traffic == "" || h.stopTraf != nil {
		return
	}
	tctx, cancel := context.WithCancel(ctx)
	h.stopTraf, h.trafDone = cancel, make(chan error, 1)
	go func() {
		// The flow ends when tctx is cancelled; that is its normal end.
		_, err := h.m.Traffic(tctx, h.supi, h.env.UE.Traffic, trafficRate, trafficPacketSize, time.Hour)
		h.trafDone <- err
	}()
}

// XnHandover hands the UE over to gNB B by Xn (Path Switch Request on a
// second association) and records the switch's timing, the vacated tunnel
// and the Acknowledge's Switched List. A Path Switch Request that went
// unacknowledged is an error wrapping engine.ErrPathSwitchNotAcknowledged.
func (h *UEHarness) XnHandover(ctx context.Context) error {
	sess, err := h.m.Status(h.supi)
	if err != nil {
		return fmt.Errorf("handover (Xn): %w", err)
	}
	events, unsub := h.m.Subscribe()
	defer unsub()
	started := time.Now()
	src := sess.Result.DLTEID
	res, err := h.m.XnHandover(ctx, h.supi, engine.GNBEndpoint{
		Config: h.TargetGNB(), AMFAddr: h.env.AMFAddr, BindAddr: h.env.Bind, N3Addr: h.env.UE.GNBN3,
	})
	returned := time.Now()
	if err != nil {
		return fmt.Errorf("handover (Xn) to gNB %#x: %w", h.TargetGNB().ID, err)
	}
	// The events were published before XnHandover returned, so they are
	// buffered by now.
	ack, fromEvent := returned, false
drain:
	for {
		select {
		case ev := <-events:
			if ev.SUPI == h.supi && ev.State == engine.StatePathSwitchComplete {
				ack, fromEvent = ev.Time, true
			}
		default:
			break drain
		}
	}
	h.mu.Lock()
	h.obs.OldTEID = src
	h.obs.Started, h.obs.Ack, h.obs.AckFromEvent = started, ack, fromEvent
	h.obs.NewTEID, h.obs.Switched = sess.Result.DLTEID, res.PathSwitchAck
	h.mu.Unlock()
	return nil
}

// SuppliedCapabilities returns the UE security capabilities the AMF supplied
// to the serving gNB for the registered UE (the INITIAL CONTEXT SETUP
// REQUEST's UE Security Capabilities), nil when it supplied none.
func (h *UEHarness) SuppliedCapabilities() (*ngapType.UESecurityCapabilities, error) {
	sess, err := h.m.Status(h.supi)
	if err != nil {
		return nil, err
	}
	return sess.UESecurityCapabilities(), nil
}

// XnHandoverWithCapabilities hands the UE over to gNB B by Xn with a PATH
// SWITCH REQUEST that carries caps as the UE Security Capabilities, in
// place of the ones the AMF supplied, and returns the UE Security
// Capabilities of the Acknowledge (nil when it has none). A Path Switch
// Request that went unacknowledged is an error wrapping
// engine.ErrPathSwitchNotAcknowledged.
func (h *UEHarness) XnHandoverWithCapabilities(ctx context.Context, caps *ngapType.UESecurityCapabilities) (*ngapType.UESecurityCapabilities, error) {
	res, err := h.m.XnHandoverWith(ctx, h.supi, engine.GNBEndpoint{
		Config: h.TargetGNB(), AMFAddr: h.env.AMFAddr, BindAddr: h.env.Bind, N3Addr: h.env.UE.GNBN3,
	}, engine.WithPathSwitchCapabilities(caps))
	if err != nil {
		return nil, fmt.Errorf("handover (Xn) to gNB %#x: %w", h.TargetGNB().ID, err)
	}
	return res.PathSwitchCapabilities, nil
}

// Idle moves the registered UE to CM-IDLE by an AN release (engine.Manager.Idle).
func (h *UEHarness) Idle(ctx context.Context) (*engine.IdleResult, error) {
	return h.m.Idle(ctx, h.supi)
}

// TriggerDownlink asks Env.Downlink to send downlink traffic to the UE's
// PDU address.
func (h *UEHarness) TriggerDownlink(ctx context.Context) error {
	if h.env.Downlink == nil {
		return errors.New("no downlink trigger configured")
	}
	sess, err := h.m.Status(h.supi)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(sess.Result.PDUAddress)
	if err != nil {
		return fmt.Errorf("UE address %q: %w", sess.Result.PDUAddress, err)
	}
	return h.env.Downlink.SendDownlink(ctx, ip)
}

// AwaitPaging reads NGAP PAGING on the idle UE's association for up to
// wait, or until ctx ends, and returns every one it saw: it stops at the
// first that names the UE's 5G-S-TMSI. The error is ctx's when ctx, not
// the wait, ended it, or the read's when the association failed.
func (h *UEHarness) AwaitPaging(ctx context.Context, wait time.Duration) ([]engine.PagingEvent, error) {
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var seen []engine.PagingEvent
	for {
		ev, err := h.m.ObservePaging(wctx, h.supi)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return seen, ctx.Err()
			case wctx.Err() != nil:
				return seen, nil
			}
			return seen, err
		}
		seen = append(seen, *ev)
		if ev.ForUE {
			return seen, nil
		}
	}
}

// ServiceRequest brings the idle UE back (engine.Manager.ServiceRequest).
func (h *UEHarness) ServiceRequest(ctx context.Context, opts ...engine.ServiceOption) (*engine.ServiceResult, error) {
	return h.m.ServiceRequest(ctx, h.supi, opts...)
}

// PDUSessionID is the ID of the PDU session the harness establishes.
func (h *UEHarness) PDUSessionID() uint8 { return h.pdu.PDUSessionID }

// WatchDownlink installs the downlink observers on the UE's current data
// path, opening it if needed. Register does this for the tunnel of the
// registration; a tunnel reactivated by a Service Request has a new lane,
// which this observes.
func (h *UEHarness) WatchDownlink() error {
	if h.stopObs != nil {
		h.stopObs()
		h.stopObs = nil
	}
	stop, err := h.m.ObserveDownlink(h.supi, h.noteEndMarker, h.noteGPDU)
	if err != nil {
		return fmt.Errorf("data path: %w", err)
	}
	h.stopObs = stop
	return nil
}

// AwaitDownlink waits up to wait, or until ctx ends, for a downlink G-PDU
// on the gNB tunnel teid and returns the first one seen, nil when none
// came in the wait. The error is ctx's when ctx ended first.
func (h *UEHarness) AwaitDownlink(ctx context.Context, wait time.Duration, teid uint32) (*datapath.GPDUArrival, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		h.mu.Lock()
		for _, g := range h.gpdus {
			if g.TEID == teid && !g.Stale {
				h.mu.Unlock()
				return &g, nil
			}
		}
		h.mu.Unlock()
		select {
		case <-h.gpduSeen:
		case <-timer.C:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// AwaitDownlinkQuiet waits until no downlink G-PDU has arrived for quiet,
// for at most limit, and reports whether the downlink went quiet. The
// error is ctx's when ctx ended first.
func (h *UEHarness) AwaitDownlinkQuiet(ctx context.Context, quiet, limit time.Duration) (bool, error) {
	deadline := time.Now().Add(limit)
	for {
		h.mu.Lock()
		var last time.Time
		if n := len(h.gpdus); n > 0 {
			last = h.gpdus[n-1].Arrival
		}
		h.mu.Unlock()
		idle := time.Since(last)
		if idle >= quiet {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		if !sleepCtx(ctx, min(quiet-idle, time.Until(deadline))) {
			return false, ctx.Err()
		}
	}
}

// N2Handover hands the UE over to gNB B by N2 (Handover Required on gNB
// A's association, Handover Request and Notify on a second one) and returns
// the HANDOVER COMMAND's PDU Session Resource Handover List as received. It
// is nil when the handover failed before the command, and set along with
// the error when it failed after it. The harness's Manager runs the
// strict-3gpp core profile: no core-specific quirk shapes what gNB B sends.
func (h *UEHarness) N2Handover(ctx context.Context) (*gnb.HandoverCommandInfo, error) {
	res, err := h.m.Handover(ctx, h.supi, engine.GNBEndpoint{
		Config: h.TargetGNB(), AMFAddr: h.env.AMFAddr, BindAddr: h.env.Bind, N3Addr: h.env.UE.GNBN3,
	})
	if err != nil {
		return res.Command, fmt.Errorf("handover (N2) to gNB %#x: %w", h.TargetGNB().ID, err)
	}
	return res.Command, nil
}

// RequestContextRelease sends a UE CONTEXT RELEASE REQUEST for the UE from
// gNB A and waits up to wait for the AMF's UE CONTEXT RELEASE COMMAND. A
// released UE stays registered, and Deregister (or Close) then sends its
// Deregistration Request as an initial NAS message.
func (h *UEHarness) RequestContextRelease(ctx context.Context, wait time.Duration) (engine.ContextReleaseResult, error) {
	return h.m.RequestContextRelease(ctx, h.supi, wait)
}

// CancelN2Handover prepares an N2 handover to gNB B, up to the HANDOVER
// COMMAND, and cancels it from gNB A with a HANDOVER CANCEL, waiting up to
// wait for the acknowledge. The UE stays on gNB A. An error wrapping
// engine.ErrHandoverNotPrepared means the handover never reached the
// command.
func (h *UEHarness) CancelN2Handover(ctx context.Context, wait time.Duration) (engine.HandoverCancelResult, error) {
	res, err := h.m.CancelHandover(ctx, h.supi, engine.GNBEndpoint{
		Config: h.TargetGNB(), AMFAddr: h.env.AMFAddr, BindAddr: h.env.Bind, N3Addr: h.env.UE.GNBN3,
	}, wait)
	if err != nil {
		return res, fmt.Errorf("handover (N2) to gNB %#x, cancelled: %w", h.TargetGNB().ID, err)
	}
	return res, nil
}

// Observe returns what has been seen so far, with the switch's timing.
func (h *UEHarness) Observe() PathObservation {
	h.mu.Lock()
	defer h.mu.Unlock()
	o := h.obs
	o.EndMarkers = append([]datapath.EndMarker(nil), h.markers...)
	o.GPDUs = append([]datapath.GPDUArrival(nil), h.gpdus...)
	o.Traffic = h.env.UE.Traffic
	return o
}

// Close stops the traffic and the observers and deregisters the UE, which
// closes its association (the target's after a handover; the source's was
// closed by the handover). A failed registration or handover has already
// closed the associations it opened. Safe to call more than once.
func (h *UEHarness) Close() {
	if h.stopTraf != nil {
		h.stopTraf()
		<-h.trafDone
		h.stopTraf = nil
	}
	if h.stopObs != nil {
		h.stopObs()
		h.stopObs = nil
	}
	if h.probe != nil {
		h.probe.Close()
		h.probe = nil
	}
	if h.registered {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Switch-off deregistration does not wait for the core; an error
		// here leaves nothing open, since the association closes either way.
		_ = h.Deregister(ctx)
	}
}

// Deregister deregisters a registered UE (switch-off), which closes its
// association, and returns the error that Close discards. It is a no-op
// when no UE is registered, and Close after it does not deregister again.
func (h *UEHarness) Deregister(ctx context.Context) error {
	if !h.registered {
		return nil
	}
	h.registered = false
	return h.m.Deregister(ctx, h.supi)
}

// DeregisterWith deregisters a registered UE as engine.Manager.DeregisterWith
// does with opts, which closes its association, and returns what the core
// answered. Close after it does not deregister again. With no UE registered
// it returns an error.
func (h *UEHarness) DeregisterWith(ctx context.Context, opts ...engine.DeregisterOption) (*engine.DeregisterResult, error) {
	if !h.registered {
		return nil, errors.New("no UE registered")
	}
	h.registered = false
	return h.m.DeregisterWith(ctx, h.supi, opts...)
}

// ReleasePDUSession runs the UE-requested PDU session release for the
// session Register established, as engine.Manager.ReleasePDUSession does
// with opts. The downlink observers and the echo flow stay as they are, so
// Observe shows what arrived on the released tunnel afterwards.
func (h *UEHarness) ReleasePDUSession(ctx context.Context, opts ...engine.PDUReleaseOption) (*engine.PDUReleaseResult, error) {
	return h.m.ReleasePDUSession(ctx, h.supi, opts...)
}

// ParseNASAlgorithms turns a list of algorithm names (prefix followed by a
// digit, case-insensitive) into the UE security capability bitmap, bit 8
// for algorithm 0 (TS 24.501 9.11.3.54). Algorithms 0 to 3 exist; min is
// the lowest one the flag accepts (1 for integrity: NIA0 is refused, since
// the UE never registers for emergency). nil is the default, 0; an empty
// list is refused. flag names the input in errors (a flag or a plan path).
func ParseNASAlgorithms(flag, prefix string, names []string, min int) (uint8, error) {
	if names == nil {
		return 0, nil
	}
	if len(names) == 0 {
		return 0, fmt.Errorf("%s: name at least one algorithm", flag)
	}
	var bits uint8
	for _, n := range names {
		up := strings.ToUpper(strings.TrimSpace(n))
		if len(up) != len(prefix)+1 || !strings.HasPrefix(up, prefix) || up[len(prefix)] < '0' || up[len(prefix)] > '3' {
			return 0, fmt.Errorf("%s: %q is not one of %s%d to %s3", flag, n, prefix, min, prefix)
		}
		a := int(up[len(prefix)] - '0')
		if a < min {
			return 0, fmt.Errorf("%s: %s is refused: the UE never registers for emergency, so it does not offer NIA0", flag, up)
		}
		bits |= 0x80 >> a
	}
	return bits, nil
}
