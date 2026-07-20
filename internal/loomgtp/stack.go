// The per-gNB gVisor netstack bridge (design §2.2 + §6, Phase 6): ONE
// userspace TCP/IP stack per gNB carries the TCP apps (http, video) of every
// UE that gNB serves, over the same SharedTunnel socket the UDP (dgram) apps
// ride. VoIP never pays the gVisor cost; TCP apps get real SYN/TLS bytes as
// inner IP packets inside GTP-U.
//
//	loom app (httpx/vidstream)             GNBStack (one per gNB)
//	   │ net.Conn                             │
//	StackNetwork (retargetable facade) ─► netstack.Stack.Network(ueIP) view
//	                                          │ frames (complete inner IP)
//	   TX: stackTx ── inner SOURCE IP ─► that UE's Uplink (TEID/QFI stamped)
//	   RX: UERx.SetDefaultSink ─► ring ─► Stack's RxDatapath (arrival ts kept)
package loomgtp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	loomdp "github.com/bgrewell/loom/core/datapath"
	"github.com/bgrewell/loom/core/netpath"
	"github.com/bgrewell/loom/core/netstack"

	"github.com/bgrewell/orbit/internal/datapath"
)

// stackRingCapacity bounds the per-gNB stack's downlink feed. TCP bursts a
// full receive window at line rate — much deeper than a paced 20ms media
// lane — so this ring is 4× the per-UE media rings; overflow is drop-oldest
// with a counter (Ring semantics), which TCP reads as loss and repairs.
const stackRingCapacity = 1024

// GNBStack is one gNB's netstack bridge: a lazy, refcount-scoped (the engine
// keeps it beside the gNB's n3Pool entry and closes it when the shared tunnel
// closes) gVisor Stack hosting the PDU-session address of every attached UE.
//
//   - TX: the Stack's TxDatapath routes each outbound inner IP packet by its
//     SOURCE address to that UE's Uplink, which stamps the UE's own UL
//     TEID/QFI on the shared socket. Packets sourced from an address no UE
//     registered are dropped and counted (UnknownSourceDrops) — one stack
//     serves all UEs of a gNB, so an unroutable source is a bug or a race
//     with release, never silently someone else's tunnel.
//   - RX: each attached UE lane's default sink (everything the demux's
//     ICMP/UDP subscriptions did not claim — i.e. TCP) feeds one shared ring;
//     the Stack's RxDatapath polls it, preserving the demux reader's
//     socket-read arrival timestamps into loom Frame.Meta (ADR-0020). Kept,
//     not yet consumed: loom's netstack link endpoint ignores Frame.Meta
//     when injecting packets today, so the stamp matters to TCP only once
//     loom reads it — the seam is pinned by TestAttachSinkPreservesArrivalStamp
//     so it is already correct when that lands.
//
// Handover: intra-gNB moves are invisible (the TEID swap happens below the
// stack, in the UE's Uplink and the shared Demux). Inter-gNB moves relocate
// the UE's address between stacks — see the engine's rebindLocked and the
// StackNetwork facade.
type GNBStack struct {
	stack *netstack.Stack
	tx    *stackTx
	ring  *datapath.Ring

	mu     sync.Mutex
	lanes  map[netip.Addr]*datapath.UERx
	closed bool
}

// NewGNBStack builds the bridge with its gVisor stack. innerMTU bounds inner
// IP packets (<= 0 uses DefaultInnerMTU, the ~1400-byte GTP-U inner MTU of
// design §2.2 — computed the same way as the dgram path).
func NewGNBStack(innerMTU int) (*GNBStack, error) {
	if innerMTU <= 0 {
		innerMTU = DefaultInnerMTU
	}
	ring := datapath.NewRing(stackRingCapacity)
	tx := newStackTx(innerMTU)
	// The Stack owns tx and rx: its Close closes both (closing the ring wakes
	// the receive loop). Frames are sized innerMTU, satisfying netstack.New's
	// frame-size >= MTU invariant.
	st, err := netstack.New(netstack.Config{MTU: innerMTU}, tx, newRx(ring, nil))
	if err != nil {
		ring.Close()
		return nil, fmt.Errorf("loomgtp: build per-gNB netstack: %w", err)
	}
	return &GNBStack{
		stack: st,
		tx:    tx,
		ring:  ring,
		lanes: make(map[netip.Addr]*datapath.UERx),
	}, nil
}

// Attach adds one UE to the stack: AddAddress(ueIP) (the session's first TCP
// app), the ueIP→uplink route for TX, and the lane's default sink for RX.
// up must stamp the UE's CURRENT UL TEID/QFI per packet (the engine passes
// the *Session, which follows handover rebinds); lane is the UE's live
// downlink lane on the gNB's shared Demux.
func (g *GNBStack) Attach(ueIP net.IP, up Uplink, lane *datapath.UERx) error {
	if up == nil || lane == nil {
		return errors.New("loomgtp: Attach requires an Uplink and a UERx lane")
	}
	a, err := ip4Addr(ueIP)
	if err != nil {
		return err
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return net.ErrClosed
	}
	if _, dup := g.lanes[a]; dup {
		g.mu.Unlock()
		return fmt.Errorf("loomgtp: UE %s already attached to this gNB stack", a)
	}
	if err := g.stack.AddAddress(a); err != nil {
		g.mu.Unlock()
		return err
	}
	g.tx.setUplink(a, up)
	g.lanes[a] = lane
	g.mu.Unlock()
	// Sink installed last, after the address and uplink route exist, so the
	// stack never sees traffic it cannot answer. Runs on the demux reader;
	// Ring.Push never blocks (drop-oldest), preserving the arrival stamp.
	lane.SetDefaultSink(func(innerIP []byte, arrival time.Time) {
		g.ring.Push(innerIP, arrival)
	})
	return nil
}

// Detach removes one UE from the stack — the UE-release (and inter-gNB
// handover source-side) cleanup: the lane's default sink reverts to
// drop-and-count, the address leaves the stack, and the ueIP→uplink route is
// removed. Removing the address ABORTS any live TCP conns and listeners
// bound to it (loom netstack.Stack.RemoveAddress closes them outright rather
// than leaving zombies retransmitting to RTO exhaustion) — the honest
// semantics of a UE leaving this gNB. The abort is LOCAL-only: gVisor
// removes the address before the endpoints close, so the RST/FIN the closes
// would emit has no route and is dropped inside the stack — nothing reaches
// the wire, and the N6 far end holds a half-open connection until its own
// keepalive/idle timeout reaps it. Detaching an address that is not attached
// is a no-op.
func (g *GNBStack) Detach(ueIP net.IP) {
	a, err := ip4Addr(ueIP)
	if err != nil {
		return
	}
	g.mu.Lock()
	lane, ok := g.lanes[a]
	delete(g.lanes, a)
	closed := g.closed
	g.mu.Unlock()
	if !ok {
		return
	}
	// Order: stop feeding RX first, then remove the address (aborting its
	// conns), and drop the TX route last — a dispatcher mid-batch keeps its
	// uplink route until the aborts complete, so teardown never turns an
	// unrelated in-flight frame into an UnknownSourceDrops miscount. (No
	// abort segment rides that route: see the Detach comment above.)
	lane.SetDefaultSink(nil)
	if !closed {
		_ = g.stack.RemoveAddress(a)
	}
	g.tx.clearUplink(a)
}

// Attached reports whether ueIP currently has an address on this stack.
func (g *GNBStack) Attached(ueIP net.IP) bool {
	a, err := ip4Addr(ueIP)
	if err != nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.lanes[a]
	return ok
}

// Network mints a source-bound netpath.Network view of the stack for an
// attached UE (netstack.Stack.Network(ueIP), design §2.2): DialContext binds
// ueIP as the connection source, Listen binds on it. Closing the view closes
// only the conns created through it, never the stack. The engine wraps views
// in a StackNetwork facade so inter-gNB handovers can retarget them.
func (g *GNBStack) Network(ueIP net.IP) (netpath.Network, error) {
	a, err := ip4Addr(ueIP)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	_, ok := g.lanes[a]
	closed := g.closed
	g.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	if !ok {
		return nil, fmt.Errorf("loomgtp: UE %s is not attached to this gNB stack", a)
	}
	return g.stack.Network(a), nil
}

// UnknownSourceDrops counts outbound packets dropped because their inner
// source IP matched no attached UE.
func (g *GNBStack) UnknownSourceDrops() uint64 { return g.tx.unknownDrops.Load() }

// UplinkSendErrors counts outbound packets dropped because the owning UE's
// uplink send failed (e.g. its data path tore down mid-flight). Dropped, not
// fatal: one UE's dead tunnel must not stall the whole gNB's stack.
func (g *GNBStack) UplinkSendErrors() uint64 { return g.tx.sendErrs.Load() }

// RxDrops counts downlink packets the stack's feed ring evicted under
// overload (drop-oldest; TCP repairs the loss).
func (g *GNBStack) RxDrops() uint64 { return g.ring.Drops() }

// Close tears the bridge down: every remaining lane's sink reverts to
// drop-and-count, then the gVisor stack (aborting all conns, waiting out its
// workers) and its datapaths close. The engine calls it when the gNB's
// shared tunnel closes (last session released the n3Pool entry). Idempotent.
func (g *GNBStack) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	lanes := make([]*datapath.UERx, 0, len(g.lanes))
	for _, l := range g.lanes {
		lanes = append(lanes, l)
	}
	g.lanes = make(map[netip.Addr]*datapath.UERx)
	g.mu.Unlock()
	for _, l := range lanes {
		l.SetDefaultSink(nil)
	}
	return g.stack.Close()
}

// stackTx is the per-gNB stack's TxDatapath (Capabilities.RawL3): every
// committed frame is a complete inner IP packet gVisor built, routed to the
// owning UE's uplink by INNER SOURCE ADDRESS. Frames whose source matches no
// attached UE — and frames whose uplink send fails — are consumed and
// counted, never returned as datapath errors: to the stack the tunnel is a
// lossy link, and one UE's teardown race must not abort another UE's batch
// (gVisor's transports retransmit through loss; an aborted batch would tear
// the NIC's whole write path).
//
// The committer (netstack's dpEndpoint) serializes TxReserve/TxCommit pairs
// under its own mutex, so one reused frame pool is safe — same contract as
// rawTx.
type stackTx struct {
	mu   sync.RWMutex
	ups  map[netip.Addr]Uplink
	pool []loomdp.Frame

	unknownDrops atomic.Uint64
	sendErrs     atomic.Uint64
}

func newStackTx(frameSize int) *stackTx {
	if frameSize <= 0 {
		frameSize = DefaultInnerMTU
	}
	t := &stackTx{ups: make(map[netip.Addr]Uplink), pool: make([]loomdp.Frame, poolDepth)}
	for i := range t.pool {
		t.pool[i].Data = make([]byte, frameSize)
	}
	return t
}

func (t *stackTx) setUplink(a netip.Addr, up Uplink) {
	t.mu.Lock()
	t.ups[a] = up
	t.mu.Unlock()
}

func (t *stackTx) clearUplink(a netip.Addr) {
	t.mu.Lock()
	delete(t.ups, a)
	t.mu.Unlock()
}

func (t *stackTx) uplink(a netip.Addr) Uplink {
	t.mu.RLock()
	up := t.ups[a]
	t.mu.RUnlock()
	return up
}

func (t *stackTx) Name() string { return "orbit-gtp-netstack" }
func (t *stackTx) Caps() loomdp.Capabilities {
	return loomdp.Capabilities{RawL3: true} // frames are complete IP packets
}
func (t *stackTx) Close() error { return nil }

func (t *stackTx) TxReserve(n int) []loomdp.Frame {
	if n > len(t.pool) {
		n = len(t.pool)
	}
	for i := 0; i < n; i++ {
		t.pool[i].Len = 0
	}
	return t.pool[:n]
}

func (t *stackTx) TxCommit(frames []loomdp.Frame) (int, error) {
	sent := 0
	for i := range frames {
		if frames[i].Len == 0 {
			continue
		}
		pkt := frames[i].Data[:frames[i].Len]
		src, ok := innerIPv4Src(pkt)
		if !ok {
			t.unknownDrops.Add(1)
			sent++ // consumed (dropped): see the type comment
			continue
		}
		up := t.uplink(src)
		if up == nil {
			t.unknownDrops.Add(1)
			sent++
			continue
		}
		if err := up.SendUplink(pkt); err != nil {
			t.sendErrs.Add(1)
			sent++
			continue
		}
		sent++
	}
	return sent, nil
}

// innerIPv4Src extracts the source address of a complete IPv4 packet. The
// stack is IPv4-only today (netstack.AddAddress refuses IPv6), so anything
// else is unroutable here.
func innerIPv4Src(pkt []byte) (netip.Addr, bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte(pkt[12:16])), true
}

// ip4Addr normalizes a UE PDU-session address to a netip IPv4 address.
func ip4Addr(ip net.IP) (netip.Addr, error) {
	ip4 := ip.To4()
	if ip4 == nil {
		return netip.Addr{}, fmt.Errorf("loomgtp: UE IP %v is not IPv4 (the netstack bridge is IPv4-only)", ip)
	}
	a, ok := netip.AddrFromSlice(ip4)
	if !ok {
		return netip.Addr{}, fmt.Errorf("loomgtp: invalid UE IP %v", ip)
	}
	return a, nil
}

// StackNetwork is the retargetable netpath.Network facade the engine hands
// to TCP app sessions: it delegates to the UE's CURRENT source-bound stack
// view and survives inter-gNB handovers, where the UE's address moves to the
// target gNB's stack and the underlying view must be swapped (Retarget).
//
// Cross-gNB moves and TCP (stated per loom's RemoveAddress semantics, which
// the engine invokes via GNBStack.Detach): connections that were LIVE on the
// address during the move window are ABORTED — gVisor closes them outright
// when the address leaves the source stack — so in-flight requests fail and
// the app must reconnect; that reconnect, through this facade, lands on the
// target gNB's stack and succeeds. An address with no live conns moves with
// no TCP-visible effect at all. Intra-gNB handovers never reach this type:
// the TEID swap happens below the stack and TCP sees only delay/loss. The
// engine emits a correlation-visible event (TCP_CONNS_RESET) when a move
// aborted live conns.
type StackNetwork struct {
	mu     sync.Mutex
	cur    netpath.Network // current source-bound stack view
	closed bool
	live   map[*facadeTracked]struct{} // conns + listeners opened through the facade
	// onClose lets the owner (the engine Session) drop its retarget
	// bookkeeping when the app closes the facade; nil-safe, called once.
	onClose func(*StackNetwork)
}

var _ netpath.Network = (*StackNetwork)(nil)

// NewStackNetwork wraps one stack view. onClose, if non-nil, runs exactly
// once when Close is called (not on Retarget).
func NewStackNetwork(view netpath.Network, onClose func(*StackNetwork)) *StackNetwork {
	return &StackNetwork{cur: view, live: make(map[*facadeTracked]struct{}), onClose: onClose}
}

// Name implements netpath.Network.
func (n *StackNetwork) Name() string { return "orbit-gtp-netstack" }

// view returns the current delegate, or an error when the facade is closed.
func (n *StackNetwork) view() (netpath.Network, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, net.ErrClosed
	}
	return n.cur, nil
}

// track registers a closer opened through the facade; if the facade closed
// concurrently the closer is closed and an error returned.
func (n *StackNetwork) track(c interface{ Close() error }) (*facadeTracked, error) {
	ft := &facadeTracked{n: n, c: c}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		_ = c.Close()
		return nil, net.ErrClosed
	}
	n.live[ft] = struct{}{}
	n.mu.Unlock()
	return ft, nil
}

// DialContext implements netpath.Network via the current stack view.
func (n *StackNetwork) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	v, err := n.view()
	if err != nil {
		return nil, err
	}
	c, err := v.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	ft, err := n.track(c)
	if err != nil {
		return nil, err
	}
	return &facadeConn{Conn: c, t: ft}, nil
}

// ListenPacket implements netpath.Network via the current stack view.
func (n *StackNetwork) ListenPacket(network, address string) (net.PacketConn, error) {
	v, err := n.view()
	if err != nil {
		return nil, err
	}
	pc, err := v.ListenPacket(network, address)
	if err != nil {
		return nil, err
	}
	ft, err := n.track(pc)
	if err != nil {
		return nil, err
	}
	return &facadePacketConn{PacketConn: pc, t: ft}, nil
}

// Listen implements netpath.Network via the current stack view. Conns the
// listener accepts are tracked like dialed ones.
func (n *StackNetwork) Listen(network, address string) (net.Listener, error) {
	v, err := n.view()
	if err != nil {
		return nil, err
	}
	ln, err := v.Listen(network, address)
	if err != nil {
		return nil, err
	}
	ft, err := n.track(ln)
	if err != nil {
		return nil, err
	}
	return &facadeListener{Listener: ln, n: n, t: ft}, nil
}

// Retarget swaps the facade onto a new stack view (the inter-gNB handover
// move) and returns how many conns/listeners opened through the facade were
// still live at the swap — the ones the address move aborted. The old view
// is closed (its conns are already dead: RemoveAddress on the source stack
// aborted them); new dials and listens go to the new view. Retargeting a
// closed facade closes the new view and reports 0.
func (n *StackNetwork) Retarget(view netpath.Network) int {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		if view != nil {
			_ = view.Close()
		}
		return 0
	}
	old := n.cur
	n.cur = view
	aborted := len(n.live)
	n.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return aborted
}

// LiveConns reports the conns/listeners opened through the facade and not
// yet closed.
func (n *StackNetwork) LiveConns() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.live)
}

// Close implements netpath.Network: it closes the current view (and with it
// every conn/listener created through the facade) and runs the onClose hook.
// It does NOT remove the UE's address from the stack — the address lives
// until UE release (engine teardown), so the next TCP app on the same
// session attaches nothing and just mints a fresh facade.
func (n *StackNetwork) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	cur := n.cur
	n.cur = nil
	n.live = nil
	onClose := n.onClose
	n.onClose = nil
	n.mu.Unlock()
	var err error
	if cur != nil {
		err = cur.Close()
	}
	if onClose != nil {
		onClose(n)
	}
	return err
}

// drop unregisters a tracked closer (its owner was closed by the app).
func (n *StackNetwork) drop(ft *facadeTracked) {
	n.mu.Lock()
	if n.live != nil {
		delete(n.live, ft)
	}
	n.mu.Unlock()
}

// facadeTracked ties one conn/listener to its facade for live accounting.
type facadeTracked struct {
	n    *StackNetwork
	c    interface{ Close() error }
	once sync.Once
	err  error
}

func (ft *facadeTracked) close() error {
	ft.once.Do(func() {
		ft.n.drop(ft)
		ft.err = ft.c.Close()
	})
	return ft.err
}

// facadeConn / facadePacketConn / facadeListener untrack themselves on Close.
type facadeConn struct {
	net.Conn
	t *facadeTracked
}

func (c *facadeConn) Close() error { return c.t.close() }

type facadePacketConn struct {
	net.PacketConn
	t *facadeTracked
}

func (c *facadePacketConn) Close() error { return c.t.close() }

type facadeListener struct {
	net.Listener
	n *StackNetwork
	t *facadeTracked
}

func (l *facadeListener) Close() error { return l.t.close() }

// Accept tracks accepted conns too, so server-side sockets count as live
// across a move like dialed ones.
func (l *facadeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	ft, terr := l.n.track(c)
	if terr != nil {
		return nil, terr
	}
	return &facadeConn{Conn: c, t: ft}, nil
}
