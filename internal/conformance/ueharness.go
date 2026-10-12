package conformance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
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
}

// Configured reports whether a UE is configured.
func (s UESpec) Configured() bool { return s.SUPI != "" }

// targetGNBOffset separates the handover target's gNB ID from the other IDs
// a suite presents, because the AMF does not cleanly re-key a reused gNB ID
// from a new association. gNB ID offsets in use from the base (--gnb-base):
// each check's own gNB at base + registry index (RunStream), a mobility
// check's handover target at check ID + 0x8000 (here), and the liveness
// probe at check ID + 0x9000 (Env.Alive). A new check needing another gNB
// must pick an offset clear of all three.
const targetGNBOffset = 0x8000

// trafficRate and trafficPacketSize shape the optional echo flow: enough
// downlink to show the old path in use, little enough for any testbed.
const (
	trafficRate       = "1Mbps"
	trafficPacketSize = 256
)

// UEHarness drives one UE through engine.Manager for the mobility checks: it
// registers the UE with a PDU session on Env.GNB (gNB A), observes its
// downlink lane, runs an optional echo flow, hands it over by Xn to gNB B
// (Env.GNB.ID + targetGNBOffset, same PLMN, TAC and slices) and records what
// arrived on the old path. Both gNB associations come from Env.Bind, which
// must leave the port to the kernel. Close releases everything.
type UEHarness struct {
	env  Env
	m    *engine.Manager
	supi string
	id   ue.Identity
	sub  auth.Subscription
	pdu  ue.PDUSessionParams

	registered bool
	stopObs    func()
	stopTraf   context.CancelFunc
	trafDone   chan error

	mu      sync.Mutex
	markers []datapath.EndMarker
	gpdus   []datapath.GPDUArrival
	obs     PathObservation
}

// PathObservation is what a harness saw on the old path around an Xn path
// switch, for a check to judge.
type PathObservation struct {
	// OldTEID and SourceN3 are the vacated downlink tunnel: the TEID gNB A
	// gave the core and gNB A's N3 socket (host:port).
	OldTEID  uint32
	SourceN3 string
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
	if net.ParseIP(env.UE.GNBN3) == nil {
		return nil, fmt.Errorf("the UE needs a gNB N3 IP address (--gnb-n3) for the data path, got %q", env.UE.GNBN3)
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
	return &UEHarness{
		env: env, m: engine.NewManager(log), supi: supi, id: id,
		sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc}, pdu: pdu,
	}, nil
}

// TargetGNB is gNB B, the handover target.
func (h *UEHarness) TargetGNB() gnb.Config {
	t := h.env.GNB
	t.ID = h.env.GNB.ID + targetGNBOffset
	t.Name = "orbit-conf-xn-target"
	return t
}

// Register attaches the UE with a PDU session on gNB A and starts observing
// its downlink lane. An error names the step that failed.
func (h *UEHarness) Register(ctx context.Context) (*engine.AttachResult, error) {
	res, err := h.m.Register(ctx, engine.GNBSpec{Config: h.env.GNB, AMFAddr: h.env.AMFAddr, BindAddr: h.env.Bind},
		engine.UEConfig{Identity: h.id, Sub: h.sub, PDUSession: &h.pdu, GNBN3Addr: h.env.UE.GNBN3})
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
	h.mu.Lock()
	h.obs.OldTEID = res.DLTEID
	h.mu.Unlock()
	return res, nil
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
// second association) and records the switch's timing and the vacated
// tunnel.
func (h *UEHarness) XnHandover(ctx context.Context) error {
	sess, err := h.m.Status(h.supi)
	if err != nil {
		return fmt.Errorf("handover (Xn): %w", err)
	}
	events, unsub := h.m.Subscribe()
	defer unsub()
	started := time.Now()
	src := sess.Result.DLTEID
	_, err = h.m.XnHandover(ctx, h.supi, engine.GNBEndpoint{
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
	h.obs.SourceN3 = gnbN3Socket(h.env.UE.GNBN3)
	h.obs.Started, h.obs.Ack, h.obs.AckFromEvent = started, ack, fromEvent
	h.mu.Unlock()
	return nil
}

// gnbN3Socket is the N3 socket address the engine binds for a gNB N3 IP,
// as the demux reports it on End Markers and G-PDUs.
func gnbN3Socket(ip string) string {
	return net.JoinHostPort(ip, "2152")
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
	if h.registered {
		h.registered = false
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Switch-off deregistration does not wait for the core; an error
		// here leaves nothing open, since the association closes either way.
		_ = h.m.Deregister(ctx, h.supi)
	}
}
