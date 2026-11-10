package engine

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/testnet"
	"github.com/bgrewell/orbit/internal/ue"
)

const idleSUPI = "208930100000031"

// idleManager is a Manager whose deregistration waits briefly for the
// core's release.
func idleManager() *Manager {
	m := NewManager(testLogger())
	m.releaseLinger = time.Second
	return m
}

// goIdle releases the UE to CM-IDLE and checks both ends agree.
func goIdle(t *testing.T, p *mockPair, m *Manager) *IdleResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := m.Idle(ctx, idleSUPI)
	if err != nil {
		t.Fatalf("Idle: %v", err)
	}
	sess, err := m.Status(idleSUPI)
	if err != nil {
		t.Fatal(err)
	}
	if !sess.Idle() || sess.State() != StateIdle {
		t.Fatalf("after Idle the session is idle=%v state %s", sess.Idle(), sess.State())
	}
	if !p.upf.WaitFor(3*time.Second, func(u *mockupf.UPF) bool { _, b := u.Buffered(idleSUPI, 1); return b }) {
		t.Fatal("the UPF never started buffering: the mock AMF did not see the UE go CM-IDLE")
	}
	if !p.amf.Idle(idleSUPI) {
		t.Fatal("the mock AMF does not hold the UE CM-IDLE")
	}
	return res
}

// downlinkTo sends one packet from the N6 side to the UE's address.
func downlinkTo(t *testing.T, p *mockPair, ueIP string) {
	t.Helper()
	pkt, err := datapath.BuildICMPEchoRequest(net.IPv4(192, 0, 2, 9), net.ParseIP(ueIP), 0x1d1e, 1, []byte("wake"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.upf.DownlinkFromN6(pkt); err != nil {
		t.Fatalf("downlink from N6: %v", err)
	}
}

// The whole idle cycle over the real-SCTP mock pair: AN release to CM-IDLE,
// downlink data, PAGING naming the UE in its tracking area, a Service
// Request that reactivates the session on a new downlink tunnel, the
// buffered packet delivered there, uplink flowing again, and a normal
// deregistration. Under NEA2 the request's NAS message container is
// ciphered.
func TestIdlePagingServiceRequest(t *testing.T) {
	tests := []struct {
		name  string
		net24 int
		ea    uint8
	}{
		{"NEA0", testnet.EngineIdlePaging, nas.NEA0},
		{"NEA2", testnet.EngineIdlePagingNEA2, nas.NEA2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockPairWith(t, tc.net24, mockupf.Options{}, false, mockamf.WithGUTI(), mockamf.WithCipheringOrder(tc.ea))
			m := idleManager()
			sess := p.register(t, m, idleSUPI, func(c *UEConfig) {
				caps, err := ue.NewSecurityCapability([]uint8{tc.ea}, []uint8{nas.NIA2})
				if err != nil {
					t.Fatal(err)
				}
				c.SecurityCapabilities = &caps
			})
			oldDL, ueIP := sess.Result.DLTEID, sess.Result.PDUAddress
			stmsi, ok := sess.STMSI()
			if !ok {
				t.Fatal("no 5G-S-TMSI after registration with a 5G-GUTI")
			}

			idle := goIdle(t, p, m)
			if idle.Cause != "nas/0" || idle.STMSI != stmsi || idle.Released.Before(idle.Requested) {
				t.Errorf("IdleResult = %+v, want cause nas/0 (normal release) and the UE's 5G-S-TMSI %s", idle, stmsi)
			}
			if _, _, err := sess.dataplane(); err == nil || !strings.Contains(err.Error(), "no user plane") {
				t.Errorf("data path of a CM-IDLE UE: %v, want the no-user-plane error", err)
			}

			downlinkTo(t, p, ueIP)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			pg, err := m.ObservePaging(ctx, idleSUPI)
			if err != nil {
				t.Fatalf("ObservePaging: %v", err)
			}
			if !pg.ForUE || pg.STMSI != stmsi {
				t.Errorf("PAGING names %s (for the UE: %v), want %s", pg.STMSI, pg.ForUE, stmsi)
			}
			if len(pg.TAIs) != 1 || pg.TAIs[0].MCC != "208" || pg.TAIs[0].MNC != "93" || pg.TAIs[0].TAC != 1 {
				t.Errorf("TAI List for Paging = %+v, want 208/93 TAC 1", pg.TAIs)
			}

			sr, err := m.ServiceRequest(ctx, idleSUPI, WithMobileTerminated())
			if err != nil {
				t.Fatalf("ServiceRequest: %v", err)
			}
			if !sr.Accepted || sr.Rejected || !sr.ContextSetup || !sr.UserPlane(1) || sr.Requested != 1<<1 {
				t.Fatalf("ServiceResult = %+v, want an Accept with session 1 reactivated", sr)
			}
			if sr.Accept == nil || !sr.Accept.HasPDUSessionStatus || sr.Accept.PDUSessionStatus != 1<<1 ||
				!sr.Accept.HasReactivationResult || sr.Accept.ReactivationResult != 0 {
				t.Errorf("Service Accept = %+v, want PDU session status {1} and an empty reactivation result", sr.Accept)
			}
			if sess.Idle() || sess.State() != StateSessionActive {
				t.Errorf("after the Service Request the session is idle=%v state %s", sess.Idle(), sess.State())
			}
			newDL := sess.Result.DLTEID
			if newDL == oldDL || newDL != sr.Reactivated[0].DLTEID {
				t.Errorf("downlink TEID %#x after reactivation (was %#x), want the new tunnel %#x", newDL, oldDL, sr.Reactivated[0].DLTEID)
			}
			if got := p.amf.ServiceRequests(); got != 1 {
				t.Errorf("the mock AMF verified %d Service Requests, want 1", got)
			}
			// The buffered packet goes down the new tunnel.
			if !p.upf.WaitFor(3*time.Second, func(u *mockupf.UPF) bool {
				for _, s := range u.Sent() {
					if s.Kind == mockupf.KindGPDU && s.TEID == newDL {
						return true
					}
				}
				return false
			}) {
				t.Error("the buffered downlink packet was not sent on the reactivated tunnel")
			}
			if _, buffering := p.upf.Buffered(idleSUPI, 1); buffering {
				t.Error("the UPF still buffers after the session came up")
			}
			// Uplink flows on the reopened data path.
			if _, _, err := sess.dataplane(); err != nil {
				t.Fatalf("reopen the data path: %v", err)
			}
			up, err := datapath.BuildICMPEchoRequest(net.ParseIP(ueIP), net.IPv4(192, 0, 2, 9), 0x1d1e, 2, []byte("up"))
			if err != nil {
				t.Fatal(err)
			}
			before := len(p.upf.Uplink())
			if err := sess.SendUplink(up); err != nil {
				t.Fatalf("uplink after reactivation: %v", err)
			}
			if !p.upf.WaitFor(3*time.Second, func(u *mockupf.UPF) bool {
				ul := u.Uplink()
				return len(ul) > before && ul[len(ul)-1].Known
			}) {
				t.Error("no uplink reached the UPF on a known tunnel after reactivation")
			}

			if err := m.Deregister(ctx, idleSUPI); err != nil {
				t.Fatalf("Deregister: %v", err)
			}
			if got := p.amf.Deregistrations(); got != 1 {
				t.Errorf("the mock AMF saw %d deregistrations, want 1", got)
			}
		})
	}
}

// What ServiceRequest reports for each answer a core can give, and that a
// UE left CM-IDLE still deregisters: its request opens a new connection in
// an Initial UE Message, and the release that answers it is completed.
func TestServiceRequestAnswers(t *testing.T) {
	tests := []struct {
		name   string
		net24  int
		answer mockamf.ServiceAnswer
		opts   []ServiceOption
		check  func(t *testing.T, res *ServiceResult, err error, sess *Session)
		idle   bool // the UE is still CM-IDLE afterwards
	}{
		{name: "reject", net24: testnet.EngineIdleReject, answer: mockamf.ServiceReject, idle: true,
			check: func(t *testing.T, res *ServiceResult, err error, _ *Session) {
				if err != nil || !res.Rejected || res.Accepted || res.RejectCause != 22 || !res.Released || len(res.Reactivated) != 0 {
					t.Errorf("result %+v, err %v; want a Reject with cause 22 and the release", res, err)
				}
			}},
		{name: "accept without user plane", net24: testnet.EngineIdleNoUserPlane, answer: mockamf.ServiceAcceptNoUserPlane,
			opts: []ServiceOption{WithUserPlaneWait(200 * time.Millisecond)},
			check: func(t *testing.T, res *ServiceResult, err error, sess *Session) {
				if err != nil || !res.Accepted || !res.ContextSetup || len(res.Reactivated) != 0 || res.UserPlane(1) {
					t.Errorf("result %+v, err %v; want an Accept with no session reactivated", res, err)
				}
				if sess.State() != StateRegistered {
					t.Errorf("state %s, want %s", sess.State(), StateRegistered)
				}
				if _, _, derr := sess.dataplane(); derr == nil {
					t.Error("the data path opened on a tunnel the core never set up again")
				}
			}},
		{name: "unanswered", net24: testnet.EngineIdleUnanswered, answer: mockamf.ServiceIgnore, idle: true,
			check: func(t *testing.T, res *ServiceResult, err error, _ *Session) {
				if !errors.Is(err, ErrServiceUnanswered) || !errors.Is(err, context.DeadlineExceeded) || res != nil {
					t.Errorf("result %+v, err %v; want ErrServiceUnanswered wrapping the deadline", res, err)
				}
			}},
		{name: "signalling only", net24: testnet.EngineIdleSignalling, answer: mockamf.ServiceAccept,
			opts: []ServiceOption{WithUplinkDataStatus()},
			check: func(t *testing.T, res *ServiceResult, err error, sess *Session) {
				if err != nil || !res.Accepted || res.Requested != 0 || len(res.Reactivated) != 0 {
					t.Errorf("result %+v, err %v; want an Accept of a request with no Uplink data status", res, err)
				}
				if res.Accept == nil || res.Accept.HasReactivationResult {
					t.Errorf("Service Accept %+v, want no reactivation result", res.Accept)
				}
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockPairWith(t, tc.net24, mockupf.Options{}, false, mockamf.WithGUTI(), mockamf.WithServiceAnswer(tc.answer, 22))
			m := idleManager()
			sess := p.register(t, m, idleSUPI)
			goIdle(t, p, m)

			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			res, err := m.ServiceRequest(ctx, idleSUPI, tc.opts...)
			cancel()
			tc.check(t, res, err, sess)
			if sess.Idle() != tc.idle {
				t.Errorf("idle = %v afterwards, want %v", sess.Idle(), tc.idle)
			}

			if tc.answer == mockamf.ServiceReject {
				// The mock holds the UE idle again only once the gNB has
				// completed the release.
				deadline := time.Now().Add(2 * time.Second)
				for p.amf.ReleaseCompletes() < 2 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
			}
			dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer dcancel()
			if err := m.Deregister(dctx, idleSUPI); err != nil {
				t.Fatalf("Deregister: %v", err)
			}
			if got := p.amf.Deregistrations(); got != 1 {
				t.Errorf("the mock AMF saw %d deregistrations, want 1", got)
			}
			if got := p.amf.DeregistrationReleaseIDs(); len(got) != 1 {
				t.Errorf("the mock AMF released %d UEs for a deregistration, want 1", len(got))
			}
			deadline := time.Now().Add(2 * time.Second)
			want := len(p.amf.DeregistrationReleaseIDs())
			for time.Now().Before(deadline) {
				done := 0
				for _, c := range p.amf.ReleaseCompleteIDs() {
					for _, r := range p.amf.DeregistrationReleaseIDs() {
						if c == r {
							done++
						}
					}
				}
				if done >= want {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Error("the deregistration's release was never completed with its ID pair")
		})
	}
}

// ObservePaging reports a PAGING for another identity as not the UE's, and
// a wait that ends on its context loses nothing: the PAGING that arrives
// afterwards is still read.
func TestObservePaging(t *testing.T) {
	t.Run("another identity", func(t *testing.T) {
		p := newMockPairWith(t, testnet.EngineIdleWrongIdentity, mockupf.Options{}, false, mockamf.WithGUTI(), mockamf.WithWrongPagingIdentity())
		m := idleManager()
		sess := p.register(t, m, idleSUPI)
		goIdle(t, p, m)
		downlinkTo(t, p, sess.Result.PDUAddress)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pg, err := m.ObservePaging(ctx, idleSUPI)
		if err != nil {
			t.Fatalf("ObservePaging: %v", err)
		}
		own, _ := sess.STMSI()
		if pg.ForUE || !pg.HasSTMSI || pg.STMSI == own {
			t.Errorf("PAGING %+v reported as the UE's (%s)", pg, own)
		}
		_ = m.Deregister(ctx, idleSUPI)
	})
	t.Run("no paging", func(t *testing.T) {
		p := newMockPairWith(t, testnet.EngineIdleNoPaging, mockupf.Options{}, false, mockamf.WithGUTI())
		m := idleManager()
		sess := p.register(t, m, idleSUPI)
		goIdle(t, p, m)
		for i := 0; i < 3; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			_, err := m.ObservePaging(ctx, idleSUPI)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("ObservePaging with no downlink data: %v, want the deadline", err)
			}
		}
		downlinkTo(t, p, sess.Result.PDUAddress)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pg, err := m.ObservePaging(ctx, idleSUPI)
		if err != nil || !pg.ForUE {
			t.Fatalf("the PAGING after timed-out waits: %+v, %v", pg, err)
		}
		// A second packet in the same buffering period raises no second
		// notification.
		downlinkTo(t, p, sess.Result.PDUAddress)
		if got := p.amf.Pagings(); got != 1 {
			t.Errorf("the mock AMF paged %d times, want 1", got)
		}
		_ = m.Deregister(ctx, idleSUPI)
	})
}

// A UE the core gave no 5G-GUTI cannot be paged or come back: Idle refuses
// it, and it stays connected. A connected UE has no paging to observe and
// no Service Request to send.
func TestIdleNeedsGUTI(t *testing.T) {
	p := newMockPair(t, testnet.EngineIdleNoGUTI, mockupf.Options{}, false)
	m := idleManager()
	sess := p.register(t, m, idleSUPI)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := m.Idle(ctx, idleSUPI); err == nil || !strings.Contains(err.Error(), "no 5G-GUTI") {
		t.Errorf("Idle without a 5G-GUTI: %v", err)
	}
	if sess.Idle() {
		t.Error("the UE went idle without a 5G-GUTI")
	}
	if _, err := m.ObservePaging(ctx, idleSUPI); err == nil || !strings.Contains(err.Error(), "not CM-IDLE") {
		t.Errorf("ObservePaging on a connected UE: %v", err)
	}
	if _, err := m.ServiceRequest(ctx, idleSUPI); err == nil || !strings.Contains(err.Error(), "not CM-IDLE") {
		t.Errorf("ServiceRequest on a connected UE: %v", err)
	}
	if _, err := m.Idle(ctx, "208930199999999"); err == nil {
		t.Error("Idle of an unknown UE succeeded")
	}
}
