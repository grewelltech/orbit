// Package datapath carries user-plane packets over N3: Stage-1 userspace
// GTP-U (DESIGN §5d). Since the Phase-5 cutover the primary surface is the
// per-gNB SharedTunnel — ONE UDP socket per gNB N3 address whose Demux owns
// the downlink read path — with per-UE views (UETunnel) that stamp their own
// uplink TEID/QFI and subscribe downlink lanes. The engine's sessions share
// those sockets through a refcounted pool; nothing binds a gNB's port 2152
// twice.
package datapath

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ErrDownlinkOwned is returned by ReadDownlink once Demux() has been called:
// downlink consumers register lanes with the Demux (design §6) instead of
// draining the tunnel's single legacy read queue.
var ErrDownlinkOwned = errors.New("tunnel downlink is owned by its Demux; consume via UERx subscriptions")

// Tunnel is the retired per-session N3 endpoint, reduced to the shared
// implementation's single-user case: one SharedTunnel with exactly one UE
// registered. It keeps the classic surface (SendUplink / ReadDownlink /
// Stats / Demux) for single-flow tools and tests; the engine no longer uses
// it — Sessions ride per-gNB SharedTunnels via the Manager's pool, which is
// what removed the port-2152 collision between UEs.
type Tunnel struct {
	st   *SharedTunnel
	ue   *UETunnel
	conn *net.UDPConn // the shared socket (test convenience: LocalAddr)

	// mu guards the legacy single-consumer read queue. ReadDownlink drains a
	// catch-all ring fed by the lane's default sink; Demux() retires it so
	// lane subscriptions become the only downlink surface (§6 single-owner
	// invariant, enforced: a blocked ReadDownlink is woken with
	// ErrDownlinkOwned, never left racing the lanes).
	mu      sync.Mutex
	legacy  *Ring
	demuxed atomic.Bool
}

// QFIStats holds per-QoS-flow byte and packet counters.
type QFIStats struct {
	UplinkPackets   atomic.Uint64
	UplinkBytes     atomic.Uint64
	DownlinkPackets atomic.Uint64
	DownlinkBytes   atomic.Uint64
}

// QFIStatsSnapshot is a point-in-time copy of a flow's counters.
type QFIStatsSnapshot struct {
	UplinkPackets, UplinkBytes     uint64
	DownlinkPackets, DownlinkBytes uint64
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

// NewTunnel binds the N3 socket (via a single-user SharedTunnel) and
// registers the one UE view. The legacy catch-all read queue is armed
// immediately so downlink arriving before the first ReadDownlink call is
// buffered, matching the old direct-read semantics.
func NewTunnel(cfg Config) (*Tunnel, error) {
	st, err := NewSharedTunnel(cfg.LocalN3, cfg.UPFN3)
	if err != nil {
		return nil, err
	}
	ue, err := st.Register(UETunnelConfig{ULTEID: cfg.ULTEID, DLTEID: cfg.DLTEID, QFI: cfg.QFI})
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	t := &Tunnel{st: st, ue: ue, conn: st.conn}
	t.legacy = NewRing(defaultRingCapacity)
	r := t.legacy
	ue.Lane().SetDefaultSink(func(innerIP []byte, arrival time.Time) { r.Push(innerIP, arrival) })
	return t, nil
}

// SendUplink encapsulates an inner IP packet and sends it to the UPF.
func (t *Tunnel) SendUplink(innerIP []byte) error { return t.ue.SendUplink(innerIP) }

// ReadDownlink blocks up to timeout for one downlink G-PDU addressed to this
// tunnel's downlink TEID and returns the inner IP packet (G-PDUs for other
// TEIDs are skipped by the demux — the classic cross-talk defence). Once
// Demux() has been called it returns ErrDownlinkOwned.
func (t *Tunnel) ReadDownlink(timeout time.Duration) ([]byte, error) {
	t.mu.Lock()
	r := t.legacy
	t.mu.Unlock()
	if t.demuxed.Load() {
		return nil, ErrDownlinkOwned
	}
	if r == nil {
		return nil, net.ErrClosed // tunnel closed
	}
	f, err := r.Read(timeout)
	if err != nil {
		if errors.Is(err, net.ErrClosed) && t.demuxed.Load() {
			// Demux() force-retired the legacy queue to take ownership.
			return nil, ErrDownlinkOwned
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// Preserve the classic direct-socket timeout surface: a deadline
			// read used to return an error satisfying net.Error with
			// Timeout() == true and errors.Is(err, os.ErrDeadlineExceeded).
			return nil, os.ErrDeadlineExceeded
		}
		return nil, err
	}
	return f.Payload, nil
}

// Stats returns a snapshot of the per-QFI counters.
func (t *Tunnel) Stats() map[uint8]QFIStatsSnapshot { return t.ue.Stats() }

// Demux hands the tunnel's downlink to lane subscriptions: the legacy
// ReadDownlink queue is retired (a blocked reader wakes with
// ErrDownlinkOwned) and the shared Demux — the socket's owner since
// construction — is returned. Per-QFI downlink counters keep flowing (the
// lane accounting hook); SendUplink is unaffected. Idempotent.
func (t *Tunnel) Demux() *Demux {
	t.mu.Lock()
	t.demuxed.Store(true)
	if t.legacy != nil {
		t.ue.Lane().SetDefaultSink(nil)
		t.legacy.Close()
		t.legacy = nil
	}
	t.mu.Unlock()
	return t.st.Demux()
}

// Close releases the N3 socket (stopping the Demux reader with it).
func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.legacy != nil {
		t.legacy.Close()
		t.legacy = nil
	}
	t.mu.Unlock()
	return t.st.Close()
}
