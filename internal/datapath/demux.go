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
// never read the socket directly. Since the Phase-5 cutover the Demux runs on
// the per-gNB SharedTunnel conn (the only 2152 bind per gNB); every UE served
// by that gNB has its own lane. The dispatch chain is
//
//	socket → Demux (TEID → UERx) → UERx (inner IP proto/port → Ring) → consumer
//
// so the ICMP latency probe and media lanes of ALL UEs share one downlink.

// Demux owns the single reader goroutine on one N3 UDP socket and routes
// downlink G-PDUs to per-UE lanes by downlink TEID. G-PDUs for a TEID with no
// registered lane are skipped and counted (the classic cross-talk defence,
// made observable); malformed frames are skipped and counted.
type Demux struct {
	conn  *net.UDPConn
	local string // the socket's bound gNB N3 address, stamped on End Markers

	mu    sync.RWMutex
	lanes map[uint32]*UERx
	// tombs are short-lived tombstones for TEIDs a handover just vacated
	// (Rebind's old TEID, Detach's TEID): the UPF sends its GTP-U End Marker
	// on the OLD tunnel after the path switch (TS 29.281 §7.3), so for a few
	// seconds an End Marker on a vacated TEID is the "source path drained"
	// correlation signal, not unknown-TEID noise. Only End Markers consult
	// tombstones — data G-PDUs on a vacated TEID still miss.
	tombs map[uint32]tombstone

	teidMisses      atomic.Uint64
	decodeErrs      atomic.Uint64
	staleEndMarkers atomic.Uint64

	// tombsLive mirrors len(tombs) so the reader loop can decide to sweep
	// without taking the mutex on every packet.
	tombsLive atomic.Int64

	// tombTTL is how long a vacated TEID keeps its tombstone (tests shorten).
	tombTTL time.Duration

	closed atomic.Bool
	done   chan struct{}
}

// EndMarkerGraceTTL is the default tombstone lifetime: End Markers chase
// the path switch within milliseconds, so a few seconds is generous without
// letting a recycled TEID resurrect a long-gone lane. Exported so the engine
// can keep a source gNB's shared socket alive for the same window after an
// inter-gNB handover moved its last UE away — otherwise the UPF's End Marker
// on the old path would hit a closed port and the tombstone never fires.
const EndMarkerGraceTTL = 5 * time.Second

// tombstone remembers the lane that held a TEID until a handover moved it.
type tombstone struct {
	rx      *UERx
	expires time.Time
}

// NewDemux takes ownership of conn's read path and starts the reader
// goroutine (SharedTunnel does this at construction). After this, nothing
// else may read conn.
func NewDemux(conn *net.UDPConn) *Demux {
	d := &Demux{
		conn:    conn,
		local:   conn.LocalAddr().String(),
		lanes:   make(map[uint32]*UERx),
		tombs:   make(map[uint32]tombstone),
		tombTTL: EndMarkerGraceTTL,
		done:    make(chan struct{}),
	}
	// Clear any deadline a previous reader left.
	_ = conn.SetReadDeadline(time.Time{})
	go d.run()
	return d
}

// Register returns the per-UE downlink lane for dlTEID, creating it if
// needed. SharedTunnel.Register goes through Attach instead so a TEID
// collision between two UEs is an error, not silent lane sharing.
func (d *Demux) Register(dlTEID uint32) *UERx {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rx, ok := d.lanes[dlTEID]; ok {
		return rx
	}
	rx := newUERx()
	d.lanes[dlTEID] = rx
	delete(d.tombs, dlTEID) // a live lane supersedes any tombstone
	d.tombsLive.Store(int64(len(d.tombs)))
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
		rx.Close()
	}
}

// Detach removes the lane registered at dlTEID WITHOUT closing its
// subscriptions, and returns it. It is the cross-socket half of the handover
// data-path move (Rebind covers the same-socket TEID swap): the caller
// re-attaches the live lane — rings, End-Marker callback and all — to the
// target tunnel's Demux via Attach, so consumers blocked on the lane's rings
// never observe a close. A lane detached from a Demux that is later closed
// stays open. The vacated TEID keeps a short-lived tombstone so the UPF's
// post-handover End Marker on the old path still reaches the lane's
// End-Marker callback (the source gNB's socket outlives the move).
func (d *Demux) Detach(dlTEID uint32) (*UERx, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rx, ok := d.lanes[dlTEID]
	if ok {
		delete(d.lanes, dlTEID)
		d.tombstoneLocked(dlTEID, rx)
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
	delete(d.tombs, dlTEID) // a live lane supersedes any tombstone
	d.tombsLive.Store(int64(len(d.tombs)))
	return nil
}

// Rebind atomically moves the lane registered at oldTEID to newTEID — the
// handover TEID swap. Packets never see an intermediate state: before the
// swap they route (or miss) via oldTEID, after it via newTEID. The old TEID
// keeps a short-lived tombstone so the UPF's post-handover End Marker on the
// vacated TEID still reaches the lane's End-Marker callback.
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
	d.tombstoneLocked(oldTEID, rx)
	delete(d.tombs, newTEID)
	d.tombsLive.Store(int64(len(d.tombs)))
	return nil
}

// tombstoneLocked records a vacated TEID's lane for the End-Marker grace
// window and sweeps expired entries (the map only ever holds a handful of
// recent handovers, so the sweep is cheap). Callers hold d.mu.
func (d *Demux) tombstoneLocked(teid uint32, rx *UERx) {
	now := time.Now()
	for t, tomb := range d.tombs {
		if now.After(tomb.expires) {
			delete(d.tombs, t)
		}
	}
	d.tombs[teid] = tombstone{rx: rx, expires: now.Add(d.tombTTL)}
	d.tombsLive.Store(int64(len(d.tombs)))
}

// sweepTombs drops every expired tombstone. The reader loop calls it on a
// coarse cadence so the FINAL handover's tombstone (which no later insert
// would ever sweep) does not pin its UERx for the life of the socket.
func (d *Demux) sweepTombs(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for t, tomb := range d.tombs {
		if now.After(tomb.expires) {
			delete(d.tombs, t)
		}
	}
	d.tombsLive.Store(int64(len(d.tombs)))
}

// tombstoneLane returns the lane tombstoned at teid, if the grace window is
// still open.
func (d *Demux) tombstoneLane(teid uint32, at time.Time) *UERx {
	d.mu.Lock()
	defer d.mu.Unlock()
	tomb, ok := d.tombs[teid]
	if !ok {
		return nil
	}
	if at.After(tomb.expires) {
		delete(d.tombs, teid)
		d.tombsLive.Store(int64(len(d.tombs)))
		return nil
	}
	return tomb.rx
}

// TEIDMisses counts downlink G-PDUs (and End Markers) whose TEID matched no
// registered lane — skipped, per the original ReadDownlink semantics.
func (d *Demux) TEIDMisses() uint64 { return d.teidMisses.Load() }

// DecodeErrors counts frames that failed GTP-U decoding (skipped).
func (d *Demux) DecodeErrors() uint64 { return d.decodeErrs.Load() }

// StaleEndMarkers counts End Markers that arrived on a tombstoned TEID — the
// expected post-handover signal on the vacated path, delivered to the moved
// lane instead of being dropped as a TEID miss.
func (d *Demux) StaleEndMarkers() uint64 { return d.staleEndMarkers.Load() }

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
		// The socket is gone: no End Marker can arrive, so drop the
		// tombstones (each pins a UERx) with it.
		d.tombs = make(map[uint32]tombstone)
		d.tombsLive.Store(0)
		d.mu.Unlock()
		for _, rx := range lanes {
			rx.Close()
		}
		close(d.done)
	}()
	buf := make([]byte, 65536)
	var nextSweep time.Time
	for {
		n, _, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			if d.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue // transient per-packet error (e.g. stray deadline)
		}
		arrival := time.Now()
		// Coarse tombstone sweep: at most once per TTL, only while any exist.
		if d.tombsLive.Load() > 0 && arrival.After(nextSweep) {
			d.sweepTombs(arrival)
			nextSweep = arrival.Add(d.tombTTL)
		}
		g, err := gtpu.DecodeGPDU(buf[:n])
		if err != nil {
			d.decodeErrs.Add(1)
			continue
		}
		switch g.MsgType {
		case gtpu.MsgTypeEndMarker:
			em := EndMarker{Arrival: arrival, TEID: g.TEID, GNB: d.local}
			if rx := d.lane(g.TEID); rx != nil {
				rx.noteEndMarker(em)
			} else if rx := d.tombstoneLane(g.TEID, arrival); rx != nil {
				// The vacated (pre-handover) TEID: the UPF drained the old
				// path. Route it to the moved lane as a correlation input.
				em.Stale = true
				d.staleEndMarkers.Add(1)
				rx.noteEndMarker(em)
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
			rx.noteDownlink(g.QFI, g.HasQFI, len(g.Payload))
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
// subscription go to the default sink (the netstack InjectInbound seam —
// loomgtp's per-gNB GNBStack); with no sink set they are dropped and counted.
//
// Dispatched packet slices are shared read-only between ICMP subscribers and
// must not be mutated.
type UERx struct {
	mu          sync.RWMutex
	icmp        []*Ring
	udp         map[uint16]*Ring
	udpAll      *Ring // wildcard UDP lane (SubscribeUDPAll); per-port lanes win
	defaultSink func(innerIP []byte, arrival time.Time)
	endMarkerFn func(em EndMarker)
	dlStats     func(qfi uint8, hasQFI bool, payloadBytes int)

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
// subscription — the netstack InjectInbound seam (design §6): the loomgtp
// GNBStack bridge feeds these packets, with the arrival timestamp the demux
// reader stamped at the socket read, into the per-gNB gVisor stack's receive
// ring. f runs on the reader goroutine and must not block. nil restores
// drop-and-count.
func (u *UERx) SetDefaultSink(f func(innerIP []byte, arrival time.Time)) {
	u.mu.Lock()
	u.defaultSink = f
	u.mu.Unlock()
}

// DefaultDrops counts packets that matched no subscription while no default
// sink was set.
func (u *UERx) DefaultDrops() uint64 { return u.defaultDrops.Load() }

// EndMarker describes one received GTP-U End Marker G-PDU (message type 254)
// — the UPF's "old path is drained" signal after a handover path switch
// (TS 29.281 §7.3), a correlation input (design §7).
type EndMarker struct {
	Arrival time.Time
	TEID    uint32 // the TEID the marker arrived on (Stale: the vacated one)
	GNB     string // the gNB N3 socket address it arrived on
	// Stale marks a marker that arrived on a tombstoned TEID a handover just
	// vacated (Rebind/Detach) — the expected post-switch case: the source
	// gNB's shared socket outlives the move and the UPF drains the old path.
	Stale bool
}

// EndMarkers counts GTP-U End Marker G-PDUs received for this UE — on its
// live TEID or, within the post-handover grace window, on a vacated one.
func (u *UERx) EndMarkers() uint64 { return u.endMarkers.Load() }

// SetEndMarkerFunc installs a callback fired (from the reader goroutine, must
// not block) with each End Marker's details. nil removes it; the counter
// always runs. After an inter-gNB move the source socket's reader may still
// fire it for the tombstoned TEID (em.Stale), concurrently with the target's.
func (u *UERx) SetEndMarkerFunc(f func(em EndMarker)) {
	u.mu.Lock()
	u.endMarkerFn = f
	u.mu.Unlock()
}

// setDownlinkStats installs the per-UE downlink accounting hook, called from
// the reader goroutine for every accepted G-PDU on this lane (registered
// TEID, non-empty payload). SharedTunnel.Register wires it into the UE's
// QFI counters; because the hook rides the lane, accounting follows the lane
// across Detach/Attach handover moves.
func (u *UERx) setDownlinkStats(f func(qfi uint8, hasQFI bool, payloadBytes int)) {
	u.mu.Lock()
	u.dlStats = f
	u.mu.Unlock()
}

func (u *UERx) noteDownlink(qfi uint8, hasQFI bool, payloadBytes int) {
	u.mu.RLock()
	f := u.dlStats
	u.mu.RUnlock()
	if f != nil {
		f(qfi, hasQFI, payloadBytes)
	}
}

func (u *UERx) noteEndMarker(em EndMarker) {
	u.endMarkers.Add(1)
	u.mu.RLock()
	f := u.endMarkerFn
	u.mu.RUnlock()
	if f != nil {
		f(em)
	}
}

// dispatch routes one inner IP packet. Called only from the Demux reader.
func (u *UERx) dispatch(pkt []byte, arrival time.Time) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		u.toDefault(pkt, arrival)
		return
	}
	ihl := int(pkt[0]&0x0F) * 4
	if ihl < 20 || len(pkt) < ihl {
		u.toDefault(pkt, arrival)
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
			u.toDefault(pkt, arrival)
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
			u.toDefault(pkt, arrival)
			return
		}
		r.Push(pkt, arrival)
	default:
		u.toDefault(pkt, arrival)
	}
}

func (u *UERx) toDefault(pkt []byte, arrival time.Time) {
	u.mu.RLock()
	sink := u.defaultSink
	u.mu.RUnlock()
	if sink != nil {
		sink(pkt, arrival)
		return
	}
	u.defaultDrops.Add(1)
}

// Close closes every subscription, waking blocked consumers with
// net.ErrClosed. The Demux calls it when the lane is unregistered or the
// demux tears down; the engine calls it directly only for a lane that was
// Detached for a handover move and could not be re-attached — consumers must
// see a closed signal, never a silent blackhole.
func (u *UERx) Close() {
	u.mu.Lock()
	icmp := u.icmp
	u.icmp = nil
	udp := u.udp
	u.udp = make(map[uint16]*Ring)
	udpAll := u.udpAll
	u.udpAll = nil
	// Drop the callbacks too: a tombstone may keep this lane reachable from
	// the reader for the grace window after the owning session is gone, and
	// a stale End Marker must not fire into a finalized consumer.
	u.endMarkerFn = nil
	u.dlStats = nil
	u.defaultSink = nil
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
