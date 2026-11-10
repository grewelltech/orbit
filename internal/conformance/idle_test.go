package conformance

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
	"github.com/bgrewell/orbit/internal/ue"
)

// classifyService passes only on a Service Accept, the session's tunnel
// set up again and downlink on it; every other outcome fails with the
// check's message and what was seen. The Service Accept's IEs are recorded
// and change no verdict.
func TestClassifyService(t *testing.T) {
	t0 := time.Unix(1000, 0)
	tunnel := engine.SessionResult{PDUSessionID: 1, UPFAddress: "192.0.2.10", UPFTEID: 0x51, DLTEID: 0x7}
	accept := func(a *ue.ServiceAccept, mod func(*engine.ServiceResult)) *engine.ServiceResult {
		r := &engine.ServiceResult{Sent: t0, Answered: t0.Add(40 * time.Millisecond), Accepted: true, Requested: 2,
			Accept: a, ContextSetup: true, Reactivated: []engine.SessionResult{tunnel}}
		if mod != nil {
			mod(r)
		}
		return r
	}
	gpdu := &datapath.GPDUArrival{Arrival: t0.Add(3*time.Second + 15*time.Millisecond), TEID: 0x7}
	const message = "Service Request did not re-establish the PDU session user plane. TS 24.501 5.6.1.4.1 (5.6.1.4 in Rel-15; Rel-15 onward). Likely cause: AMF/SMF reactivation path. Seen: "
	tests := []struct {
		name         string
		res          *engine.ServiceResult
		unanswered   bool
		traffic      *datapath.GPDUArrival
		want         Verdict
		wantObserved string
		wantDetail   []string
		notDetail    []string
	}{
		{name: "accept, tunnel and traffic", res: accept(&ue.ServiceAccept{PDUSessionStatus: 2, HasPDUSessionStatus: true, HasReactivationResult: true}, nil), traffic: gpdu,
			want: Pass, wantObserved: "Service Accept and downlink traffic on the reactivated N3 tunnel",
			wantDetail: []string{"Service Accept 40ms after the Service Request", "PDU session status 0x0002",
				"PDU session reactivation result 0x0000 reports the session re-established (recorded only)",
				"N3 tunnel set up again by Initial Context Setup: gNB downlink TEID 0x7, UPF 192.0.2.10 uplink TEID 0x51",
				"downlink G-PDU on the reactivated tunnel 15ms after the trigger"},
			notDetail: []string{"did not re-establish"}},
		{name: "no reactivation result IE still passes", res: accept(&ue.ServiceAccept{}, func(r *engine.ServiceResult) { r.ContextSetup = false }), traffic: gpdu,
			want: Pass, wantDetail: []string{"no PDU session reactivation result IE (recorded only)", "set up again by PDU Session Resource Setup"}},
		{name: "a reactivation result that reports failure is recorded", traffic: gpdu,
			res:  accept(&ue.ServiceAccept{HasReactivationResult: true, ReactivationResult: 2, ReactivationErrors: []byte{1, 92}}, nil),
			want: Pass, wantDetail: []string{"reports the session not re-established (recorded only)", "error cause 01 5c"}},
		{name: "unanswered", unanswered: true, want: Fail, wantObserved: "no answer to the Service Request",
			wantDetail: []string{message + "neither Service Accept nor Service Reject within 15s (TS 24.501 5.6.1.5"}},
		{name: "reject", res: &engine.ServiceResult{Sent: t0, Answered: t0.Add(20 * time.Millisecond), Rejected: true, RejectCause: 22, Released: true},
			want: Fail, wantObserved: "Service Reject",
			wantDetail: []string{message + "Service Reject with 5GMM cause #22, 20ms after the Service Request", "the core released the UE context after the reject"}},
		{name: "accept, then release", res: accept(nil, func(r *engine.ServiceResult) { r.Released = true }), want: Fail,
			wantObserved: "Service Accept, then the UE context released", wantDetail: []string{message + "the core released the UE context after its Service Accept"}},
		{name: "accept without the session", res: accept(nil, func(r *engine.ServiceResult) { r.Reactivated = nil }), want: Fail,
			wantObserved: "Service Accept without user plane",
			wantDetail:   []string{message + "Service Accept, and an Initial Context Setup without the session, and no PDU Session Resource Setup for PDU session 1 within 15s"}},
		{name: "accept in a downlink NAS transport only", res: accept(nil, func(r *engine.ServiceResult) { r.Reactivated, r.ContextSetup = nil, false }), want: Fail,
			wantObserved: "Service Accept without user plane",
			wantDetail:   []string{"Service Accept, and no Initial Context Setup or PDU Session Resource Setup for PDU session 1"}},
		{name: "accept for another session only", res: accept(nil, func(r *engine.ServiceResult) { r.Reactivated[0].PDUSessionID = 2 }), want: Fail,
			wantObserved: "Service Accept without user plane"},
		{name: "tunnel without traffic", res: accept(nil, nil), want: Fail, wantObserved: "Service Accept and N3 tunnel, no downlink traffic",
			wantDetail: []string{message + "Service Accept and the N3 tunnel set up again on N2, and no downlink G-PDU on it within 5s", "TS 23.502 4.2.3.2 step 18a",
				"gNB downlink TEID 0x7"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := serviceObservation{Session: 1, ReleaseCause: "nas/0", Before: 12 * time.Millisecond, Result: tc.res, Unanswered: tc.unanswered,
				AnswerWindow: 15 * time.Second, Triggered: t0.Add(3 * time.Second), Traffic: tc.traffic, TrafficWindow: 5 * time.Second}
			v, observed, detail := classifyService(o)
			if v != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", v, observed, detail, tc.want)
			}
			if tc.wantObserved != "" && observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", observed, tc.wantObserved)
			}
			always := []string{"PDU session 1 in the Uplink data status", "AN release cause nas/0", "downlink reached the UE 12ms after the trigger before the release"}
			for _, w := range append(always, tc.wantDetail...) {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
			for _, w := range tc.notDetail {
				if strings.Contains(detail, w) {
					t.Errorf("detail %q has %q", detail, w)
				}
			}
		})
	}
}

// Without a UE or without a downlink trigger the check SKIPs before any
// association is opened; an env the harness refuses is an ERROR.
func TestServiceCheckWithoutHarness(t *testing.T) {
	env := ueEnv("192.0.2.1:38412", "", "127.0.0.1")
	env.Downlink = engine.DownlinkTriggerFunc(func(context.Context, netip.Addr) error { return nil })
	noUE, noTrigger, fixedPort := env, env, env
	noUE.UE = UESpec{}
	noTrigger.Downlink = nil
	fixedPort.Bind = "127.0.0.1:38412"
	tests := []struct {
		name         string
		env          Env
		want         Verdict
		wantObserved string
	}{
		{"no UE", noUE, Skip, "no UE configured"},
		{"no downlink trigger", noTrigger, Skip, "no N6 downlink trigger configured"},
		{"a fixed-port bind", fixedPort, Error, "harness setup failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			r := serviceRequestReactivates{}.Run(ctx, tc.env)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Errorf("verdict %s (%s; %s), want %s (%s)", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
		})
	}
	c := serviceRequestReactivates{}
	if got := c.Budget(); got != 46*time.Second {
		t.Errorf("Budget = %s, want 46s", got)
	}
	if err := ValidateTest(c); err != nil {
		t.Error(err)
	}
	if WorstVerdict(c) != Fail || MinRelease(c) != 15 {
		t.Errorf("worst verdict %s, min release %s; want FAIL and Rel-15", WorstVerdict(c), MinRelease(c))
	}
	if n := NeedsOf(c); n.UEs != 1 || n.RequiresIdleCore {
		t.Errorf("needs %+v, want one UE and no idle core", n)
	}
}

// n6Packet is a DownlinkTrigger that hands the stub UPF one packet for the
// UE's address, as a host on N6 would send it, once the stub holds the
// session (the mock AMF tells it of a new session on its own goroutine).
func n6Packet(upf *mockupf.UPF) engine.DownlinkTrigger {
	return engine.DownlinkTriggerFunc(func(_ context.Context, ueIP netip.Addr) error {
		upf.WaitFor(2*time.Second, func(u *mockupf.UPF) bool { _, ok := u.Session("208930100000011", 1); return ok })
		pkt, err := datapath.BuildICMPEchoRequest(net.IPv4(192, 0, 2, 9), net.IP(ueIP.AsSlice()), 0x1d1e, 1, []byte("data"))
		if err != nil {
			return err
		}
		return upf.DownlinkFromN6(pkt)
	})
}

// quickService is the check with waits short enough for the mock pair.
var quickService = serviceRequestReactivates{traffic: 400 * time.Millisecond, quiet: 40 * time.Millisecond, settle: 10 * time.Millisecond,
	answer: 400 * time.Millisecond, userPlane: 200 * time.Millisecond}

// Every verdict over the real-SCTP mock pair: the core accepts and its UPF
// forwards on the new tunnel (PASS); it rejects, accepts without the
// session's tunnel, sets the tunnel up without telling its UPF, or leaves
// the request unanswered (FAIL); and the runs that prove nothing about the
// core (ERROR): no downlink reaches the UE before the release, the trigger
// fails, its traffic never stops, the release is ignored, or the UE has no
// 5G-GUTI. Each run leaves no association open and the UE deregistered.
func TestServiceCheckAgainstMockPair(t *testing.T) {
	silent := engine.DownlinkTriggerFunc(func(context.Context, netip.Addr) error { return nil })
	broken := engine.DownlinkTriggerFunc(func(context.Context, netip.Addr) error { return errors.New("responder unreachable") })
	tests := []struct {
		name         string
		net24        int
		amfOpts      []mockamf.Option
		trigger      func(t *testing.T, upf *mockupf.UPF) engine.DownlinkTrigger // nil: n6Packet
		timeout      time.Duration
		want         Verdict
		wantObserved string
		wantDetail   []string
		wantRequests int64
		wantDeregs   int64
		wantOnNewDL  bool // the stub UPF sent a G-PDU to a tunnel other than the registration's
	}{
		{name: "reactivated", net24: testnet.ConformanceServicePass, amfOpts: []mockamf.Option{mockamf.WithGUTI()},
			want: Pass, wantObserved: "Service Accept and downlink traffic on the reactivated N3 tunnel", wantRequests: 1, wantDeregs: 1, wantOnNewDL: true,
			wantDetail: []string{"PDU session 1 in the Uplink data status", "AN release cause nas/0", "PDU session status 0x0002",
				"reports the session re-established (recorded only)", "N3 tunnel set up again by Initial Context Setup", "downlink G-PDU on the reactivated tunnel"}},
		{name: "reject", net24: testnet.ConformanceServiceReject, amfOpts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithServiceAnswer(mockamf.ServiceReject, 22)},
			want: Fail, wantObserved: "Service Reject", wantRequests: 1, wantDeregs: 1,
			wantDetail: []string{"Service Request did not re-establish the PDU session user plane. TS 24.501 5.6.1.4.1", "Likely cause: AMF/SMF reactivation path.",
				"Service Reject with 5GMM cause #22", "the core released the UE context after the reject"}},
		{name: "accept without user plane", net24: testnet.ConformanceServiceNoUP,
			amfOpts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithServiceAnswer(mockamf.ServiceAcceptNoUserPlane, 0)},
			want:    Fail, wantObserved: "Service Accept without user plane", wantRequests: 1, wantDeregs: 1,
			wantDetail: []string{"Service Request did not re-establish the PDU session user plane.", "an Initial Context Setup without the session"}},
		{name: "UPF not told", net24: testnet.ConformanceServiceUPFStale,
			amfOpts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithServiceAnswer(mockamf.ServiceAcceptUPFNotTold, 0)},
			want:    Fail, wantObserved: "Service Accept and N3 tunnel, no downlink traffic", wantRequests: 1, wantDeregs: 1,
			wantDetail: []string{"Service Request did not re-establish the PDU session user plane.", "no downlink G-PDU on it within 400ms", "N3 tunnel set up again by Initial Context Setup"}},
		{name: "unanswered", net24: testnet.ConformanceServiceIgnored, amfOpts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithServiceAnswer(mockamf.ServiceIgnore, 0)},
			want: Fail, wantObserved: "no answer to the Service Request", wantRequests: 1, wantDeregs: 1,
			wantDetail: []string{"Service Request did not re-establish the PDU session user plane.", "neither Service Accept nor Service Reject within 600ms"}},
		{name: "no traffic before the release", net24: testnet.ConformanceServiceNoTraffic, amfOpts: []mockamf.Option{mockamf.WithGUTI()},
			trigger: func(*testing.T, *mockupf.UPF) engine.DownlinkTrigger { return silent },
			want:    Error, wantObserved: "no downlink traffic reached the UE before the release", wantDeregs: 1,
			wantDetail: []string{"did not arrive on its N3 tunnel within 400ms", "check the responder's route"}},
		{name: "trigger fails", net24: testnet.ConformanceServiceNoTrigger, amfOpts: []mockamf.Option{mockamf.WithGUTI()},
			trigger: func(*testing.T, *mockupf.UPF) engine.DownlinkTrigger { return broken },
			want:    Error, wantObserved: "the downlink trigger failed", wantDetail: []string{"responder unreachable"}, wantDeregs: 1},
		{name: "traffic never stops", net24: testnet.ConformanceServiceBusy, amfOpts: []mockamf.Option{mockamf.WithGUTI()},
			trigger: func(t *testing.T, upf *mockupf.UPF) engine.DownlinkTrigger {
				one := n6Packet(upf)
				return engine.DownlinkTriggerFunc(func(_ context.Context, ueIP netip.Addr) error {
					stop := make(chan struct{})
					t.Cleanup(func() { close(stop) })
					go func() {
						tick := time.NewTicker(5 * time.Millisecond)
						defer tick.Stop()
						for {
							// The session is gone once the UE deregisters.
							_ = one.SendDownlink(context.Background(), ueIP)
							select {
							case <-stop:
								return
							case <-tick.C:
							}
						}
					}()
					return nil
				})
			},
			want: Error, wantObserved: "the downlink traffic did not stop before the release", wantDeregs: 1},
		{name: "release request ignored", net24: testnet.ConformanceServiceNoRelease, amfOpts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithIgnoreUEContextReleaseRequest()},
			timeout: 3 * time.Second, want: Error, wantObserved: "the AN release to CM-IDLE did not complete",
			wantDetail: []string{"no UE Context Release Command"}, wantDeregs: 1},
		{name: "no 5G-GUTI", net24: testnet.ConformanceServiceNoGUTI,
			want: Error, wantObserved: "the AN release to CM-IDLE did not complete", wantDetail: []string{"no 5G-GUTI"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCore(t, tc.net24, mockupf.Options{}, testKi, tc.amfOpts...)
			env := ueEnv(c.front.Addr, "", c.n3)
			env.Downlink = n6Packet(c.upf)
			if tc.trigger != nil {
				env.Downlink = tc.trigger(t, c.upf)
			}
			timeout := tc.timeout
			if timeout == 0 {
				timeout = 20 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			r := quickService.Run(ctx, env)
			if r.Verdict != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", r.Verdict, r.Observed, r.Detail, tc.want)
			}
			if r.Observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", r.Observed, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if got := c.amf.ServiceRequests(); got != tc.wantRequests {
				t.Errorf("the mock AMF verified %d Service Requests, want %d", got, tc.wantRequests)
			}
			if got := c.amf.Deregistrations(); got != tc.wantDeregs {
				t.Errorf("the mock AMF saw %d deregistrations, want %d", got, tc.wantDeregs)
			}
			teids := map[uint32]bool{}
			for _, s := range c.upf.Sent() {
				if s.Kind == mockupf.KindGPDU {
					teids[s.TEID] = true
				}
			}
			if got := len(teids) > 1; got != tc.wantOnNewDL {
				t.Errorf("the stub UPF sent downlink on %d tunnel(s); on a reactivated one: want %v", len(teids), tc.wantOnNewDL)
			}
			c.assertClosed(t, env)
		})
	}
}

// A per-test timeout that ends while the Service Request is unanswered is
// an ERROR that names the budget, never a FAIL.
func TestServiceCheckCutShort(t *testing.T) {
	c := newMockCore(t, testnet.ConformanceServiceCutShort, mockupf.Options{}, testKi, mockamf.WithGUTI(), mockamf.WithServiceAnswer(mockamf.ServiceIgnore, 0))
	env := ueEnv(c.front.Addr, "", c.n3)
	env.Downlink = n6Packet(c.upf)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	check := quickService
	check.answer = time.Minute
	r := check.Run(ctx, env)
	if r.Verdict != Error || r.Observed != "the Service Request was cut short" || !strings.Contains(r.Detail, "--per-test-timeout") {
		t.Errorf("verdict %s (%s; %s), want the cut-short ERROR", r.Verdict, r.Observed, r.Detail)
	}
	c.assertClosed(t, env)
}
