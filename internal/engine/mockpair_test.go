package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// mockPair is the mock AMF fronted by a real loopback SCTP listener plus the
// stub UPF it drives, with the three N3 addresses a session and an Xn path
// switch need. Each test gets its own loopback /24 so a source N3 socket held
// through the End-Marker grace window never collides with the next test's.
type mockPair struct {
	amf     *mockamf.AMF
	upf     *mockupf.UPF
	amfAddr string
	srcN3   string // source gNB N3
	tgtN3   string // target gNB N3
	ki, opc []byte
	cfg     gnb.Config // source gNB; the target is ID 2
}

func newMockPair(t *testing.T, net24 int, opts mockupf.Options, reallocUL bool) *mockPair {
	t.Helper()
	base := testnet.Base(net24)
	upf, err := mockupf.Listen(base+"10:2152", opts)
	testnet.RequireBind(t, "the stub UPF on "+base+"10:2152", err)
	t.Cleanup(func() { _ = upf.Close() })
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{
		Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
		UPF: upf, UPFAddress: base + "10", ReallocateULOnPathSwitch: reallocUL,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every PDU the mock answered with must have encoded: a dropped one
	// would turn a mock defect into a silent engine timeout.
	t.Cleanup(func() {
		if n := amf.EncodeErrors(); n != 0 {
			t.Errorf("mock AMF dropped %d PDUs that failed to encode", n)
		}
	})
	return &mockPair{
		amf: amf, upf: upf, amfAddr: sctpFront(t, amf),
		srcN3: base + "11", tgtN3: base + "12", ki: ki, opc: opc,
		cfg: gnb.Config{ID: 1, Name: "orbit-gnb-1", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}},
	}
}

// register attaches one UE with a PDU session through the source gNB and
// returns its session with the data path open.
func (p *mockPair) register(t testing.TB, m *Manager, supi string) *Session {
	t.Helper()
	id, err := ue.ParseIdentity(supi, "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := m.Register(ctx, GNBSpec{Config: p.cfg, AMFAddr: p.amfAddr}, UEConfig{
		Identity:   id,
		Sub:        auth.Subscription{SUPI: supi, Ki: p.ki, OPc: p.opc},
		PDUSession: &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"},
		GNBN3Addr:  p.srcN3,
	})
	if err != nil {
		t.Fatalf("Register with a PDU session: %v", err)
	}
	if !res.SessionActive {
		t.Fatalf("no PDU session: %+v", res)
	}
	sess, err := m.Status(supi)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sess.dataplane(); err != nil {
		t.Fatalf("open data path: %v", err)
	}
	// Cleanups run last-in first-out: close the data path, then wait until
	// every N3 socket (including a source held through the End-Marker grace
	// window) is released, so a repeated run can bind the same addresses. The
	// wait outlasts the pool's grace window; a socket still held after it is
	// a leak, and fails the test rather than the next run's bind.
	t.Cleanup(func() {
		if err := n3Released(m, m.n3.grace+2*time.Second); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(sess.closeDataPath)
	if !p.upf.WaitFor(5*time.Second, func(u *mockupf.UPF) bool { _, ok := u.Session(supi, 1); return ok }) {
		t.Fatal("the UPF never heard about the session")
	}
	return sess
}

// n3Released waits up to within for m's N3 pool to empty, and says how many
// sockets are still held if it does not.
func n3Released(m *Manager, within time.Duration) error {
	deadline := time.Now().Add(within)
	for m.n3.size() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := m.n3.size(); n != 0 {
		return fmt.Errorf("%d N3 socket(s) still held %s after the test (grace window %s)", n, within, m.n3.grace)
	}
	return nil
}

// A socket still held after the wait is reported; a released pool is not.
func TestN3ReleasedReportsAHeldSocket(t *testing.T) {
	upf := newFakeUPFSocket(t)
	m := NewManager(testLogger())
	ref, err := m.n3.acquire("127.0.0.1:0", upf.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := n3Released(m, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "1 N3 socket(s) still held") {
		t.Errorf("n3Released with a socket held = %v, want it reported", err)
	}
	m.n3.release(ref)
	if err := n3Released(m, time.Second); err != nil {
		t.Errorf("n3Released after the release = %v, want nil", err)
	}
}

// cleanupRecorder is a testing.TB that keeps the cleanups registered on it,
// and the errors they report, for a test to run and read.
type cleanupRecorder struct {
	testing.TB
	cleanups []func()
	errs     []string
}

func (r *cleanupRecorder) Cleanup(f func())  { r.cleanups = append(r.cleanups, f) }
func (r *cleanupRecorder) Error(args ...any) { r.errs = append(r.errs, fmt.Sprint(args...)) }
func (r *cleanupRecorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// run runs the kept cleanups last-in first-out, as testing does.
func (r *cleanupRecorder) run() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}

// register's cleanup fails the test when an N3 socket outlives the wait,
// rather than leaving it for the next run's bind to collide with.
func TestRegisterCleanupReportsAHeldN3Socket(t *testing.T) {
	p := newMockPair(t, testnet.EngineRegisterCleanup, mockupf.Options{}, false)
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.n3.grace = 0
	rec := &cleanupRecorder{TB: t}
	p.register(rec, m, "208930100007800")
	// A socket the session's teardown does not release: the leak.
	held, err := m.n3.acquire(p.tgtN3+":2152", p.amfAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer m.n3.release(held)
	rec.run()
	if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "1 N3 socket(s) still held") {
		t.Errorf("register's cleanup reported %q, want the held socket", rec.errs)
	}
}

// xn switches the UE to gNB 2.
func (p *mockPair) xn(t *testing.T, m *Manager, supi string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tgt := p.cfg
	tgt.ID, tgt.Name = 2, "orbit-gnb-2"
	if _, err := m.XnHandover(ctx, supi, GNBEndpoint{Config: tgt, AMFAddr: p.amfAddr, N3Addr: p.tgtN3}); err != nil {
		t.Fatalf("Xn handover: %v", err)
	}
}

// endMarkers collects the End Markers a lane's callback sees.
type endMarkers struct {
	mu   sync.Mutex
	seen []datapath.EndMarker
}

func (e *endMarkers) add(em datapath.EndMarker) {
	e.mu.Lock()
	e.seen = append(e.seen, em)
	e.mu.Unlock()
}

func (e *endMarkers) list() []datapath.EndMarker {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]datapath.EndMarker(nil), e.seen...)
}

// A UE registers with a PDU session against the mock pair: the session
// carries the stub UPF's uplink tunnel and a UE address from the mock, the
// UPF learns the gNB's downlink tunnel, and both directions of user plane
// flow over real N3 sockets.
func TestMockPairPDUSessionCarriesUserPlane(t *testing.T) {
	p := newMockPair(t, testnet.EngineUserPlane, mockupf.Options{}, false)
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	const supi = "208930100000001"
	sess := p.register(t, m, supi)

	if sess.Result.UPFAddress != testnet.Addr(testnet.EngineUserPlane, 10) || sess.Result.UPFTEID == 0 {
		t.Errorf("session UPF tunnel = %s TEID %#x, want the stub's address and a TEID", sess.Result.UPFAddress, sess.Result.UPFTEID)
	}
	if ip := net.ParseIP(sess.Result.PDUAddress); ip == nil || ip.To4()[0] != 10 {
		t.Errorf("UE address = %q, want one from the mock's 10.60/16 pool", sess.Result.PDUAddress)
	}
	info, _ := p.upf.Session(supi, 1)
	if info.DL.Addr != p.srcN3 || info.DL.TEID != sess.Result.DLTEID || info.UL.TEID != sess.Result.UPFTEID {
		t.Errorf("UPF view = %+v, want DL %s/%#x and UL %#x", info, p.srcN3, sess.Result.DLTEID, sess.Result.UPFTEID)
	}

	// Uplink reaches the UPF on the session's UL TEID.
	if err := sess.SendUplink([]byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 1, 0, 0, 10, 60, 0, 1, 8, 8, 8, 8}); err != nil {
		t.Fatalf("SendUplink: %v", err)
	}
	if !p.upf.WaitFor(2*time.Second, func(u *mockupf.UPF) bool {
		for _, ul := range u.Uplink() {
			if ul.Known && ul.TEID == sess.Result.UPFTEID {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("UPF saw no uplink on TEID %#x: %+v", sess.Result.UPFTEID, p.upf.Uplink())
	}

	// Downlink from the UPF reaches the UE's lane.
	ring := sess.rx.SubscribeICMP()
	defer sess.rx.UnsubscribeICMP(ring)
	pkt, err := datapath.BuildICMPEchoRequest(net.IPv4(8, 8, 8, 8), net.ParseIP(sess.Result.PDUAddress), 7, 1, []byte("dl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.upf.SendDownlink(supi, 1, pkt); err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("downlink did not reach the lane: %v", err)
	}
}

// An Xn path switch against the mock pair: the AMF acknowledges on the target
// association, the UPF is told the old and new downlink tunnels, and its
// path-switch behaviour per Options reaches the engine: an End Marker on the
// vacated TEID at the source gNB (live or stale, depending on whether it
// overtakes the engine's rebind; the source socket's grace window keeps it
// deliverable either way) or none, a
// late G-PDU after it, the QFI extension header, and a reallocated uplink
// TEID. Downlink then flows on the target path into the same lane.
func TestMockPairXnPathSwitch(t *testing.T) {
	tests := []struct {
		name          string
		net24         int
		opts          mockupf.Options
		reallocUL     bool
		wantEndMarker bool
		wantSent      []string
	}{
		{name: "end marker", net24: testnet.EngineXnEndMarker, opts: mockupf.Options{EndMarker: true}, wantEndMarker: true, wantSent: []string{mockupf.KindEndMarker}},
		{name: "no end marker", net24: testnet.EngineXnNoEndMarker, opts: mockupf.Options{}, wantEndMarker: false},
		{name: "late g-pdu after the end marker", net24: testnet.EngineXnLateGPDU, opts: mockupf.Options{EndMarker: true, LateGPDU: true}, wantEndMarker: true,
			wantSent: []string{mockupf.KindEndMarker, mockupf.KindGPDU}},
		{name: "end marker with QFI header", net24: testnet.EngineXnEndMarkerQFI, opts: mockupf.Options{EndMarker: true, QFI: true}, wantEndMarker: true,
			wantSent: []string{mockupf.KindEndMarker}},
		{name: "uplink tunnel reallocated", net24: testnet.EngineXnULReallocated, opts: mockupf.Options{EndMarker: true}, reallocUL: true, wantEndMarker: true,
			wantSent: []string{mockupf.KindEndMarker}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockPair(t, tc.net24, tc.opts, tc.reallocUL)
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			// The stub sends its End Marker within milliseconds of the ack;
			// a short grace window keeps the source socket for it without
			// holding the address for the production 5 s.
			m.n3.grace = 250 * time.Millisecond
			const supi = "208930100000002"
			sess := p.register(t, m, supi)
			oldDL, oldUL := sess.Result.DLTEID, sess.Result.UPFTEID
			var ems endMarkers
			sess.rx.SetEndMarkerFunc(ems.add)
			ring := sess.rx.SubscribeICMP()
			defer sess.rx.UnsubscribeICMP(ring)

			p.xn(t, m, supi)

			sw := p.upf.Switches()
			if len(sw) != 1 {
				t.Fatalf("UPF saw %d path switches, want 1", len(sw))
			}
			if sw[0].OldDL != (mockamf.Tunnel{Addr: p.srcN3, TEID: oldDL}) || sw[0].NewDL.Addr != p.tgtN3 || sw[0].NewDL.TEID != sess.Result.DLTEID {
				t.Errorf("path switch = %+v, want old %s/%#x, new %s/%#x", sw[0], p.srcN3, oldDL, p.tgtN3, sess.Result.DLTEID)
			}

			// What the UPF sent to the old tunnel, in order.
			var sent []string
			if len(tc.wantSent) > 0 && !p.upf.WaitFor(2*time.Second, func(u *mockupf.UPF) bool { return len(u.Sent()) >= len(tc.wantSent) }) {
				t.Fatalf("UPF sent %+v, want %v", p.upf.Sent(), tc.wantSent)
			}
			for _, s := range p.upf.Sent() {
				if s.TEID != oldDL || s.To.IP.String() != p.srcN3 {
					t.Errorf("sent %s to %s TEID %#x, want the old tunnel %s/%#x", s.Kind, s.To, s.TEID, p.srcN3, oldDL)
				}
				if s.HasQFI != tc.opts.QFI {
					t.Errorf("sent %s with QFI header %v, want %v", s.Kind, s.HasQFI, tc.opts.QFI)
				}
				sent = append(sent, s.Kind)
			}
			if fmt.Sprint(sent) != fmt.Sprint(tc.wantSent) {
				t.Errorf("UPF sent %v to the old tunnel, want %v", sent, tc.wantSent)
			}

			// The engine's view: a stale End Marker on the vacated TEID, or none.
			if tc.wantEndMarker {
				deadline := time.Now().Add(2 * time.Second)
				for len(ems.list()) == 0 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if len(ems.list()) == 0 {
					t.Fatal("no End Marker reached the lane")
				}
				// The marker is on the vacated TEID at the source gNB. Whether
				// the lane sees it as stale depends on whether it overtakes the
				// engine's rebind after the ack, and both orders are legitimate,
				// so the flag is not asserted.
				em := ems.list()[0]
				if em.TEID != oldDL || em.GNB != net.JoinHostPort(p.srcN3, "2152") {
					t.Errorf("End Marker = %+v, want TEID %#x at the source gNB %s", em, oldDL, p.srcN3)
				}
			} else {
				time.Sleep(500 * time.Millisecond)
				if n := len(ems.list()); n != 0 {
					t.Errorf("lane saw %d End Markers with End Marker disabled", n)
				}
			}

			// Uplink after the switch goes to the current UL TEID (the new one
			// when reallocated) and the UPF knows it.
			wantUL := oldUL
			if tc.reallocUL {
				wantUL = sw[0].NewUL.TEID
				if wantUL == oldUL || sess.Result.UPFTEID != wantUL {
					t.Fatalf("UL TEID after reallocation: switch %#x, session %#x, old %#x", wantUL, sess.Result.UPFTEID, oldUL)
				}
			}
			if err := sess.SendUplink([]byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 1, 0, 0, 10, 60, 0, 1, 8, 8, 8, 8}); err != nil {
				t.Fatalf("SendUplink after the switch: %v", err)
			}
			if !p.upf.WaitFor(2*time.Second, func(u *mockupf.UPF) bool {
				for _, ul := range u.Uplink() {
					if ul.Known && ul.TEID == wantUL && ul.From.IP.String() == p.tgtN3 {
						return true
					}
				}
				return false
			}) {
				t.Errorf("no uplink from the target on TEID %#x: %+v", wantUL, p.upf.Uplink())
			}

			// Downlink after the switch arrives on the target path into the
			// same lane.
			pkt, err := datapath.BuildICMPEchoRequest(net.IPv4(8, 8, 8, 8), net.ParseIP(sess.Result.PDUAddress), 9, 1, []byte("after"))
			if err != nil {
				t.Fatal(err)
			}
			if err := p.upf.SendDownlink(supi, 1, pkt); err != nil {
				t.Fatal(err)
			}
			for {
				f, err := ring.Read(2 * time.Second)
				if err != nil {
					t.Fatalf("downlink after the switch did not reach the lane: %v", err)
				}
				if string(f.Payload[len(f.Payload)-5:]) == "after" {
					break // earlier frames are the late G-PDU, if any
				}
			}
		})
	}
}

// ObserveDownlink sees what the UPF sends on the old path across an Xn path
// switch: the End Marker on the vacated TEID at the source gNB, with its QFI
// container when the UPF adds one, and a late G-PDU on that TEID after it,
// in arrival order. An unregistered UE is an error.
func TestObserveDownlinkAcrossXn(t *testing.T) {
	p := newMockPair(t, testnet.EngineObserveAcrossXn, mockupf.Options{EndMarker: true, LateGPDU: true, QFI: true}, false)
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.n3.grace = 250 * time.Millisecond
	const supi = "208930100000003"

	if _, err := m.ObserveDownlink(supi, nil, nil); err == nil {
		t.Fatal("ObserveDownlink on an unregistered UE succeeded")
	}

	sess := p.register(t, m, supi)
	oldDL := sess.Result.DLTEID
	type obs struct {
		at     time.Time
		marker bool
		teid   uint32
		gnb    string
		hasQFI bool
	}
	var mu sync.Mutex
	var seen []obs
	stop, err := m.ObserveDownlink(supi,
		func(em datapath.EndMarker) {
			mu.Lock()
			seen = append(seen, obs{at: em.Arrival, marker: true, teid: em.TEID, gnb: em.GNB, hasQFI: em.HasQFI})
			mu.Unlock()
		},
		func(g datapath.GPDUArrival) {
			mu.Lock()
			seen = append(seen, obs{at: g.Arrival, teid: g.TEID, gnb: g.GNB})
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("ObserveDownlink: %v", err)
	}
	defer stop()
	if a := sess.N3LocalAddr(); a == nil || a.String() != net.JoinHostPort(p.srcN3, "2152") {
		t.Errorf("N3LocalAddr before the switch = %v, want the source gNB socket", a)
	}

	p.xn(t, m, supi)

	if a := sess.N3LocalAddr(); a == nil || a.String() != net.JoinHostPort(p.tgtN3, "2152") {
		t.Errorf("N3LocalAddr after the switch = %v, want the target gNB socket", a)
	}
	snapshot := func() []obs {
		mu.Lock()
		defer mu.Unlock()
		return append([]obs(nil), seen...)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := snapshot()
	if len(got) != 2 {
		t.Fatalf("observed %+v, want the End Marker then the late G-PDU", got)
	}
	src := net.JoinHostPort(p.srcN3, "2152")
	for _, o := range got {
		if o.teid != oldDL || o.gnb != src {
			t.Errorf("observation %+v, want TEID %#x at %s", o, oldDL, src)
		}
	}
	if !got[0].marker || got[1].marker || got[1].at.Before(got[0].at) {
		t.Errorf("observed %+v, want the End Marker first and the G-PDU after it", got)
	}
	if !got[0].hasQFI {
		t.Error("End Marker QFI container not recorded")
	}
}
