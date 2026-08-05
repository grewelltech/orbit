package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gtpu"
)

// Phase-5 engine coverage: the per-gNB SharedTunnel pool behind
// Session.dataplane() — refcount lifecycle, the EADDRINUSE fix (two UEs on
// one gNB, one socket), the handover rebind paths (intra-gNB TEID swap,
// inter-gNB lane carry), and gNB removal with live sessions.

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

// freeUDPPort reserves a currently-free loopback UDP port (bind :0, note the
// port, release it) so two sessions can share a FIXED port — the shape that
// used to EADDRINUSE on a real gNB's 2152.
func freeUDPPort(t *testing.T) string {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	return fmt.Sprint(port)
}

// newDataSession builds a synthetic SESSION_ACTIVE UE on m's shared pool.
func newDataSession(m *Manager, supi, upfAddr, n3Port string, ulTEID, dlTEID uint32, qfi uint8) *Session {
	sess := &Session{
		SUPI: supi,
		Result: &AttachResult{
			SessionActive: true,
			PDUAddress:    "192.168.100.9",
			UPFAddress:    upfAddr, // host:port — upfN3() respects it
			UPFTEID:       ulTEID,
			DLTEID:        dlTEID,
			QFI:           qfi,
		},
		conn:  stubTransport{},
		gnbN3: "127.0.0.1",
	}
	sess.n3 = m.n3
	sess.n3Port = n3Port
	sess.notify = m.hub.publish
	m.mu.Lock()
	m.sessions[supi] = sess
	m.mu.Unlock()
	return sess
}

func newFakeUPFSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	upf, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upf.Close() })
	return upf
}

func sendDownlink(t *testing.T, upf *net.UDPConn, gnb net.Addr, teid uint32, qfi uint8) {
	t.Helper()
	pkt, err := datapath.BuildICMPEchoRequest(net.IPv4(8, 8, 8, 8), net.IPv4(192, 168, 100, 9), 0x77, 1, []byte("dl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upf.WriteToUDP(gtpu.EncodeGPDU(teid, qfi, pkt), gnb.(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
}

// TestN3PoolRefcountLifecycle: create on first acquire, share on the second,
// survive one release, close on the last.
func TestN3PoolRefcountLifecycle(t *testing.T) {
	upf := newFakeUPFSocket(t)
	p := newN3Pool()

	r1, err := p.acquire("127.0.0.1:0", upf.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := p.acquire("127.0.0.1:0", upf.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Fatal("second acquire on the same gNB N3 address must share the tunnel")
	}
	if r1.refs != 2 || p.size() != 1 {
		t.Fatalf("refs = %d, pool size = %d, want 2/1", r1.refs, p.size())
	}

	// A different gNB N3 address is its own socket.
	other, err := p.acquire("127.0.0.1:"+freeUDPPort(t), upf.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if other == r1 || p.size() != 2 {
		t.Fatalf("distinct gNB addresses must get distinct tunnels (size %d)", p.size())
	}
	p.release(other)

	// One release: still open — a lane keeps receiving.
	ue, err := r1.st.Register(datapath.UETunnelConfig{ULTEID: 1, DLTEID: 2, QFI: 1})
	if err != nil {
		t.Fatal(err)
	}
	ring := ue.Lane().SubscribeICMP()
	p.release(r1)
	if p.size() != 1 {
		t.Fatalf("pool size after first release = %d, want 1", p.size())
	}
	sendDownlink(t, upf, r1.st.LocalAddr(), 2, 1)
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("shared tunnel died before its last release: %v", err)
	}

	// Last release: socket closed, lanes closed, consumers woken.
	p.release(r1)
	if p.size() != 0 {
		t.Fatalf("pool size after last release = %d, want 0", p.size())
	}
	if _, err := ring.Read(2 * time.Second); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("lane read after pool close = %v, want net.ErrClosed", err)
	}
	// release(nil) must be a safe no-op for teardown paths.
	p.release(nil)
}

// TestTwoUEsOneGNBNoEADDRINUSE is the Phase-5 regression the cutover exists
// for: two sessions on the SAME gNB N3 address (one FIXED port) both open
// data paths — previously the second per-session Tunnel bind failed with
// EADDRINUSE. They must share ONE socket, keep downlink isolated by TEID,
// and release it refcounted.
func TestTwoUEsOneGNBNoEADDRINUSE(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	port := freeUDPPort(t)

	sessA := newDataSession(m, "001010000000001", upf.LocalAddr().String(), port, 0xA1, 0xA2, 1)
	sessB := newDataSession(m, "001010000000002", upf.LocalAddr().String(), port, 0xB1, 0xB2, 1)

	tunA, rxA, err := sessA.dataplane()
	if err != nil {
		t.Fatalf("first UE data path: %v", err)
	}
	t.Cleanup(sessA.closeDataPath)
	tunB, rxB, err := sessB.dataplane()
	if err != nil {
		t.Fatalf("second UE data path on the same gNB N3 (the EADDRINUSE regression): %v", err)
	}
	t.Cleanup(sessB.closeDataPath)
	if sessA.n3ref != sessB.n3ref {
		t.Fatal("both sessions must share one SharedTunnel (one socket per gNB)")
	}
	if m.n3.size() != 1 {
		t.Fatalf("pool size = %d, want 1", m.n3.size())
	}

	// Uplink: each session stamps its own TEID on the shared socket.
	pkt, err := datapath.BuildICMPEchoRequest(net.IPv4(192, 168, 100, 9), net.IPv4(8, 8, 8, 8), 1, 1, []byte("ul"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		up   interface{ SendUplink([]byte) error }
		teid uint32
	}{{tunA, 0xA1}, {tunB, 0xB1}} {
		if err := tc.up.SendUplink(pkt); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2048)
		_ = upf.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := upf.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		g, err := gtpu.DecodeGPDU(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		if g.TEID != tc.teid {
			t.Errorf("uplink TEID = %#x, want %#x", g.TEID, tc.teid)
		}
	}

	// Downlink isolation on the one socket.
	ringA := rxA.SubscribeICMP()
	defer rxA.UnsubscribeICMP(ringA)
	ringB := rxB.SubscribeICMP()
	defer rxB.UnsubscribeICMP(ringB)
	gnb := sessA.n3ref.st.LocalAddr()
	sendDownlink(t, upf, gnb, 0xA2, 1)
	sendDownlink(t, upf, gnb, 0xB2, 1)
	if _, err := ringA.Read(2 * time.Second); err != nil {
		t.Fatalf("UE A downlink: %v", err)
	}
	if _, err := ringB.Read(2 * time.Second); err != nil {
		t.Fatalf("UE B downlink: %v", err)
	}
	if _, err := ringA.Read(30 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cross-talk into UE A's lane (err=%v)", err)
	}

	// Releasing one UE keeps the shared socket alive for the other.
	sessA.closeDataPath()
	if m.n3.size() != 1 {
		t.Fatalf("pool emptied while UE B still holds the tunnel (size %d)", m.n3.size())
	}
	sendDownlink(t, upf, gnb, 0xB2, 1)
	if _, err := ringB.Read(2 * time.Second); err != nil {
		t.Fatalf("UE B lost downlink after UE A released: %v", err)
	}
	sessB.closeDataPath()
	if m.n3.size() != 0 {
		t.Fatalf("pool size after last UE released = %d, want 0", m.n3.size())
	}
}

// TestRebindDataPathIntraGNB: a handover that keeps the gNB N3 address (same
// pool key) is a pure TEID swap on the shared Demux — same socket, same
// view, live ring intact, downlink resuming on the new TEID.
func TestRebindDataPathIntraGNB(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	sess := newDataSession(m, "001010000000001", upf.LocalAddr().String(), "0", 0x11, 0x100, 1)

	_, rx, err := sess.dataplane()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.closeDataPath)
	ring := rx.SubscribeICMP()
	defer rx.UnsubscribeICMP(ring)
	oldUE, oldRef := sess.ue, sess.n3ref

	sess.Result.DLTEID = 0x200
	if err := sess.rebindDataPath(); err != nil {
		t.Fatalf("intra-gNB rebind: %v", err)
	}
	if sess.ue != oldUE || sess.n3ref != oldRef {
		t.Fatal("intra-gNB rebind must not replace the view or the socket")
	}
	if m.n3.size() != 1 {
		t.Fatalf("pool size = %d, want 1", m.n3.size())
	}

	gnb := sess.n3ref.st.LocalAddr()
	sendDownlink(t, upf, gnb, 0x200, 1)
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("downlink did not resume on the swapped TEID: %v", err)
	}
	// The old TEID no longer routes to this UE.
	sendDownlink(t, upf, gnb, 0x100, 1)
	if _, err := ring.Read(50 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("old TEID still routes after the swap (err=%v)", err)
	}
}

// TestRebindDataPathInterGNB: a handover to a different gNB N3 address moves
// the session onto the target's shared tunnel — live lane and per-UE
// counters carried, source tunnel refcount-released (closed only when this
// was its last UE), other UEs on the source unaffected.
func TestRebindDataPathInterGNB(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	m.n3.grace = 0 // immediate source release; the End-Marker grace window has its own test
	portSrc, portTgt := freeUDPPort(t), freeUDPPort(t)

	mover := newDataSession(m, "001010000000001", upf.LocalAddr().String(), portSrc, 0x11, 0x100, 1)
	stayer := newDataSession(m, "001010000000002", upf.LocalAddr().String(), portSrc, 0x21, 0x900, 1)

	_, rxM, err := mover.dataplane()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mover.closeDataPath)
	if _, _, err := stayer.dataplane(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stayer.closeDataPath)
	srcAddr := mover.n3ref.st.LocalAddr()

	ring := rxM.SubscribeICMP()
	defer rxM.UnsubscribeICMP(ring)
	sendDownlink(t, upf, srcAddr, 0x100, 1)
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("pre-handover downlink: %v", err)
	}
	statsBefore := mover.ue.Stats()[1].DownlinkPackets

	// The Xn/N2 move: new gNB N3 address + new DL TEID, then rebind.
	mover.gnbN3 = "127.0.0.1"
	mover.n3Port = portTgt
	mover.Result.DLTEID = 0x300
	if err := mover.rebindDataPath(); err != nil {
		t.Fatalf("inter-gNB rebind: %v", err)
	}
	if mover.n3ref == stayer.n3ref {
		t.Fatal("mover still on the source tunnel")
	}
	if m.n3.size() != 2 {
		t.Fatalf("pool size = %d, want 2 (source kept by the stayer, target new)", m.n3.size())
	}

	// Downlink resumes on the target socket into the SAME live ring, and the
	// per-UE counters carried across the move.
	tgtAddr := mover.n3ref.st.LocalAddr()
	sendDownlink(t, upf, tgtAddr, 0x300, 1)
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("post-handover downlink on the carried lane: %v", err)
	}
	if got := mover.ue.Stats()[1].DownlinkPackets; got != statsBefore+1 {
		t.Errorf("DL packets after move = %d, want %d (stats must be cumulative)", got, statsBefore+1)
	}

	// The stayer's data path on the source gNB is untouched.
	ringS := stayer.rx.SubscribeICMP()
	defer stayer.rx.UnsubscribeICMP(ringS)
	sendDownlink(t, upf, srcAddr, 0x900, 1)
	if _, err := ringS.Read(2 * time.Second); err != nil {
		t.Fatalf("stayer lost downlink after the mover left: %v", err)
	}

	// When the mover was the LAST UE on a tunnel, leaving closes it.
	stayer.closeDataPath()
	if m.n3.size() != 1 {
		t.Fatalf("pool size = %d, want 1 after the stayer released the source", m.n3.size())
	}
}

// TestRebindDataPathInterGNBLastUEEndMarkerGrace: when the moving UE was the
// LAST UE on the source gNB (the normal single-UE Xn shape), the source
// socket must outlive the move by the End-Marker grace window — the UPF
// sends its End Marker on the OLD path after the path switch (TS 29.281
// §7.3), and it must reach the carried lane via the vacated TEID's
// tombstone, not a closed port. After the grace the source socket closes.
func TestRebindDataPathInterGNBLastUEEndMarkerGrace(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	m.n3.grace = 250 * time.Millisecond // test-speed grace window
	portSrc, portTgt := freeUDPPort(t), freeUDPPort(t)

	sess := newDataSession(m, "001010000000001", upf.LocalAddr().String(), portSrc, 0x11, 0x100, 1)
	_, rx, err := sess.dataplane()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.closeDataPath)
	srcAddr := sess.n3ref.st.LocalAddr().(*net.UDPAddr)

	markers := make(chan datapath.EndMarker, 4)
	rx.SetEndMarkerFunc(func(em datapath.EndMarker) {
		select {
		case markers <- em:
		default:
		}
	})

	// The Xn move to another gNB N3 socket — sess was the source's ONLY UE.
	sess.n3Port = portTgt
	if err := sess.retargetDataPath(dataPathMove{gnbN3: "127.0.0.1", dlTEID: 0x300}); err != nil {
		t.Fatalf("inter-gNB retarget: %v", err)
	}
	if m.n3.size() != 2 {
		t.Fatalf("pool size right after the move = %d, want 2 (source held for the grace window)", m.n3.size())
	}

	// The UPF drains the old path: End Marker on the vacated TEID at the OLD
	// socket. It must reach the carried lane, marked stale.
	if _, err := upf.WriteToUDP(gtpu.EncodeEndMarker(0x100), srcAddr); err != nil {
		t.Fatal(err)
	}
	select {
	case em := <-markers:
		if !em.Stale || em.TEID != 0x100 {
			t.Fatalf("End Marker = %+v, want stale on TEID 0x100", em)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("End Marker on the vacated last-UE source path was lost (socket closed too early?)")
	}

	// After the grace window the source socket is released and closed.
	deadline := time.Now().Add(3 * time.Second)
	for m.n3.size() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("pool size = %d, want 1 after the grace release", m.n3.size())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRetargetDataPathConcurrentDataplane pins the handover locking contract
// under -race: retargetDataPath writes the data-path identity (gnbN3,
// Result.DLTEID/UPFTEID) under dpMu — the same lock dataplane()/SendUplink
// readers take — so a Ping/Traffic/app-session racing an Xn/N2 handover
// never observes a torn pair. (The old split — writes under m.mu, reads
// under dpMu — was a detector-confirmed race.)
func TestRetargetDataPathConcurrentDataplane(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	m.n3.grace = 0
	port := freeUDPPort(t)
	sess := newDataSession(m, "001010000000001", upf.LocalAddr().String(), port, 0x11, 0x100, 1)
	if _, _, err := sess.dataplane(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.closeDataPath)

	done := make(chan struct{})
	go func() {
		defer close(done)
		hosts := [2]string{"127.0.0.1", "127.0.0.2"}
		for i := 0; i < 40; i++ {
			// Alternate inter-gNB moves (new N3 address + new TEID), as
			// runXnHandover does after each PathSwitchRequestAcknowledge.
			if err := sess.retargetDataPath(dataPathMove{
				gnbN3:  hosts[i%2],
				dlTEID: 0x200 + uint32(i),
			}); err != nil {
				t.Errorf("retarget %d: %v", i, err)
				return
			}
		}
	}()
	pkt := []byte{0x45, 0, 0, 4}
	for i := 0; i < 400; i++ {
		if _, _, err := sess.dataplane(); err != nil {
			t.Fatalf("dataplane during handovers: %v", err)
		}
		if err := sess.SendUplink(pkt); err != nil {
			t.Fatalf("uplink during handovers: %v", err)
		}
	}
	<-done
}

// TestReleaseGNBClosesLiveSessions: removing a gNB with live sessions closes
// their lanes (blocked consumers wake with net.ErrClosed) and fails further
// uplink — the documented no-silent-blackhole behavior; the sessions stay
// registered and may lazily re-open later.
func TestReleaseGNBClosesLiveSessions(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	sess := newDataSession(m, "001010000000001", upf.LocalAddr().String(), "0", 0x11, 0x100, 1)

	_, rx, err := sess.dataplane()
	if err != nil {
		t.Fatal(err)
	}
	ring := rx.SubscribeICMP()

	blocked := make(chan error, 1)
	go func() {
		_, err := ring.Read(5 * time.Second)
		blocked <- err
	}()
	time.Sleep(20 * time.Millisecond) // let the consumer block

	if n := m.ReleaseGNB("127.0.0.1"); n != 1 {
		t.Fatalf("ReleaseGNB closed %d sessions, want 1", n)
	}
	select {
	case err := <-blocked:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("blocked consumer woke with %v, want net.ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("consumer still blocked after gNB release — a silent blackhole")
	}
	if err := sess.SendUplink([]byte{0x45, 0, 0, 4}); err == nil {
		t.Fatal("uplink still accepted after gNB release")
	}
	if m.n3.size() != 0 {
		t.Fatalf("pool size = %d, want 0", m.n3.size())
	}
	// The session is still registered and can lazily re-open.
	if _, _, err := sess.dataplane(); err != nil {
		t.Fatalf("lazy re-open after gNB release: %v", err)
	}
	sess.closeDataPath()
}

// A fleet run must borrow the host Manager's N3 pool. A gNB's N3 address is
// one UDP bind for the whole process, so a run with its own pool cannot open a
// data path on an address an ad-hoc `orbit ue` session already holds — and the
// failure is per-UE and quiet, so the run reports a healthy population
// carrying zero bytes. That is the shape of a real incident: 100 UEs attached,
// 0 failed, and not a single byte moved.
func TestFleetDepsShareTheManagersN3Pool(t *testing.T) {
	m := NewManager(testLogger())

	// With a Manager, the run uses ITS pool — so an address the Manager has
	// already bound is reused rather than fought over.
	if got := (FleetDeps{Manager: m}).n3(); got != m.n3 {
		t.Error("a run with a Manager must borrow its pool, or the two race for one UDP bind")
	}

	// Standalone (the local `orbit run <fleet>` path) gets its own.
	solo := (FleetDeps{}).n3()
	if solo == nil {
		t.Fatal("a run without a Manager still needs a pool")
	}
	if solo == m.n3 {
		t.Error("a standalone run must not reach into a Manager it was not given")
	}
}

// The concrete regression: an ad-hoc session holds a gNB's N3 socket, and a
// fleet run on the same address then acquires the SAME tunnel instead of
// failing to bind.
func TestManagerAndFleetShareOneGNBSocket(t *testing.T) {
	upf, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer upf.Close()

	m := NewManager(testLogger())
	port := freeUDPPort(t)
	localN3 := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))

	// An ad-hoc `orbit ue` session opens its data path first.
	sess := newDataSession(m, "001010000000001", upf.LocalAddr().String(), port, 0xA1, 0xA2, 1)
	if _, _, err := sess.dataplane(); err != nil {
		t.Fatalf("ad-hoc session data path: %v", err)
	}
	t.Cleanup(sess.closeDataPath)

	// A fleet run then wants the same gNB N3 address.
	pool := FleetDeps{Manager: m}.n3()
	ref, err := pool.acquire(localN3, upf.LocalAddr().String())
	if err != nil {
		t.Fatalf("fleet run could not acquire the gNB N3 socket an ad-hoc session holds: %v "+
			"(this is the bug: every app client's bridge fails and the run reports zero traffic)", err)
	}
	defer pool.release(ref)

	if ref.st != sess.n3ref.st {
		t.Error("the run and the session must share ONE socket for the gNB's N3 address")
	}
	if pool.size() != 1 {
		t.Errorf("pool holds %d entries for one address, want 1", pool.size())
	}
}
