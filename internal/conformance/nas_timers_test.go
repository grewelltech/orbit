package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// classifyT3560 passes four retransmissions then a release within 1.5 T3560
// of the fourth; it fails naming the first part that missed, with both parts
// in the detail; a reject or no request at all is ERROR; and the spacing is
// recorded, with a note when it is far from the nominal timer.
func TestClassifyT3560(t *testing.T) {
	const T = 6 * time.Second
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	reqs := func(n int, gap time.Duration) []time.Time {
		out := make([]time.Time, n)
		for i := range out {
			out[i] = t0.Add(time.Duration(i) * gap)
		}
		return out
	}
	after := func(r []time.Time, d time.Duration) time.Time { return r[len(r)-1].Add(d) }
	five := reqs(5, T)
	tests := []struct {
		name         string
		snap         t3560Snapshot
		want         Verdict
		wantObserved string
		wantDetail   []string
		notDetail    []string
	}{
		{name: "four retransmissions then release", snap: t3560Snapshot{requests: five, release: after(five, T)},
			want: Pass, wantObserved: "4 retransmissions of Authentication Request, then UE Context Release Command",
			wantDetail: []string{"gaps 6.0/6.0/6.0/6.0 s", "UE Context Release Command 6.0 s after the last request"},
			notDetail:  []string{"timer differs", "did not"}},
		{name: "release at the bound", snap: t3560Snapshot{requests: five, release: after(five, T+T/2)}, want: Pass,
			wantObserved: "4 retransmissions of Authentication Request, then UE Context Release Command"},
		{name: "no retransmission", snap: t3560Snapshot{requests: reqs(1, T), windowClosed: true}, want: Fail,
			wantObserved: "no retransmission of Authentication Request",
			wantDetail: []string{"observed 0 retransmissions, want 4", "Likely cause: T3560 not implemented in the AMF",
				"no UE Context Release Command within 3s, 0.5 x T3560, after the inferred fifth expiry", "one Authentication Request"}},
		{name: "two retransmissions", snap: t3560Snapshot{requests: reqs(3, T), release: after(reqs(3, T), T)}, want: Fail,
			wantObserved: "2 retransmissions of Authentication Request, want 4",
			wantDetail:   []string{"observed 2 retransmissions, want 4"}, notDetail: []string{"did not release"}},
		{name: "five retransmissions", snap: t3560Snapshot{requests: reqs(6, T), release: after(reqs(6, T), T)}, want: Fail,
			wantObserved: "5 retransmissions of Authentication Request, want 4"},
		{name: "no release", snap: t3560Snapshot{requests: five, windowClosed: true}, want: Fail,
			wantObserved: "no UE Context Release Command after the fifth T3560 expiry",
			wantDetail:   []string{"AMF did not release the N1 NAS signalling connection on the fifth T3560 expiry", "Likely cause: the AMF aborts locally without releasing"},
			notDetail:    []string{"did not retransmit"}},
		{name: "release too late", snap: t3560Snapshot{requests: five, release: after(five, 2*T)}, want: Fail,
			wantObserved: "no UE Context Release Command after the fifth T3560 expiry",
			wantDetail:   []string{"UE Context Release Command 12.0 s after the last request"}},
		{name: "timer differs from the default", snap: t3560Snapshot{requests: reqs(5, 20*time.Second), release: after(reqs(5, 20*time.Second), T)},
			want: Pass, wantDetail: []string{"gaps 20.0/20.0/20.0/20.0 s", "timer differs from the 6s default (TS 24.501 Table 10.2.2)"}},
		{name: "authentication reject", snap: t3560Snapshot{requests: reqs(1, T), reject: "Authentication Reject"}, want: Error,
			wantObserved: "Authentication Reject before the T3560 window"},
		{name: "no authentication request", snap: t3560Snapshot{}, want: Error,
			wantObserved: "registration did not reach Authentication Request", wantDetail: []string{"within 10s of the Initial UE Message"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := classifyT3560(tc.snap, T, authRequestWait)
			if v != tc.want || (tc.wantObserved != "" && observed != tc.wantObserved) {
				t.Errorf("= %s %q, want %s %q", v, observed, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
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

// The check's budget covers its window (nine T3560) plus the wait for the
// first request, 64 s at the default timer.
func TestT3560Budget(t *testing.T) {
	if got := (nasT3560AuthRetransmit{}).Budget(); got != 64*time.Second {
		t.Errorf("Budget = %v, want 64s", got)
	}
}

// Every verdict branch against the mock AMF over a real SCTP listener, with
// a 50 ms T3560: the conforming timer PASSes; no timer, a wrong count and no
// release FAIL; an Authentication Reject is ERROR; a core that goes silent at
// the release is the crash FAIL; no UE SKIPs. The association closes after
// each run, and no detail carries key material.
func TestT3560CheckAgainstMock(t *testing.T) {
	const T = 50 * time.Millisecond
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noUE         bool
		want         Verdict
		wantObserved string
		dead         bool
	}{
		{name: "pass", opts: []mockamf.Option{mockamf.WithT3560(T, 4)}, want: Pass,
			wantObserved: "4 retransmissions of Authentication Request, then UE Context Release Command"},
		{name: "no retransmission", want: Fail, wantObserved: "no retransmission of Authentication Request"},
		{name: "two retransmissions", opts: []mockamf.Option{mockamf.WithT3560(T, 2)}, want: Fail,
			wantObserved: "2 retransmissions of Authentication Request, want 4"},
		{name: "five retransmissions", opts: []mockamf.Option{mockamf.WithT3560(T, 5)}, want: Fail,
			wantObserved: "5 retransmissions of Authentication Request, want 4"},
		{name: "no release", opts: []mockamf.Option{mockamf.WithT3560(T, 4), mockamf.WithT3560NoRelease()}, want: Fail,
			wantObserved: "no UE Context Release Command after the fifth T3560 expiry"},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error,
			wantObserved: "Authentication Reject before the T3560 window"},
		{name: "no authentication request", opts: []mockamf.Option{mockamf.WithNoAuthenticationRequest()}, want: Error,
			wantObserved: "registration did not reach Authentication Request"},
		{name: "core dies at the release", opts: []mockamf.Option{mockamf.WithT3560(T, 4), mockamf.WithCrashAfterRelease()},
			want: Fail, wantObserved: ObservedAMFUnresponsive + " (4 retransmissions of Authentication Request, then UE Context Release Command)", dead: true},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, _ := auth.ParseHexKey("Ki", testKi)
			opc, _ := auth.ParseHexKey("OPc", testOPc)
			amf, err := mockamf.New(mockamf.Config{
				Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			front := sctpfront.Start(t, amf)
			env := ueEnv(front.Addr, "", "127.0.0.1")
			if tc.noUE {
				env.UE = UESpec{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r := runOne(ctx, nasT3560AuthRetransmit{t3560: T, authWait: 100 * time.Millisecond}, env, 15*time.Second)
			if tc.name == "no authentication request" && !strings.Contains(r.Detail, "no Authentication Request within 100ms of the Initial UE Message") {
				t.Errorf("detail %q does not name the 100ms wait", r.Detail)
			}
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			deadline := time.Now().Add(3 * time.Second)
			for front.Active() != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if !tc.dead && front.Active() != 0 {
				t.Errorf("%d associations still open after the check", front.Active())
			}
		})
	}
}

// A context that ends before the window closes is ERROR, never a verdict on
// the core; and the associations still close.
func TestT3560CheckTimeoutBeforeWindow(t *testing.T) {
	k, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	amf, err := mockamf.New(mockamf.Config{
		Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	}, mockamf.WithT3560(100*time.Millisecond, 4))
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	r := nasT3560AuthRetransmit{t3560: 100 * time.Millisecond, authWait: 100 * time.Millisecond}.Run(ctx, ueEnv(front.Addr, "", "127.0.0.1"))
	if r.Verdict != Error || r.Observed != "timed out before the T3560 window closed" || !strings.Contains(r.Detail, "raise --per-test-timeout to at least 1s") {
		t.Errorf("= %s %q %q, want the timed-out ERROR", r.Verdict, r.Observed, r.Detail)
	}
	deadline := time.Now().Add(3 * time.Second)
	for front.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := front.Active(); n != 0 {
		t.Errorf("%d associations still open", n)
	}
}
