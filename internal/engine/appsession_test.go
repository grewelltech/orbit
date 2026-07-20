package engine

import (
	"context"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgrewell/loom/control"
	loomapp "github.com/bgrewell/loom/core/app"
	"github.com/bgrewell/loom/core/components"
	"github.com/bgrewell/loom/core/registry"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gtpu"
)

// The in-process e2e rig (design §12 Phase 4 test shape): a REAL loom control
// server/agent on loopback plays the N6 loomd, and a fake-UPF loopback
// gateway carries the data path — uplink G-PDUs are decapped and their inner
// UDP datagrams forwarded to the real N6 socket, replies re-encapped downlink
// — so a genuine bidirectional VoIP call runs through the whole stack:
// voip client → dgram → GTP-U → fake UPF → host UDP → loomd voip server.

const (
	appTestULTEID = 0x0101
	appTestDLTEID = 0x0202
	appTestQFI    = uint8(9)
	appTestSUPI   = "001010000000042"
	appTestUEIP   = "192.168.100.7"
)

// startLoomAgent runs a real loom control agent in-process on loopback (the
// same shape as loom's own control tests) and returns its address.
func startLoomAgent(t *testing.T, version string, opts ...control.Option) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := control.NewServer(version, opts...)
	gs := control.NewGRPCServer(srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// fakeUPF is the loopback N6 gateway: it decaps uplink G-PDUs, forwards each
// inner UDP datagram to its real destination socket on the host, and relays
// replies back down the tunnel with headers rebuilt — the UPF's N3⇄N6 job,
// reduced to what a UDP media flow needs.
type fakeUPF struct {
	t    *testing.T
	conn *net.UDPConn

	// dlTEID is the TEID the UPF encapsulates downlink under — swapped by
	// the handover test the way a real path switch reprograms the FAR.
	dlTEID atomic.Uint32

	mu    sync.Mutex
	gnb   *net.UDPAddr            // source of the last uplink G-PDU
	flows map[uint16]*net.UDPConn // UE source port → N6-facing socket
}

func newFakeUPF(t *testing.T) *fakeUPF {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	u := &fakeUPF{t: t, conn: conn, flows: make(map[uint16]*net.UDPConn)}
	u.dlTEID.Store(appTestDLTEID)
	go u.serve()
	t.Cleanup(u.close)
	return u
}

func (u *fakeUPF) addr() string { return u.conn.LocalAddr().String() }

func (u *fakeUPF) close() {
	u.conn.Close()
	u.closeFlows()
}

// closeFlows tears down the per-flow N6 sockets (and their relay goroutines).
func (u *fakeUPF) closeFlows() {
	u.mu.Lock()
	defer u.mu.Unlock()
	for port, c := range u.flows {
		c.Close()
		delete(u.flows, port)
	}
}

func (u *fakeUPF) serve() {
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
		if g.TEID != appTestULTEID {
			u.t.Errorf("uplink TEID %#x, want %#x", g.TEID, appTestULTEID)
			continue
		}
		inner := g.Payload
		payload, src, ok := datapath.ExtractUDPPayload(inner, 0)
		if !ok {
			continue // not UDP (e.g. a probe) — nothing to forward
		}
		ihl := int(inner[0]&0x0F) * 4
		dstIP := net.IP(append([]byte(nil), inner[16:20]...))
		dstPort := int(uint16(inner[ihl+2])<<8 | uint16(inner[ihl+3]))

		u.mu.Lock()
		u.gnb = from
		fc := u.flows[uint16(src.Port)]
		if fc == nil {
			var err error
			fc, err = net.DialUDP("udp", nil, &net.UDPAddr{IP: dstIP, Port: dstPort})
			if err != nil {
				u.mu.Unlock()
				u.t.Errorf("fake UPF dial N6 %v:%d: %v", dstIP, dstPort, err)
				continue
			}
			u.flows[uint16(src.Port)] = fc
			go u.relayDownlink(fc, dstIP, dstPort, src)
		}
		u.mu.Unlock()
		if _, err := fc.Write(payload); err != nil {
			return
		}
	}
}

// relayDownlink pumps one N6 flow's replies back down the tunnel: rebuild the
// inner IPv4+UDP packet (server → UE) and encap it under the DL TEID.
func (u *fakeUPF) relayDownlink(fc *net.UDPConn, serverIP net.IP, serverPort int, ue *net.UDPAddr) {
	buf := make([]byte, 65536)
	for {
		n, err := fc.Read(buf)
		if err != nil {
			return
		}
		inner, err := datapath.BuildUDPPacket(serverIP, ue.IP, uint16(serverPort), uint16(ue.Port), buf[:n])
		if err != nil {
			u.t.Errorf("fake UPF rebuild downlink: %v", err)
			return
		}
		u.mu.Lock()
		gnb := u.gnb
		u.mu.Unlock()
		if gnb == nil {
			continue
		}
		if _, err := u.conn.WriteToUDP(gtpu.EncodeGPDU(u.dlTEID.Load(), appTestQFI, inner), gnb); err != nil {
			return
		}
	}
}

// newAppTestManager builds a Manager holding one synthetic SESSION_ACTIVE UE
// whose data path is pre-opened against the fake UPF (ephemeral local bind,
// so no 2152 port is touched), with test-speed app tuning.
func newAppTestManager(t *testing.T, upf *fakeUPF) *Manager {
	t.Helper()
	tun, err := datapath.NewTunnel(datapath.Config{
		LocalN3: "127.0.0.1:0",
		UPFN3:   upf.addr(),
		ULTEID:  appTestULTEID,
		DLTEID:  appTestDLTEID,
		QFI:     appTestQFI,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.apps.tuning = appTuning{
		syncInterval:   100 * time.Millisecond,
		syncBurst:      3,
		sampleInterval: 100 * time.Millisecond,
		trackerWindow:  200 * time.Millisecond,
		trackerN:       4,
		rpcTimeout:     5 * time.Second,
		stopWait:       5 * time.Second,
	}
	sess := &Session{
		SUPI: appTestSUPI,
		Result: &AttachResult{
			SessionActive: true,
			PDUAddress:    appTestUEIP,
			UPFAddress:    upf.addr(), // host:port — openTunnelLocked respects it
			UPFTEID:       appTestULTEID,
			DLTEID:        appTestDLTEID,
			QFI:           appTestQFI,
		},
		conn:  stubTransport{},
		gnbN3: "127.0.0.1",
	}
	sess.n3Port = "0" // rebinds bind ephemeral, like the pre-opened tunnel
	sess.dataPath = tun
	sess.demux = tun.Demux()
	sess.rx = sess.demux.Register(appTestDLTEID)
	sess.rxTEID = appTestDLTEID
	m.sessions[appTestSUPI] = sess
	t.Cleanup(sess.closeDataPath)
	return m
}

// stubTransport satisfies gnb.Transport for synthetic sessions so Deregister
// can run its teardown path.
type stubTransport struct{}

func (stubTransport) WriteNGAP(uint16, []byte) error                 { return nil }
func (stubTransport) ReadMsg([]byte) ([]byte, uint16, uint32, error) { return nil, 0, 0, io.EOF }
func (stubTransport) Close() error                                   { return nil }

// TestAppSessionVoIPEndToEnd runs a short bidirectional G.711 call through
// the whole in-process stack and checks: both ends score a sane MOS, the
// remote agent's telemetry series is received and re-stamped, and teardown
// releases every goroutine the session spawned.
func TestAppSessionVoIPEndToEnd(t *testing.T) {
	upf := newFakeUPF(t)
	m := newAppTestManager(t, upf)
	agent := startLoomAgent(t, "v0.10.0-test")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	baseline := runtime.NumGoroutine()

	id, err := m.StartAppSession(ctx, appTestSUPI, AppSessionConfig{
		App:        "voip",
		PeerAgent:  agent,
		PeerDataIP: "127.0.0.1",
		Params:     map[string]string{"codec": "pcmu"},
		Duration:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartAppSession: %v", err)
	}

	// Phase 4 is single-call per UE (the wildcard UDP lane is single-slot);
	// a second concurrent call must be refused, not silently break the first.
	if _, err := m.StartAppSession(ctx, appTestSUPI, AppSessionConfig{
		App: "voip", PeerAgent: agent, PeerDataIP: "127.0.0.1", Duration: 30 * time.Second,
	}); err == nil || !strings.Contains(err.Error(), "already has a running app session") {
		t.Errorf("second concurrent call: %v", err)
	}

	ch, cancelSub := m.AppSessionEvents(id)
	defer cancelSub()
	var local, remote *AppSample
	deadline := time.After(20 * time.Second)
wait:
	for local == nil || remote == nil {
		select {
		case s, ok := <-ch:
			if !ok {
				break wait
			}
			if s.VoIP == nil {
				continue
			}
			// Require a few intervals' worth of media (≥10 packets is
			// ≥200ms at 20ms ptime) so the call spans several boundaries.
			switch {
			case s.End == AppEndUE && s.VoIP.RxPackets >= 10 && s.VoIP.MOSCQ > 0:
				sc := s
				local = &sc
			case s.End == AppEndN6 && s.VoIP.RxPackets >= 10 && s.VoIP.MOSCQ > 0:
				sc := s
				remote = &sc
			}
		case <-deadline:
			t.Fatalf("no scored both-end samples in time (local %v, remote %v)", local != nil, remote != nil)
		}
	}
	if local == nil || remote == nil {
		t.Fatal("event stream closed before both ends scored")
	}
	if local.TimeSource != "local" {
		t.Errorf("local sample TimeSource = %q, want local", local.TimeSource)
	}
	// The remote sample is re-stamped onto orbit's clock once the tracker has
	// an offset; either way the provenance label must be honest.
	if remote.TimeSource != "timesync" && remote.TimeSource != "remote-clock" {
		t.Errorf("remote sample TimeSource = %q", remote.TimeSource)
	}

	rep, err := m.StopAppSession(ctx, id)
	if err != nil {
		t.Fatalf("StopAppSession: %v", err)
	}
	if rep.Err != "" {
		t.Errorf("session ended with error: %s", rep.Err)
	}
	if mos := rep.Local.MOSCQ; mos < 3.0 || mos > 5.0 {
		t.Errorf("local whole-call MOS-CQ = %v, want a sane loopback score in [3, 5]", mos)
	}
	if rep.Local.RxPackets == 0 {
		t.Error("local end received no media")
	}
	if rep.Remote == nil {
		t.Fatal("no remote FINAL telemetry sample retained")
	}
	if mos := rep.Remote.MOSCQ; mos < 3.0 || mos > 5.0 {
		t.Errorf("remote whole-call MOS-CQ = %v, want a sane loopback score in [3, 5]", mos)
	}
	if rep.Remote.RxPackets == 0 {
		t.Error("remote end received no media (uplink never arrived)")
	}
	if len(rep.LocalSeries) == 0 || len(rep.RemoteSeries) == 0 {
		t.Errorf("interval series missing: local %d, remote %d", len(rep.LocalSeries), len(rep.RemoteSeries))
	}
	if rep.DataPort == 0 || rep.Ended.IsZero() {
		t.Errorf("report incomplete: data port %d, ended %v", rep.DataPort, rep.Ended)
	}

	// The id is forgotten once reaped.
	if _, err := m.StopAppSession(ctx, id); err == nil {
		t.Error("second StopAppSession should fail for a reaped id")
	}

	// Teardown must release the session's goroutines (client engine, loops,
	// grpc client). The fake UPF's per-flow relays are harness-owned — close
	// them before measuring. Slack absorbs runtime/grpc background threads.
	upf.closeFlows()
	waitGoroutines(t, baseline+5, 8*time.Second)
}

// TestAppSessionHandoverMidCall drives the Phase-4 flagship scenario on the
// e2e rig: a live call, then the exact user-plane cutover a handover performs
// (hub phase events + DL TEID swap + data-path rebind onto a new N3 socket).
// Downlink media must RESUME after the switch — the media lanes, End-Marker
// callback, and uplink must survive the rebind — and the session must end
// clean with the handover joined in the correlator's annotations. Before the
// rebind fix this scenario blackholed media permanently in both directions.
func TestAppSessionHandoverMidCall(t *testing.T) {
	upf := newFakeUPF(t)
	m := newAppTestManager(t, upf)
	agent := startLoomAgent(t, "v0.10.0-test")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	id, err := m.StartAppSession(ctx, appTestSUPI, AppSessionConfig{
		App:        "voip",
		PeerAgent:  agent,
		PeerDataIP: "127.0.0.1",
		Params:     map[string]string{"codec": "pcmu"},
		Duration:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartAppSession: %v", err)
	}

	ch, cancelSub := m.AppSessionEvents(id)
	defer cancelSub()

	// waitLocalRx waits for a local interval sample whose RxPackets exceeds
	// min, returning the observed count.
	waitLocalRx := func(min uint64, what string) uint64 {
		deadline := time.After(20 * time.Second)
		for {
			select {
			case s, ok := <-ch:
				if !ok {
					t.Fatalf("event stream closed while waiting for %s", what)
				}
				if s.VoIP != nil && s.End == AppEndUE && s.VoIP.RxPackets > min {
					return s.VoIP.RxPackets
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	preRx := waitLocalRx(10, "pre-handover downlink media")

	// The Xn user-plane cutover, exactly as runXnHandover performs it after
	// PathSwitchRequestAcknowledge: phase events on the hub, the core's
	// downlink switch (fake UPF encapsulates under the new TEID), then the
	// session's DL TEID swap + data-path rebind onto the target's socket.
	const newTEID = 0x0303
	m.publishMobility(StateEvent{SUPI: appTestSUPI, State: StateHandoverStarted,
		Detail: "Xn handover gnb1 → gnb2"})
	m.mu.Lock()
	sess := m.sessions[appTestSUPI]
	m.mu.Unlock()
	upf.dlTEID.Store(newTEID) // core reprogrammed the FAR at path switch
	m.mu.Lock()
	sess.Result.DLTEID = newTEID
	rerr := sess.rebindDataPath()
	m.mu.Unlock()
	if rerr != nil {
		t.Fatalf("rebindDataPath: %v", rerr)
	}
	m.publishMobility(StateEvent{SUPI: appTestSUPI, State: StatePathSwitchComplete,
		Detail: "PathSwitchRequestAcknowledge; downlink → target"})
	m.publishMobility(StateEvent{SUPI: appTestSUPI, State: StateHandoverComplete,
		Detail: "UE on gNB gnb2 via Xn"})

	// Downlink media must resume on the new path: the rebind carried the
	// live lane over, and the first post-switch uplink teaches the fake UPF
	// the new gNB socket (as real path switch reprograms the tunnel).
	waitLocalRx(preRx+20, "post-handover downlink media to resume")

	rep, err := m.StopAppSession(ctx, id)
	if err != nil {
		t.Fatalf("StopAppSession: %v", err)
	}
	if rep.Err != "" {
		t.Errorf("session ended with error after handover: %s", rep.Err)
	}
	if rep.Local.RxPackets <= preRx {
		t.Errorf("downlink media did not grow after the handover: %d <= %d",
			rep.Local.RxPackets, preRx)
	}
	// The handover is joined in the annotations, and total-blackout wording
	// must NOT appear — media recovered.
	joined := strings.Join(rep.Annotations, "\n")
	if !strings.Contains(joined, "XnHandover @") {
		t.Errorf("no handover annotation in %q", rep.Annotations)
	}
	if strings.Contains(joined, "media silent since handover") {
		t.Errorf("recovered call annotated as a blackout: %q", rep.Annotations)
	}
}

// TestAppSessionSkewGate points StartAppSession at a live agent whose build
// carries no app engines: the capabilities gate must refuse with the
// design's actionable wording before any flow is configured.
func TestAppSessionSkewGate(t *testing.T) {
	upf := newFakeUPF(t)
	m := newAppTestManager(t, upf)

	comps := components.Default()
	comps.AppClients = registry.New[loomapp.Client, loomapp.Options]()
	comps.AppServers = registry.New[loomapp.Server, loomapp.Options]()
	agent := startLoomAgent(t, "v0.9.1", control.WithComponents(comps))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := m.StartAppSession(ctx, appTestSUPI, AppSessionConfig{
		App: "voip", PeerAgent: agent, Duration: 10 * time.Second,
	})
	if err == nil {
		t.Fatal("expected the version-skew gate to refuse")
	}
	for _, want := range []string{agent, "(v0.9.1)", `lacks app "voip"`, "run loom >= v0.10"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("gate error %q does not contain %q", err, want)
		}
	}
}

// TestAppSessionUEReleaseMidCall deregisters the UE while the call runs: the
// app session must wind down cleanly (before the data path is torn away),
// stamp the release as a correlation event, and stay reapable for its report.
func TestAppSessionUEReleaseMidCall(t *testing.T) {
	upf := newFakeUPF(t)
	m := newAppTestManager(t, upf)
	agent := startLoomAgent(t, "v0.10.0-test")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id, err := m.StartAppSession(ctx, appTestSUPI, AppSessionConfig{
		App:        "voip",
		PeerAgent:  agent,
		PeerDataIP: "127.0.0.1",
		Params:     map[string]string{"codec": "pcmu"},
		Duration:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartAppSession: %v", err)
	}

	// Let media flow before pulling the UE.
	ch, cancelSub := m.AppSessionEvents(id)
	deadline := time.After(15 * time.Second)
	for scored := false; !scored; {
		select {
		case s, ok := <-ch:
			if !ok {
				t.Fatal("event stream closed before media flowed")
			}
			scored = s.VoIP != nil && s.End == AppEndUE && s.VoIP.RxPackets > 0
		case <-deadline:
			t.Fatal("no media before the release")
		}
	}
	cancelSub()

	// Synthetic sessions have no NAS security context, so the deregistration
	// signalling itself fails — but the app-session release and data-path
	// teardown have run by then, which is what this test is about.
	if err := m.Deregister(ctx, appTestSUPI); err == nil {
		t.Log("Deregister unexpectedly succeeded (fine for this test)")
	}

	rep, err := m.StopAppSession(ctx, id)
	if err != nil {
		t.Fatalf("StopAppSession after release: %v", err)
	}
	if rep.Ended.IsZero() {
		t.Error("session not finalized after UE release")
	}
	released := false
	for _, ev := range rep.Events {
		if ev.Event == AppEventUEReleased {
			released = true
		}
	}
	if !released {
		t.Errorf("no %s correlation event in %v", AppEventUEReleased, rep.Events)
	}
	if rep.Local.RxPackets == 0 {
		t.Error("report lost the pre-release media stats")
	}
}

// TestAppSessionValidation pins the fail-fast paths: unsupported app, missing
// peer, missing duration, unknown UE, and the unknown-id event stream.
func TestAppSessionValidation(t *testing.T) {
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	if _, err := m.StartAppSession(ctx, appTestSUPI, AppSessionConfig{App: "http", PeerAgent: "n6:9551", Duration: time.Second}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("unsupported app: %v", err)
	}
	if _, err := m.StartAppSession(ctx, appTestSUPI, AppSessionConfig{App: "voip", Duration: time.Second}); err == nil || !strings.Contains(err.Error(), "peer loomd") {
		t.Errorf("missing peer: %v", err)
	}
	if _, err := m.StartAppSession(ctx, appTestSUPI, AppSessionConfig{App: "voip", PeerAgent: "n6:9551"}); err == nil || !strings.Contains(err.Error(), "positive duration") {
		t.Errorf("missing duration: %v", err)
	}
	if _, err := m.StartAppSession(ctx, appTestSUPI, AppSessionConfig{App: "voip", PeerAgent: "n6:9551", Duration: time.Second}); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("unknown UE: %v", err)
	}
	if _, err := m.StopAppSession(ctx, "app-404"); err == nil {
		t.Error("StopAppSession on an unknown id should fail")
	}
	ch, cancel := m.AppSessionEvents("app-404")
	defer cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("unknown-id event stream delivered a sample")
		}
	case <-time.After(time.Second):
		t.Error("unknown-id event stream is not closed")
	}
}

// waitGoroutines polls until the goroutine count drops to max or the deadline
// passes (then fails with the count, mockamf's NumGoroutine spirit).
func waitGoroutines(t *testing.T, max int, wait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		n := runtime.NumGoroutine()
		if n <= max {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Errorf("goroutines did not settle: %d > %d\n%s", n, max, buf[:runtime.Stack(buf, true)])
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
