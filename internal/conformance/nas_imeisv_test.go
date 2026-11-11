package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/mockamf"
)

// judgeIMEISV: a request followed by an Identity Request for the IMEISV is
// INFO; no request, an Accept without it, a Reject, another identity type,
// a second Security Mode Command, a release and no answer are INFO-DIFF,
// each with its own Observed; a registration that never reached a Security
// Mode Command is ERROR.
func TestJudgeIMEISV(t *testing.T) {
	accept := &nasMessage.RegistrationAccept{}
	tests := []struct {
		name         string
		o            initialNASOutcome
		want         Verdict
		wantObserved string
	}{
		{name: "not requested", o: initialNASOutcome{smcs: 1, registered: true, accept: accept}, want: infoDiffers, wantObserved: imeisvNotRequested},
		{name: "requested, identified", o: initialNASOutcome{smcs: 1, imeisvRequested: true, identities: []uint8{5}, registered: true, accept: accept},
			want: Info, wantObserved: imeisvIdentified},
		{name: "requested, accepted without it", o: initialNASOutcome{smcs: 1, imeisvRequested: true, registered: true, accept: accept},
			want: infoDiffers, wantObserved: imeisvAccepted},
		{name: "requested, rejected", o: initialNASOutcome{smcs: 1, imeisvRequested: true, reject: rejectPlain(5)},
			want: infoDiffers, wantObserved: "IMEISV requested in the SECURITY MODE COMMAND; left out, REGISTRATION REJECT, 5GMM cause #5"},
		{name: "requested, another identity asked for", o: initialNASOutcome{smcs: 1, imeisvRequested: true, identities: []uint8{3}, registered: true, accept: accept},
			want: infoDiffers, wantObserved: "IMEISV requested in the SECURITY MODE COMMAND; left out, IDENTITY REQUEST for another identity (IMEI)"},
		{name: "requested, an unnamed identity type asked for", o: initialNASOutcome{smcs: 1, imeisvRequested: true, identities: []uint8{6, 1}, registered: true, accept: accept},
			want: infoDiffers, wantObserved: "IMEISV requested in the SECURITY MODE COMMAND; left out, IDENTITY REQUEST for another identity (type 6)"},
		{name: "requested, second SMC", o: initialNASOutcome{smcs: 2, imeisvRequested: true}, want: infoDiffers, wantObserved: imeisvSecondSMC},
		{name: "requested, released", o: initialNASOutcome{smcs: 1, imeisvRequested: true, released: true}, want: infoDiffers, wantObserved: imeisvReleased},
		{name: "requested, no answer", o: initialNASOutcome{smcs: 1, imeisvRequested: true, noAnswer: true}, want: infoDiffers, wantObserved: imeisvUnanswered},
		{name: "no SMC", o: initialNASOutcome{noAnswer: true}, want: Error, wantObserved: "no Registration Reject or Accept"},
		{name: "authentication reject", o: initialNASOutcome{authReject: true}, want: Error, wantObserved: "Authentication Reject"},
		{name: "setup failed", o: initialNASOutcome{err: errors.New("dial")}, want: Error, wantObserved: "UE setup failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if v, observed, detail := judgeIMEISV(tc.o, initialNASWait); v != tc.want || observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
		})
	}
}

// NAS-SMC-IMEISV-REQUEST against the mock AMF over real SCTP, as a suite
// run scores it. A mock that requests the IMEISV and identifies the UE when
// it is missing is INFO; one that does not request it, one that accepts
// without it and one that rejects are INFO-DIFF; none gates. No Security
// Mode Complete carries an IMEISV, the Identity Request is answered with
// one, and a registered UE is deregistered.
func TestIMEISVCheckAgainstMock(t *testing.T) {
	tests := []struct {
		name          string
		opts          []mockamf.Option
		noUE          bool
		want          Verdict
		wantObserved  string
		wantDetail    []string
		wantIdentity  int
		wantDeregs    int64
		wantCompletes int
	}{
		{name: "requests and identifies", opts: []mockamf.Option{mockamf.WithSMCIMEISVRequest(), mockamf.WithMissingIMEISV(mockamf.IdentifyMissingIMEISV)},
			want: Info, wantObserved: imeisvIdentified, wantDetail: []string{"state left: the UE was deregistered", "a fresh NG Setup afterwards completes"},
			wantIdentity: 1, wantDeregs: 1, wantCompletes: 1},
		{name: "does not request", want: Info, wantObserved: imeisvNotRequested, wantDetail: []string{"answer: Registration Accept"}, wantDeregs: 1, wantCompletes: 1},
		{name: "requests, accepts without", opts: []mockamf.Option{mockamf.WithSMCIMEISVRequest()}, want: Info, wantObserved: imeisvAccepted,
			wantDeregs: 1, wantCompletes: 1},
		{name: "requests, rejects", opts: []mockamf.Option{mockamf.WithSMCIMEISVRequest(), mockamf.WithMissingIMEISV(mockamf.RejectMissingIMEISV)}, want: Info,
			wantObserved: "IMEISV requested in the SECURITY MODE COMMAND; left out, REGISTRATION REJECT, 5GMM cause #5",
			wantDetail:   []string{"state left: the association was closed with the UE not registered"}, wantCompletes: 1},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error, wantObserved: "Authentication Reject"},
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
			reg := &Registry{}
			reg.Register(nasSMCIMEISVRequest{initialNASWaits{wait: 500 * time.Millisecond}})
			results := reg.Run(ctx, env, 20*time.Second)
			r := results[0]
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Fatalf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			for _, w := range append(tc.wantDetail, informationalNote, "requires Rel-15 or later; TS 24.501 5.4.2.2 IMEISV request") {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if sum := Summarize(results); sum.OK() != (tc.want != Error) || sum.Fail != 0 || sum.Pass != 0 {
				t.Errorf("summary %+v: only an ERROR may gate", sum)
			}
			completes := amf.SecurityModeCompleteIMEISVs()
			if len(completes) != tc.wantCompletes {
				t.Fatalf("mock saw %d Security Mode Completes, want %d", len(completes), tc.wantCompletes)
			}
			for _, imeisv := range completes {
				if imeisv != nil {
					t.Errorf("a Security Mode Complete carried the IMEISV %x", imeisv)
				}
			}
			ids := amf.IdentityResponses()
			if len(ids) != tc.wantIdentity {
				t.Fatalf("mock saw %d Identity Responses, want %d", len(ids), tc.wantIdentity)
			}
			for _, id := range ids {
				if len(id) == 0 || id[0]&0x07 != nasMessage.MobileIdentity5GSTypeImeisv {
					t.Errorf("Identity Response %x is not an IMEISV", id)
				}
			}
			if got := amf.Deregistrations(); got != tc.wantDeregs {
				t.Errorf("mock saw %d deregistrations, want %d", got, tc.wantDeregs)
			}
			if n := amf.EncodeErrors(); n != 0 {
				t.Errorf("mock AMF dropped %d PDUs that failed to encode", n)
			}
			awaitClosed(t, front)
		})
	}
}

// The check is informational, on descriptive references alone.
func TestIMEISVCheckIsInformational(t *testing.T) {
	c := nasSMCIMEISVRequest{}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	if !IsInformational(c) || VerdictClass(c) != Info || MinRelease(c) != 15 || ExpectedDuration(c, DefaultPerTestTimeout) != 20*time.Second {
		t.Errorf("informational %v, class %s, min %s, up to %s", IsInformational(c), VerdictClass(c), MinRelease(c), ExpectedDuration(c, DefaultPerTestTimeout))
	}
	for _, r := range c.SpecRefs() {
		if r.Strength != Descriptive {
			t.Errorf("%s %s is cited as %s", r.Spec, r.Clause, r.Strength)
		}
	}
}
