package loomgtp

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	loomapp "github.com/bgrewell/loom/core/app"
	"github.com/bgrewell/loom/core/app/httpx"
	loomdp "github.com/bgrewell/loom/core/datapath"
	"github.com/bgrewell/loom/core/metrics"
	"github.com/bgrewell/loom/core/netstack"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gtpu"
)

// Phase-6 rig: the per-gNB GNBStack bridge over a real SharedTunnel, a fake
// UPF in the middle, and a SECOND loom netstack as the N6 far end running
// loom's httpx origin — so an HTTP/TLS fetch crosses real GTP-U encap/decap
// with real TCP SYN and TLS ClientHello bytes as inner IP packets.
//
//	httpx client ─ GNBStack(nw per UE) ─ SharedTunnel ══ GTP-U ══ fake UPF
//	                                                              │ capture
//	                       httpx origin ─ far-end netstack ───────┘

const (
	ue1ULTEID, ue1DLTEID = 0xA1, 0xA2
	ue2ULTEID, ue2DLTEID = 0xB1, 0xB2
	tcpRigQFI            = 9
)

var (
	tcpUE1IP = net.ParseIP("192.168.100.11")
	tcpUE2IP = net.ParseIP("192.168.100.12")
	n6IP     = netip.MustParseAddr("10.0.0.9")
)

// ulCapture accumulates what the fake UPF saw on one uplink TEID.
type ulCapture struct {
	wantSrc netip.Addr
	packets int
	tcp     int
	badSrc  int
	tlsRec  bool // a TCP payload starting with a TLS handshake record (0x16 0x03)
}

// tcpUPF is the fake UPF: it decaps every uplink G-PDU, captures the inner
// packet per TEID, and forwards it into the far-end netstack's receive ring.
type tcpUPF struct {
	t    *testing.T
	conn *net.UDPConn
	ring *datapath.Ring

	mu   sync.Mutex
	gnb  *net.UDPAddr
	caps map[uint32]*ulCapture
}

func (u *tcpUPF) run() {
	buf := make([]byte, 65536)
	for {
		n, from, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		g, err := gtpu.DecodeGPDU(buf[:n])
		if err != nil || g.MsgType != gtpu.MsgTypeGPDU || len(g.Payload) == 0 {
			continue
		}
		pkt := make([]byte, len(g.Payload))
		copy(pkt, g.Payload)
		u.mu.Lock()
		u.gnb = from
		if c := u.caps[g.TEID]; c != nil {
			c.packets++
			if len(pkt) >= 20 && pkt[0]>>4 == 4 {
				if netip.AddrFrom4([4]byte(pkt[12:16])) != c.wantSrc {
					c.badSrc++
				}
				if pkt[9] == 6 { // inner TCP — the real SYN/data segments
					c.tcp++
					ihl := int(pkt[0]&0x0F) * 4
					if len(pkt) >= ihl+20 {
						off := int(pkt[ihl+12]>>4) * 4
						if pl := pkt[ihl+off:]; len(pl) >= 2 && pl[0] == 0x16 && pl[1] == 0x03 {
							c.tlsRec = true // TLS handshake record (ClientHello…)
						}
					}
				}
			}
		} else {
			u.t.Errorf("uplink G-PDU on unexpected TEID %#x", g.TEID)
		}
		u.mu.Unlock()
		u.ring.Push(pkt, time.Now())
	}
}

func (u *tcpUPF) gnbAddr() *net.UDPAddr {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.gnb
}

func (u *tcpUPF) capture(teid uint32) ulCapture {
	u.mu.Lock()
	defer u.mu.Unlock()
	return *u.caps[teid]
}

// n6Tx is the far-end netstack's TxDatapath: it routes each outbound inner
// packet by DESTINATION address to that UE's DL TEID and sends it down the
// tunnel to the gNB socket — the UPF's downlink FAR, in one type.
type n6Tx struct {
	upf    *tcpUPF
	routes map[netip.Addr]uint32
	pool   []loomdp.Frame
}

func newN6Tx(upf *tcpUPF, routes map[netip.Addr]uint32) *n6Tx {
	t := &n6Tx{upf: upf, routes: routes, pool: make([]loomdp.Frame, poolDepth)}
	for i := range t.pool {
		t.pool[i].Data = make([]byte, DefaultInnerMTU)
	}
	return t
}

func (t *n6Tx) Name() string              { return "test-n6" }
func (t *n6Tx) Caps() loomdp.Capabilities { return loomdp.Capabilities{RawL3: true} }
func (t *n6Tx) Close() error              { return nil }
func (t *n6Tx) TxReserve(n int) []loomdp.Frame {
	if n > len(t.pool) {
		n = len(t.pool)
	}
	for i := 0; i < n; i++ {
		t.pool[i].Len = 0
	}
	return t.pool[:n]
}

func (t *n6Tx) TxCommit(frames []loomdp.Frame) (int, error) {
	sent := 0
	for i := range frames {
		if frames[i].Len == 0 {
			continue
		}
		pkt := frames[i].Data[:frames[i].Len]
		sent++
		if len(pkt) < 20 || pkt[0]>>4 != 4 {
			continue
		}
		teid, ok := t.routes[netip.AddrFrom4([4]byte(pkt[16:20]))]
		gnb := t.upf.gnbAddr()
		if !ok || gnb == nil {
			continue // downlink before any uplink (no gNB yet) — TCP retransmits
		}
		if _, err := t.upf.conn.WriteToUDP(gtpu.EncodeGPDU(teid, tcpRigQFI, pkt), gnb); err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// TestGNBStackHTTPTwoUEsOverGTPU is the Phase-6 acceptance shape: two UEs on
// ONE gNB socket and ONE shared gVisor stack fetch HTTPS objects from a loom
// httpx origin through the tunnel, concurrently. The fake UPF asserts the
// wire truth: the inner packets are TCP (protocol 6), carry a TLS handshake
// record, and each UE's uplink TEID carries ONLY that UE's source address —
// the ueIP→uplink TX routing of the shared stack.
func TestGNBStackHTTPTwoUEsOverGTPU(t *testing.T) {
	upfConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upfConn.Close() })

	// Far end: a second netstack hosting the httpx origin behind the UPF.
	n6Ring := datapath.NewRing(1024)
	upf := &tcpUPF{
		t: t, conn: upfConn, ring: n6Ring,
		caps: map[uint32]*ulCapture{
			ue1ULTEID: {wantSrc: netip.MustParseAddr("192.168.100.11")},
			ue2ULTEID: {wantSrc: netip.MustParseAddr("192.168.100.12")},
		},
	}
	n6Stack, err := netstack.New(netstack.Config{MTU: DefaultInnerMTU},
		newN6Tx(upf, map[netip.Addr]uint32{
			netip.MustParseAddr("192.168.100.11"): ue1DLTEID,
			netip.MustParseAddr("192.168.100.12"): ue2DLTEID,
		}),
		newRx(n6Ring, nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n6Stack.Close() })
	if err := n6Stack.AddAddress(n6IP); err != nil {
		t.Fatal(err)
	}
	go upf.run()

	origin, err := httpx.NewServer(loomapp.Options{
		Network: n6Stack.Network(n6IP),
		Params:  map[string]string{"tls": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	certPEM := origin.(interface{ CertificatePEM() []byte }).CertificatePEM()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	originDone := make(chan struct{})
	go func() { defer close(originDone); _ = origin.Run(ctx) }()
	target := fmt.Sprintf("%s:%d", n6IP, origin.Addr().Port())

	// UE side: one SharedTunnel (the gNB's single N3 socket), one GNBStack.
	st, err := datapath.NewSharedTunnel("127.0.0.1:0", upfConn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tun1, err := st.Register(datapath.UETunnelConfig{ULTEID: ue1ULTEID, DLTEID: ue1DLTEID, QFI: tcpRigQFI})
	if err != nil {
		t.Fatal(err)
	}
	tun2, err := st.Register(datapath.UETunnelConfig{ULTEID: ue2ULTEID, DLTEID: ue2DLTEID, QFI: tcpRigQFI})
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGNBStack(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	if err := g.Attach(tcpUE1IP, tun1, tun1.Lane()); err != nil {
		t.Fatal(err)
	}
	if err := g.Attach(tcpUE2IP, tun2, tun2.Lane()); err != nil {
		t.Fatal(err)
	}

	// Two UEs browse CONCURRENTLY over the one socket and one shared stack.
	fetch := func(ueIP net.IP) error {
		nw, err := g.Network(ueIP)
		if err != nil {
			return err
		}
		defer nw.Close()
		cl, err := httpx.NewClient(loomapp.Options{
			Network: nw,
			Target:  target,
			Params: map[string]string{
				"tls":         "true",
				"tls_ca":      base64.StdEncoding.EncodeToString(certPEM),
				"objects":     "3",
				"object_size": "16KB",
			},
		})
		if err != nil {
			return err
		}
		rctx, rcancel := context.WithTimeout(ctx, 60*time.Second)
		defer rcancel()
		if err := cl.Run(rctx); err != nil {
			return fmt.Errorf("UE %v client: %w", ueIP, err)
		}
		h, ok := cl.(interface{ CumulativeMetrics() metrics.Snapshot }).CumulativeMetrics().(metrics.HTTP)
		if !ok {
			return fmt.Errorf("UE %v: no HTTP metrics snapshot", ueIP)
		}
		if h.Requests != 3 || h.Errors != 0 {
			return fmt.Errorf("UE %v: requests=%d errors=%d, want 3/0", ueIP, h.Requests, h.Errors)
		}
		return nil
	}
	errs := make(chan error, 2)
	for _, ip := range []net.IP{tcpUE1IP, tcpUE2IP} {
		go func(ip net.IP) { errs <- fetch(ip) }(ip)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	// Wire truth, per UE: real TCP inside GTP-U, a real TLS handshake record,
	// and no cross-UE source leakage on either uplink TEID.
	for teid, ue := range map[uint32]string{ue1ULTEID: "UE1", ue2ULTEID: "UE2"} {
		c := upf.capture(teid)
		if c.tcp == 0 {
			t.Errorf("%s: no inner TCP packets captured on TEID %#x", ue, teid)
		}
		if !c.tlsRec {
			t.Errorf("%s: no TLS handshake record seen inside GTP-U on TEID %#x", ue, teid)
		}
		if c.badSrc != 0 {
			t.Errorf("%s: %d packets on TEID %#x with the WRONG inner source IP (ueIP→uplink routing broken)", ue, c.badSrc, teid)
		}
	}
	if n := g.UnknownSourceDrops(); n != 0 {
		t.Errorf("UnknownSourceDrops = %d during a clean two-UE run, want 0", n)
	}
	cancel()
	<-originDone
}

// captureUplink counts and retains uplink sends.
type captureUplink struct {
	mu   sync.Mutex
	pkts [][]byte
}

func (c *captureUplink) SendUplink(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pkts = append(c.pkts, append([]byte(nil), p...))
	return nil
}

func (c *captureUplink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pkts)
}

// TestStackTxUnknownSourceDrops pins the TX routing contract: a frame whose
// inner source IP matches no attached UE is consumed, dropped, and COUNTED —
// never sent up someone else's tunnel and never surfaced as a datapath error
// (which would abort the whole gNB's write batch).
func TestStackTxUnknownSourceDrops(t *testing.T) {
	up := &captureUplink{}
	tx := newStackTx(512)
	known := netip.MustParseAddr("192.168.100.7")
	tx.setUplink(known, up)

	mk := func(src net.IP) []byte {
		pkt, err := datapath.BuildUDPPacket(src, net.IPv4(10, 0, 0, 9), 1234, 80, []byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		return pkt
	}
	frames := tx.TxReserve(3)
	if len(frames) != 3 {
		t.Fatalf("TxReserve gave %d frames, want 3", len(frames))
	}
	fill := func(i int, b []byte) {
		frames[i].Len = copy(frames[i].Data, b)
	}
	fill(0, mk(net.ParseIP("192.168.100.7"))) // routable
	fill(1, mk(net.ParseIP("192.168.100.8"))) // unknown UE
	fill(2, []byte{0x60, 0, 0, 0})            // not IPv4 at all

	sent, err := tx.TxCommit(frames)
	if err != nil {
		t.Fatalf("TxCommit: %v", err)
	}
	if sent != 3 {
		t.Errorf("sent = %d, want 3 (dropped frames are consumed, not retried forever)", sent)
	}
	if got := up.count(); got != 1 {
		t.Errorf("uplink got %d packets, want exactly the 1 routable one", got)
	}
	if got := tx.unknownDrops.Load(); got != 2 {
		t.Errorf("unknownDrops = %d, want 2", got)
	}
}

// TestGNBStackAddressLifecycle covers Attach/Detach on one bridge: duplicate
// attach refused, Detach removes the address (aborting its listeners — the
// documented loom RemoveAddress semantics), cleans the ueIP→uplink route so
// stragglers count as unknown-source drops, and a re-Attach works.
func TestGNBStackAddressLifecycle(t *testing.T) {
	lane := (&upfRigLane{}).uerx(t)
	g, err := NewGNBStack(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	up := &captureUplink{}
	ueIP := net.ParseIP("192.168.100.7")
	a := netip.MustParseAddr("192.168.100.7")

	if err := g.Attach(ueIP, up, lane); err != nil {
		t.Fatal(err)
	}
	if !g.Attached(ueIP) {
		t.Fatal("Attached = false right after Attach")
	}
	if err := g.Attach(ueIP, up, lane); err == nil {
		t.Error("duplicate Attach accepted")
	}
	nw, err := g.Network(ueIP)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := nw.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen on the attached address: %v", err)
	}

	g.Detach(ueIP)
	if g.Attached(ueIP) {
		t.Error("Attached = true after Detach")
	}
	if _, err := g.Network(ueIP); err == nil {
		t.Error("Network minted a view for a detached address")
	}
	// RemoveAddress aborts what lived on the address (no zombies).
	acceptErr := make(chan error, 1)
	go func() { _, err := ln.Accept(); acceptErr <- err }()
	select {
	case err := <-acceptErr:
		if err == nil {
			t.Error("listener accepted after Detach")
		}
	case <-time.After(3 * time.Second):
		t.Error("listener still alive 3s after Detach (zombie)")
	}
	// The uplink route is gone: packets sourced from the released address
	// now count as unknown-source drops instead of riding a stale tunnel.
	if got := g.tx.uplink(a); got != nil {
		t.Error("uplink route survived Detach")
	}
	before := g.UnknownSourceDrops()
	pkt, err := datapath.BuildUDPPacket(ueIP, net.IPv4(10, 0, 0, 9), 1234, 80, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	frames := g.tx.TxReserve(1)
	frames[0].Len = copy(frames[0].Data, pkt)
	if _, err := g.tx.TxCommit(frames); err != nil {
		t.Fatal(err)
	}
	if got := g.UnknownSourceDrops(); got != before+1 {
		t.Errorf("UnknownSourceDrops after post-Detach send = %d, want %d", got, before+1)
	}
	// Detach twice is a no-op; a fresh Attach brings the address back.
	g.Detach(ueIP)
	if err := g.Attach(ueIP, up, lane); err != nil {
		t.Fatalf("re-Attach after Detach: %v", err)
	}
	if _, err := g.Network(ueIP); err != nil {
		t.Fatalf("Network after re-Attach: %v", err)
	}
}

// TestAttachSinkPreservesArrivalStamp pins the RX-timestamp seam the bridge
// documents ("arrival ts kept", ADR-0020): the default sink Attach installs
// on the UE's demux lane must push the DEMUX READER's socket-read arrival
// stamp into the stack's feed ring — not a zero time and not the (possibly
// much later) dequeue time. The jitter regression (jitter_test.go) starts
// one layer below, at a raw ring, so this is the only pin on the GNBStack
// sink itself; the bridge is assembled by hand around an observable ring the
// netstack does NOT consume, keeping the pushed frames inspectable.
func TestAttachSinkPreservesArrivalStamp(t *testing.T) {
	// Real demux over a real socket: the lane's sink runs on the demux
	// reader with the socket-read timestamp, exactly as in production.
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	d := datapath.NewDemux(conn)
	t.Cleanup(func() { d.Close() })
	const teid = 0xC1
	lane := d.Register(teid)

	obsRing := datapath.NewRing(16)
	tx := newStackTx(DefaultInnerMTU)
	st, err := netstack.New(netstack.Config{MTU: DefaultInnerMTU}, tx, newRx(datapath.NewRing(4), nil))
	if err != nil {
		t.Fatal(err)
	}
	g := &GNBStack{stack: st, tx: tx, ring: obsRing, lanes: make(map[netip.Addr]*datapath.UERx)}
	t.Cleanup(func() { _ = g.Close() })

	ueIP := net.ParseIP("192.168.100.21")
	if err := g.Attach(ueIP, &captureUplink{}, lane); err != nil {
		t.Fatal(err)
	}

	// One downlink G-PDU whose inner packet nothing subscribed to (UDP, no
	// port lane) — it must reach the default sink, i.e. the stack feed ring.
	inner, err := datapath.BuildUDPPacket(net.IPv4(10, 0, 0, 9), ueIP, 9999, 8888, []byte("stamp"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := net.DialUDP("udp", nil, conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	before := time.Now()
	if _, err := src.Write(gtpu.EncodeGPDU(teid, tcpRigQFI, inner)); err != nil {
		t.Fatal(err)
	}

	// Dequeue LATE on purpose: a sink that stamped dequeue time (or dropped
	// the stamp) fails; the demux reader's arrival stamp passes.
	time.Sleep(300 * time.Millisecond)
	f, err := obsRing.Read(2 * time.Second)
	if err != nil {
		t.Fatalf("stack feed ring got no frame: %v", err)
	}
	after := time.Now()
	if f.Arrival.IsZero() {
		t.Fatal("sink pushed a zero arrival stamp (Frame.Meta timestamp lost)")
	}
	if f.Arrival.Before(before) || f.Arrival.After(before.Add(150*time.Millisecond)) {
		t.Errorf("arrival stamp %v not at socket-read time (sent @%v, dequeued @%v): dequeue-time stamping?",
			f.Arrival, before, after)
	}
	if after.Sub(f.Arrival) < 250*time.Millisecond {
		t.Errorf("arrival stamp is dequeue-time (%v before dequeue), not the demux reader's socket-read stamp",
			after.Sub(f.Arrival))
	}
}
