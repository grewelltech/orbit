package datapath

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gtpu"
)

// demuxRig is a loopback N3 pair: a stand-in UPF socket and a Tunnel whose
// downlink is owned by a Demux — the Phase-4 shape (demux layered on the
// existing per-session tunnel socket).
type demuxRig struct {
	upf *net.UDPConn
	tun *Tunnel
	d   *Demux
}

func newDemuxRig(t *testing.T, ulTEID, dlTEID uint32, qfi uint8) *demuxRig {
	t.Helper()
	upf, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	tun, err := NewTunnel(Config{
		LocalN3: "127.0.0.1:0",
		UPFN3:   upf.LocalAddr().String(),
		ULTEID:  ulTEID, DLTEID: dlTEID, QFI: qfi,
	})
	if err != nil {
		upf.Close()
		t.Fatal(err)
	}
	r := &demuxRig{upf: upf, tun: tun, d: tun.Demux()}
	t.Cleanup(func() {
		tun.Close()
		r.d.Close()
		upf.Close()
	})
	return r
}

// sendDL encapsulates inner in a G-PDU for teid and sends it downlink.
func (r *demuxRig) sendDL(t *testing.T, teid uint32, qfi uint8, inner []byte) {
	t.Helper()
	if _, err := r.upf.WriteToUDP(gtpu.EncodeGPDU(teid, qfi, inner), r.tun.conn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
}

func icmpPkt(t *testing.T, id, seq uint16) []byte {
	t.Helper()
	p, err := BuildICMPEchoRequest(net.IPv4(10, 0, 0, 9), net.IPv4(192, 168, 100, 5), id, seq, []byte("dl"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func udpPkt(t *testing.T, dstPort uint16, payload []byte) []byte {
	t.Helper()
	p, err := BuildUDPPacket(net.IPv4(10, 0, 0, 9), net.IPv4(192, 168, 100, 5), 9000, dstPort, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestDemuxTEIDRoutingAndMismatch: G-PDUs for the registered TEID reach the
// UE lane (with per-QFI stats still flowing on the tunnel); G-PDUs for an
// unknown TEID are skipped and counted, exactly like ReadDownlink's
// cross-talk defence.
func TestDemuxTEIDRoutingAndMismatch(t *testing.T) {
	const dlTEID = 0x222
	r := newDemuxRig(t, 0x111, dlTEID, 1)
	rx := r.d.Register(dlTEID)
	ring := rx.SubscribeICMP()

	before := time.Now()
	r.sendDL(t, dlTEID, 1, icmpPkt(t, 0xAA, 1))
	f, err := ring.Read(2 * time.Second)
	if err != nil {
		t.Fatalf("no frame on the ICMP lane: %v", err)
	}
	if _, ok := MatchICMPEchoReply(f.Payload, 0xAA, 1); ok {
		t.Fatal("echo request misclassified as reply") // sanity of the fixture
	}
	if len(f.Payload) < 20 || f.Payload[9] != 1 {
		t.Fatalf("lane delivered a non-ICMP packet: % x", f.Payload[:8])
	}
	if f.Arrival.Before(before) || time.Since(f.Arrival) > 5*time.Second {
		t.Errorf("arrival timestamp %v not preserved", f.Arrival)
	}

	// Wrong TEID → skipped + counted, nothing delivered.
	r.sendDL(t, 0xBAD, 1, icmpPkt(t, 0xAA, 2))
	waitFor(t, "TEID miss counter", func() bool { return r.d.TEIDMisses() == 1 })
	if _, err := ring.Read(50 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("mismatched-TEID packet leaked into the lane (err=%v)", err)
	}

	// Per-QFI stats kept flowing through the demux hook (Stats unchanged).
	st := r.tun.Stats()[1]
	if st.DownlinkPackets != 1 {
		t.Errorf("DL packets = %d, want 1 (mismatch must not count)", st.DownlinkPackets)
	}
	// A different QFI on the wire lands in its own bucket.
	r.sendDL(t, dlTEID, 5, icmpPkt(t, 0xAA, 3))
	waitFor(t, "QFI 5 stats", func() bool { return r.tun.Stats()[5].DownlinkPackets == 1 })
}

// TestTunnelReadDownlinkGuard: once the Demux owns the socket, the legacy
// single-consumer read is refused (the §6 one-reader invariant, enforced).
func TestTunnelReadDownlinkGuard(t *testing.T) {
	r := newDemuxRig(t, 1, 2, 1)
	if _, err := r.tun.ReadDownlink(10 * time.Millisecond); !errors.Is(err, ErrDownlinkOwned) {
		t.Fatalf("ReadDownlink after Demux() = %v, want ErrDownlinkOwned", err)
	}
	if r.tun.Demux() != r.d {
		t.Fatal("Demux() is not idempotent")
	}
}

// TestTunnelDemuxEvictsBlockedReader: a goroutine already blocked inside
// ReadDownlink when Demux() is called must be evicted (ErrDownlinkOwned),
// not left racing the demux reader for downlink G-PDUs — the §6 single-
// reader invariant under its worst-case interleaving. Before enforcement,
// the stale reader won the packet and the demux lane starved.
func TestTunnelDemuxEvictsBlockedReader(t *testing.T) {
	upf, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer upf.Close()
	tun, err := NewTunnel(Config{
		LocalN3: "127.0.0.1:0",
		UPFN3:   upf.LocalAddr().String(),
		ULTEID:  0x111, DLTEID: 0x42, QFI: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	// Block a legacy reader in the socket read.
	readerErr := make(chan error, 1)
	go func() {
		_, err := tun.ReadDownlink(10 * time.Second)
		readerErr <- err
	}()
	time.Sleep(50 * time.Millisecond) // let it reach ReadFromUDP

	d := tun.Demux()
	defer d.Close()
	rx := d.Register(0x42)
	ring := rx.SubscribeICMP()

	// The evicted reader must be told why.
	select {
	case err := <-readerErr:
		if !errors.Is(err, ErrDownlinkOwned) {
			t.Fatalf("blocked ReadDownlink returned %v, want ErrDownlinkOwned", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadDownlink still blocked after Demux() took ownership")
	}

	// And the demux lane — not a stale direct reader — gets the packet.
	if _, err := upf.WriteToUDP(gtpu.EncodeGPDU(0x42, 1, icmpPkt(t, 0xAB, 7)), tun.conn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("demux lane did not receive the downlink packet: %v", err)
	}
}

// TestDemuxDetachAttach: the cross-socket handover move — a lane detached
// from a dying demux keeps its subscriptions open and resumes delivery once
// attached to the new socket's demux under the new TEID.
func TestDemuxDetachAttach(t *testing.T) {
	const oldTEID, newTEID = 0x100, 0x200
	r1 := newDemuxRig(t, 1, oldTEID, 1)
	rx := r1.d.Register(oldTEID)
	ring := rx.SubscribeICMP()

	r1.sendDL(t, oldTEID, 1, icmpPkt(t, 0xAA, 1))
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("pre-move delivery: %v", err)
	}

	got, ok := r1.d.Detach(oldTEID)
	if !ok || got != rx {
		t.Fatalf("Detach = (%v, %v)", got, ok)
	}
	// Tear the old data path down the way rebindDataPath does: the detached
	// lane must survive with its rings open.
	r1.tun.Close()
	r1.d.Close()
	select {
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := ring.Read(10 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("detached lane's ring was closed by the old demux teardown: %v", err)
	}

	r2 := newDemuxRig(t, 1, newTEID, 1)
	if err := r2.d.Attach(newTEID, rx); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := r2.d.Attach(newTEID, rx); err == nil {
		t.Fatal("Attach onto an occupied TEID must refuse")
	}
	r2.sendDL(t, newTEID, 1, icmpPkt(t, 0xAA, 2))
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("post-move delivery on the new socket: %v", err)
	}
}

// TestDemuxRebind: the handover TEID swap moves the lane atomically, errors
// on unknown/occupied TEIDs, and packets route by the new TEID afterwards.
func TestDemuxRebind(t *testing.T) {
	const oldTEID, newTEID = 0x100, 0x200
	r := newDemuxRig(t, 1, oldTEID, 1)
	rx := r.d.Register(oldTEID)
	ring := rx.SubscribeICMP()

	if err := r.d.Rebind(0xDEAD, newTEID); err == nil {
		t.Fatal("rebind from an unregistered TEID must fail")
	}
	other := r.d.Register(0x300)
	if other == rx {
		t.Fatal("distinct TEIDs must get distinct lanes")
	}
	if err := r.d.Rebind(oldTEID, 0x300); err == nil {
		t.Fatal("rebind onto an occupied TEID must fail")
	}
	if err := r.d.Rebind(oldTEID, newTEID); err != nil {
		t.Fatal(err)
	}

	r.sendDL(t, newTEID, 1, icmpPkt(t, 1, 1))
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("packet to rebound TEID not delivered: %v", err)
	}
	r.sendDL(t, oldTEID, 1, icmpPkt(t, 1, 2))
	waitFor(t, "old TEID to miss", func() bool { return r.d.TEIDMisses() == 1 })
}

// TestDemuxRebindAtomicUnderTraffic hammers the demux with downlink packets
// alternating between two TEIDs while another goroutine rebinds back and
// forth and a consumer drains the lane. Run under -race this exercises the
// lane map swap; the accounting invariant — every accepted G-PDU is either
// delivered, ring-dropped, or TEID-missed — must hold exactly.
func TestDemuxRebindAtomicUnderTraffic(t *testing.T) {
	const teidA, teidB = 0xA0A0, 0xB0B0
	r := newDemuxRig(t, 1, teidA, 1)
	rx := r.d.Register(teidA)
	ring := rx.SubscribeICMP()

	var received atomic.Uint64
	var stop sync.WaitGroup
	done := make(chan struct{})
	stop.Add(2)

	// Consumer: drain the lane.
	go func() {
		defer stop.Done()
		for {
			if _, err := ring.Read(20 * time.Millisecond); err == nil {
				received.Add(1)
			} else if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-done:
				// Final drain of anything buffered.
				for {
					if _, err := ring.Read(10 * time.Millisecond); err != nil {
						return
					}
					received.Add(1)
				}
			default:
			}
		}
	}()

	// Rebinder: bounce the lane between the two TEIDs.
	go func() {
		defer stop.Done()
		cur := uint32(teidA)
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			next := uint32(teidA)
			if cur == teidA {
				next = teidB
			}
			if err := r.d.Rebind(cur, next); err != nil {
				t.Errorf("rebind %#x→%#x: %v", cur, next, err)
				return
			}
			cur = next
			time.Sleep(time.Millisecond)
		}
	}()

	// Sender: alternate TEIDs, paced so loopback UDP does not drop.
	const sent = 400
	pkt := icmpPkt(t, 7, 7)
	for i := 0; i < sent; i++ {
		teid := uint32(teidA)
		if i%2 == 1 {
			teid = teidB
		}
		r.sendDL(t, teid, 1, pkt)
		if i%20 == 19 {
			time.Sleep(2 * time.Millisecond)
		}
	}

	// Every packet must be accounted for: delivered, ring-dropped, or missed.
	waitFor(t, "full accounting", func() bool {
		return received.Load()+ring.Drops()+r.d.TEIDMisses() == sent
	})
	close(done)
	stop.Wait()
	if received.Load() == 0 {
		t.Error("no packets delivered — rebind starved the lane")
	}
	if r.d.TEIDMisses() == 0 {
		t.Error("no TEID misses — the alternating-TEID fixture is not exercising mismatch")
	}
	t.Logf("sent %d: delivered %d, ring-dropped %d, TEID-missed %d",
		sent, received.Load(), ring.Drops(), r.d.TEIDMisses())
}

// TestRingDropOldest: a full ring evicts the oldest frame, counts the drop,
// and preserves order and per-frame arrival timestamps for the survivors.
func TestRingDropOldest(t *testing.T) {
	ring := NewRing(4)
	base := time.Unix(1000, 0)
	for i := 0; i < 6; i++ {
		ring.Push([]byte{byte(i)}, base.Add(time.Duration(i)*time.Millisecond))
	}
	if got := ring.Drops(); got != 2 {
		t.Fatalf("drops = %d, want 2", got)
	}
	if ring.Len() != 4 {
		t.Fatalf("len = %d, want 4", ring.Len())
	}
	for i := 2; i < 6; i++ {
		f, err := ring.Read(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if f.Payload[0] != byte(i) {
			t.Errorf("frame %d: payload %d, want %d (drop-oldest order)", i, f.Payload[0], i)
		}
		if want := base.Add(time.Duration(i) * time.Millisecond); !f.Arrival.Equal(want) {
			t.Errorf("frame %d: arrival %v, want %v", i, f.Arrival, want)
		}
	}
	if _, err := ring.Read(10 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("empty ring read = %v, want DeadlineExceeded", err)
	}
	ring.Close()
	if _, err := ring.Read(10 * time.Millisecond); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed ring read = %v, want net.ErrClosed", err)
	}
}

// TestUERxDispatch: inner-IP dispatch — ICMP fans out to every ICMP
// subscriber, UDP routes by destination port, and everything else hits the
// default sink (or the drop counter when none is set).
func TestUERxDispatch(t *testing.T) {
	rx := newUERx()
	now := time.Now()

	// No subscribers, no sink → drop + count.
	rx.dispatch(icmpPkt(t, 1, 1), now)
	if rx.DefaultDrops() != 1 {
		t.Fatalf("default drops = %d, want 1", rx.DefaultDrops())
	}

	icmpA, icmpB := rx.SubscribeICMP(), rx.SubscribeICMP()
	media := rx.SubscribeUDP(4000)

	rx.dispatch(icmpPkt(t, 2, 1), now)
	for name, r := range map[string]*Ring{"A": icmpA, "B": icmpB} {
		if f, err := r.Read(time.Second); err != nil || f.Payload[9] != 1 {
			t.Fatalf("ICMP subscriber %s: %v", name, err)
		}
	}

	rx.dispatch(udpPkt(t, 4000, []byte("rtp")), now)
	f, err := media.Read(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if payload, _, ok := ExtractUDPPayload(f.Payload, 4000); !ok || string(payload) != "rtp" {
		t.Fatalf("media lane payload = %q ok=%v", payload, ok)
	}
	if media.Len() != 0 || icmpA.Len() != 0 {
		t.Fatal("packet delivered to more lanes than its own")
	}

	// UDP to an unsubscribed port → default; then a sink captures instead.
	rx.dispatch(udpPkt(t, 5555, []byte("x")), now)
	if rx.DefaultDrops() != 2 {
		t.Fatalf("default drops = %d, want 2", rx.DefaultDrops())
	}
	var sunk atomic.Uint64
	rx.SetDefaultSink(func(innerIP []byte) { sunk.Add(1) })
	rx.dispatch(udpPkt(t, 5555, []byte("x")), now) // unmatched UDP port
	tcp := udpPkt(t, 5555, []byte("x"))
	tcp[9] = 6 // rewrite protocol to TCP
	rx.dispatch(tcp, now)
	if sunk.Load() != 2 {
		t.Fatalf("default sink saw %d packets, want 2", sunk.Load())
	}
	if rx.DefaultDrops() != 2 {
		t.Fatalf("drops advanced to %d with a sink installed", rx.DefaultDrops())
	}

	// Unsubscribed lanes close and stop receiving.
	rx.UnsubscribeICMP(icmpA)
	rx.UnsubscribeUDP(4000)
	rx.dispatch(icmpPkt(t, 3, 1), now)
	if _, err := icmpB.Read(time.Second); err != nil {
		t.Fatalf("remaining ICMP subscriber starved: %v", err)
	}
	if _, err := icmpA.Read(10 * time.Millisecond); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("unsubscribed ring read = %v, want net.ErrClosed", err)
	}
	if _, err := media.Read(10 * time.Millisecond); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("unsubscribed media ring read = %v, want net.ErrClosed", err)
	}
}

// TestDemuxEndMarker: End Marker G-PDUs (type 254) surface on the UE lane as
// a counter and an event callback with the arrival time; End Markers for an
// unknown TEID count as misses. They never enter the packet lanes or stats.
func TestDemuxEndMarker(t *testing.T) {
	const dlTEID = 0x777
	r := newDemuxRig(t, 1, dlTEID, 1)
	rx := r.d.Register(dlTEID)
	ring := rx.SubscribeICMP()

	var seen atomic.Int64
	var arrival atomic.Value
	rx.SetEndMarkerFunc(func(at time.Time) { arrival.Store(at); seen.Add(1) })

	if _, err := r.upf.WriteToUDP(gtpu.EncodeEndMarker(dlTEID), r.tun.conn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "end marker", func() bool { return rx.EndMarkers() == 1 })
	if seen.Load() != 1 {
		t.Fatalf("callback fired %d times, want 1", seen.Load())
	}
	if at := arrival.Load().(time.Time); time.Since(at) > 5*time.Second || at.IsZero() {
		t.Errorf("end-marker arrival %v not plausible", at)
	}
	if _, err := ring.Read(30 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("end marker leaked into a packet lane")
	}
	if st := r.tun.Stats()[1]; st.DownlinkPackets != 0 {
		t.Errorf("end marker counted as downlink data (%d pkts)", st.DownlinkPackets)
	}

	// Unknown TEID → miss counter, no event.
	if _, err := r.upf.WriteToUDP(gtpu.EncodeEndMarker(0xBAD), r.tun.conn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "end-marker TEID miss", func() bool { return r.d.TEIDMisses() == 1 })
	if rx.EndMarkers() != 1 {
		t.Errorf("foreign end marker reached this UE's lane")
	}
}

// TestDemuxMalformedAndForeign: junk frames are counted and skipped; the
// reader survives and keeps routing.
func TestDemuxMalformedAndForeign(t *testing.T) {
	const dlTEID = 0x321
	r := newDemuxRig(t, 1, dlTEID, 1)
	rx := r.d.Register(dlTEID)
	ring := rx.SubscribeICMP()

	dst := r.tun.conn.LocalAddr().(*net.UDPAddr)
	if _, err := r.upf.WriteToUDP([]byte{0xFF, 0xFF, 0x00}, dst); err != nil { // not GTP
		t.Fatal(err)
	}
	waitFor(t, "decode error counter", func() bool { return r.d.DecodeErrors() == 1 })

	// Echo Request (no payload semantics for us) is skipped silently.
	echo := []byte{0x30, gtpu.MsgTypeEchoRequest, 0x00, 0x00, 0, 0, 0, 0}
	if _, err := r.upf.WriteToUDP(echo, dst); err != nil {
		t.Fatal(err)
	}

	// Still alive: a real packet routes.
	r.sendDL(t, dlTEID, 1, icmpPkt(t, 9, 9))
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("demux stopped routing after junk: %v", err)
	}
	if r.d.TEIDMisses() != 0 {
		t.Errorf("echo/junk counted as TEID misses (%d)", r.d.TEIDMisses())
	}
}

// TestDemuxCloseWakesConsumers: tearing down the tunnel (handover/release)
// stops the reader and closes every lane so blocked consumers unblock.
func TestDemuxCloseWakesConsumers(t *testing.T) {
	r := newDemuxRig(t, 1, 2, 1)
	rx := r.d.Register(2)
	ring := rx.SubscribeICMP()

	got := make(chan error, 1)
	go func() {
		_, err := ring.Read(5 * time.Second)
		got <- err
	}()
	time.Sleep(20 * time.Millisecond) // let the consumer block
	r.tun.Close()                     // socket close stops the reader…
	if err := r.d.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("blocked consumer woke with %v, want net.ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("consumer still blocked after demux close")
	}
}

// TestUERxWildcardUDP: the wildcard UDP lane (SubscribeUDPAll — the dgram
// RxDatapath feed) receives inner UDP to any port that has no per-port lane,
// per-port subscriptions win over it, and unsubscribing restores
// drop-and-count.
func TestUERxWildcardUDP(t *testing.T) {
	const dlTEID = 0x777
	r := newDemuxRig(t, 0x666, dlTEID, 1)
	rx := r.d.Register(dlTEID)

	all := rx.SubscribeUDPAll()

	// Two different destination ports, one wildcard lane, timestamps intact.
	before := time.Now()
	r.sendDL(t, dlTEID, 1, udpPkt(t, 5004, []byte("rtp")))
	r.sendDL(t, dlTEID, 1, udpPkt(t, 49152, []byte("rtcp")))
	for _, want := range []string{"rtp", "rtcp"} {
		f, err := all.Read(2 * time.Second)
		if err != nil {
			t.Fatalf("wildcard lane missing %q: %v", want, err)
		}
		if payload, _, ok := ExtractUDPPayload(f.Payload, 0); !ok || string(payload) != want {
			t.Fatalf("wildcard lane got %q, want %q", payload, want)
		}
		if f.Arrival.Before(before) || time.Since(f.Arrival) > 5*time.Second {
			t.Errorf("arrival timestamp %v not preserved on the wildcard lane", f.Arrival)
		}
	}

	// A per-port lane takes precedence for its port; the wildcard keeps the rest.
	port := rx.SubscribeUDP(6000)
	r.sendDL(t, dlTEID, 1, udpPkt(t, 6000, []byte("lane")))
	r.sendDL(t, dlTEID, 1, udpPkt(t, 6001, []byte("wild")))
	f, err := port.Read(2 * time.Second)
	if err != nil {
		t.Fatalf("per-port lane starved by the wildcard: %v", err)
	}
	if payload, _, _ := ExtractUDPPayload(f.Payload, 0); string(payload) != "lane" {
		t.Fatalf("per-port lane got %q, want %q", payload, "lane")
	}
	f, err = all.Read(2 * time.Second)
	if err != nil {
		t.Fatalf("wildcard lane missing the unclaimed port: %v", err)
	}
	if payload, _, _ := ExtractUDPPayload(f.Payload, 0); string(payload) != "wild" {
		t.Fatalf("wildcard lane got %q, want %q", payload, "wild")
	}

	// Unsubscribe closes the ring and unclaimed UDP falls to drop-and-count.
	rx.UnsubscribeUDPAll(all)
	if _, err := all.Read(50 * time.Millisecond); err == nil {
		t.Fatal("wildcard ring still readable after UnsubscribeUDPAll")
	}
	drops := rx.DefaultDrops()
	r.sendDL(t, dlTEID, 1, udpPkt(t, 6002, []byte("orphan")))
	waitFor(t, "default drop after wildcard unsubscribe", func() bool {
		return rx.DefaultDrops() == drops+1
	})
}

// TestUERxWildcardReplaceAndStaleUnsubscribe: resubscribing replaces (and
// closes) the previous wildcard ring, and a stale handle cannot unhook the
// newer subscriber.
func TestUERxWildcardReplaceAndStaleUnsubscribe(t *testing.T) {
	const dlTEID = 0x778
	r := newDemuxRig(t, 0x668, dlTEID, 1)
	rx := r.d.Register(dlTEID)

	first := rx.SubscribeUDPAll()
	second := rx.SubscribeUDPAll()
	if _, err := first.Read(50 * time.Millisecond); err == nil {
		t.Fatal("replaced wildcard ring was not closed")
	}
	rx.UnsubscribeUDPAll(first) // stale handle: must not unhook second
	r.sendDL(t, dlTEID, 1, udpPkt(t, 7000, []byte("still-here")))
	f, err := second.Read(2 * time.Second)
	if err != nil {
		t.Fatalf("active wildcard lane unhooked by a stale unsubscribe: %v", err)
	}
	if payload, _, _ := ExtractUDPPayload(f.Payload, 0); string(payload) != "still-here" {
		t.Fatalf("wildcard lane got %q, want %q", payload, "still-here")
	}
}
