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

// classifyT3550 passes exactly four retransmissions and fails none, fewer
// or more (a fifth: no abort); an Accept without a 5G-GUTI SKIPs; a reject
// or no Accept is ERROR; a release is recorded, never asserted.
func TestClassifyT3550(t *testing.T) {
	const T = 6 * time.Second
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	accepts := func(n int) []time.Time {
		out := make([]time.Time, n)
		for i := range out {
			out[i] = t0.Add(time.Duration(i) * T)
		}
		return out
	}
	tests := []struct {
		name         string
		snap         retransmitSnapshot
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "four retransmissions", snap: retransmitSnapshot{times: accepts(5), windowClosed: true}, want: Pass,
			wantObserved: "4 retransmissions of Registration Accept", wantDetail: []string{"gaps 6.0/6.0/6.0/6.0 s (estimated timer 6.0 s)"}},
		{name: "release recorded only", snap: retransmitSnapshot{times: accepts(5), release: accepts(6)[5]}, want: Pass,
			wantDetail: []string{"UE Context Release Command 6.0 s after the last Registration Accept (recorded only: 5.5.1.2.8 c) requires no release)"}},
		{name: "no retransmission", snap: retransmitSnapshot{times: accepts(1), windowClosed: true}, want: Fail,
			wantObserved: "no retransmission of Registration Accept",
			wantDetail:   []string{"AMF did not retransmit Registration Accept on T3550 expiry (observed 0 retransmissions, want 4). TS 24.501 5.5.1.2.8 c). Likely cause: T3550 not implemented.", "one Registration Accept"}},
		{name: "two retransmissions", snap: retransmitSnapshot{times: accepts(3), windowClosed: true}, want: Fail,
			wantObserved: "2 retransmissions of Registration Accept, want 4"},
		{name: "a fifth retransmission", snap: retransmitSnapshot{times: accepts(6), windowClosed: true}, want: Fail,
			wantObserved: "5 retransmissions of Registration Accept, want 4",
			wantDetail:   []string{"after the fifth T3550 expiry", "the procedure is aborted on the fifth expiry"}},
		{name: "no 5G-GUTI", snap: retransmitSnapshot{times: accepts(1), refused: true}, want: Skip,
			wantObserved: "Registration Accept carried no 5G-GUTI; T3550 not started",
			wantDetail:   []string{"other T3550 triggers (SOR, CAG, NSSAI ...) not evaluated"}},
		{name: "registration reject", snap: retransmitSnapshot{reject: "Registration Reject"}, want: Error,
			wantObserved: "Registration Reject before the T3550 window"},
		{name: "no accept", want: Error, wantObserved: "registration did not reach Registration Accept",
			wantDetail: []string{"no Registration Accept within 10s of the Initial UE Message"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := classifyT3550(tc.snap, T, authRequestWait)
			if v != tc.want || (tc.wantObserved != "" && observed != tc.wantObserved) {
				t.Errorf("= %s %q, want %s %q", v, observed, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
		})
	}
}

// The budget is the wait for the first Accept plus five waits of 1.5 × the
// 18 s maximum: 145 s.
func TestT3550Budget(t *testing.T) {
	if got := (nasT3550AcceptRetransmit{}).Budget(); got != 145*time.Second {
		t.Errorf("Budget = %v, want 145s", got)
	}
}

// Every verdict branch against the mock AMF over a real SCTP listener with
// a 50 ms nominal T3550 and a 150 ms maximum (6 s and 18 s scaled): four
// retransmissions PASS, at the default and at 10 s and 18 s (scaled), and a
// first retransmission after 1.5 × the maximum is none; none, two and five FAIL; an
// Accept without a 5G-GUTI and no UE SKIP; no Accept and a reject are
// ERROR; a core that dies at the abort is the crash FAIL. Associations
// close, and no detail carries key material.
func TestT3550CheckAgainstMock(t *testing.T) {
	// Scaled from 6 s and 18 s, keeping their ratio.
	const T, Tmax = 50 * time.Millisecond, 150 * time.Millisecond
	guti := mockamf.WithGUTI()
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noUE         bool
		want         Verdict
		wantObserved string
		dead         bool
	}{
		{name: "pass", opts: []mockamf.Option{guti, mockamf.WithT3550(T, 4)}, want: Pass, wantObserved: "4 retransmissions of Registration Accept"},
		{name: "no retransmission", opts: []mockamf.Option{guti}, want: Fail, wantObserved: "no retransmission of Registration Accept"},
		{name: "two retransmissions", opts: []mockamf.Option{guti, mockamf.WithT3550(T, 2)}, want: Fail,
			wantObserved: "2 retransmissions of Registration Accept, want 4"},
		{name: "five retransmissions", opts: []mockamf.Option{guti, mockamf.WithT3550(T, 5)}, want: Fail,
			wantObserved: "5 retransmissions of Registration Accept, want 4"},
		{name: "a 10 s timer (scaled)", opts: []mockamf.Option{guti, mockamf.WithT3550(T*10/6, 4)}, want: Pass,
			wantObserved: "4 retransmissions of Registration Accept"},
		{name: "the 18 s WB-N1/CE timer (scaled)", opts: []mockamf.Option{guti, mockamf.WithT3550(Tmax, 4)}, want: Pass,
			wantObserved: "4 retransmissions of Registration Accept"},
		{name: "first retransmission after 1.5 Tmax", opts: []mockamf.Option{guti, mockamf.WithT3550(Tmax*16/10, 4)}, want: Fail,
			wantObserved: "no retransmission of Registration Accept"},
		{name: "no 5G-GUTI", opts: []mockamf.Option{mockamf.WithT3550(T, 4)}, want: Skip,
			wantObserved: "Registration Accept carried no 5G-GUTI; T3550 not started"},
		{name: "no registration accept", opts: []mockamf.Option{mockamf.WithNoAuthenticationRequest()}, want: Error,
			wantObserved: "registration did not reach Registration Accept"},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error,
			wantObserved: "Authentication Reject before the T3550 window"},
		{name: "core dies at the abort", opts: []mockamf.Option{guti, mockamf.WithT3550(T, 4), mockamf.WithCrashAfterT3550Abort()},
			want: Fail, wantObserved: ObservedAMFUnresponsive + " (4 retransmissions of Registration Accept)", dead: true},
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
			r := runOne(ctx, nasT3550AcceptRetransmit{t3550: T, tmax: Tmax, acceptWait: 100 * time.Millisecond}, env, 15*time.Second)
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
