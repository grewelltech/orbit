package engine

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gtpu"
)

// readGPDU reads one G-PDU from the fake UPF within d and returns the source
// address it came from and its TEID; ok is false when nothing arrived.
func readGPDU(t *testing.T, upf *net.UDPConn, d time.Duration) (src *net.UDPAddr, teid uint32, ok bool) {
	t.Helper()
	buf := make([]byte, 2048)
	if err := upf.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	n, from, err := upf.ReadFromUDP(buf)
	if err != nil {
		return nil, 0, false
	}
	hdr, err := gtpu.DecodeGPDU(buf[:n])
	if err != nil {
		t.Fatalf("decode G-PDU from %s: %v", from, err)
	}
	return from, hdr.TEID, true
}

// An uplink write that lands on the source socket an inter-gNB rebind has
// just released fails with net.ErrClosed (the race #110 describes, made
// deterministic here with grace 0 and a view held across the move). The
// retrying sender then delivers the packet on the current view: the fake UPF
// receives exactly one G-PDU, from the target source address, with the UE's
// UL TEID.
func TestSendUplinkRetriesOnceOnStaleView(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	m.n3.grace = 0
	portSrc, portTgt := freeUDPPort(t), freeUDPPort(t)
	sess := newDataSession(m, "001010000000001", upf.LocalAddr().String(), portSrc, 0x11, 0x100, 1)
	if _, _, err := sess.dataplane(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.closeDataPath)
	pkt := []byte{0x45, 0, 0, 4}

	old := sess.ueUp.Load()
	if old == nil {
		t.Fatal("no view after dataplane()")
	}
	// The inter-gNB move: new N3 port on the same host, new DL TEID.
	sess.n3Port = portTgt
	if err := sess.retargetDataPath(dataPathMove{gnbN3: "127.0.0.1", dlTEID: 0x300}); err != nil {
		t.Fatalf("inter-gNB retarget: %v", err)
	}
	cur := sess.ueUp.Load()
	if cur == nil || cur == old {
		t.Fatal("the view did not move")
	}
	tgtAddr := sess.n3ref.st.LocalAddr().(*net.UDPAddr)

	// Precondition, proven not assumed: the stale view's socket is closed.
	if err := old.SendUplink(pkt); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stale view write err = %v, want net.ErrClosed", err)
	}

	// A sender that loaded the stale view before the move and writes after it.
	loads := 0
	load := func() *datapath.UETunnel {
		loads++
		if loads == 1 {
			return old
		}
		return sess.ueUp.Load()
	}
	if err := sendUplinkRetrying(load, sess.SUPI, pkt); err != nil {
		t.Fatalf("retrying send: %v", err)
	}
	if loads != 2 {
		t.Errorf("view loaded %d times, want 2 (stale, then current)", loads)
	}
	src, teid, ok := readGPDU(t, upf, 2*time.Second)
	if !ok {
		t.Fatal("the UPF received no G-PDU")
	}
	if src.Port != tgtAddr.Port {
		t.Errorf("G-PDU came from %s, want the target source %s", src, tgtAddr)
	}
	if teid != 0x11 {
		t.Errorf("G-PDU TEID = %#x, want the UE's UL TEID 0x11", teid)
	}
	if src2, _, ok := readGPDU(t, upf, 300*time.Millisecond); ok {
		t.Errorf("a second G-PDU arrived from %s; the packet must be sent exactly once", src2)
	}

	// The ordinary path after the move needs no retry and still delivers one.
	if err := sess.SendUplink(pkt); err != nil {
		t.Fatalf("SendUplink after the move: %v", err)
	}
	if _, _, ok := readGPDU(t, upf, 2*time.Second); !ok {
		t.Fatal("the UPF received no G-PDU from the moved session")
	}
}

// A torn-down session reports "no open N3 data path"; a stale view held
// across the teardown returns the closed error with no retry, because the
// current view is nil: a teardown is the documented no-silent-blackhole
// outcome, not a handover.
func TestSendUplinkTeardownDoesNotRetry(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	m.n3.grace = 0
	sess := newDataSession(m, "001010000000001", upf.LocalAddr().String(), freeUDPPort(t), 0x11, 0x100, 1)
	if _, _, err := sess.dataplane(); err != nil {
		t.Fatal(err)
	}
	old := sess.ueUp.Load()
	sess.closeDataPath()
	pkt := []byte{0x45, 0, 0, 4}

	err := sess.SendUplink(pkt)
	if err == nil || !strings.Contains(err.Error(), "has no open N3 data path") {
		t.Errorf("SendUplink after teardown err = %v, want no open N3 data path", err)
	}

	loads := 0
	load := func() *datapath.UETunnel {
		loads++
		if loads == 1 {
			return old
		}
		return sess.ueUp.Load() // nil after the teardown
	}
	err = sendUplinkRetrying(load, sess.SUPI, pkt)
	if !errors.Is(err, net.ErrClosed) {
		t.Errorf("stale view across a teardown err = %v, want net.ErrClosed", err)
	}
	if loads != 2 {
		t.Errorf("view loaded %d times, want 2", loads)
	}
	if _, _, ok := readGPDU(t, upf, 300*time.Millisecond); ok {
		t.Error("a G-PDU arrived from a torn-down session")
	}
}

// The retry never loops: when the second view's socket is closed too, its
// error comes back once and the view is not loaded again. Two sessions'
// views, both torn down, stand in for a stale view and its closed successor.
func TestSendUplinkRetryDoesNotLoop(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	m.n3.grace = 0
	a := newDataSession(m, "001010000000001", upf.LocalAddr().String(), freeUDPPort(t), 0x11, 0x100, 1)
	b := newDataSession(m, "001010000000002", upf.LocalAddr().String(), freeUDPPort(t), 0x21, 0x200, 1)
	if _, _, err := a.dataplane(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.dataplane(); err != nil {
		t.Fatal(err)
	}
	viewA, viewB := a.ueUp.Load(), b.ueUp.Load()
	a.closeDataPath()
	b.closeDataPath()
	pkt := []byte{0x45, 0, 0, 4}

	views := []*datapath.UETunnel{viewA, viewB, viewA, viewB}
	loads := 0
	load := func() *datapath.UETunnel {
		v := views[loads]
		loads++
		return v
	}
	err := sendUplinkRetrying(load, a.SUPI, pkt)
	if !errors.Is(err, net.ErrClosed) {
		t.Errorf("err = %v, want net.ErrClosed from the second view", err)
	}
	if loads != 2 {
		t.Errorf("view loaded %d times, want exactly 2 (one retry, never a loop)", loads)
	}

	// Errors other than ErrClosed are returned as they are, with no retry:
	// a view whose write fails for another reason is not a moved view. A
	// nil first view is the no-data-path error without a second load.
	loads = 0
	err = sendUplinkRetrying(func() *datapath.UETunnel { loads++; return nil }, a.SUPI, pkt)
	if err == nil || !strings.Contains(err.Error(), "has no open N3 data path") || loads != 1 {
		t.Errorf("nil view: err = %v, loads = %d; want the no-data-path error after one load", err, loads)
	}
}
