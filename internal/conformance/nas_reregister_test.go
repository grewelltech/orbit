package conformance

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/mockamf"
)

// judgeRegAfterRelease: one registration without a NAS answer is FAIL
// whatever the others did; with none, a missing release is INFO, a
// registration of the SUPI the core did not complete BLOCKED on the
// registration, and any other registration that could not be sent ERROR;
// every one answered, with an Accept, a Reject or a registration that then
// stalled, is PASS.
func TestJudgeRegAfterRelease(t *testing.T) {
	answered := regAttempt{gap: 0, measured: 80 * time.Microsecond, answer: "AUTHENTICATION REQUEST", after: 2 * time.Millisecond, registered: true}
	rejected := regAttempt{gap: 5 * time.Millisecond, measured: 5 * time.Millisecond, answer: "REGISTRATION REJECT", after: time.Millisecond, rejected: true, ended: "the registration was rejected"}
	stalled := regAttempt{gap: 5 * time.Millisecond, measured: 5 * time.Millisecond, answer: "AUTHENTICATION REQUEST", after: time.Millisecond, ended: "the registration did not complete within 10s"}
	silent := regAttempt{gap: 0, measured: 90 * time.Microsecond, ended: "none within 10s"}
	releasedOnly := regAttempt{gap: 0, measured: 90 * time.Microsecond, other: "received instead: UEContextRelease", ended: "the core released the new UE context"}
	noRelease := regAttempt{gap: 0, notRun: "the core sent no UE Context Release within 5s", noRelease: true}
	notRun := regAttempt{gap: 20 * time.Millisecond, notRun: "the check's time ran out before the UE could register again"}
	blocked := regAttempt{gap: 20 * time.Millisecond, notRun: "a registration of the same SUPI 1s later did not complete (rejected)", blocked: true}
	tests := []struct {
		name         string
		attempts     []regAttempt
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "every registration answered", attempts: []regAttempt{answered, answered, answered}, want: Pass, wantObserved: regAfterReleaseAnswered,
			wantDetail: []string{"gap 0s (REGISTRATION REQUEST written 80µs after the UE CONTEXT RELEASE COMPLETE): AUTHENTICATION REQUEST after 2ms, registration completed"}},
		{name: "a Reject is an answer", attempts: []regAttempt{answered, rejected}, want: Pass, wantObserved: regAfterReleaseAnswered,
			wantDetail: []string{"gap 5ms", "REGISTRATION REJECT after 1ms; the registration was rejected"}},
		{name: "an answered registration that stalls is answered", attempts: []regAttempt{stalled}, want: Pass, wantObserved: regAfterReleaseAnswered,
			wantDetail: []string{"the registration did not complete within 10s"}},
		{name: "one unanswered", attempts: []regAttempt{silent, answered, answered}, want: Fail, wantObserved: regAfterReleaseUnanswered,
			wantDetail: []string{regAfterReleaseFailure, "1 of 3 registrations drew no NAS message within 10s", "no NAS answer; none within 10s"}},
		{name: "a release in place of a NAS answer", attempts: []regAttempt{releasedOnly}, want: Fail, wantObserved: regAfterReleaseUnanswered,
			wantDetail: []string{"no NAS answer; received instead: UEContextRelease; the core released the new UE context"}},
		{name: "unanswered, then not run", attempts: []regAttempt{silent, blocked}, want: Fail, wantObserved: regAfterReleaseUnanswered,
			wantDetail: []string{"1 of 2 registrations", "gap 20ms: not run: a registration of the same SUPI 1s later did not complete (rejected)"}},
		{name: "unanswered outweighs a missing release", attempts: []regAttempt{silent, noRelease}, want: Fail, wantObserved: regAfterReleaseUnanswered},
		{name: "no release", attempts: []regAttempt{noRelease, noRelease}, want: Blocked, wantObserved: regAfterReleaseNoRelease,
			wantDetail: []string{"0 of 2 registrations answered", "not run: the core sent no UE Context Release within 5s"}},
		{name: "answered, then no release", attempts: []regAttempt{answered, noRelease}, want: Blocked, wantObserved: regAfterReleaseNoRelease,
			wantDetail: []string{"1 of 2 registrations answered"}},
		{name: "answered, then not run", attempts: []regAttempt{answered, notRun}, want: Error, wantObserved: "a registration after the release could not be sent"},
		{name: "answered, then the SUPI not registered again", attempts: []regAttempt{answered, blocked}, want: Blocked,
			wantObserved: "a registration of the same SUPI did not complete", wantDetail: []string{"gap 20ms: not run"}},
		{name: "nothing to try", want: Error, wantObserved: "no registration attempted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := judgeRegAfterRelease(Result{}, tc.attempts, 10*time.Second)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Fatalf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			wantOn := ProcRegistration
			if tc.wantObserved == regAfterReleaseNoRelease {
				wantOn = ProcSwitchOffRelease
			}
			if r.Verdict == Blocked && r.BlockedOn != wantOn {
				t.Errorf("blocked on %q, want %q", r.BlockedOn, wantOn)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
		})
	}
}

// nasAnswerName names the messages that can answer a registration.
func TestNASAnswerName(t *testing.T) {
	tests := []struct {
		typ  uint8
		want string
	}{
		{f5nas.MsgTypeAuthenticationRequest, "AUTHENTICATION REQUEST"},
		{f5nas.MsgTypeIdentityRequest, "IDENTITY REQUEST"},
		{f5nas.MsgTypeSecurityModeCommand, "SECURITY MODE COMMAND"},
		{f5nas.MsgTypeRegistrationAccept, "REGISTRATION ACCEPT"},
		{f5nas.MsgTypeRegistrationReject, "REGISTRATION REJECT"},
		{f5nas.MsgTypeAuthenticationReject, "AUTHENTICATION REJECT"},
		{0, "a NAS message of unreadable type"},
		{0x64, "5GMM message type 0x64"},
	}
	for _, tc := range tests {
		if got := nasAnswerName(tc.typ); got != tc.want {
			t.Errorf("nasAnswerName(%#x) = %q, want %q", tc.typ, got, tc.want)
		}
	}
}

// NAS-REGISTRATION-AFTER-RELEASE against the mock AMF over real SCTP. A
// mock that answers every registration PASSes; one that drops a
// registration arriving within its window of the release FAILs, whether
// the dropped one is the first or the last and whether or not the SUPI
// registers again afterwards; one that sends no release is INFO; one that
// dies at the dropped registration is CRASH; a first registration that
// fails is BLOCKED on the registration; no UE SKIPs. Every registration is of the one SUPI,
// the UE is deregistered at the end wherever the core still registers it,
// and no association stays open.
func TestRegistrationAfterReleaseAgainstMock(t *testing.T) {
	const window = 300 * time.Millisecond
	late := 900 * time.Millisecond // a gap outside the mock's window
	drop := mockamf.WithDropRegistrationAfterRelease(window)
	tests := []struct {
		name         string
		opts         []mockamf.Option
		gaps         []time.Duration
		noUE         bool
		want         Verdict
		wantObserved string
		wantDetail   []string
		wantRegs     int   // Registration Requests the mock answered
		wantDropped  int64 // and dropped
		wantDeregs   int64
		dead         bool
	}{
		{name: "answered at every gap", gaps: []time.Duration{0, 5 * time.Millisecond, 20 * time.Millisecond}, want: Pass,
			wantObserved: regAfterReleaseAnswered,
			wantDetail: []string{"gap 0s (REGISTRATION REQUEST written", "gap 5ms", "gap 20ms", "AUTHENTICATION REQUEST after", "registration completed",
				"state left: the UE was deregistered (switch-off): the core released the UE context"},
			wantRegs: 4, wantDeregs: 4},
		{name: "the first registration dropped", opts: []mockamf.Option{drop}, gaps: []time.Duration{0, late}, want: Fail,
			wantObserved: regAfterReleaseUnanswered,
			wantDetail: []string{regAfterReleaseFailure, "1 of 2 registrations drew no NAS message within 400ms",
				"no NAS answer; none within 400ms; the previous UE context was released and the new request drew nothing",
				"gap 900ms", "registration completed", "state left: the UE was deregistered (switch-off)"},
			wantRegs: 3, wantDropped: 1, wantDeregs: 3},
		{name: "the last registration dropped", opts: []mockamf.Option{drop}, gaps: []time.Duration{late, 0}, want: Fail,
			wantObserved: regAfterReleaseUnanswered,
			wantDetail: []string{"1 of 2 registrations drew no NAS message",
				"state left: a registration of the same SUPI 400ms after the last one completed; the UE was deregistered (switch-off)"},
			wantRegs: 3, wantDropped: 1, wantDeregs: 3},
		{name: "every registration dropped", opts: []mockamf.Option{mockamf.WithDropRegistrationAfterRelease(time.Hour)}, gaps: []time.Duration{0, 5 * time.Millisecond}, want: Fail,
			wantObserved: regAfterReleaseUnanswered,
			wantDetail: []string{"1 of 2 registrations drew no NAS message", "gap 5ms: not run: a registration of the same SUPI 400ms later did not complete",
				"state left: the association was closed with the UE not registered"},
			wantRegs: 1, wantDropped: 2, wantDeregs: 1},
		{name: "no release", opts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()}, gaps: []time.Duration{0, 5 * time.Millisecond}, want: Blocked,
			wantObserved: regAfterReleaseNoRelease,
			wantDetail:   []string{"0 of 2 registrations answered", "gap 5ms: not run: not tried after a deregistration without a release", "state left: the association was closed with the UE not registered"},
			wantRegs:     1, wantDeregs: 1},
		{name: "core dies at the registration", opts: []mockamf.Option{mockamf.WithDropRegistrationAfterRelease(time.Hour), mockamf.WithCrashOnDroppedRegistration()},
			gaps: []time.Duration{0}, want: Crash, wantObserved: ObservedAMFUnresponsive + " (" + regAfterReleaseUnanswered + ")",
			wantRegs: 1, wantDropped: 1, wantDeregs: 1, dead: true},
		{name: "first registration fails", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, gaps: []time.Duration{0}, want: Blocked,
			wantObserved: "registration failed", wantDetail: []string{"the core did not complete a registration, which this check needs first"}, wantRegs: 1},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, front := initialNASMock(t, tc.opts...)
			env := ueEnv(front.Addr, "", "127.0.0.1")
			if tc.noUE {
				env.UE = UESpec{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c := nasRegistrationAfterRelease{registerWait: 2 * time.Second, wait: 400 * time.Millisecond, settle: 400 * time.Millisecond,
				linger: 300 * time.Millisecond, gaps: tc.gaps}
			r := runOne(ctx, c, env, 25*time.Second)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Fatalf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			supis := amf.RegistrationSUPIs()
			if len(supis) != tc.wantRegs {
				t.Errorf("mock answered %d Registration Requests, want %d", len(supis), tc.wantRegs)
			}
			for _, s := range supis {
				if s != env.UE.SUPI {
					t.Errorf("registered %s, want only %s", s, env.UE.SUPI)
				}
			}
			if got := amf.DroppedRegistrations(); got != tc.wantDropped {
				t.Errorf("mock dropped %d Registration Requests, want %d", got, tc.wantDropped)
			}
			if got := amf.Deregistrations(); got != tc.wantDeregs {
				t.Errorf("mock saw %d deregistrations, want %d", got, tc.wantDeregs)
			}
			if n := amf.StrayReleaseCompletes(); n != 0 {
				t.Errorf("%d stray UE Context Release Completes", n)
			}
			if n := amf.EncodeErrors(); n != 0 {
				t.Errorf("mock AMF dropped %d PDUs that failed to encode", n)
			}
			if !tc.dead {
				awaitClosed(t, front)
			}
		})
	}
}

// The check is FAIL-class on two shalls, runs in the main phase with one
// subscriber, and its budget covers the six attempts of its sweep.
func TestRegistrationAfterReleaseClass(t *testing.T) {
	c := nasRegistrationAfterRelease{}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	n := NeedsOf(c)
	if VerdictClass(c) != Fail || PhaseOf(c) != PhaseMain || n.UEs != 1 || subscribersOf(c) != 1 || MinRelease(c) != 15 || !n.needs(ProcRegistration) {
		t.Errorf("class %s, phase %s, needs %+v, min release %s", VerdictClass(c), PhaseOf(c), n, MinRelease(c))
	}
	// 10 s to register, six attempts of 5 s + 10 s + 1 s + 10 s plus
	// their gaps, 5 s to deregister and 5 s for the liveness probe.
	if got, want := c.Budget(), 176*time.Second; got != want {
		t.Errorf("budget %s, want %s", got, want)
	}
	want := []time.Duration{0, 10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond, 40 * time.Millisecond, 50 * time.Millisecond}
	if !slices.Equal(regAfterReleaseGaps, want) {
		t.Errorf("gaps %v, want the even sweep %v", regAfterReleaseGaps, want)
	}
}

func TestSweep(t *testing.T) {
	cases := []struct {
		step, bound time.Duration
		want        []time.Duration
	}{
		{step: 10 * time.Millisecond, bound: 30 * time.Millisecond, want: []time.Duration{0, 10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}},
		{step: 20 * time.Millisecond, bound: 50 * time.Millisecond, want: []time.Duration{0, 20 * time.Millisecond, 40 * time.Millisecond}},
		{step: time.Second, bound: 0, want: []time.Duration{0}},
	}
	for _, tc := range cases {
		if got := sweep(tc.step, tc.bound); !slices.Equal(got, tc.want) {
			t.Errorf("sweep(%s, %s) = %v, want %v", tc.step, tc.bound, got, tc.want)
		}
	}
}
