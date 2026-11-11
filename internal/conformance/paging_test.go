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
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
)

// classifyPaging passes on a PAGING naming the UE and fails on none or on
// other identities only; the TAI list, the delay, MICO and the answer are
// recorded and change no verdict.
func TestClassifyPaging(t *testing.T) {
	own := gnb.STMSI{AMFSetID: 0x3f8, TMSI: 7}
	other := gnb.STMSI{AMFSetID: 0x3f8, TMSI: 8}
	serving := gnb.TAI{MCC: "208", MNC: "93", TAC: 1}
	t0 := time.Unix(1000, 0)
	ev := func(id *gnb.STMSI, forUE bool, tais ...gnb.TAI) engine.PagingEvent {
		p := gnb.Paging{TAIs: tais}
		if id != nil {
			p.STMSI, p.HasSTMSI = *id, true
		}
		return engine.PagingEvent{At: t0.Add(120 * time.Millisecond), Paging: p, ForUE: forUE}
	}
	tests := []struct {
		name         string
		seen         []engine.PagingEvent
		mico         bool
		answer       string
		want         Outcome
		wantObserved string
		wantDetail   []string
		notDetail    []string
	}{
		{name: "paged in the serving TAI", seen: []engine.PagingEvent{ev(&own, true, serving)}, answer: "Service Request after the PAGING accepted, 1 session(s) reactivated",
			want: pagingSent, wantObserved: "PAGING with the UE's 5G-S-TMSI",
			wantDetail: []string{"PAGING 120ms after the downlink trigger", "TAI List for Paging [208/93 TAC 1] holds the serving TAI 208/93 TAC 1 (recorded only)", "accepted, 1 session(s) reactivated (recorded only)"}},
		{name: "paged without the serving TAI still passes", seen: []engine.PagingEvent{ev(&own, true, gnb.TAI{MCC: "208", MNC: "93", TAC: 9})},
			want: pagingSent, wantDetail: []string{"does not hold the serving TAI"}},
		{name: "paged after a PAGING for another UE", seen: []engine.PagingEvent{ev(&other, false, serving), ev(&own, true, serving)},
			want: pagingSent, wantDetail: []string{"PAGING for other identities: " + other.String()}},
		{name: "no paging", want: pagingNone, wantObserved: "no PAGING",
			wantDetail: []string{"no NGAP PAGING within 10s of downlink data to a CM-IDLE UE. TS 24.501 5.6.2.2.1 (Rel-15 onward); TS 38.413 8.5.1.2 (Rel-15 onward); TS 23.502 4.2.3.3 step 4b (Rel-15 onward). Likely cause: UPF/SMF downlink data notification or AMF N1N2MessageTransfer paging path not implemented."}},
		{name: "another identity only", seen: []engine.PagingEvent{ev(&other, false, serving)}, want: pagingOtherIdentity, wantObserved: "PAGING for another identity only",
			wantDetail: []string{"1 PAGING message(s) named another UE Paging Identity", "TS 38.413 9.3.3.18", other.String()}, notDetail: []string{"Likely cause"}},
		{name: "a PAGING with no identity only", seen: []engine.PagingEvent{ev(nil, false, serving)}, want: pagingOtherIdentity, wantObserved: "PAGING for another identity only",
			wantDetail: []string{"PAGING for other identities: none"}},
		{name: "MICO is recorded", mico: true, want: pagingNone, wantDetail: []string{"MICO indication"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := pagingObservation{STMSI: own, Serving: serving, ReleaseCause: "nas/0", Sent: t0, Seen: tc.seen, MICO: tc.mico, Answer: tc.answer}
			out, observed, detail := classifyPaging(o, 10*time.Second)
			if out.ID != covers(tc.want).ID || out.Verdict != tc.want.Verdict {
				t.Fatalf("outcome %s %s (%s; %s), want %s %s", out.ID, out.Verdict, observed, detail, tc.want.ID, tc.want.Verdict)
			}
			if tc.wantObserved != "" && observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", observed, tc.wantObserved)
			}
			for _, w := range append([]string{"UE 5G-S-TMSI " + own.String(), "AN release cause nas/0"}, tc.wantDetail...) {
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
func TestPagingCheckWithoutHarness(t *testing.T) {
	trig := engine.DownlinkTriggerFunc(func(context.Context, netip.Addr) error { return nil })
	env := ueEnv("192.0.2.1:38412", "", "127.0.0.1")
	env.Downlink = trig
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
			r := pagingOnDLData{}.Run(ctx, tc.env)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Errorf("verdict %s (%s; %s), want %s (%s)", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
		})
	}
	if got := (pagingOnDLData{}).Budget(); got != 30*time.Second {
		t.Errorf("Budget = %s, want 30s", got)
	}
	if err := ValidateTest(pagingOnDLData{}); err != nil {
		t.Error(err)
	}
	if WorstVerdict(pagingOnDLData{}) != Fail || MinRelease(pagingOnDLData{}) != 15 {
		t.Errorf("worst verdict %s, min release %s; want FAIL and Rel-15", WorstVerdict(pagingOnDLData{}), MinRelease(pagingOnDLData{}))
	}
}

// n6Trigger is a DownlinkTrigger that hands the stub UPF one packet for the
// UE's address, as a host on N6 would send it, once the stub buffers for
// the session (the mock AMF handles the release's completion on its own
// goroutine).
func n6Trigger(upf *mockupf.UPF) engine.DownlinkTrigger {
	return engine.DownlinkTriggerFunc(func(_ context.Context, ueIP netip.Addr) error {
		upf.WaitFor(2*time.Second, func(u *mockupf.UPF) bool { _, b := u.Buffered("208930100000011", 1); return b })
		pkt, err := datapath.BuildICMPEchoRequest(net.IPv4(192, 0, 2, 9), net.IP(ueIP.AsSlice()), 0x1d1e, 1, []byte("wake"))
		if err != nil {
			return err
		}
		return upf.DownlinkFromN6(pkt)
	})
}

// Every verdict over the real-SCTP mock pair: the core pages (PASS), keeps
// the notification to itself (FAIL), pages another identity (FAIL), never
// releases the UE or gave it no 5G-GUTI (ERROR), the trigger fails (ERROR),
// and the per-test timeout ends the wait (ERROR). Each run leaves no
// association open and the UE deregistered.
func TestPagingCheckAgainstMockPair(t *testing.T) {
	tests := []struct {
		name         string
		net24        int
		amfOpts      []mockamf.Option
		trigger      engine.DownlinkTrigger // nil: the stub UPF's N6
		timeout      time.Duration
		want         Verdict
		wantObserved string
		wantDetail   []string
		wantPagings  int64
		wantDeregs   int64
	}{
		{name: "paging", net24: testnet.ConformancePagingPass, amfOpts: []mockamf.Option{mockamf.WithGUTI()},
			want: Pass, wantObserved: "PAGING with the UE's 5G-S-TMSI", wantPagings: 1, wantDeregs: 1,
			wantDetail: []string{"holds the serving TAI 208/93 TAC 1", "AN release cause nas/0", "Service Request after the PAGING accepted, 1 session(s) reactivated (recorded only)"}},
		{name: "paging withheld", net24: testnet.ConformancePagingWithheld, amfOpts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithNoPaging()},
			want: Fail, wantObserved: "no PAGING", wantDeregs: 1,
			wantDetail: []string{"no NGAP PAGING within 300ms of downlink data to a CM-IDLE UE. TS 24.501 5.6.2.2.1", "Likely cause: UPF/SMF downlink data notification or AMF N1N2MessageTransfer paging path not implemented."}},
		{name: "another identity", net24: testnet.ConformancePagingWrongID, amfOpts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithWrongPagingIdentity()},
			want: Fail, wantObserved: "PAGING for another identity only", wantPagings: 1, wantDeregs: 1,
			wantDetail: []string{"1 PAGING message(s) named another UE Paging Identity"}},
		{name: "release request ignored", net24: testnet.ConformancePagingNoRelease, amfOpts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithIgnoreUEContextReleaseRequest()},
			timeout: 3 * time.Second, want: Blocked, wantObserved: "the AN release to CM-IDLE did not complete",
			wantDetail: []string{"no UE Context Release Command"}, wantDeregs: 1},
		{name: "no 5G-GUTI", net24: testnet.ConformancePagingNoGUTI,
			want: Blocked, wantObserved: "the AN release to CM-IDLE did not complete", wantDetail: []string{"no 5G-GUTI"}},
		{name: "trigger fails", net24: testnet.ConformancePagingNoTrigger, amfOpts: []mockamf.Option{mockamf.WithGUTI()},
			trigger: engine.DownlinkTriggerFunc(func(context.Context, netip.Addr) error { return errors.New("responder unreachable") }),
			want:    Error, wantObserved: "the downlink trigger failed", wantDetail: []string{"responder unreachable"}, wantDeregs: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCore(t, tc.net24, mockupf.Options{}, testKi, tc.amfOpts...)
			c.upf.OnDownlinkData(c.amf.DownlinkData)
			env := ueEnv(c.front.Addr, "", c.n3)
			env.Downlink = tc.trigger
			if env.Downlink == nil {
				env.Downlink = n6Trigger(c.upf)
			}
			timeout := tc.timeout
			if timeout == 0 {
				timeout = 20 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			r := pagingOnDLData{window: 300 * time.Millisecond, settle: 10 * time.Millisecond}.Run(ctx, env)
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
			if got := c.amf.Pagings(); got != tc.wantPagings {
				t.Errorf("the mock AMF paged %d times, want %d", got, tc.wantPagings)
			}
			if got := c.amf.Deregistrations(); got != tc.wantDeregs {
				t.Errorf("the mock AMF saw %d deregistrations, want %d", got, tc.wantDeregs)
			}
			c.assertClosed(t, env)
		})
	}
}

// A per-test timeout that ends during the paging window is an ERROR that
// names the budget, never a FAIL.
func TestPagingCheckCutShort(t *testing.T) {
	c := newMockCore(t, testnet.ConformancePagingCutShort, mockupf.Options{}, testKi, mockamf.WithGUTI(), mockamf.WithNoPaging())
	c.upf.OnDownlinkData(c.amf.DownlinkData)
	env := ueEnv(c.front.Addr, "", c.n3)
	env.Downlink = n6Trigger(c.upf)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := pagingOnDLData{window: time.Minute, settle: 10 * time.Millisecond}.Run(ctx, env)
	if r.Verdict != Error || r.Observed != "the wait for PAGING was cut short" || !strings.Contains(r.Detail, "--per-test-timeout") {
		t.Errorf("verdict %s (%s; %s), want the cut-short ERROR", r.Verdict, r.Observed, r.Detail)
	}
	c.assertClosed(t, env)
}
