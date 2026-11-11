package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// classifyInitialRegistration: an Accept is PASS; a Registration Reject,
// readable or not, and an Authentication Reject are WARN for availability;
// no answer and a release without one are FAIL; any other ending is WARN.
// Every outcome but the Accept reports the registration as not completed.
func TestClassifyInitialRegistration(t *testing.T) {
	tests := []struct {
		name         string
		o            initialNASOutcome
		want         Outcome
		wantObserved string
		wantDetail   string
	}{
		{name: "accepted", o: initialNASOutcome{smcs: 1, registered: true}, want: initialRegAccepted, wantObserved: "REGISTRATION ACCEPT"},
		{name: "rejected", o: initialNASOutcome{smcs: 1, reject: rejectPlain(62)}, want: initialRegRejected,
			wantObserved: "REGISTRATION REJECT, 5GMM cause #62", wantDetail: "5.5.1.2.5 lets a network that cannot accept the request reject it"},
		{name: "unreadable reject", o: initialNASOutcome{smcs: 1, rejectErr: nas.ErrIntegrity}, want: initialRegRejected,
			wantObserved: "REGISTRATION REJECT that could not be read"},
		{name: "authentication reject", o: initialNASOutcome{authReject: true}, want: initialRegAuthRejected, wantObserved: "AUTHENTICATION REJECT"},
		{name: "released", o: initialNASOutcome{released: true}, want: initialRegReleased,
			wantObserved: "UE CONTEXT RELEASE COMMAND without REGISTRATION ACCEPT or REJECT", wantDetail: "0 SECURITY MODE COMMAND(s) before it"},
		{name: "no answer", o: initialNASOutcome{smcs: 1, noAnswer: true}, want: initialRegUnanswered,
			wantObserved: "no REGISTRATION ACCEPT or REJECT", wantDetail: "within 10s (absence inferred within this bound)"},
		{name: "another ending", o: initialNASOutcome{err: errors.New("association lost")}, want: initialRegBroken,
			wantObserved: "registration did not complete", wantDetail: "association lost"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, observed, detail := classifyInitialRegistration(tc.o, initialRegWait)
			if covers(o).ID != tc.want.ID || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("= %s %q (%s), want %s %q with detail containing %q", o.ID, observed, detail, tc.want.ID, tc.wantObserved, tc.wantDetail)
			}
			if completed := o.NotCompleted == ""; completed != (o.ID == initialRegAccepted.ID) {
				t.Errorf("%s: NotCompleted = %q; only the Accept completes the registration", o.ID, o.NotCompleted)
			}
		})
	}
}

// The declaration: a PASS and two FAILs on the two shalls, WARNs for
// availability; the check owns the registration and needs only the NG
// Setup.
func TestInitialRegistrationDeclaration(t *testing.T) {
	c := nasInitialRegistration{}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	if got := OwnsOf(c); len(got) != 1 || got[0] != ProcRegistration {
		t.Errorf("owns %v, want the registration", got)
	}
	if got := NeedsOf(c).Procedures; len(got) != 1 || got[0] != ProcNGSetup || NeedsOf(c).UEs != 1 {
		t.Errorf("needs %+v, want one UE and the NG Setup", NeedsOf(c))
	}
	for _, o := range []Outcome{initialRegRejected, initialRegAuthRejected, initialRegBroken} {
		if o.Verdict != Warn || o.Criterion != CriterionAvailability {
			t.Errorf("%s: %s (%s), want WARN for availability", o.ID, o.Verdict, o.Criterion)
		}
	}
	if WorstVerdict(c) != Fail || c.Budget() != 20*time.Second {
		t.Errorf("worst %s, budget %s", WorstVerdict(c), c.Budget())
	}
}

// otherPLMNMock starts a mock AMF that serves PLMN 001/01, behind a real
// SCTP listener: it refuses the NG Setup of a gNB presenting another PLMN.
func otherPLMNMock(t *testing.T) (*mockamf.AMF, *sctpfront.Front) {
	t.Helper()
	k, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	amf, err := mockamf.New(mockamf.Config{
		Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "001", MNC: "01", SST: 1, SD: "010203",
	})
	if err != nil {
		t.Fatal(err)
	}
	return amf, sctpfront.Start(t, amf)
}

// NAS-INITIAL-REGISTRATION against the mock AMF over real SCTP, as a suite
// run scores it: an Accept PASSes and the UE is deregistered; a Reject and
// an Authentication Reject are WARN; silence and a release are FAIL; an
// NG Setup the mock refuses is BLOCKED on it; no UE SKIPs; a mock that
// dies after releasing the UE is CRASH.
func TestInitialRegistrationAgainstMock(t *testing.T) {
	nia1Only := func(env *Env) { env.UE.NASIntegrity = 0x80 >> nas.NIA1 }
	release := []mockamf.Option{mockamf.WithIntegrityOrder(nas.NIA2), mockamf.WithNoCommonIntegrity(mockamf.ReleaseNoCommonIntegrity)}
	tests := []struct {
		name         string
		opts         []mockamf.Option
		env          func(*Env)
		noUE         bool
		otherPLMN    bool // the mock serves another PLMN, so it refuses the NG Setup
		dead         bool
		want         Verdict
		wantOutcome  string
		wantObserved string
		wantDetail   []string
		wantDeregs   int64
	}{
		{name: "accepted", want: Pass, wantOutcome: "accepted", wantObserved: "REGISTRATION ACCEPT",
			wantDetail: []string{"state left: the UE was deregistered", "requires Rel-15 or later; TS 24.501 5.5.1.2.4"}, wantDeregs: 1},
		{name: "rejected", opts: []mockamf.Option{mockamf.WithRegistrationReject(nasMessage.Cause5GMMIllegalUE)},
			want: Warn, wantOutcome: "rejected", wantObserved: "REGISTRATION REJECT, 5GMM cause #3",
			wantDetail: []string{"no requirement violated; WARN for availability impact", "the association was closed with the UE not registered"}},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()},
			want: Warn, wantOutcome: "authentication-rejected", wantObserved: "AUTHENTICATION REJECT"},
		{name: "silent", opts: []mockamf.Option{mockamf.WithNoAuthenticationRequest()},
			want: Fail, wantOutcome: "unanswered", wantObserved: "no REGISTRATION ACCEPT or REJECT",
			wantDetail: []string{"within 500ms (absence inferred within this bound)", "violates TS 24.501 5.5.1.2.4 (shall), TS 24.501 5.5.1.2.5 (shall)"}},
		{name: "released", opts: release, env: nia1Only,
			want: Fail, wantOutcome: "released", wantObserved: "UE CONTEXT RELEASE COMMAND without REGISTRATION ACCEPT or REJECT"},
		{name: "NG Setup refused", otherPLMN: true, want: Blocked, wantOutcome: "blocked-ng-setup", wantObserved: "UE setup failed",
			wantDetail: []string{"the core did not complete an NG Setup, which this check needs first"}},
		{name: "dies after the release", opts: append(append([]mockamf.Option(nil), release...), mockamf.WithCrashAfterRelease()), env: nia1Only, dead: true,
			want: Crash, wantOutcome: "crash", wantObserved: ObservedAMFUnresponsive + " (UE CONTEXT RELEASE COMMAND without REGISTRATION ACCEPT or REJECT)"},
		{name: "no UE", noUE: true, want: Skip, wantOutcome: "skip-no-ue", wantObserved: "no UE configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, front := initialNASMock(t, tc.opts...)
			if tc.otherPLMN {
				amf, front = otherPLMNMock(t)
			}
			env := ueEnv(front.Addr, "", "127.0.0.1")
			if tc.env != nil {
				tc.env(&env)
			}
			if tc.noUE {
				env.UE = UESpec{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r := runOne(ctx, nasInitialRegistration{wait: 500 * time.Millisecond}, env, 20*time.Second)
			if r.Verdict != tc.want || r.Outcome != tc.wantOutcome || r.Observed != tc.wantObserved {
				t.Fatalf("= %s %s %q (%s), want %s %s %q", r.Verdict, r.Outcome, r.Observed, r.Detail, tc.want, tc.wantOutcome, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if notCompleted := r.NotCompleted == ProcRegistration; notCompleted != (tc.want == Warn || tc.want == Fail) {
				t.Errorf("NotCompleted = %q for %s", r.NotCompleted, tc.want)
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if got := amf.Deregistrations(); got != tc.wantDeregs {
				t.Errorf("mock saw %d deregistrations, want %d", got, tc.wantDeregs)
			}
			if !tc.dead {
				awaitClosed(t, front)
			}
		})
	}
}
