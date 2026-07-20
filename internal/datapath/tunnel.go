// Package datapath carries user-plane packets over N3. Phase-1b scope:
// Stage-1 userspace GTP-U (DESIGN §5d) — a single UDP socket per gNB that
// encapsulates uplink IP packets to the UPF and decapsulates downlink,
// keyed by TEID. This is the "native flow" path (craft/consume packets in
// process); the per-UE TUN (Mode A) layers on top of the same tunnel.
package datapath

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bgrewell/orbit/internal/gtpu"
)

// ErrDownlinkOwned is returned by ReadDownlink once Demux() has been called:
// the demux reader goroutine is the socket's only reader (design §6), and
// downlink must be consumed via UERx subscriptions.
var ErrDownlinkOwned = errors.New("tunnel downlink is owned by its Demux; consume via UERx subscriptions")

// Tunnel is one gNB N3 endpoint. Uplink packets are sent to the UPF with the
// UPF-allocated uplink TEID; downlink packets addressed to the gNB's
// downlink TEID are decapsulated. Per-QFI counters track both directions.
type Tunnel struct {
	conn    *net.UDPConn
	upfAddr *net.UDPAddr
	ulTEID  uint32 // UPF's uplink TEID (destination of our uplink)
	dlTEID  uint32 // our downlink TEID (the UPF sends to this)
	qfi     uint8

	mu    sync.Mutex
	stats map[uint8]*QFIStats

	// readMu serializes direct downlink reads (ReadDownlink) against the
	// demux handoff: Demux() acquires it after waking any blocked reader, so
	// the demux reader goroutine never coexists with a direct read (§6
	// single-reader invariant — enforced, not just documented).
	readMu    sync.Mutex
	demuxOnce sync.Once
	demuxed   atomic.Bool
	demux     *Demux
}

// QFIStats holds per-QoS-flow byte and packet counters.
type QFIStats struct {
	UplinkPackets   atomic.Uint64
	UplinkBytes     atomic.Uint64
	DownlinkPackets atomic.Uint64
	DownlinkBytes   atomic.Uint64
}

// Config parameters a tunnel. LocalN3 is the gNB N3 address (host:port) to
// bind — it must be the address reported to the UPF in PDU Session Resource
// Setup Response so downlink returns here. UPFN3 is the UPF N3 endpoint.
type Config struct {
	LocalN3 string // e.g. "172.17.50.13:2152"
	UPFN3   string // e.g. "172.17.50.241:2152"
	ULTEID  uint32
	DLTEID  uint32
	QFI     uint8
}

// NewTunnel binds the N3 socket and prepares encapsulation.
func NewTunnel(cfg Config) (*Tunnel, error) {
	laddr, err := net.ResolveUDPAddr("udp", cfg.LocalN3)
	if err != nil {
		return nil, fmt.Errorf("resolve local N3 %q: %w", cfg.LocalN3, err)
	}
	raddr, err := net.ResolveUDPAddr("udp", cfg.UPFN3)
	if err != nil {
		return nil, fmt.Errorf("resolve UPF N3 %q: %w", cfg.UPFN3, err)
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, fmt.Errorf("bind N3 socket %q: %w", cfg.LocalN3, err)
	}
	return &Tunnel{
		conn: conn, upfAddr: raddr,
		ulTEID: cfg.ULTEID, dlTEID: cfg.DLTEID, qfi: cfg.QFI,
		stats: map[uint8]*QFIStats{cfg.QFI: {}},
	}, nil
}

// SendUplink encapsulates an inner IP packet and sends it to the UPF.
func (t *Tunnel) SendUplink(innerIP []byte) error {
	pkt := gtpu.EncodeGPDU(t.ulTEID, t.qfi, innerIP)
	if _, err := t.conn.WriteToUDP(pkt, t.upfAddr); err != nil {
		return fmt.Errorf("send uplink G-PDU: %w", err)
	}
	s := t.qfiStats(t.qfi)
	s.UplinkPackets.Add(1)
	s.UplinkBytes.Add(uint64(len(innerIP)))
	return nil
}

// ReadDownlink blocks up to timeout for one downlink G-PDU addressed to this
// tunnel's downlink TEID and returns the inner IP packet. G-PDUs for a
// different TEID are skipped (defence against cross-talk on a shared UPF).
func (t *Tunnel) ReadDownlink(timeout time.Duration) ([]byte, error) {
	// Hold readMu for the whole read so Demux() can wait out (after waking) a
	// reader already blocked in ReadFromUDP — otherwise that reader would keep
	// stealing G-PDUs from the demux lanes.
	t.readMu.Lock()
	defer t.readMu.Unlock()
	if t.demuxed.Load() {
		return nil, ErrDownlinkOwned
	}
	deadline := time.Now().Add(timeout)
	if err := t.conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	buf := make([]byte, 65536)
	for {
		n, _, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if t.demuxed.Load() {
				// Demux() force-woke this read to take ownership.
				return nil, ErrDownlinkOwned
			}
			return nil, err
		}
		g, err := gtpu.DecodeGPDU(buf[:n])
		if err != nil {
			continue // ignore malformed frames rather than failing the flow
		}
		if g.TEID != t.dlTEID || len(g.Payload) == 0 {
			continue
		}
		qfi := t.qfi
		if g.HasQFI {
			qfi = g.QFI
		}
		s := t.qfiStats(qfi)
		s.DownlinkPackets.Add(1)
		s.DownlinkBytes.Add(uint64(len(g.Payload)))
		out := make([]byte, len(g.Payload))
		copy(out, g.Payload)
		return out, nil
	}
}

// Stats returns a snapshot of the per-QFI counters.
func (t *Tunnel) Stats() map[uint8]QFIStatsSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[uint8]QFIStatsSnapshot, len(t.stats))
	for qfi, s := range t.stats {
		out[qfi] = QFIStatsSnapshot{
			UplinkPackets:   s.UplinkPackets.Load(),
			UplinkBytes:     s.UplinkBytes.Load(),
			DownlinkPackets: s.DownlinkPackets.Load(),
			DownlinkBytes:   s.DownlinkBytes.Load(),
		}
	}
	return out
}

// QFIStatsSnapshot is a point-in-time copy of a flow's counters.
type QFIStatsSnapshot struct {
	UplinkPackets, UplinkBytes     uint64
	DownlinkPackets, DownlinkBytes uint64
}

// Demux hands the tunnel's downlink read path to a Demux (created on first
// call, then shared): its reader goroutine becomes the socket's only reader
// and ReadDownlink is disabled. Per-QFI downlink counters keep flowing —
// Stats() is unchanged — via the demux's accounting hook. Uplink
// (SendUplink) is unaffected. Closing the tunnel stops the demux.
func (t *Tunnel) Demux() *Demux {
	t.demuxOnce.Do(func() {
		t.demuxed.Store(true)
		// Wake a reader blocked inside ReadDownlink's ReadFromUDP, then wait
		// for it to leave (readMu) so the demux goroutine is the socket's
		// only reader from its very first poll.
		_ = t.conn.SetReadDeadline(time.Now())
		t.readMu.Lock()
		defer t.readMu.Unlock()
		t.demux = NewDemux(t.conn, WithDownlinkStats(func(qfi uint8, hasQFI bool, payloadBytes int) {
			q := t.qfi
			if hasQFI {
				q = qfi
			}
			s := t.qfiStats(q)
			s.DownlinkPackets.Add(1)
			s.DownlinkBytes.Add(uint64(payloadBytes))
		}))
	})
	return t.demux
}

// Close releases the N3 socket (stopping any Demux reader with it).
func (t *Tunnel) Close() error { return t.conn.Close() }

func (t *Tunnel) qfiStats(qfi uint8) *QFIStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.stats[qfi]
	if !ok {
		s = &QFIStats{}
		t.stats[qfi] = s
	}
	return s
}
