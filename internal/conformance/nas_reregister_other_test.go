package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/mockamf"
)

// judgeRegOther: one registration without a NAS answer is FAIL whatever
// the others did; with none, a missing release is BLOCKED on the release
// after switch-off, a second gNB not set up BLOCKED on the NG Setup (ERROR
// when the fault is local), a registration the core did not complete
// BLOCKED on the registration, and any other registration that could not
// be sent ERROR; every one answered is PASS.
func TestJudgeRegOther(t *testing.T) {
	at := func(a regAttempt) otherAttempt {
		return otherAttempt{regAttempt: a, releaseAfter: 3 * time.Millisecond}
	}
	answered := at(regAttempt{gap: 0, measured: 80 * time.Microsecond, answer: "AUTHENTICATION REQUEST", after: 2 * time.Millisecond, registered: true})
	rejected := at(regAttempt{gap: 10 * time.Millisecond, measured: 10 * time.Millisecond, answer: "REGISTRATION REJECT", after: time.Millisecond, rejected: true, ended: "the registration was rejected"})
	silent := at(regAttempt{gap: 0, measured: 90 * time.Microsecond, ended: "none within 10s"})
	noRelease := otherAttempt{regAttempt: regAttempt{gap: 0, notRun: "the core sent no UE Context Release within 5s", noRelease: true}}
	ngErr := engine.ErrNGSetup
	noSetup := otherAttempt{regAttempt: regAttempt{gap: 10 * time.Millisecond, notRun: "gNB 0x8100: NG Setup rejected"}, ngSetupErr: ngErr}
	localSetup := otherAttempt{regAttempt: regAttempt{gap: 10 * time.Millisecond, notRun: "gNB 0x8100: SCTP dial"}, ngSetupErr: context.Canceled}
	notRun := otherAttempt{regAttempt: regAttempt{gap: 20 * time.Millisecond, notRun: "the check's time ran out before the UE could register again"}}
	blocked := otherAttempt{regAttempt: regAttempt{gap: 20 * time.Millisecond, notRun: "a registration of the same SUPI 1s later did not complete (rejected)", blocked: true}}
	tests := []struct {
		name         string
		attempts     []otherAttempt
		want         Verdict
		wantOutcome  string
		wantObserved string
		wantOn       Procedure
		wantDetail   []string
	}{
		{name: "every registration answered", attempts: []otherAttempt{answered, rejected}, want: Pass, wantOutcome: "answered", wantObserved: regOtherAnswered,
			wantDetail: []string{"gap 0s (REGISTRATION REQUEST written 80µs after the UE CONTEXT RELEASE COMPLETE): AUTHENTICATION REQUEST after 2ms, registration completed [the release came 3ms after the DEREGISTRATION REQUEST]",
				"REGISTRATION REJECT after 1ms"}},
		{name: "one unanswered", attempts: []otherAttempt{answered, silent}, want: Fail, wantOutcome: "unanswered", wantObserved: regOtherUnanswered,
			wantDetail: []string{regOtherFailure, "1 of 2 registrations drew no NAS message within 10s", "no NAS answer; none within 10s"}},
		{name: "unanswered outweighs a missing release", attempts: []otherAttempt{silent, noRelease}, want: Fail, wantOutcome: "unanswered", wantObserved: regOtherUnanswered},
		{name: "unanswered outweighs a second gNB not set up", attempts: []otherAttempt{silent, noSetup}, want: Fail, wantOutcome: "unanswered", wantObserved: regOtherUnanswered},
		{name: "no release", attempts: []otherAttempt{answered, noRelease}, want: Blocked, wantOutcome: blockedID(ProcSwitchOffRelease), wantObserved: regOtherNoRelease,
			wantOn: ProcSwitchOffRelease, wantDetail: []string{"1 of 2 registrations answered"}},
		{name: "second gNB not set up", attempts: []otherAttempt{answered, noSetup}, want: Blocked, wantOutcome: blockedID(ProcNGSetup), wantObserved: regOtherNoNGSetup,
			wantOn: ProcNGSetup, wantDetail: []string{"1 of 2 registrations answered", "gap 10ms: not run: gNB 0x8100: NG Setup rejected"}},
		{name: "second gNB not set up for a local fault", attempts: []otherAttempt{localSetup}, want: Error, wantOutcome: OutcomeError, wantObserved: regOtherNoNGSetup},
		{name: "a registration not completed", attempts: []otherAttempt{answered, blocked}, want: Blocked, wantOutcome: blockedID(ProcRegistration),
			wantObserved: "a registration of the same SUPI did not complete", wantOn: ProcRegistration},
		{name: "not run", attempts: []otherAttempt{answered, notRun}, want: Error, wantOutcome: OutcomeError, wantObserved: "a registration after the release could not be sent"},
		{name: "nothing to try", want: Error, wantOutcome: OutcomeError, wantObserved: "no registration attempted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := judgeRegOther(Result{}, tc.attempts, 10*time.Second)
			if r.Verdict != tc.want || r.Outcome != tc.wantOutcome || r.Observed != tc.wantObserved {
				t.Fatalf("= %s %s %q (%s), want %s %s %q", r.Verdict, r.Outcome, r.Observed, r.Detail, tc.want, tc.wantOutcome, tc.wantObserved)
			}
			if r.BlockedOn != tc.wantOn {
				t.Errorf("blocked on %q, want %q", r.BlockedOn, tc.wantOn)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
		})
	}
	if !errors.Is(noSetup.ngSetupErr, engine.ErrNGSetup) {
		t.Fatal("the NG Setup fixture is not ErrNGSetup")
	}
}

// NAS-REGISTRATION-DURING-OTHER-RELEASE against the mock AMF over real
// SCTP. A mock that answers every registration PASSes, also when it drops
// a registration of the released SUPI only; one that drops any
// registration arriving within its window of a release FAILs, whether the
// dropped one is the first or the last and whether or not that subscriber
// registers again afterwards; one that sends no release is BLOCKED on the
// release; one that refuses the second gNB is BLOCKED on the NG Setup; one
// that dies at the dropped registration is CRASH; a first registration
// that fails is BLOCKED on the registration; no UE SKIPs. The two
// subscribers take turns, the registered UE is deregistered at the end,
// and no association stays open.
func TestRegistrationDuringOtherReleaseAgainstMock(t *testing.T) {
	const window = 300 * time.Millisecond
	late := 900 * time.Millisecond // a gap outside the mock's window
	drop := mockamf.WithDropRegistrationAfterAnyRelease(window)
	const supiA, supiB = "208930100000011", "208930100000012"
	tests := []struct {
		name         string
		opts         []mockamf.Option
		gaps         []time.Duration
		noUE         bool
		oneSUPI      bool
		sameGNBID    bool
		want         Verdict
		wantOutcome  string
		wantObserved string
		wantDetail   []string
		wantRegs     []string // SUPIs whose Registration Requests the mock answered, in order
		wantDropped  int64
		wantDeregs   int64
		dead         bool
	}{
		{name: "answered at every gap", gaps: []time.Duration{0, 10 * time.Millisecond, 20 * time.Millisecond}, want: Pass, wantOutcome: "answered",
			wantObserved: regOtherAnswered,
			wantDetail: []string{"gap 0s (REGISTRATION REQUEST written", "gap 10ms", "gap 20ms", "AUTHENTICATION REQUEST after", "registration completed",
				"after the DEREGISTRATION REQUEST]", "state left: the UE was deregistered (switch-off): the core released the UE context"},
			wantRegs: []string{supiA, supiB, supiA, supiB}, wantDeregs: 4},
		{name: "a drop of the released SUPI only is not this check's", opts: []mockamf.Option{mockamf.WithDropRegistrationAfterRelease(time.Hour)},
			gaps: []time.Duration{0}, want: Pass, wantOutcome: "answered", wantObserved: regOtherAnswered,
			wantRegs: []string{supiA, supiB}, wantDeregs: 2},
		{name: "the first registration dropped", opts: []mockamf.Option{drop}, gaps: []time.Duration{0, late}, want: Fail, wantOutcome: "unanswered",
			wantObserved: regOtherUnanswered,
			wantDetail: []string{regOtherFailure, "1 of 2 registrations drew no NAS message within 400ms",
				"no NAS answer; none within 400ms; the other UE's context was released and this request drew nothing",
				"gap 900ms", "registration completed", "state left: the UE was deregistered (switch-off)"},
			// B dropped, B registered afresh, then A registered at the late gap.
			wantRegs: []string{supiA, supiB, supiA}, wantDropped: 1, wantDeregs: 3},
		{name: "the last registration dropped", opts: []mockamf.Option{drop}, gaps: []time.Duration{late, 0}, want: Fail, wantOutcome: "unanswered",
			wantObserved: regOtherUnanswered,
			wantDetail: []string{"1 of 2 registrations drew no NAS message",
				"state left: a registration of the same SUPI 400ms after the last one completed; the UE was deregistered (switch-off)"},
			wantRegs: []string{supiA, supiB, supiA}, wantDropped: 1, wantDeregs: 3},
		{name: "every registration dropped", opts: []mockamf.Option{mockamf.WithDropRegistrationAfterAnyRelease(time.Hour)}, gaps: []time.Duration{0, 10 * time.Millisecond},
			want: Fail, wantOutcome: "unanswered", wantObserved: regOtherUnanswered,
			wantDetail: []string{"1 of 2 registrations drew no NAS message", "gap 10ms: not run: a registration of the same SUPI 400ms later did not complete",
				"state left: the associations were closed with no UE registered"},
			wantRegs: []string{supiA}, wantDropped: 2, wantDeregs: 1},
		{name: "one SUPI in the pool", oneSUPI: true, gaps: []time.Duration{0}, want: Pass, wantOutcome: "answered", wantObserved: regOtherAnswered,
			wantDetail: []string{"the subscriber pool has one SUPI, so the other UE was the same subscriber on its own association"},
			wantRegs:   []string{supiA, supiA}, wantDeregs: 2},
		{name: "no release", opts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()}, gaps: []time.Duration{0, 10 * time.Millisecond}, want: Blocked,
			wantOutcome: blockedID(ProcSwitchOffRelease), wantObserved: regOtherNoRelease,
			wantDetail: []string{"0 of 2 registrations answered", "gap 10ms: not run: not tried after a deregistration without a release", "state left: the associations were closed with no UE registered"},
			wantRegs:   []string{supiA}, wantDeregs: 1},
		{name: "second gNB refused", opts: []mockamf.Option{mockamf.WithDuplicateRANNodeID(mockamf.RejectDuplicateRANNode)}, sameGNBID: true,
			gaps: []time.Duration{0, 10 * time.Millisecond}, want: Blocked, wantOutcome: blockedID(ProcNGSetup), wantObserved: regOtherNoNGSetup,
			wantDetail: []string{"0 of 2 registrations answered", "NG Setup rejected", "gap 10ms: not run: not tried after the second gNB's NG Setup did not complete",
				"state left: the UE was deregistered (switch-off)"},
			wantRegs: []string{supiA}, wantDeregs: 1},
		{name: "core dies at the registration", opts: []mockamf.Option{mockamf.WithDropRegistrationAfterAnyRelease(time.Hour), mockamf.WithCrashOnDroppedRegistration()},
			gaps: []time.Duration{0}, want: Crash, wantOutcome: OutcomeCrash, wantObserved: ObservedAMFUnresponsive + " (" + regOtherUnanswered + ")",
			wantRegs: []string{supiA}, wantDropped: 1, wantDeregs: 1, dead: true},
		{name: "first registration fails", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, gaps: []time.Duration{0}, want: Blocked,
			wantOutcome: blockedID(ProcRegistration), wantObserved: "registration failed",
			wantDetail: []string{"the core did not complete a registration, which this check needs first"}, wantRegs: []string{supiA}},
		{name: "no UE", noUE: true, want: Skip, wantOutcome: skipInputID(InputUE), wantObserved: "no UE configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, front := initialNASMock(t, tc.opts...)
			env := ueEnv(front.Addr, "", "127.0.0.1")
			env.UE.Subscribers = 2
			if tc.oneSUPI {
				env.UE.Subscribers = 1
			}
			if tc.sameGNBID {
				env.TargetGNBID = env.GNB.ID
			}
			if tc.noUE {
				env.UE = UESpec{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			c := nasRegistrationDuringOtherRelease{registerWait: 2 * time.Second, wait: 400 * time.Millisecond, settle: 400 * time.Millisecond,
				linger: 300 * time.Millisecond, gaps: tc.gaps}
			r := runOne(ctx, c, env, 50*time.Second)
			if r.Verdict != tc.want || r.Outcome != tc.wantOutcome || r.Observed != tc.wantObserved {
				t.Fatalf("= %s %s %q (%s), want %s %s %q", r.Verdict, r.Outcome, r.Observed, r.Detail, tc.want, tc.wantOutcome, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if got := amf.RegistrationSUPIs(); strings.Join(got, ",") != strings.Join(tc.wantRegs, ",") {
				t.Errorf("mock answered Registration Requests of %v, want %v", got, tc.wantRegs)
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

// The check is FAIL-class on the two shalls of its sibling, runs in the
// main phase with one UE at a time, two subscribers and a second gNB, and
// its budget covers six attempts.
func TestRegistrationDuringOtherReleaseClass(t *testing.T) {
	c := nasRegistrationDuringOtherRelease{}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	n := NeedsOf(c)
	if VerdictClass(c) != Fail || PhaseOf(c) != PhaseMain || n.UEs != 1 || subscribersOf(c) != 2 || MinRelease(c) != 15 ||
		!n.needs(ProcRegistration) || !n.needs(ProcSwitchOffRelease) || !n.needs(ProcNGSetup) {
		t.Errorf("class %s, phase %s, needs %+v, min release %s", VerdictClass(c), PhaseOf(c), n, MinRelease(c))
	}
	hasRole := map[GNBRole]bool{}
	for _, role := range n.GNBRoles {
		hasRole[role] = true
	}
	if !hasRole[RoleOwn] || !hasRole[RoleHandover] || !hasRole[RoleLiveness] {
		t.Errorf("gNB roles %v, want own, a second and the liveness probe", n.GNBRoles)
	}
	// 10 s to register; six attempts of 4 s + 5 s + 10 s + 1 s + 10 s plus
	// their gaps (150 ms in all); 5 s to deregister and 5 s for the
	// liveness probe.
	if got, want := c.Budget(), 200*time.Second; got != want {
		t.Errorf("budget %s, want %s", got, want)
	}
	if len(regOtherGaps) != 6 || regOtherGaps[0] != 0 {
		t.Fatalf("gaps %v, want six starting with none", regOtherGaps)
	}
	for i := 1; i < len(regOtherGaps); i++ {
		if step := regOtherGaps[i] - regOtherGaps[i-1]; step != 10*time.Millisecond {
			t.Errorf("gap %d is %s after the one before, want an even 10ms", i, step)
		}
	}
}
