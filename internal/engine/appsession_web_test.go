package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	loomdp "github.com/bgrewell/loom/core/datapath"
	"github.com/bgrewell/loom/core/netstack"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gtpu"
)

// The TCP e2e rig (design §12 Phases 6-7 test shape): the same in-process
// loom control agent plays the N6 loomd — its HTTPOrigin flow serves on the
// REAL kernel loopback (network "host", exactly what a stock loomd does) —
// and an n6Gateway plays the UPF's N6 boundary for TCP: uplink G-PDUs are
// decapped and their inner TCP terminated on a far-side loom netstack owning
// the N6 data address, each accepted connection spliced byte-for-byte onto a
// kernel TCP connection to the origin. So a session crosses the whole stack:
//
//	httpx/vidstream client → per-gNB gVisor stack → GTP-U → n6Gateway
//	   (netstack terminate + splice, optionally throttled) → kernel TCP
//	   → loomd HTTPOrigin on loopback
//
// The splice throttle is the rig's link shaper: capping the downlink byte
// rate below the ladder bitrate forces real player stalls (Phase 7's
// throttled-variant test) without touching qdiscs.

const (
	webTestSUPI   = "001010000000077"
	webTestULTEID = 0x0501
	webTestDLTEID = 0x0502
	webTestQFI    = uint8(9)
	// webGatewayIP is the N6 data address the gateway's netstack owns — the
	// address app sessions aim their media at (PeerDataIP).
	webGatewayIP = "10.99.0.1"
)

// webPollTimeout mirrors the loomgtp rx poll window: blocking RxPolls wake at
// least this often so close is observed.
const webPollTimeout = 200 * time.Millisecond

type webTimeoutError struct{}

func (webTimeoutError) Error() string   { return "n6 gateway rx: poll window elapsed" }
func (webTimeoutError) Timeout() bool   { return true }
func (webTimeoutError) Temporary() bool { return true }

// gwRx feeds the gateway netstack from the UPF socket reader's ring — the
// same RxDatapath shape as loomgtp's bridge (RawL3 frames, arrival stamps).
type gwRx struct {
	ring   *datapath.Ring
	frames []loomdp.Frame
}

func (r *gwRx) Name() string              { return "test-n6-gateway" }
func (r *gwRx) Caps() loomdp.Capabilities { return loomdp.Capabilities{RawL3: true} }
func (r *gwRx) Close() error              { r.ring.Close(); return nil }
func (r *gwRx) RxRelease([]loomdp.Frame)  {}

func (r *gwRx) RxPoll(max int) ([]loomdp.Frame, error) {
	if max <= 0 {
		return nil, nil
	}
	f, err := r.ring.Read(webPollTimeout)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, net.Error(webTimeoutError{})
		}
		return nil, err
	}
	r.frames = r.frames[:0]
	r.frames = append(r.frames, loomdp.Frame{Data: f.Payload, Len: len(f.Payload)})
	for len(r.frames) < max && r.ring.Len() > 0 {
		f, err := r.ring.Read(0)
		if err != nil {
			break
		}
		r.frames = append(r.frames, loomdp.Frame{Data: f.Payload, Len: len(f.Payload)})
	}
	return r.frames, nil
}

// gwTx sends the gateway netstack's outbound inner IP packets (server → UE)
// down the tunnel: GTP-U encap under the UE's DL TEID toward the gNB socket
// learned from the last uplink — the UPF's downlink FAR for one UE.
type gwTx struct {
	upf    *net.UDPConn
	gnb    *atomic.Pointer[net.UDPAddr]
	dlTEID uint32
	pool   []loomdp.Frame
}

func newGwTx(upf *net.UDPConn, gnb *atomic.Pointer[net.UDPAddr], dlTEID uint32) *gwTx {
	t := &gwTx{upf: upf, gnb: gnb, dlTEID: dlTEID, pool: make([]loomdp.Frame, 64)}
	for i := range t.pool {
		t.pool[i].Data = make([]byte, 1400)
	}
	return t
}

func (t *gwTx) Name() string              { return "test-n6-gateway" }
func (t *gwTx) Caps() loomdp.Capabilities { return loomdp.Capabilities{RawL3: true} }
func (t *gwTx) Close() error              { return nil }

func (t *gwTx) TxReserve(n int) []loomdp.Frame {
	if n > len(t.pool) {
		n = len(t.pool)
	}
	for i := 0; i < n; i++ {
		t.pool[i].Len = 0
	}
	return t.pool[:n]
}

func (t *gwTx) TxCommit(frames []loomdp.Frame) (int, error) {
	sent := 0
	for i := range frames {
		if frames[i].Len == 0 {
			continue
		}
		sent++
		gnb := t.gnb.Load()
		if gnb == nil {
			continue // downlink before any uplink — TCP retransmits
		}
		pkt := frames[i].Data[:frames[i].Len]
		if _, err := t.upf.WriteToUDP(gtpu.EncodeGPDU(t.dlTEID, webTestQFI, pkt), gnb); err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// n6Gateway is the rig's UPF-N6 boundary for TCP flows (one UE).
type n6Gateway struct {
	t     *testing.T
	upf   *net.UDPConn
	stack *netstack.Stack
	ring  *datapath.Ring
	gnb   atomic.Pointer[net.UDPAddr]
	// rateBps throttles the DOWNLINK splice direction (origin → UE) in bytes
	// per second; 0 = unlimited. The rig's link shaper.
	rateBps atomic.Int64
}

// newN6Gateway builds the gateway on upf's socket, terminating TCP for
// webGatewayIP:port on its netstack and splicing every accepted connection
// to originAddr on the kernel. It owns the UPF socket's single reader.
func newN6Gateway(t *testing.T, upf *net.UDPConn, dlTEID uint32, port int, originAddr string) *n6Gateway {
	t.Helper()
	g := &n6Gateway{t: t, upf: upf, ring: datapath.NewRing(1024)}
	st, err := netstack.New(netstack.Config{MTU: 1400},
		newGwTx(upf, &g.gnb, dlTEID), &gwRx{ring: g.ring})
	if err != nil {
		t.Fatal(err)
	}
	g.stack = st
	t.Cleanup(func() { st.Close() })
	addr := netip.MustParseAddr(webGatewayIP)
	if err := st.AddAddress(addr); err != nil {
		t.Fatal(err)
	}
	ln, err := st.Network(addr).Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	// Single reader on the UPF socket: decap, learn the gNB address, feed the
	// gateway stack. Payload copied — DecodeGPDU aliases the read buffer.
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := upf.ReadFromUDP(buf)
			if err != nil {
				return
			}
			gp, err := gtpu.DecodeGPDU(buf[:n])
			if err != nil || gp.MsgType != gtpu.MsgTypeGPDU || len(gp.Payload) == 0 {
				continue
			}
			g.gnb.Store(from)
			pkt := make([]byte, len(gp.Payload))
			copy(pkt, gp.Payload)
			g.ring.Push(pkt, time.Now())
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go g.splice(c, originAddr)
		}
	}()
	return g
}

// splice bridges one terminated UE connection onto a kernel TCP connection
// to the origin, pacing the downlink direction when a rate is set.
func (g *n6Gateway) splice(ue net.Conn, originAddr string) {
	kern, err := net.Dial("tcp", originAddr)
	if err != nil {
		g.t.Logf("n6 gateway dial origin %s: %v", originAddr, err)
		ue.Close()
		return
	}
	go func() { // uplink direction: UE → origin
		_, _ = io.Copy(kern, ue)
		_ = kern.(*net.TCPConn).CloseWrite()
	}()
	buf := make([]byte, 4096)
	for {
		n, rerr := kern.Read(buf)
		if n > 0 {
			if rate := g.rateBps.Load(); rate > 0 {
				time.Sleep(time.Duration(int64(n) * int64(time.Second) / rate))
			}
			if _, werr := ue.Write(buf[:n]); werr != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	ue.Close()
	kern.Close()
}

// freeTCPPortInt reserves a currently-free TCP port so the origin (kernel)
// and the gateway listener (netstack) can agree on it up front via loom's
// port_min/port_max params — the firewall-determinism knob doing test duty.
func freeTCPPortInt(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// newWebTestRig assembles manager + UE session + gateway and returns them
// with the pinned origin port.
func newWebTestRig(t *testing.T) (*Manager, *Session, *n6Gateway, int) {
	t.Helper()
	upf := newFakeUPFSocket(t)
	port := freeTCPPortInt(t)
	gw := newN6Gateway(t, upf, webTestDLTEID, port, fmt.Sprintf("127.0.0.1:%d", port))
	m := NewManager(testLogger())
	m.apps.tuning = appTuning{
		syncInterval:   100 * time.Millisecond,
		syncBurst:      3,
		sampleInterval: 100 * time.Millisecond,
		trackerWindow:  200 * time.Millisecond,
		trackerN:       4,
		rpcTimeout:     5 * time.Second,
		stopWait:       5 * time.Second,
	}
	sess := newDataSession(m, webTestSUPI, upf.LocalAddr().String(), "0", webTestULTEID, webTestDLTEID, webTestQFI)
	t.Cleanup(sess.closeDataPath)
	return m, sess, gw, port
}

// collectWebSamples drains the session stream until want returns true or the
// deadline passes, returning the local/remote samples seen so far.
func collectWebSamples(t *testing.T, ch <-chan AppSample, deadline time.Duration, want func(local, remote []AppSample) bool) (local, remote []AppSample) {
	t.Helper()
	limit := time.After(deadline)
	for {
		select {
		case s, ok := <-ch:
			if !ok {
				return local, remote
			}
			if s.HTTP == nil && s.Video == nil {
				continue
			}
			if s.End == AppEndN6 {
				remote = append(remote, s)
			} else {
				local = append(local, s)
			}
			if want(local, remote) {
				return local, remote
			}
		case <-limit:
			t.Fatalf("web samples deadline: local %d, remote %d", len(local), len(remote))
		}
	}
}

// TestAppSessionHTTPEndToEnd runs a real HTTPS fetch loop from the UE through
// the per-gNB gVisor stack and the GTP-U tunnel to a real loom agent's
// HTTPOrigin on the kernel loopback: interval samples from both ends stream
// (client TTFB/goodput, origin request counts), TLS really handshakes inside
// the tunnel, and the final report carries populated whole-run HTTP snapshots
// from both ends.
//
// TLS note (honest limitation, documented in docs/USAGE.md): loom's control
// plane does not expose a remote origin's per-flow self-signed certificate
// (ConfigureResponse carries only flow_id/data_port; CertificatePEM is an
// in-process accessor), so a remote origin cannot be tls_ca-pinned by orbit —
// tls_insecure is the explicit lab opt-in this test exercises.
func TestAppSessionHTTPEndToEnd(t *testing.T) {
	m, _, _, port := newWebTestRig(t)
	agent := startLoomAgent(t, "v0.11.0-test")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id, err := m.StartAppSession(ctx, webTestSUPI, AppSessionConfig{
		App:        "http",
		PeerAgent:  agent,
		PeerDataIP: webGatewayIP,
		Params: map[string]string{
			"port_min":     fmt.Sprint(port),
			"port_max":     fmt.Sprint(port),
			"object_size":  "24KB",
			"think":        "20ms",
			"tls":          "true",
			"tls_insecure": "true",
		},
		Duration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartAppSession: %v", err)
	}
	ch, cancelSub := m.AppSessionEvents(id)
	defer cancelSub()

	// Both ends must rate intervals: the UE side with real timings, the
	// origin side with served-request counts.
	collectWebSamples(t, ch, 30*time.Second, func(local, remote []AppSample) bool {
		var localOK, remoteOK bool
		for _, s := range local {
			if s.HTTP.Requests > s.HTTP.Errors && s.HTTP.TTFBMsP95 > 0 {
				localOK = true
			}
		}
		for _, s := range remote {
			if s.HTTP.Requests > 0 {
				remoteOK = true
			}
		}
		return localOK && remoteOK
	})

	rep, err := m.StopAppSession(ctx, id)
	if err != nil {
		t.Fatalf("StopAppSession: %v", err)
	}
	if rep.Err != "" {
		t.Errorf("session ended with error: %s", rep.Err)
	}
	if rep.DataPort != uint32(port) {
		t.Errorf("DataPort = %d, want the pinned origin port %d", rep.DataPort, port)
	}
	h := rep.LocalHTTP
	if h == nil {
		t.Fatal("no whole-run local HTTP snapshot in the report")
	}
	if h.Requests == 0 || h.Requests <= h.Errors {
		t.Errorf("local requests=%d errors=%d, want successful requests", h.Requests, h.Errors)
	}
	if h.TTFBMsP95 <= 0 || h.ObjectMsP95 <= 0 {
		t.Errorf("local timing percentiles unpopulated: ttfb-p95 %v, object-p95 %v", h.TTFBMsP95, h.ObjectMsP95)
	}
	if h.GoodputMbps <= 0 {
		t.Errorf("local goodput = %v, want > 0", h.GoodputMbps)
	}
	if h.TLSHandshakeMs <= 0 {
		t.Errorf("TLSHandshakeMs = %v, want > 0 (the handshake really crossed the tunnel)", h.TLSHandshakeMs)
	}
	if rep.RemoteHTTP == nil {
		t.Fatal("no remote FINAL HTTP snapshot retained (origin telemetry)")
	}
	if rep.RemoteHTTP.Requests == 0 {
		t.Error("origin served no requests according to its final sample")
	}
	if rep.Local.RxPackets != 0 || rep.Remote != nil {
		t.Error("http report carries voip snapshots")
	}
}

// TestAppSessionVideoEndToEnd plays a short generated HLS ladder end to end:
// the vidstream player rides the netstack/tunnel path against the real loom
// agent's origin (the video far end IS the http app — the skew gate and
// Configure must map it), reaches startup and steady play with no stalls on
// an unthrottled loopback, and the report pairs the player's whole-run QoE
// with the origin's http FINAL sample.
func TestAppSessionVideoEndToEnd(t *testing.T) {
	m, _, _, port := newWebTestRig(t)
	agent := startLoomAgent(t, "v0.11.0-test")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id, err := m.StartAppSession(ctx, webTestSUPI, AppSessionConfig{
		App:        "video",
		PeerAgent:  agent,
		PeerDataIP: webGatewayIP,
		Params: map[string]string{
			"port_min":        fmt.Sprint(port),
			"port_max":        fmt.Sprint(port),
			"ladder":          "240p:400k",
			"seg_duration":    "1s",
			"segments":        "6",
			"start_threshold": "1s",
			"buffer_target":   "2s",
			"rebuffer_target": "1s",
		},
		Duration: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartAppSession: %v", err)
	}
	ch, cancelSub := m.AppSessionEvents(id)
	defer cancelSub()

	// Startup and steady play must be visible in the live interval stream.
	local, _ := collectWebSamples(t, ch, 60*time.Second, func(local, remote []AppSample) bool {
		for _, s := range local {
			if s.Video != nil && s.Video.StartupMs > 0 && s.Video.SegmentsFetched > 0 {
				return true
			}
		}
		return false
	})
	for _, s := range local {
		if s.HTTP != nil {
			t.Error("video session published local HTTP samples")
		}
	}

	// The 6-second stream plays out and the session ends by itself; the
	// stream channel closing is the session's own finalize.
	drainDeadline := time.After(45 * time.Second)
drain:
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				break drain
			}
		case <-drainDeadline:
			t.Fatal("video session did not finish playing out in time")
		}
	}

	rep, err := m.StopAppSession(ctx, id)
	if err != nil {
		t.Fatalf("StopAppSession: %v", err)
	}
	if rep.Err != "" {
		t.Errorf("session ended with error: %s", rep.Err)
	}
	v := rep.LocalVideo
	if v == nil {
		t.Fatal("no whole-run video snapshot in the report")
	}
	if v.StartupMs <= 0 {
		t.Errorf("StartupMs = %v, want > 0 (playback started)", v.StartupMs)
	}
	if v.SegmentsFetched != 6 {
		t.Errorf("SegmentsFetched = %d, want all 6", v.SegmentsFetched)
	}
	if v.Stalls != 0 || len(v.StallEvents) != 0 {
		t.Errorf("unthrottled loopback play stalled: %d stalls %v", v.Stalls, v.StallEvents)
	}
	if v.AvgBitrateKbps < 350 || v.AvgBitrateKbps > 450 {
		t.Errorf("AvgBitrateKbps = %v, want ~400 (the single-rung ladder)", v.AvgBitrateKbps)
	}
	// The far end is the http origin: master manifest + media playlist + 6
	// segments = at least 8 served requests in its FINAL sample.
	if rep.RemoteHTTP == nil {
		t.Fatal("no remote FINAL HTTP snapshot retained (the video far end is the http origin)")
	}
	if rep.RemoteHTTP.Requests < 8 {
		t.Errorf("origin served %d requests, want >= 8 (manifest + playlist + 6 segments)", rep.RemoteHTTP.Requests)
	}
}

// TestAppSessionVideoThrottledStall is the Phase-7 lossy-path variant: the
// gateway's downlink splice is paced below the ladder bitrate (400 kbps rung,
// ~200 kbps link), so the player must stall for real — and the report must
// say so with counted stalls, accumulated stall time, and timestamped stall
// events (the MediaGapSummary-shaped entries the wire carries).
func TestAppSessionVideoThrottledStall(t *testing.T) {
	m, _, gw, port := newWebTestRig(t)
	gw.rateBps.Store(25_000) // ~200 kbps against a 400 kbps rung
	agent := startLoomAgent(t, "v0.11.0-test")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id, err := m.StartAppSession(ctx, webTestSUPI, AppSessionConfig{
		App:        "video",
		PeerAgent:  agent,
		PeerDataIP: webGatewayIP,
		Params: map[string]string{
			"port_min":        fmt.Sprint(port),
			"port_max":        fmt.Sprint(port),
			"ladder":          "240p:400k",
			"seg_duration":    "1s",
			"segments":        "4",
			"start_threshold": "1s",
			"buffer_target":   "2s",
			"rebuffer_target": "1s",
		},
		Duration: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartAppSession: %v", err)
	}
	ch, cancelSub := m.AppSessionEvents(id)
	defer cancelSub()

	// Play out to the natural end (the stream closes when the session
	// finalizes itself), so stall events straddling the run are complete.
	drainDeadline := time.After(75 * time.Second)
drain:
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				break drain
			}
		case <-drainDeadline:
			t.Fatal("throttled video session did not finish in time")
		}
	}

	rep, err := m.StopAppSession(ctx, id)
	if err != nil {
		t.Fatalf("StopAppSession: %v", err)
	}
	if rep.Err != "" {
		t.Errorf("session ended with error: %s", rep.Err)
	}
	v := rep.LocalVideo
	if v == nil {
		t.Fatal("no whole-run video snapshot in the report")
	}
	if v.StartupMs <= 0 {
		t.Error("throttled play never started")
	}
	if v.Stalls == 0 {
		t.Fatalf("no stalls on a %d B/s link under a 400 kbps rung: %+v", gw.rateBps.Load(), v)
	}
	if v.StallTimeMs <= 0 || v.RebufferRatio <= 0 {
		t.Errorf("stall accounting empty: stall_time %v ms, rebuffer ratio %v", v.StallTimeMs, v.RebufferRatio)
	}
	if len(v.StallEvents) == 0 {
		t.Fatal("no stall events in the report (the timestamped entries the wire carries)")
	}
	for _, g := range v.StallEvents {
		if !g.End.After(g.Start) {
			t.Errorf("stall event with non-positive span: %v..%v", g.Start, g.End)
		}
	}
}
