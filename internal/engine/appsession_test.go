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
// reduced to what UDP media flows and an ICMP probe need. It serves any
// number of UEs, each keyed by its UL TEID with its own DL TEID (swappable
// per UE the way a real path switch reprograms one UE's FAR).
type fakeUPF struct {
	t    *testing.T
	conn *net.UDPConn

	mu    sync.Mutex
	gnb   *net.UDPAddr            // source of the last uplink G-PDU (the shared gNB socket)
	ues   map[uint32]*fakeUPFUE   // UL TEID → UE
	flows map[string]*net.UDPConn // UE "ip:port" → N6-facing socket
}

// fakeUPFUE is one UE's tunnel state on the fake UPF.
type fakeUPFUE struct {
	ulTEID uint32
	dlTEID atomic.Uint32 // downlink encapsulation TEID (handover swaps it)
}

func newFakeUPF(t *testing.T) *fakeUPF {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	u := &fakeUPF{t: t, conn: conn, ues: make(map[uint32]*fakeUPFUE), flows: make(map[string]*net.UDPConn)}
	u.addUE(appTestULTEID, appTestDLTEID)
	go u.serve()
	t.Cleanup(u.close)
	return u
}

// addUE provisions one UE's tunnel pair; the returned handle's dlTEID is the
// per-UE FAR the handover tests reprogram.
func (u *fakeUPF) addUE(ulTEID, dlTEID uint32) *fakeUPFUE {
	ue := &fakeUPFUE{ulTEID: ulTEID}
	ue.dlTEID.Store(dlTEID)
	u.mu.Lock()
	u.ues[ulTEID] = ue
	u.mu.Unlock()
	return ue
}

// ue returns the provisioned UE keyed by its UL TEID.
func (u *fakeUPF) ue(ulTEID uint32) *fakeUPFUE {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.ues[ulTEID]
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
	for key, c := range u.flows {
		c.Close()
		delete(u.flows, key)
	}
}

// sendEndMarker emits a GTP-U End Marker on teid toward the gNB socket — what
// the UPF sends on the OLD tunnel after a handover path switch (TS 29.281
// §7.3) so the source path is known drained.
func (u *fakeUPF) sendEndMarker(t *testing.T, teid uint32) {
	t.Helper()
	u.mu.Lock()
	gnb := u.gnb
	u.mu.Unlock()
	if gnb == nil {
		t.Fatal("fake UPF has seen no uplink; no gNB address to end-mark")
	}
	u.sendEndMarkerTo(t, teid, gnb)
}

// sendEndMarkerTo emits an End Marker toward an explicit gNB N3 socket — the
// inter-gNB case, where the OLD tunnel's socket differs from wherever the
// last uplink came from.
func (u *fakeUPF) sendEndMarkerTo(t *testing.T, teid uint32, gnb *net.UDPAddr) {
	t.Helper()
	if _, err := u.conn.WriteToUDP(gtpu.EncodeEndMarker(teid), gnb); err != nil {
		t.Fatal(err)
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
		u.mu.Lock()
		u.gnb = from
		ue := u.ues[g.TEID]
		u.mu.Unlock()
		if ue == nil {
			u.t.Errorf("uplink G-PDU on unknown UL TEID %#x", g.TEID)
			continue
		}
		inner := g.Payload
		if reply, ok := icmpEchoReply(inner); ok {
			// The probe's echo: answer it ourselves (the N6 "internet").
			if _, err := u.conn.WriteToUDP(gtpu.EncodeGPDU(ue.dlTEID.Load(), appTestQFI, reply), from); err != nil {
				return
			}
			continue
		}
		payload, src, ok := datapath.ExtractUDPPayload(inner, 0)
		if !ok {
			continue // neither UDP nor an ICMP echo — nothing to forward
		}
		// src.IP aliases the reused read buffer — copy it, or the relay's UE
		// address would mutate to whichever UE's uplink was decoded last.
		src = &net.UDPAddr{IP: append(net.IP(nil), src.IP.To4()...), Port: src.Port}
		ihl := int(inner[0]&0x0F) * 4
		dstIP := net.IP(append([]byte(nil), inner[16:20]...))
		dstPort := int(uint16(inner[ihl+2])<<8 | uint16(inner[ihl+3]))

		u.mu.Lock()
		fc := u.flows[src.String()]
		if fc == nil {
			var err error
			fc, err = net.DialUDP("udp", nil, &net.UDPAddr{IP: dstIP, Port: dstPort})
			if err != nil {
				u.mu.Unlock()
				u.t.Errorf("fake UPF dial N6 %v:%d: %v", dstIP, dstPort, err)
				continue
			}
			u.flows[src.String()] = fc
			go u.relayDownlink(fc, ue, dstIP, dstPort, src)
		}
		u.mu.Unlock()
		if _, err := fc.Write(payload); err != nil {
			return
		}
	}
}

// relayDownlink pumps one N6 flow's replies back down the tunnel: rebuild the
// inner IPv4+UDP packet (server → UE) and encap it under the UE's CURRENT DL
// TEID (read per packet, so a handover swap takes effect mid-flow).
func (u *fakeUPF) relayDownlink(fc *net.UDPConn, ue *fakeUPFUE, serverIP net.IP, serverPort int, ueAddr *net.UDPAddr) {
	buf := make([]byte, 65536)
	for {
		n, err := fc.Read(buf)
		if err != nil {
			return
		}
		inner, err := datapath.BuildUDPPacket(serverIP, ueAddr.IP, uint16(serverPort), uint16(ueAddr.Port), buf[:n])
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
		if _, err := u.conn.WriteToUDP(gtpu.EncodeGPDU(ue.dlTEID.Load(), appTestQFI, inner), gnb); err != nil {
			return
		}
	}
}

// icmpEchoReply answers an inner IPv4 ICMP echo request: same payload, IPs
// swapped, type 0, checksums fixed. ok is false for anything else.
func icmpEchoReply(req []byte) ([]byte, bool) {
	if len(req) < 20 || req[0]>>4 != 4 || req[9] != 1 {
		return nil, false
	}
	ihl := int(req[0]&0x0F) * 4
	if ihl < 20 || len(req) < ihl+8 || req[ihl] != 8 {
		return nil, false
	}
	rep := append([]byte(nil), req...)
	copy(rep[12:16], req[16:20]) // src ← old dst
	copy(rep[16:20], req[12:16]) // dst ← old src (IP checksum unchanged by the swap)
	rep[ihl] = 0                 // echo reply
	rep[ihl+2], rep[ihl+3] = 0, 0
	cs := ipChecksum(rep[ihl:])
	rep[ihl+2], rep[ihl+3] = byte(cs>>8), byte(cs)
	return rep, true
}

// ipChecksum is the 16-bit one's-complement sum (RFC 1071).
func ipChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xFFFF) + (sum >> 16)
	}
	return ^uint16(sum)
}

// newAppTestManager builds a Manager holding one synthetic SESSION_ACTIVE UE
// whose data path is pre-opened on the Manager's shared per-gNB tunnel pool
// against the fake UPF (ephemeral local bind, so no 2152 port is touched),
// with test-speed app tuning.
func newAppTestManager(t *testing.T, upf *fakeUPF) *Manager {
	t.Helper()
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
	addAppTestUE(t, m, upf, appTestSUPI, appTestUEIP, appTestULTEID, appTestDLTEID)
	return m
}

// addAppTestUE registers one synthetic SESSION_ACTIVE UE on m, pre-opens its
// data path on the shared per-gNB pool ("127.0.0.1:0" — every UE added this
// way shares the ONE ephemeral-port socket, the Phase-5 shape), and returns
// the session. The UE must already be provisioned on the fake UPF via addUE
// (newAppTestManager does the default one).
func addAppTestUE(t *testing.T, m *Manager, upf *fakeUPF, supi, ueIP string, ulTEID, dlTEID uint32) *Session {
	t.Helper()
	sess := &Session{
		SUPI: supi,
		Result: &AttachResult{
			SessionActive: true,
			PDUAddress:    ueIP,
			UPFAddress:    upf.addr(), // host:port — upfN3() respects it
			UPFTEID:       ulTEID,
			DLTEID:        dlTEID,
			QFI:           appTestQFI,
		},
		conn:  stubTransport{},
		gnbN3: "127.0.0.1",
	}
	sess.n3 = m.n3
	sess.n3Port = "0" // ephemeral bind (the pool key stays "127.0.0.1:0")
	if _, _, err := sess.dataplane(); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.sessions[supi] = sess
	m.mu.Unlock()
	t.Cleanup(sess.closeDataPath)
	return sess
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

// TestAppSessionHandoverMidCall drives the flagship scenario on the e2e rig:
// a live call, then the exact user-plane cutover an INTER-gNB Xn handover
// performs (hub phase events + gNB N3 move + DL TEID swap + data-path
// rebind) — the shape real Xn handovers take: the session's socket changes,
// and the live lane (loom's dgram receive loop, the SubscribeUDPAll wildcard
// ring, the End-Marker callback) is Detach/Attach-carried onto the target
// gNB's shared tunnel. Downlink media must RESUME after the switch, the
// UPF's End Marker on the OLD socket must reach the carried lane through the
// vacated TEID's tombstone (the grace-held source socket), and the session
// must end clean with the handover joined in the correlator's annotations.
// Before the rebind fix this scenario blackholed media permanently in both
// directions. (The intra-gNB pure TEID-swap variant is covered by
// TestAppSessionTwoUEsConcurrentMedia.)
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
	preRx := waitLocalRx(t, ch, 10, "pre-handover downlink media")

	// The Xn user-plane cutover, exactly as runXnHandover performs it after
	// PathSwitchRequestAcknowledge: phase events on the hub, the core's
	// downlink switch (fake UPF encapsulates under the new TEID), then the
	// session's move to the TARGET gNB N3 address — a different socket —
	// with the new DL TEID (retargetDataPath, the production entry point).
	const newTEID = 0x0303
	m.publishMobility(StateEvent{SUPI: appTestSUPI, State: StateHandoverStarted,
		Detail: "Xn handover gnb1 → gnb2"})
	m.mu.Lock()
	sess := m.sessions[appTestSUPI]
	m.mu.Unlock()
	sess.dpMu.Lock()
	srcAddr := sess.n3ref.st.LocalAddr().(*net.UDPAddr) // the source gNB socket
	sess.dpMu.Unlock()
	upf.ue(appTestULTEID).dlTEID.Store(newTEID) // core reprogrammed the FAR at path switch
	if err := sess.retargetDataPath(dataPathMove{gnbN3: "127.0.0.2", dlTEID: newTEID}); err != nil {
		t.Fatalf("retargetDataPath: %v", err)
	}
	// The UPF drains the old path: End Marker on the vacated TEID at the OLD
	// gNB socket (grace-held open although the mover was its last UE).
	upf.sendEndMarkerTo(t, appTestDLTEID, srcAddr)
	m.publishMobility(StateEvent{SUPI: appTestSUPI, State: StatePathSwitchComplete,
		Detail: "PathSwitchRequestAcknowledge; downlink → target"})
	m.publishMobility(StateEvent{SUPI: appTestSUPI, State: StateHandoverComplete,
		Detail: "UE on gNB gnb2 via Xn"})

	// Downlink media must resume on the new path: the rebind carried the
	// live lane over, and the first post-switch uplink teaches the fake UPF
	// the new gNB socket (as real path switch reprograms the tunnel).
	waitLocalRx(t, ch, preRx+20, "post-handover downlink media to resume")

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
	// The End Marker crossed the socket move via the tombstoned TEID on the
	// grace-held source socket and reached this session's report.
	var marker *AppSample
	for i, ev := range rep.Events {
		if ev.Event == AppEventEndMarker {
			marker = &rep.Events[i]
		}
	}
	if marker == nil {
		t.Fatalf("no %s event in the report: %v", AppEventEndMarker, rep.Events)
	}
	if !strings.Contains(marker.Detail, "vacated pre-handover TEID") {
		t.Errorf("End Marker detail %q not marked as the vacated-TEID drain", marker.Detail)
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

// waitLocalRx consumes ch until a local (UE-end) interval sample reports more
// than min received packets, returning the observed count.
func waitLocalRx(t *testing.T, ch <-chan AppSample, min uint64, what string) uint64 {
	t.Helper()
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

// TestAppSessionTwoUEsConcurrentMedia is the Phase-5 demo in test form
// (design §12): TWO UEs on ONE gNB N3 address hold concurrent VoIP calls over
// the single shared socket — no port collision — plus an ICMP latency probe
// on the same demux; an Xn handover during UE-A's call (TEID rebind on the
// shared socket, UPF End Marker on the vacated TEID) leaves UE-B's call
// untouched (loss/discard ~0) while A's media resumes; both calls score a
// sane MOS and A's report carries the End Marker + handover annotations.
func TestAppSessionTwoUEsConcurrentMedia(t *testing.T) {
	const (
		supiB   = "001010000000043"
		ueIPB   = "192.168.100.8"
		ulTEIDB = 0x0111
		dlTEIDB = 0x0212
	)
	upf := newFakeUPF(t)
	m := newAppTestManager(t, upf)
	upf.addUE(ulTEIDB, dlTEIDB)
	addAppTestUE(t, m, upf, supiB, ueIPB, ulTEIDB, dlTEIDB)
	agent := startLoomAgent(t, "v0.10.0-test")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The EADDRINUSE fix, asserted: both UEs' data paths ride ONE socket.
	if m.n3.size() != 1 {
		t.Fatalf("pool size = %d, want 1 (one N3 socket per gNB)", m.n3.size())
	}

	start := func(supi string) string {
		id, err := m.StartAppSession(ctx, supi, AppSessionConfig{
			App:        "voip",
			PeerAgent:  agent,
			PeerDataIP: "127.0.0.1",
			Params:     map[string]string{"codec": "pcmu"},
			Duration:   30 * time.Second,
		})
		if err != nil {
			t.Fatalf("StartAppSession(%s): %v", supi, err)
		}
		return id
	}
	idA, idB := start(appTestSUPI), start(supiB)
	chA, cancelA := m.AppSessionEvents(idA)
	defer cancelA()
	chB, cancelB := m.AppSessionEvents(idB)
	defer cancelB()

	preA := waitLocalRx(t, chA, 10, "UE A downlink media")
	preB := waitLocalRx(t, chB, 10, "UE B downlink media")

	// The ICMP latency probe shares UE B's demux lane with B's live call.
	probe, err := m.Latency(ctx, supiB, "8.8.8.8", 10, 20*time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("latency probe alongside two live calls: %v", err)
	}
	if probe.Received < 8 {
		t.Errorf("probe received %d/%d echoes alongside media", probe.Received, probe.Sent)
	}

	// Xn user-plane cutover on UE A ONLY (as runXnHandover performs it):
	// phase events, the core's FAR reprogram, DL TEID rebind on the shared
	// socket, and the UPF's End Marker draining the vacated tunnel.
	const newTEID = 0x0303
	m.publishMobility(StateEvent{SUPI: appTestSUPI, State: StateHandoverStarted,
		Detail: "Xn handover gnb1 → gnb2"})
	m.mu.Lock()
	sessA := m.sessions[appTestSUPI]
	m.mu.Unlock()
	upf.ue(appTestULTEID).dlTEID.Store(newTEID)
	// Same gNB N3 address → the intra-gNB pure TEID-swap fast path.
	if err := sessA.retargetDataPath(dataPathMove{dlTEID: newTEID}); err != nil {
		t.Fatalf("retargetDataPath: %v", err)
	}
	upf.sendEndMarker(t, appTestDLTEID) // the old path is drained
	m.publishMobility(StateEvent{SUPI: appTestSUPI, State: StatePathSwitchComplete,
		Detail: "PathSwitchRequestAcknowledge; downlink → target"})
	m.publishMobility(StateEvent{SUPI: appTestSUPI, State: StateHandoverComplete,
		Detail: "UE on gNB gnb2 via Xn"})

	// A's media resumes on the rebound TEID; B never stops flowing.
	waitLocalRx(t, chA, preA+20, "UE A post-handover media")
	waitLocalRx(t, chB, preB+20, "UE B media across A's handover")
	if m.n3.size() != 1 {
		t.Errorf("pool size after intra-gNB rebind = %d, want 1", m.n3.size())
	}

	repA, err := m.StopAppSession(ctx, idA)
	if err != nil {
		t.Fatalf("StopAppSession(A): %v", err)
	}
	repB, err := m.StopAppSession(ctx, idB)
	if err != nil {
		t.Fatalf("StopAppSession(B): %v", err)
	}
	for name, rep := range map[string]*AppSessionReport{"A": &repA, "B": &repB} {
		if rep.Err != "" {
			t.Errorf("UE %s session ended with error: %s", name, rep.Err)
		}
		if mos := rep.Local.MOSCQ; mos < 3.0 || mos > 5.0 {
			t.Errorf("UE %s whole-call MOS-CQ = %v, want a sane loopback score in [3, 5]", name, mos)
		}
	}

	// The handover left B's media untouched: loss/discard ~0 for the whole
	// call that spanned it.
	if repB.Local.LossPct > 0.5 || repB.Local.DiscardPct > 2.0 {
		t.Errorf("UE B call degraded across A's handover: loss %.2f%%, discard %.2f%%",
			repB.Local.LossPct, repB.Local.DiscardPct)
	}
	// And B's timeline knows nothing of A's handover.
	if len(repB.Annotations) != 0 {
		t.Errorf("UE B correlated a handover it never had: %q", repB.Annotations)
	}

	// A's End Marker arrived via the vacated-TEID tombstone and was joined
	// into the annotation timeline with the handover.
	var marker *AppSample
	for i, ev := range repA.Events {
		if ev.Event == AppEventEndMarker {
			marker = &repA.Events[i]
		}
	}
	if marker == nil {
		t.Fatalf("no %s event in UE A's report: %v", AppEventEndMarker, repA.Events)
	}
	for _, want := range []string{"0x202", "vacated pre-handover TEID"} {
		if !strings.Contains(marker.Detail, want) {
			t.Errorf("End Marker detail %q does not name %q", marker.Detail, want)
		}
	}
	joined := strings.Join(repA.Annotations, "\n")
	if !strings.Contains(joined, "XnHandover @") || !strings.Contains(joined, "End Marker @") {
		t.Errorf("UE A annotations missing the handover/End-Marker join: %q", repA.Annotations)
	}
	if strings.Contains(joined, "media silent since handover") {
		t.Errorf("recovered call annotated as a blackout: %q", repA.Annotations)
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
