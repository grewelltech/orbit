package datapath

import (
	"fmt"
	"net"

	"github.com/bgrewell/orbit/internal/gtpu"
)

// SharedTunnel is one GTP-U N3 endpoint — a single UDP socket bound to a gNB's
// N3 address (host:2152) — that every UE served by that gNB sends uplink
// through, each with its own uplink TEID. It solves the port-2152 collision
// that arises when many UEs on one gNB would each bind their own socket, so a
// whole population can carry traffic concurrently.
//
// UDP writes are goroutine-safe, so per-UE flows write the shared socket
// concurrently. Downlink demux by TEID is a future addition; the fleet traffic
// flows are uplink throughput.
type SharedTunnel struct {
	conn    *net.UDPConn
	upfAddr *net.UDPAddr
}

// NewSharedTunnel binds localN3 (the gNB N3 host:port) and targets upfN3.
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
	return &SharedTunnel{conn: conn, upfAddr: raddr}, nil
}

// UEFlow returns an Uplink view that encapsulates with a UE's uplink TEID/QFI
// and writes the shared socket. Safe to use from many goroutines at once.
func (s *SharedTunnel) UEFlow(ulTEID uint32, qfi uint8) *UEFlow {
	return &UEFlow{s: s, ulTEID: ulTEID, qfi: qfi}
}

// Close closes the shared socket; all UE flows on it stop.
func (s *SharedTunnel) Close() error { return s.conn.Close() }

// UEFlow is one UE's uplink over a SharedTunnel. It satisfies the GTP-U uplink
// interface the loom bridge needs (SendUplink).
type UEFlow struct {
	s      *SharedTunnel
	ulTEID uint32
	qfi    uint8
}

// SendUplink wraps innerIP in a GTP-U G-PDU for this UE's TEID and sends it.
func (u *UEFlow) SendUplink(innerIP []byte) error {
	if _, err := u.s.conn.WriteToUDP(gtpu.EncodeGPDU(u.ulTEID, u.qfi, innerIP), u.s.upfAddr); err != nil {
		return fmt.Errorf("send uplink G-PDU: %w", err)
	}
	return nil
}
