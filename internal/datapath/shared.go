package datapath

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/bgrewell/orbit/internal/gtpu"
)

// SharedTunnel is one GTP-U N3 endpoint — THE single UDP socket bound to a
// gNB's N3 address (host:2152) — shared by every UE that gNB serves (design
// §6, Phase 5 cutover). It solves the port-2152 collision that arose when
// each UE bound its own socket: uplink, every UE view writes the shared
// socket stamping its own UL TEID/QFI (UDP writes are goroutine-safe);
// downlink, ONE Demux owns the socket's read path from construction and
// routes G-PDUs to per-UE lanes by DL TEID. Nothing else may ever bind the
// gNB's N3 address or read this socket.
type SharedTunnel struct {
	conn    *net.UDPConn
	upfAddr *net.UDPAddr // default UPF N3 endpoint (per-UE override in Register)
	demux   *Demux
}

// NewSharedTunnel binds localN3 (the gNB N3 host:port) once, targets upfN3 by
// default, and starts the Demux reader that owns the socket's downlink.
func NewSharedTunnel(localN3, upfN3 string) (*SharedTunnel, error) {
	laddr, err := net.ResolveUDPAddr("udp", localN3)
	if err != nil {
		return nil, fmt.Errorf("resolve local N3 %q: %w", localN3, err)
	}
	raddr, err := net.ResolveUDPAddr("udp", upfN3)
	if err != nil {
		return nil, fmt.Errorf("resolve UPF N3 %q: %w", upfN3, err)
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, fmt.Errorf("bind N3 socket %q: %w", localN3, err)
	}
	return &SharedTunnel{conn: conn, upfAddr: raddr, demux: NewDemux(conn)}, nil
}

// Demux returns the tunnel's downlink demultiplexer. Consumers never read
// the socket — they subscribe on lanes registered here.
func (s *SharedTunnel) Demux() *Demux { return s.demux }

// LocalAddr reports the bound gNB N3 address (useful when bound to port 0).
func (s *SharedTunnel) LocalAddr() net.Addr { return s.conn.LocalAddr() }

// Close closes the shared socket (stopping every UE flow on it) and waits
// for the Demux reader to exit; all lanes' subscriptions are closed, waking
// blocked consumers with net.ErrClosed.
func (s *SharedTunnel) Close() error {
	err := s.conn.Close()
	_ = s.demux.Close()
	return err
}

// UETunnelConfig parameterises one UE's view of a SharedTunnel.
type UETunnelConfig struct {
	ULTEID uint32 // UPF-allocated uplink TEID (destination of this UE's uplink)
	DLTEID uint32 // this UE's downlink TEID (the UPF sends to it)
	QFI    uint8  // default QoS flow for uplink stamping and unmarked downlink

	// UPFN3 optionally overrides the tunnel's default UPF N3 endpoint for
	// this UE's uplink ("" uses the default).
	UPFN3 string

	// Lane optionally attaches an existing live downlink lane instead of
	// creating one — the inter-gNB handover move: Detach the lane (rings,
	// End-Marker callback and all) from the source tunnel's view, Register
	// it here, and downlink consumers see only the handover gap, never a
	// closed ring. nil creates a fresh lane.
	Lane *UERx

	// Stats optionally carries an existing per-UE counter set across a
	// handover so per-UE DataStats survive the move. nil starts fresh.
	Stats *UEStats
}

// Register creates this UE's view of the shared tunnel: the session-facing
// surface the retired per-session Tunnel provided — SendUplink stamping the
// UE's own UL TEID/QFI, per-UE per-QFI Stats, and a downlink lane (Lane) for
// ICMP/UDP subscriptions — registered on the Demux under cfg.DLTEID. A TEID
// already claimed by another UE is refused (isolation, not silent sharing).
func (s *SharedTunnel) Register(cfg UETunnelConfig) (*UETunnel, error) {
	upf := s.upfAddr
	if cfg.UPFN3 != "" {
		raddr, err := net.ResolveUDPAddr("udp", cfg.UPFN3)
		if err != nil {
			return nil, fmt.Errorf("resolve UPF N3 %q: %w", cfg.UPFN3, err)
		}
		upf = raddr
	}
	stats := cfg.Stats
	if stats == nil {
		stats = NewUEStats()
	}
	stats.flow(cfg.QFI) // the session's own QFI is visible before traffic
	rx := cfg.Lane
	if rx == nil {
		rx = newUERx()
	}
	// Wire the per-UE accounting hook BEFORE the lane goes live on the demux:
	// the UPF learned this DL TEID at session setup, so downlink can already
	// be in flight — a G-PDU dispatched between Attach and the hook install
	// would be delivered but never counted.
	defQFI := cfg.QFI
	rx.setDownlinkStats(func(qfi uint8, hasQFI bool, n int) {
		q := defQFI
		if hasQFI {
			q = qfi
		}
		f := stats.flow(q)
		f.DownlinkPackets.Add(1)
		f.DownlinkBytes.Add(uint64(n))
	})
	if err := s.demux.Attach(cfg.DLTEID, rx); err != nil {
		return nil, fmt.Errorf("register UE on shared N3 tunnel: %w", err)
	}
	ue := &UETunnel{
		st: s, qfi: cfg.QFI,
		stats: stats, rx: rx, dlTEID: cfg.DLTEID,
	}
	ue.upf.Store(upf)
	ue.ulTEID.Store(cfg.ULTEID)
	return ue, nil
}

// UEFlow returns an uplink-only view that encapsulates with a UE's uplink
// TEID/QFI and writes the shared socket — the fleet traffic path, which
// needs neither downlink lanes nor per-UE stats. Safe from many goroutines.
// The tunnel's default UPF N3 endpoint is used; UEFlowTo overrides it per UE.
func (s *SharedTunnel) UEFlow(ulTEID uint32, qfi uint8) *UEFlow {
	return &UEFlow{s: s, ulTEID: ulTEID, qfi: qfi}
}

// UEFlowTo is UEFlow with a per-UE UPF N3 endpoint: sessions anchored on a
// different UPF than the tunnel's default (multi-UPF slice / DNN split) send
// their uplink where THEIR PDU session terminates, not where UE 0's does.
func (s *SharedTunnel) UEFlowTo(ulTEID uint32, qfi uint8, upfN3 string) (*UEFlow, error) {
	f := &UEFlow{s: s, ulTEID: ulTEID, qfi: qfi}
	if upfN3 != "" {
		raddr, err := net.ResolveUDPAddr("udp", upfN3)
		if err != nil {
			return nil, fmt.Errorf("resolve UPF N3 %q: %w", upfN3, err)
		}
		f.upf = raddr
	}
	return f, nil
}

// UEFlow is one UE's uplink over a SharedTunnel. It satisfies the GTP-U
// uplink interface the loom bridge needs (SendUplink).
type UEFlow struct {
	s      *SharedTunnel
	ulTEID uint32
	qfi    uint8
	upf    *net.UDPAddr // nil = the tunnel's default UPF N3 endpoint
}

// SendUplink wraps innerIP in a GTP-U G-PDU for this UE's TEID and sends it.
func (u *UEFlow) SendUplink(innerIP []byte) error {
	upf := u.upf
	if upf == nil {
		upf = u.s.upfAddr
	}
	if _, err := u.s.conn.WriteToUDP(gtpu.EncodeGPDU(u.ulTEID, u.qfi, innerIP), upf); err != nil {
		return fmt.Errorf("send uplink G-PDU: %w", err)
	}
	return nil
}

// UEStats is one UE's per-QFI uplink/downlink counters. It is carried across
// inter-gNB handovers (UETunnel.Detach → Register with Stats) so the DataStats
// RPC stays per-UE and cumulative over the session's life.
type UEStats struct {
	mu    sync.Mutex
	flows map[uint8]*QFIStats
}

// NewUEStats returns an empty per-UE counter set.
func NewUEStats() *UEStats { return &UEStats{flows: make(map[uint8]*QFIStats)} }

func (s *UEStats) flow(qfi uint8) *QFIStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[qfi]
	if !ok {
		f = &QFIStats{}
		s.flows[qfi] = f
	}
	return f
}

// Snapshot returns a point-in-time copy of the per-QFI counters.
func (s *UEStats) Snapshot() map[uint8]QFIStatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[uint8]QFIStatsSnapshot, len(s.flows))
	for qfi, f := range s.flows {
		out[qfi] = QFIStatsSnapshot{
			UplinkPackets:   f.UplinkPackets.Load(),
			UplinkBytes:     f.UplinkBytes.Load(),
			DownlinkPackets: f.DownlinkPackets.Load(),
			DownlinkBytes:   f.DownlinkBytes.Load(),
		}
	}
	return out
}

// UETunnel is one UE's session-facing view of a SharedTunnel: the surface the
// retired per-session Tunnel provided, minus socket ownership. Uplink goes
// out the shared socket stamped with this UE's TEID/QFI; downlink arrives on
// the view's demux lane; counters are per-UE.
type UETunnel struct {
	st    *SharedTunnel
	qfi   uint8
	stats *UEStats
	rx    *UERx

	// upf/ulTEID are the uplink target, atomically swappable: a handover
	// path switch may re-anchor the session on a new UPF UL F-TEID
	// (TS 38.413 — PathSwitchRequestAcknowledge carries UL NG-U UP TNL
	// info), and uplink senders must not race the update (SetUplink).
	upf    atomic.Pointer[net.UDPAddr]
	ulTEID atomic.Uint32

	mu       sync.Mutex
	dlTEID   uint32
	detached bool // lane no longer registered under this view (Detach/Close ran)
}

// SendUplink encapsulates an inner IP packet with this UE's UL TEID/QFI and
// sends it to the UPF over the shared socket.
func (u *UETunnel) SendUplink(innerIP []byte) error {
	pkt := gtpu.EncodeGPDU(u.ulTEID.Load(), u.qfi, innerIP)
	if _, err := u.st.conn.WriteToUDP(pkt, u.upf.Load()); err != nil {
		return fmt.Errorf("send uplink G-PDU: %w", err)
	}
	f := u.stats.flow(u.qfi)
	f.UplinkPackets.Add(1)
	f.UplinkBytes.Add(uint64(len(innerIP)))
	return nil
}

// SetUplink retargets this UE's uplink — the UL TEID it stamps and the UPF
// N3 endpoint it sends to — atomically with respect to concurrent
// SendUplink calls: the intra-gNB half of applying a path switch that
// reallocated the UPF's UL F-TEID (the inter-gNB half re-registers and picks
// the new values up in Register). An empty upfN3 keeps the current endpoint.
func (u *UETunnel) SetUplink(ulTEID uint32, upfN3 string) error {
	if upfN3 != "" {
		raddr, err := net.ResolveUDPAddr("udp", upfN3)
		if err != nil {
			return fmt.Errorf("resolve UPF N3 %q: %w", upfN3, err)
		}
		u.upf.Store(raddr)
	}
	u.ulTEID.Store(ulTEID)
	return nil
}

// Lane returns this UE's downlink lane for ICMP/UDP subscriptions.
func (u *UETunnel) Lane() *UERx { return u.rx }

// Stats returns a snapshot of this UE's per-QFI counters.
func (u *UETunnel) Stats() map[uint8]QFIStatsSnapshot { return u.stats.Snapshot() }

// DLTEID reports the downlink TEID the view's lane is registered under.
func (u *UETunnel) DLTEID() uint32 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.dlTEID
}

// Rebind atomically moves the view's lane to newTEID on the same shared
// tunnel — the intra-gNB handover TEID swap (Demux.Rebind). Media rings and
// subscriptions ride along untouched.
func (u *UETunnel) Rebind(newTEID uint32) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.detached {
		return fmt.Errorf("rebind: UE view is detached/closed")
	}
	if err := u.st.demux.Rebind(u.dlTEID, newTEID); err != nil {
		return err
	}
	u.dlTEID = newTEID
	return nil
}

// Detach removes the view's lane from this tunnel's demux WITHOUT closing
// its subscriptions and returns the live lane and counters, for Register on
// the target gNB's SharedTunnel — the inter-gNB handover move. The view
// itself is dead afterwards (Close is a no-op).
func (u *UETunnel) Detach() (*UERx, *UEStats) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.detached {
		u.st.demux.Detach(u.dlTEID)
		u.detached = true
	}
	return u.rx, u.stats
}

// Close unregisters the view's lane, closing its subscriptions so blocked
// consumers wake with net.ErrClosed. The shared socket stays open for the
// gNB's other UEs; the owner (engine pool) closes the SharedTunnel when the
// last UE releases it. Safe to call twice or after Detach (no-op).
func (u *UETunnel) Close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.detached {
		return
	}
	u.detached = true
	u.st.demux.Unregister(u.dlTEID)
}
