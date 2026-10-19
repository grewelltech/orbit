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

// classifyT3560 passes four retransmissions then a release within 1.5 ×
// T_est of the fourth, T_est being the nominal timer or the mean gap when
// longer; it fails naming the first part that missed, with both parts in the
// detail, and says "late" rather than "aborts locally" for a release that
// came after the bound; a reject or no request at all is ERROR; the spacing
// is recorded against T_est and the 6 s default.
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
		snap         retransmitSnapshot
		want         Verdict
		wantObserved string
		wantDetail   []string
		notDetail    []string
	}{
		{name: "four retransmissions then release", snap: retransmitSnapshot{times: five, release: after(five, T)},
			want: Pass, wantObserved: "4 retransmissions of Authentication Request, then UE Context Release Command",
			wantDetail: []string{"gaps 6.0/6.0/6.0/6.0 s (estimated timer 6.0 s)", "UE Context Release Command 6.0 s after the last request"},
			notDetail:  []string{"timer differs", "did not", "uneven"}},
		{name: "release at the bound", snap: retransmitSnapshot{times: five, release: after(five, T+T/2)}, want: Pass,
			wantObserved: "4 retransmissions of Authentication Request, then UE Context Release Command"},
		{name: "a configured 10 s timer", snap: retransmitSnapshot{times: reqs(5, 10*time.Second), release: after(reqs(5, 10*time.Second), 10*time.Second)},
			want: Pass, wantDetail: []string{"estimated timer 10.0 s", "timer differs from the 6s default"}, notDetail: []string{"uneven"}},
		{name: "the 24 s WB-N1/CE timer", snap: retransmitSnapshot{times: reqs(5, 24*time.Second), release: after(reqs(5, 24*time.Second), 24*time.Second)},
			want: Pass, wantDetail: []string{"estimated timer 24.0 s"}},
		{name: "no retransmission", snap: retransmitSnapshot{times: reqs(1, T), windowClosed: true}, want: Fail,
			wantObserved: "no retransmission of Authentication Request",
			wantDetail: []string{"observed 0 retransmissions, want 4", "Likely cause: T3560 not implemented in the AMF",
				"no UE Context Release Command within 3s, 0.5 x T_est, after the inferred fifth expiry", "one Authentication Request"}},
		{name: "two retransmissions", snap: retransmitSnapshot{times: reqs(3, T), release: after(reqs(3, T), T)}, want: Fail,
			wantObserved: "2 retransmissions of Authentication Request, want 4",
			wantDetail:   []string{"observed 2 retransmissions, want 4"}, notDetail: []string{"did not release"}},
		{name: "five retransmissions", snap: retransmitSnapshot{times: reqs(6, T)}, want: Fail,
			wantObserved: "5 retransmissions of Authentication Request, want 4"},
		{name: "no release", snap: retransmitSnapshot{times: five, windowClosed: true}, want: Fail,
			wantObserved: "no UE Context Release Command after the fifth T3560 expiry",
			wantDetail:   []string{"AMF did not release the N1 NAS signalling connection on the fifth T3560 expiry", "Likely cause: the AMF aborts locally without releasing"},
			notDetail:    []string{"did not retransmit"}},
		{name: "release late", snap: retransmitSnapshot{times: five, release: after(five, 2*T)}, want: Fail,
			wantObserved: "UE Context Release Command late after the fifth T3560 expiry",
			wantDetail:   []string{"UE Context Release Command 6.0 s after the inferred fifth expiry, later than 0.5 x T_est (3.0 s)", "UE Context Release Command 12.0 s after the last request"},
			notDetail:    []string{"aborts locally"}},
		{name: "uneven gaps", snap: retransmitSnapshot{times: []time.Time{t0, t0.Add(T), t0.Add(2 * T), t0.Add(3 * T), t0.Add(6 * T)}, release: t0.Add(7 * T)},
			want: Pass, wantDetail: []string{"gaps uneven"}},
		{name: "authentication reject", snap: retransmitSnapshot{times: reqs(1, T), reject: "Authentication Reject"}, want: Error,
			wantObserved: "Authentication Reject before the T3560 window"},
		{name: "no authentication request", snap: retransmitSnapshot{}, want: Error,
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

// The check's budget is the wait for the first request plus five waits of
// 1.5 × the 24 s maximum: 190 s.
func TestT3560Budget(t *testing.T) {
	if got := (nasT3560AuthRetransmit{}).Budget(); got != 190*time.Second {
		t.Errorf("Budget = %v, want 190s", got)
	}
}

// Every verdict branch against the mock AMF over a real SCTP listener, with
// a 50 ms nominal T3560 and a 200 ms maximum (6 s and 24 s scaled, keeping
// their ratio). The conforming timer PASSes, as do timers of 8, 10, 11 and
// 24 s (scaled) and a first retransmission just inside 1.5 × the maximum.
// No timer, a first retransmission just outside that bound, a wrong count,
// no release and a release three timers late FAIL; an Authentication
// Reject or no request is ERROR; a core that dies at the release is the
// crash FAIL; no UE SKIPs. The association closes after each run, and no
// detail carries key material.
func TestT3560CheckAgainstMock(t *testing.T) {
	// Scaled from 6 s and 24 s, keeping their ratio.
	const T, Tmax = 50 * time.Millisecond, 200 * time.Millisecond
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
		{name: "an 8 s timer (scaled)", opts: []mockamf.Option{mockamf.WithT3560(T*8/6, 4)}, want: Pass,
			wantObserved: "4 retransmissions of Authentication Request, then UE Context Release Command"},
		{name: "a 10 s timer (scaled)", opts: []mockamf.Option{mockamf.WithT3560(T*10/6, 4)}, want: Pass,
			wantObserved: "4 retransmissions of Authentication Request, then UE Context Release Command"},
		{name: "an 11 s timer (scaled)", opts: []mockamf.Option{mockamf.WithT3560(T*11/6, 4)}, want: Pass,
			wantObserved: "4 retransmissions of Authentication Request, then UE Context Release Command"},
		{name: "the 24 s WB-N1/CE timer (scaled)", opts: []mockamf.Option{mockamf.WithT3560(Tmax, 4)}, want: Pass,
			wantObserved: "4 retransmissions of Authentication Request, then UE Context Release Command"},
		{name: "first retransmission inside 1.5 Tmax", opts: []mockamf.Option{mockamf.WithT3560(Tmax*14/10, 4)}, want: Pass,
			wantObserved: "4 retransmissions of Authentication Request, then UE Context Release Command"},
		{name: "first retransmission after 1.5 Tmax", opts: []mockamf.Option{mockamf.WithT3560(Tmax*16/10, 4)}, want: Fail,
			wantObserved: "no retransmission of Authentication Request"},
		{name: "late release", opts: []mockamf.Option{mockamf.WithT3560(T, 4), mockamf.WithT3560ReleaseAfter(3 * T)}, want: Fail,
			wantObserved: "UE Context Release Command late after the fifth T3560 expiry"},
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
			r := runOne(ctx, nasT3560AuthRetransmit{t3560: T, tmax: Tmax, authWait: 100 * time.Millisecond}, env, 15*time.Second)
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
	r := nasT3560AuthRetransmit{t3560: 100 * time.Millisecond, tmax: 200 * time.Millisecond, authWait: 100 * time.Millisecond}.Run(ctx, ueEnv(front.Addr, "", "127.0.0.1"))
	if r.Verdict != Error || r.Observed != "timed out before the T3560 window closed" || !strings.Contains(r.Detail, "raise --per-test-timeout to at least 1.6s") {
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
