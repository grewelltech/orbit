package datapath

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bgrewell/orbit/internal/gtpu"
)

// N3 downlink demultiplexer (design §6). Invariant: at any time, exactly one
// reader owns a given N3 socket, and consumers register with its Demux —
// never read the socket directly. Phase 4 layers the Demux on the existing
// per-session Tunnel socket (Tunnel.Demux); Phase 5 moves it onto the
// per-gNB SharedTunnel conn. The dispatch chain is
//
//	socket → Demux (TEID → UERx) → UERx (inner IP proto/port → Ring) → consumer
//
// so the ICMP latency probe and media lanes share one downlink from day one.

// Demux owns the single reader goroutine on one N3 UDP socket and routes
// downlink G-PDUs to per-UE lanes by downlink TEID. G-PDUs for a TEID with no
// registered lane are skipped and counted (the Tunnel.ReadDownlink cross-talk
// defence, made observable); malformed frames are skipped and counted.
type Demux struct {
	conn  *net.UDPConn
	stats func(qfi uint8, hasQFI bool, payloadBytes int)

	mu    sync.RWMutex
	lanes map[uint32]*UERx

	teidMisses atomic.Uint64
	decodeErrs atomic.Uint64

	closed atomic.Bool
	done   chan struct{}
}

// DemuxOption configures a Demux.
type DemuxOption func(*Demux)

// WithDownlinkStats installs an accounting hook called from the reader
// goroutine for every accepted downlink G-PDU (registered TEID, non-empty
// payload). It is the seam that keeps Tunnel.Stats() per-QFI counters flowing
// once the Demux owns the tunnel's read path.
func WithDownlinkStats(f func(qfi uint8, hasQFI bool, payloadBytes int)) DemuxOption {
	return func(d *Demux) { d.stats = f }
}

// NewDemux takes ownership of conn's read path and starts the reader
// goroutine. It wraps either a legacy Tunnel's conn (Phase 4, via
// Tunnel.Demux) or the SharedTunnel conn (Phase 5+). After this, nothing else
// may read conn.
func NewDemux(conn *net.UDPConn, opts ...DemuxOption) *Demux {
	d := &Demux{
		conn:  conn,
		lanes: make(map[uint32]*UERx),
		done:  make(chan struct{}),
	}
	for _, o := range opts {
		o(d)
	}
	// Clear any deadline a previous direct reader (Tunnel.ReadDownlink) left.
	_ = conn.SetReadDeadline(time.Time{})
	go d.run()
	return d
}

// Register returns the per-UE downlink lane for dlTEID, creating it if
// needed. Phase 4 has one UE per socket; the map is the Phase-5 multi-UE
// seam.
func (d *Demux) Register(dlTEID uint32) *UERx {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rx, ok := d.lanes[dlTEID]; ok {
		return rx
	}
	rx := newUERx()
	d.lanes[dlTEID] = rx
	return rx
}

// Unregister removes the lane for dlTEID and closes its subscriptions,
// waking any blocked consumers.
func (d *Demux) Unregister(dlTEID uint32) {
	d.mu.Lock()
	rx := d.lanes[dlTEID]
	delete(d.lanes, dlTEID)
	d.mu.Unlock()
	if rx != nil {
		rx.close()
	}
}

// Detach removes the lane registered at dlTEID WITHOUT closing its
// subscriptions, and returns it. It is the cross-socket half of the handover
// data-path move (Rebind covers the same-socket TEID swap): the caller
// re-attaches the live lane — rings, End-Marker callback and all — to the
// target tunnel's Demux via Attach, so consumers blocked on the lane's rings
// never observe a close. A lane detached from a Demux that is later closed
// stays open.
func (d *Demux) Detach(dlTEID uint32) (*UERx, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rx, ok := d.lanes[dlTEID]
	if ok {
		delete(d.lanes, dlTEID)
	}
	return rx, ok
}

// Attach registers an existing lane (typically one Detach returned from
// another Demux) under dlTEID. It refuses a TEID that already has a lane.
func (d *Demux) Attach(dlTEID uint32, rx *UERx) error {
	if rx == nil {
		return errors.New("attach: nil lane")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, taken := d.lanes[dlTEID]; taken {
		return fmt.Errorf("attach: TEID %#x already registered", dlTEID)
	}
	d.lanes[dlTEID] = rx
	return nil
}

// Rebind atomically moves the lane registered at oldTEID to newTEID — the
// handover TEID swap. Packets never see an intermediate state: before the
// swap they route (or miss) via oldTEID, after it via newTEID.
func (d *Demux) Rebind(oldTEID, newTEID uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	rx, ok := d.lanes[oldTEID]
	if !ok {
		return fmt.Errorf("rebind: no lane registered for TEID %#x", oldTEID)
	}
	if oldTEID == newTEID {
		return nil
	}
	if _, taken := d.lanes[newTEID]; taken {
		return fmt.Errorf("rebind: TEID %#x already registered", newTEID)
	}
	delete(d.lanes, oldTEID)
	d.lanes[newTEID] = rx
	return nil
}

// TEIDMisses counts downlink G-PDUs (and End Markers) whose TEID matched no
// registered lane — skipped, per the original ReadDownlink semantics.
func (d *Demux) TEIDMisses() uint64 { return d.teidMisses.Load() }

// DecodeErrors counts frames that failed GTP-U decoding (skipped).
func (d *Demux) DecodeErrors() uint64 { return d.decodeErrs.Load() }

// Close stops the reader goroutine and waits for it to exit. It does not
// close the underlying socket (the Tunnel/SharedTunnel owns that); closing
// the socket also stops the reader.
func (d *Demux) Close() error {
	if !d.closed.Swap(true) {
		_ = d.conn.SetReadDeadline(time.Now()) // wake a blocked read
	}
	<-d.done
	return nil
}

// run is the single reader. It exits when the socket closes or Close is
// called; per-packet errors and unroutable frames are counted, never fatal.
// On exit every lane's subscriptions are closed so blocked consumers wake.
func (d *Demux) run() {
	defer func() {
		d.mu.Lock()
		lanes := make([]*UERx, 0, len(d.lanes))
		for _, rx := range d.lanes {
			lanes = append(lanes, rx)
		}
		d.mu.Unlock()
		for _, rx := range lanes {
			rx.close()
		}
		close(d.done)
	}()
	buf := make([]byte, 65536)
	for {
		n, _, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			if d.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue // transient per-packet error (e.g. stray deadline)
		}
		arrival := time.Now()
		g, err := gtpu.DecodeGPDU(buf[:n])
		if err != nil {
			d.decodeErrs.Add(1)
			continue
		}
		switch g.MsgType {
		case gtpu.MsgTypeEndMarker:
			if rx := d.lane(g.TEID); rx != nil {
				rx.noteEndMarker(arrival)
			} else {
				d.teidMisses.Add(1)
			}
		case gtpu.MsgTypeGPDU:
			if len(g.Payload) == 0 {
				continue
			}
			rx := d.lane(g.TEID)
			if rx == nil {
				d.teidMisses.Add(1)
				continue
			}
			if d.stats != nil {
				d.stats(g.QFI, g.HasQFI, len(g.Payload))
			}
			pkt := make([]byte, len(g.Payload))
			copy(pkt, g.Payload)
			rx.dispatch(pkt, arrival)
		default:
			// Echo/Error Indication etc. — no user payload; skip silently,
			// matching ReadDownlink.
		}
	}
}

func (d *Demux) lane(teid uint32) *UERx {
	d.mu.RLock()
	rx := d.lanes[teid]
	d.mu.RUnlock()
	return rx
}

// UERx is one UE's downlink lane: it dispatches inner IP packets by protocol
// and (for UDP) destination port to bounded Rings. Packets that match no
// subscription go to the default sink (the future netstack InjectInbound,
// Phase 6); with no sink set they are dropped and counted.
//
// Dispatched packet slices are shared read-only between ICMP subscribers and
// must not be mutated.
type UERx struct {
	mu          sync.RWMutex
	icmp        []*Ring
	udp         map[uint16]*Ring
	udpAll      *Ring // wildcard UDP lane (SubscribeUDPAll); per-port lanes win
	defaultSink func(innerIP []byte)
	endMarkerFn func(arrival time.Time)

	defaultDrops atomic.Uint64
	endMarkers   atomic.Uint64
}

func newUERx() *UERx {
	return &UERx{udp: make(map[uint16]*Ring)}
}

// defaultRingCapacity bounds each subscription. 256 frames is ~5 s of a
// 20 ms-ptime voice stream — deep enough to absorb consumer scheduling
// hiccups, small enough that a stuck consumer costs bounded memory.
const defaultRingCapacity = 256

// SubscribeICMP returns a new ring receiving every inner ICMP packet.
// Multiple subscribers each get every packet (the ping RPC and the latency
// probe filter by their own echo IDs). Pair with UnsubscribeICMP.
func (u *UERx) SubscribeICMP() *Ring {
	r := NewRing(defaultRingCapacity)
	u.mu.Lock()
	// Copy-on-write so dispatch can fan out from a snapshot without the lock.
	u.icmp = append(append([]*Ring(nil), u.icmp...), r)
	u.mu.Unlock()
	return r
}

// UnsubscribeICMP removes and closes a ring returned by SubscribeICMP.
func (u *UERx) UnsubscribeICMP(r *Ring) {
	u.mu.Lock()
	keep := make([]*Ring, 0, len(u.icmp))
	for _, x := range u.icmp {
		if x != r {
			keep = append(keep, x)
		}
	}
	u.icmp = keep
	u.mu.Unlock()
	r.Close()
}

// SubscribeUDP returns a ring receiving inner UDP packets addressed to
// dstPort (one consumer per port — a media lane). Subscribing a port that is
// already subscribed replaces (and closes) the previous ring.
func (u *UERx) SubscribeUDP(dstPort uint16) *Ring {
	r := NewRing(defaultRingCapacity)
	u.mu.Lock()
	old := u.udp[dstPort]
	u.udp[dstPort] = r
	u.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return r
}

// UnsubscribeUDP removes and closes the lane for dstPort, if any.
func (u *UERx) UnsubscribeUDP(dstPort uint16) {
	u.mu.Lock()
	old := u.udp[dstPort]
	delete(u.udp, dstPort)
	u.mu.Unlock()
	if old != nil {
		old.Close()
	}
}

// SubscribeUDPAll returns a ring receiving every inner UDP packet that no
// per-port lane (SubscribeUDP) claims — the feed for a consumer that does its
// own port demultiplexing, i.e. the loomgtp RxDatapath behind loom's dgram
// network, which binds ephemeral inner ports at dial time so the lane cannot
// enumerate ports up front. Per-port lanes take precedence over the wildcard;
// packets on the wildcard keep their arrival timestamps like any ring.
// Subscribing again replaces (and closes) the previous wildcard ring.
func (u *UERx) SubscribeUDPAll() *Ring {
	r := NewRing(defaultRingCapacity)
	u.mu.Lock()
	old := u.udpAll
	u.udpAll = r
	u.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return r
}

// UnsubscribeUDPAll removes and closes the wildcard UDP lane, if r is still
// the active one (a stale handle after a replacing SubscribeUDPAll is only
// closed, never unhooks the newer subscriber).
func (u *UERx) UnsubscribeUDPAll(r *Ring) {
	u.mu.Lock()
	if u.udpAll == r {
		u.udpAll = nil
	}
	u.mu.Unlock()
	if r != nil {
		r.Close()
	}
}

// SetDefaultSink installs the catch-all for packets matching no
// subscription — the netstack InjectInbound seam. f runs on the reader
// goroutine and must not block. nil restores drop-and-count.
func (u *UERx) SetDefaultSink(f func(innerIP []byte)) {
	u.mu.Lock()
	u.defaultSink = f
	u.mu.Unlock()
}

// DefaultDrops counts packets that matched no subscription while no default
// sink was set.
func (u *UERx) DefaultDrops() uint64 { return u.defaultDrops.Load() }

// EndMarkers counts GTP-U End Marker G-PDUs (message type 254) received for
// this UE's TEID — the UPF's "old path is drained" signal after a handover
// path switch (TS 29.281 §7.3), a correlation input.
func (u *UERx) EndMarkers() uint64 { return u.endMarkers.Load() }

// SetEndMarkerFunc installs a callback fired (from the reader goroutine, must
// not block) with the arrival time of each End Marker. nil removes it; the
// counter always runs.
func (u *UERx) SetEndMarkerFunc(f func(arrival time.Time)) {
	u.mu.Lock()
	u.endMarkerFn = f
	u.mu.Unlock()
}

func (u *UERx) noteEndMarker(arrival time.Time) {
	u.endMarkers.Add(1)
	u.mu.RLock()
	f := u.endMarkerFn
	u.mu.RUnlock()
	if f != nil {
		f(arrival)
	}
}

// dispatch routes one inner IP packet. Called only from the Demux reader.
func (u *UERx) dispatch(pkt []byte, arrival time.Time) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		u.toDefault(pkt)
		return
	}
	ihl := int(pkt[0]&0x0F) * 4
	if ihl < 20 || len(pkt) < ihl {
		u.toDefault(pkt)
		return
	}
	// Non-first fragments carry no L4 header — only the default sink can
	// make sense of them.
	frag := binary.BigEndian.Uint16(pkt[6:8])&0x1FFF != 0
	switch {
	case pkt[9] == 1 && !frag: // ICMP
		u.mu.RLock()
		rings := u.icmp
		u.mu.RUnlock()
		if len(rings) == 0 {
			u.toDefault(pkt)
			return
		}
		for _, r := range rings {
			r.Push(pkt, arrival)
		}
	case pkt[9] == 17 && !frag && len(pkt) >= ihl+8: // UDP
		port := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
		u.mu.RLock()
		r := u.udp[port]
		if r == nil {
			r = u.udpAll // wildcard lane; per-port subscriptions win
		}
		u.mu.RUnlock()
		if r == nil {
			u.toDefault(pkt)
			return
		}
		r.Push(pkt, arrival)
	default:
		u.toDefault(pkt)
	}
}

func (u *UERx) toDefault(pkt []byte) {
	u.mu.RLock()
	sink := u.defaultSink
	u.mu.RUnlock()
	if sink != nil {
		sink(pkt)
		return
	}
	u.defaultDrops.Add(1)
}

// close closes every subscription (lane unregistered / demux torn down).
func (u *UERx) close() {
	u.mu.Lock()
	icmp := u.icmp
	u.icmp = nil
	udp := u.udp
	u.udp = make(map[uint16]*Ring)
	udpAll := u.udpAll
	u.udpAll = nil
	u.mu.Unlock()
	for _, r := range icmp {
		r.Close()
	}
	for _, r := range udp {
		r.Close()
	}
	if udpAll != nil {
		udpAll.Close()
	}
}

// Frame is one demultiplexed downlink packet: the inner IP bytes plus the
// arrival timestamp captured at the socket read. The timestamp rides with
// the frame so jitter consumers (loom's dgram MetaConn/ReadFromMeta →
// datapath Frame.Meta) see receive time, not dequeue time.
type Frame struct {
	Payload []byte
	Arrival time.Time
}

// Ring is a bounded frame queue between the demux reader and one consumer:
// drop-oldest under overload with a drop counter (loom ADR-0005 spirit —
// interference is observable, never silent backpressure on the socket
// reader), per-frame arrival timestamps preserved.
type Ring struct {
	ch    chan Frame
	done  chan struct{}
	once  sync.Once
	drops atomic.Uint64
}

// NewRing returns a ring holding up to capacity frames. Producer side is
// single-goroutine (the demux reader); Push from multiple goroutines would
// weaken the drop-oldest guarantee, not corrupt the ring.
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = defaultRingCapacity
	}
	return &Ring{ch: make(chan Frame, capacity), done: make(chan struct{})}
}

// Push enqueues a frame with its arrival time, evicting the oldest frame
// (and counting the drop) when full. It never blocks. Frames pushed after
// Close are discarded.
func (r *Ring) Push(payload []byte, arrival time.Time) {
	select {
	case <-r.done:
		return
	default:
	}
	f := Frame{Payload: payload, Arrival: arrival}
	select {
	case r.ch <- f:
		return
	default:
	}
	// Full: evict the oldest, count it, then enqueue.
	select {
	case <-r.ch:
		r.drops.Add(1)
	default: // a concurrent consumer drained it
	}
	select {
	case r.ch <- f:
	default:
		r.drops.Add(1) // only reachable with multiple producers
	}
}

// Read returns the next frame, waiting up to timeout. It returns
// context.DeadlineExceeded on timeout (the latency probe classifies that as
// loss, like the old ReadDownlink) and net.ErrClosed once the ring is closed
// and drained.
func (r *Ring) Read(timeout time.Duration) (Frame, error) {
	select {
	case f := <-r.ch:
		return f, nil
	default:
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case f := <-r.ch:
		return f, nil
	case <-r.done:
		// Drain anything raced in before close.
		select {
		case f := <-r.ch:
			return f, nil
		default:
			return Frame{}, net.ErrClosed
		}
	case <-t.C:
		return Frame{}, context.DeadlineExceeded
	}
}

// Len reports the frames currently buffered.
func (r *Ring) Len() int { return len(r.ch) }

// Drops counts frames evicted because the ring was full.
func (r *Ring) Drops() uint64 { return r.drops.Load() }

// Close wakes blocked readers; buffered frames remain readable via Read
// until drained.
func (r *Ring) Close() { r.once.Do(func() { close(r.done) }) }
