package engine

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgrewell/loom/core/netpath/dgram"

	"github.com/bgrewell/orbit/internal/gtpu"
)

// hairpinUPF reflects every uplink G-PDU's inner packet straight back down
// the tunnel to its sender, stamped with the CURRENT DL TEID — a UPF whose
// N6 side is a mirror. A TCP dial from the UE to its OWN address therefore
// crosses real GTP-U encap/decap in both directions (the stack does not
// short-circuit local destinations), which is exactly what the handover
// tests need: live conns whose segments ride the tunnel. Exits when the UPF
// socket closes.
func hairpinUPF(upf *net.UDPConn, dlTEID *atomic.Uint32) {
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := upf.ReadFromUDP(buf)
			if err != nil {
				return
			}
			g, err := gtpu.DecodeGPDU(buf[:n])
			if err != nil || g.MsgType != gtpu.MsgTypeGPDU || len(g.Payload) == 0 {
				continue
			}
			pkt := append([]byte(nil), g.Payload...)
			if _, err := upf.WriteToUDP(gtpu.EncodeGPDU(dlTEID.Load(), 1, pkt), from); err != nil {
				return
			}
		}
	}()
}

// Phase-6 engine coverage: the per-gNB netstack bridge lifecycle behind
// Session.appNetwork — lazy stack creation beside the n3Pool entry,
// AddAddress on the first TCP app, RemoveAddress + uplink-route cleanup on
// UE release, Stack close with the shared tunnel, and the inter-gNB handover
// address move with its TCP_CONNS_RESET correlation event.

// TestAppNetworkProtocolDimension pins the app→network mapping (design §6
// end): voip rides dgram (UDP-only — TCP refused with the sentinel), http
// and video ride the gNB netstack, anything else is refused by name.
func TestAppNetworkProtocolDimension(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	sess := newDataSession(m, "001010000000041", upf.LocalAddr().String(), "0", 0x11, 0x100, 1)
	t.Cleanup(sess.closeDataPath)
	ueIP := net.ParseIP(sess.Result.PDUAddress)

	if _, err := sess.appNetwork("ftp", ueIP); err == nil || !strings.Contains(err.Error(), "no network mapping") {
		t.Errorf("unknown app: err = %v, want a no-network-mapping refusal", err)
	}

	vnw, err := sess.appNetwork("voip", ueIP)
	if err != nil {
		t.Fatalf("voip network: %v", err)
	}
	defer vnw.Close()
	if _, err := vnw.DialContext(context.Background(), "tcp", "10.0.0.9:80"); !errors.Is(err, dgram.ErrTCPUnsupported) {
		t.Errorf("voip network dial tcp: err = %v, want dgram.ErrTCPUnsupported", err)
	}

	for _, app := range []string{"http", "video"} {
		nw, err := sess.appNetwork(app, ueIP)
		if err != nil {
			t.Fatalf("%s network: %v", app, err)
		}
		if nw.Name() != "orbit-gtp-netstack" {
			t.Errorf("%s network = %q, want the netstack facade", app, nw.Name())
		}
		nw.Close()
	}
	// Both TCP apps shared ONE bridge and ONE address attach.
	sess.dpMu.Lock()
	br := sess.nsBridge
	sess.dpMu.Unlock()
	if br == nil || !br.Attached(ueIP) {
		t.Fatal("TCP apps did not leave the UE attached to its gNB stack")
	}
}

// TestTCPNetworkLifecycle: lazy per-gNB stack on the first TCP app; the
// address and uplink route removed on UE release (live listeners aborted —
// no zombies); the stack closed when the gNB's shared tunnel closes (last
// pool release); lazy full re-open afterwards.
func TestTCPNetworkLifecycle(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	sess := newDataSession(m, "001010000000042", upf.LocalAddr().String(), "0", 0x11, 0x100, 1)
	ueIP := net.ParseIP(sess.Result.PDUAddress)

	nw, err := sess.appNetwork("http", ueIP)
	if err != nil {
		t.Fatalf("first TCP app network: %v", err)
	}
	sess.dpMu.Lock()
	br, ref := sess.nsBridge, sess.n3ref
	sess.dpMu.Unlock()
	if br == nil || !br.Attached(ueIP) {
		t.Fatal("first TCP app must attach the UE address to the gNB stack")
	}
	if ref.ns != br {
		t.Fatal("the bridge must live beside the n3Pool entry (refcount-scoped)")
	}
	ln, err := nw.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen on the UE address: %v", err)
	}

	// A second TCP network on the same session: same bridge, same address.
	nw2, err := sess.appNetwork("http", ueIP)
	if err != nil {
		t.Fatal(err)
	}
	sess.dpMu.Lock()
	same := sess.nsBridge == br
	sess.dpMu.Unlock()
	if !same {
		t.Fatal("second TCP app rebuilt the bridge instead of reusing it")
	}

	// UE release: RemoveAddress (aborting the live listener), route cleanup,
	// facades closed, and — as the last session on the gNB — stack closed
	// with the shared tunnel.
	sess.closeDataPath()
	if br.Attached(ueIP) {
		t.Error("UE address still on the stack after release")
	}
	acceptErr := make(chan error, 1)
	go func() { _, err := ln.Accept(); acceptErr <- err }()
	select {
	case err := <-acceptErr:
		if err == nil {
			t.Error("listener accepted after UE release")
		}
	case <-time.After(3 * time.Second):
		t.Error("listener still alive 3s after UE release (zombie)")
	}
	if _, err := nw2.Listen("tcp", ":0"); err == nil {
		t.Error("closed facade still mints listeners")
	}
	if m.n3.size() != 0 {
		t.Fatalf("pool size after release = %d, want 0", m.n3.size())
	}
	if _, err := br.Network(ueIP); err == nil {
		t.Error("gNB stack survived its shared tunnel's close")
	}

	// Lazy re-open: tunnel, bridge, and address all come back on demand.
	nw3, err := sess.appNetwork("http", ueIP)
	if err != nil {
		t.Fatalf("lazy re-open after release: %v", err)
	}
	if ln3, err := nw3.Listen("tcp", ":0"); err != nil {
		t.Fatalf("listen after re-open: %v", err)
	} else {
		ln3.Close()
	}
	sess.closeDataPath()
}

// TestInterGNBMoveResetsNetstackConns: an inter-gNB data-path move relocates
// the UE's address between gNB stacks via the existing retargetDataPath
// hook. Connections live during the move window are ABORTED (gVisor closes
// conns whose address leaves the stack — loom RemoveAddress semantics), a
// TCP_CONNS_RESET correlation event is emitted, and the SAME facade then
// works against the target gNB's stack — reconnects succeed.
func TestInterGNBMoveResetsNetstackConns(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	m.n3.grace = 0
	portSrc, portTgt := freeUDPPort(t), freeUDPPort(t)
	sess := newDataSession(m, "001010000000043", upf.LocalAddr().String(), portSrc, 0x11, 0x100, 1)
	t.Cleanup(sess.closeDataPath)
	ueIP := net.ParseIP(sess.Result.PDUAddress)
	var dlTEID atomic.Uint32
	dlTEID.Store(0x100)
	hairpinUPF(upf, &dlTEID)
	events, unsub := m.Subscribe()
	defer unsub()

	nw, err := sess.appNetwork("http", ueIP)
	if err != nil {
		t.Fatal(err)
	}
	sess.dpMu.Lock()
	srcBr := sess.nsBridge
	sess.dpMu.Unlock()

	// A live TCP connection on the UE address: listener + dial to the UE's
	// own address. loom's netstack leaves gVisor's HandleLocal at its false
	// default, so even own-address traffic rides the link — every segment
	// crosses real GTP-U encap/decap via hairpinUPF (see its doc above);
	// this is genuine tunnel TCP, not loopback theater.
	ln, err := nw.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := nw.DialContext(dctx, "tcp", ln.Addr().String())
	dcancel()
	if err != nil {
		t.Fatalf("stack-local dial: %v", err)
	}
	var srv net.Conn
	select {
	case srv = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("accept never fired for the local dial")
	}
	defer srv.Close()
	if _, err := conn.Write([]byte("hi")); err != nil {
		t.Fatalf("pre-move write: %v", err)
	}
	buf := make([]byte, 8)
	_ = srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := srv.Read(buf); err != nil {
		t.Fatalf("pre-move read: %v", err)
	}

	// The inter-gNB move (same shape as runXnHandover after the ack).
	sess.n3Port = portTgt
	dlTEID.Store(0x300) // the UPF switches its downlink FAR with the path
	if err := sess.retargetDataPath(dataPathMove{gnbN3: "127.0.0.1", dlTEID: 0x300}); err != nil {
		t.Fatalf("inter-gNB retarget: %v", err)
	}

	// The conns that were live during the move window were aborted.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Error("conn survived the cross-gNB address move (should have been aborted)")
	}

	// The correlation-visible event names the reset.
	deadline := time.After(5 * time.Second)
	var got *StateEvent
wait:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				break wait
			}
			if ev.State == StateTCPConnsReset && ev.SUPI == sess.SUPI {
				evc := ev
				got = &evc
				break wait
			}
		case <-deadline:
			break wait
		}
	}
	if got == nil {
		t.Error("no TCP_CONNS_RESET event after an inter-gNB move with live conns")
	} else if !strings.Contains(got.Detail, "aborted") {
		t.Errorf("event detail %q does not state the aborts", got.Detail)
	}

	// The address moved: off the source stack, on the target's.
	sess.dpMu.Lock()
	tgtBr := sess.nsBridge
	sess.dpMu.Unlock()
	if tgtBr == srcBr {
		t.Fatal("inter-gNB move kept the source gNB's stack")
	}
	if srcBr.Attached(ueIP) {
		t.Error("UE address still on the SOURCE stack after the move")
	}
	if !tgtBr.Attached(ueIP) {
		t.Fatal("UE address not on the TARGET stack after the move")
	}

	// The same facade now runs against the target stack: reconnects succeed.
	ln2, err := nw.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen through the retargeted facade: %v", err)
	}
	accepted2 := make(chan net.Conn, 1)
	go func() {
		c, err := ln2.Accept()
		if err == nil {
			accepted2 <- c
		}
	}()
	dctx2, dcancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	conn2, err := nw.DialContext(dctx2, "tcp", ln2.Addr().String())
	dcancel2()
	if err != nil {
		t.Fatalf("dial through the retargeted facade: %v", err)
	}
	defer conn2.Close()
	select {
	case c := <-accepted2:
		c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("accept never fired on the target stack")
	}
}

// TestIntraGNBMoveKeepsNetstackConns: an intra-gNB handover is a TEID swap
// BELOW the stack — established TCP connections survive it untouched (the
// realism the design promises: TCP sees only delay/loss).
func TestIntraGNBMoveKeepsNetstackConns(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	sess := newDataSession(m, "001010000000044", upf.LocalAddr().String(), "0", 0x11, 0x100, 1)
	t.Cleanup(sess.closeDataPath)
	ueIP := net.ParseIP(sess.Result.PDUAddress)
	var dlTEID atomic.Uint32
	dlTEID.Store(0x100)
	hairpinUPF(upf, &dlTEID)

	nw, err := sess.appNetwork("http", ueIP)
	if err != nil {
		t.Fatal(err)
	}
	sess.dpMu.Lock()
	br := sess.nsBridge
	sess.dpMu.Unlock()

	ln, err := nw.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := nw.DialContext(dctx, "tcp", ln.Addr().String())
	dcancel()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var srv net.Conn
	select {
	case srv = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("accept never fired")
	}
	defer srv.Close()

	// Intra-gNB handover: same N3 address, new DL TEID.
	dlTEID.Store(0x200) // the UPF's downlink FAR moves with the TEID swap
	if err := sess.retargetDataPath(dataPathMove{dlTEID: 0x200}); err != nil {
		t.Fatalf("intra-gNB retarget: %v", err)
	}
	sess.dpMu.Lock()
	sameBr := sess.nsBridge == br
	sess.dpMu.Unlock()
	if !sameBr {
		t.Fatal("intra-gNB move must not touch the gNB stack")
	}
	if _, err := conn.Write([]byte("still here")); err != nil {
		t.Fatalf("write after intra-gNB move: %v", err)
	}
	buf := make([]byte, 16)
	_ = srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := srv.Read(buf)
	if err != nil || string(buf[:n]) != "still here" {
		t.Fatalf("read after intra-gNB move: %q, %v", buf[:n], err)
	}
}
