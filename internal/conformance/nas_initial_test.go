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

// initialNASMock starts a mock AMF behind a real SCTP listener for the
// initial-NAS checks. Its Accept assigns a 5G-GUTI, so a registered UE can
// deregister.
func initialNASMock(t *testing.T, opts ...mockamf.Option) (*mockamf.AMF, *sctpfront.Front) {
	t.Helper()
	k, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	amf, err := mockamf.New(mockamf.Config{
		Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	}, append([]mockamf.Option{mockamf.WithGUTI()}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return amf, sctpfront.Start(t, amf)
}

// awaitClosed waits for every association of front to close.
func awaitClosed(t *testing.T, front *sctpfront.Front) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for front.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := front.Active(); n != 0 {
		t.Errorf("%d associations still open after the check", n)
	}
}

// rejectPlain is a plain Registration Reject with cause, as the checks read
// one.
func rejectPlain(cause uint8) []byte {
	return []byte{nasMessage.Epd5GSMobilityManagementMessage, 0, 0x44, cause}
}

// judgeContainerWins passes only an answer that equals the one the
// container's request gets alone and differs from the unprotected value's;
// any other answer fails, named as the unprotected value's when it is that
// one; and a core that answers both references alike is SKIP unless the
// stimulus was answered differently again.
func TestJudgeContainerWins(t *testing.T) {
	const accept, reject, other = "Registration Accept, allowed NSSAI 01010203, rejected NSSAI none",
		"Registration Reject, 5GMM cause #62, rejected NSSAI 01eeeeee (cause 0)", "no Registration Accept or Reject"
	tests := []struct {
		name                   string
		stim, served, unserved string
		want                   Verdict
		wantObserved           string
		wantDetail             string
	}{
		{name: "container's request answered", stim: accept, served: accept, unserved: reject, want: Pass,
			wantObserved: "registration answered on the request in the NAS message container"},
		{name: "unprotected value answered", stim: reject, served: accept, unserved: reject, want: Fail,
			wantObserved: "registration answered on the unprotected Requested NSSAI, not the one in the NAS message container", wantDetail: initialMsgContainerWinsFailure},
		{name: "another answer", stim: other, served: accept, unserved: reject, want: Fail,
			wantObserved: "registration not answered as the request in the NAS message container is: " + other, wantDetail: initialMsgContainerWinsFailure},
		{name: "references alike", stim: accept, served: accept, unserved: accept, want: Skip,
			wantObserved: "core answers the served and the unserved Requested NSSAI alike; the two copies cannot be told apart", wantDetail: "NAS-REG-REJECTED-NSSAI"},
		{name: "references alike, stimulus answered otherwise", stim: other, served: accept, unserved: accept, want: Fail,
			wantObserved: "registration not answered as the request in the NAS message container is: " + other},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeContainerWins(tc.stim, tc.served, tc.unserved)
			if v != tc.want || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("= %s %q (%s), want %s %q with detail containing %q", v, observed, detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
			for _, w := range []string{"stimulus answered: " + tc.stim, "the container's Requested NSSAI sent alone: " + tc.served, "the initial message's Requested NSSAI sent alone: " + tc.unserved} {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
		})
	}
}

// NAS-INITIAL-MSG-CONTAINER-WINS against the mock AMF over real SCTP. A
// mock that takes the container's request PASSes; one that keeps the
// initial message's Requested NSSAI FAILs; one that answers every Requested
// NSSAI alike, and one that refuses the initial message, SKIP; an
// Authentication Reject is ERROR; no UE SKIPs. The initial message of the
// stimulus carries the unserved S-NSSAI in the clear and its container the
// served one; every accepted registration is deregistered and no
// association stays open.
func TestInitialMsgContainerWinsAgainstMock(t *testing.T) {
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noUE         bool
		want         Verdict
		wantObserved string
		wantDetail   []string
		wantRegs     int // Registration Requests the mock received
		wantDeregs   int64
	}{
		{name: "container wins", opts: []mockamf.Option{mockamf.WithUnservedNSSAI(mockamf.Reject62NSSAI)}, want: Pass,
			wantObserved: "registration answered on the request in the NAS message container",
			wantDetail: []string{"stimulus answered: Registration Accept, allowed NSSAI none, rejected NSSAI none",
				"the initial message's Requested NSSAI sent alone: Registration Reject, 5GMM cause #62, rejected NSSAI 01eeeeee (cause 0)",
				"after the stimulus the UE was deregistered", "after the second the association was closed with the UE not registered"},
			wantRegs: 3, wantDeregs: 2},
		{name: "container wins, the unserved S-NSSAI rejected inside an Accept", opts: []mockamf.Option{mockamf.WithUnservedNSSAI(mockamf.AcceptDefaultNSSAI)}, want: Pass,
			wantObserved: "registration answered on the request in the NAS message container",
			wantDetail:   []string{"the initial message's Requested NSSAI sent alone: Registration Accept, allowed NSSAI 01010203, rejected NSSAI 01eeeeee (cause 0)"},
			wantRegs:     3, wantDeregs: 3},
		{name: "unprotected copy wins, the unserved S-NSSAI rejected inside an Accept", opts: []mockamf.Option{mockamf.WithUnservedNSSAI(mockamf.AcceptDefaultNSSAI), mockamf.WithUnprotectedRegistrationWins()}, want: Fail,
			wantObserved: "registration answered on the unprotected Requested NSSAI, not the one in the NAS message container", wantRegs: 3, wantDeregs: 3},
		{name: "unprotected copy wins", opts: []mockamf.Option{mockamf.WithUnservedNSSAI(mockamf.Reject62NSSAI), mockamf.WithUnprotectedRegistrationWins()}, want: Fail,
			wantObserved: "registration answered on the unprotected Requested NSSAI, not the one in the NAS message container",
			wantDetail:   []string{initialMsgContainerWinsFailure, "stimulus answered: Registration Reject, 5GMM cause #62"}, wantRegs: 3, wantDeregs: 1},
		{name: "every Requested NSSAI answered alike", want: Skip,
			wantObserved: "core answers the served and the unserved Requested NSSAI alike; the two copies cannot be told apart", wantRegs: 3, wantDeregs: 3},
		{name: "initial message refused", opts: []mockamf.Option{mockamf.WithRejectNonCleartextIEs()}, want: Skip,
			wantObserved: "core refuses non-cleartext IEs in the initial message (Registration Reject, 5GMM cause #95) before the container is consulted",
			wantDetail:   []string{"no SECURITY MODE COMPLETE and no NAS message container were sent", "the association was closed with the UE not registered"}, wantRegs: 1},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error,
			wantObserved: "Authentication Reject", wantDetail: []string{"the stimulus registration"}, wantRegs: 1},
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
			r := runOne(ctx, nasInitialMsgContainerWins{wait: 500 * time.Millisecond}, env, 20*time.Second)
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
			initial, final := amf.InitialRegistrationRequests(), amf.RegistrationRequests()
			if len(initial) != tc.wantRegs {
				t.Fatalf("mock received %d Registration Requests, want %d", len(initial), tc.wantRegs)
			}
			if tc.wantRegs > 0 {
				// The stimulus: the unserved S-NSSAI in the clear.
				if iei, found, err := nas.NonCleartextRegistrationIE(initial[0]); err != nil || !found {
					t.Errorf("stimulus initial message has no non-cleartext IE (iei %#x, err %v)", iei, err)
				}
				if !strings.Contains(string(initial[0]), "\xee\xee\xee") {
					t.Errorf("stimulus initial message %x lacks the unserved S-NSSAI", initial[0])
				}
			}
			if tc.wantRegs == 3 {
				if !strings.Contains(string(final[0]), "\x01\x02\x03") || strings.Contains(string(final[0]), "\xee\xee\xee") {
					t.Errorf("stimulus container request %x, want the served S-NSSAI only", final[0])
				}
				for i := 1; i < 3; i++ {
					if _, found, err := nas.NonCleartextRegistrationIE(initial[i]); err != nil || found {
						t.Errorf("reference %d's initial message has non-cleartext IEs (err %v)", i, err)
					}
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

// The check is FAIL-class on two shalls, needs up to 50 s and declares its
// UE.
func TestInitialMsgContainerWinsClass(t *testing.T) {
	c := nasInitialMsgContainerWins{}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	if VerdictClass(c) != Fail || IsInformational(c) || c.Budget() != 50*time.Second || NeedsOf(c).UEs != 1 {
		t.Errorf("class %s, informational %v, budget %s, UEs %d", VerdictClass(c), IsInformational(c), c.Budget(), NeedsOf(c).UEs)
	}
}

// judgeNoContainer: only a second Security Mode Command with RINMR is INFO;
// one without it, an Accept, a Reject (before or after the Security Mode
// Command), a release and no answer are INFO-DIFF with their own Observed;
// what stopped the registration short of a Security Mode Command is ERROR.
func TestJudgeNoContainer(t *testing.T) {
	accept := &nasMessage.RegistrationAccept{}
	tests := []struct {
		name         string
		o            initialNASOutcome
		want         Verdict
		wantObserved string
		wantDetail   string
	}{
		{name: "second SMC with RINMR", o: initialNASOutcome{smcs: 2, secondRINMR: true}, want: Info, wantObserved: noContainerRINMR},
		{name: "second SMC without RINMR", o: initialNASOutcome{smcs: 2}, want: InfoDiff, wantObserved: noContainerSecondSMC},
		{name: "accepted", o: initialNASOutcome{smcs: 1, registered: true, accept: accept}, want: InfoDiff, wantObserved: noContainerAccepted,
			wantDetail: "Registration Accept, allowed NSSAI none"},
		{name: "rejected with #62", o: initialNASOutcome{smcs: 1, reject: rejectPlain(62)}, want: InfoDiff,
			wantObserved: "REGISTRATION REJECT, 5GMM cause #62, after the SECURITY MODE COMPLETE without the NAS message container",
			wantDetail:   "the core went on with the unprotected values"},
		{name: "rejected with another cause", o: initialNASOutcome{smcs: 1, reject: rejectPlain(111)}, want: InfoDiff,
			wantObserved: "REGISTRATION REJECT, 5GMM cause #111, after the SECURITY MODE COMPLETE without the NAS message container"},
		{name: "refused before the SMC", o: initialNASOutcome{reject: rejectPlain(95), rejectBeforeSMC: true}, want: InfoDiff,
			wantObserved: "initial message refused before the SECURITY MODE COMMAND (Registration Reject, 5GMM cause #95)", wantDetail: "was not exercised"},
		{name: "released", o: initialNASOutcome{smcs: 1, released: true}, want: InfoDiff, wantObserved: noContainerReleased},
		{name: "no answer", o: initialNASOutcome{smcs: 1, noAnswer: true}, want: InfoDiff, wantObserved: noContainerUnanswered, wantDetail: "within 10s"},
		{name: "released before the SMC", o: initialNASOutcome{released: true}, want: Error, wantObserved: "released without a Registration Reject or Accept"},
		{name: "nothing before the SMC", o: initialNASOutcome{noAnswer: true}, want: Error, wantObserved: "no Registration Reject or Accept"},
		{name: "authentication reject", o: initialNASOutcome{authReject: true}, want: Error, wantObserved: "Authentication Reject"},
		{name: "unreadable reject", o: initialNASOutcome{smcs: 1, rejectErr: nas.ErrIntegrity}, want: Error, wantObserved: "Registration Reject could not be read"},
		{name: "setup failed", o: initialNASOutcome{err: errors.New("dial")}, want: Error, wantObserved: "UE setup failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeNoContainer(tc.o, initialNASWait)
			if v != tc.want || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("= %s %q (%s), want %s %q with detail containing %q", v, observed, detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
		})
	}
}

// NAS-INITIAL-MSG-NO-CONTAINER against the mock AMF over real SCTP, as a
// suite run scores it: a mock that asks for the initial NAS message again
// is INFO; one that repeats the command without RINMR, one that accepts,
// one that rejects, one that refuses the initial message and one that says
// nothing are INFO-DIFF, and none of them gates. The Security Mode
// Complete carries no container, and no association stays open.
func TestInitialMsgNoContainerAgainstMock(t *testing.T) {
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noUE         bool
		want         Verdict
		wantObserved string
		wantDetail   []string
		wantSMCs     int64
		wantDeregs   int64
	}{
		{name: "asks again with RINMR", opts: []mockamf.Option{mockamf.WithMissingContainer(mockamf.RequestInitialNAS)}, want: Info,
			wantObserved: noContainerRINMR, wantDetail: []string{"the association was closed with the UE not registered", "a fresh NG Setup afterwards completes"}, wantSMCs: 2},
		{name: "repeats the command", opts: []mockamf.Option{mockamf.WithMissingContainer(mockamf.RepeatSecurityModeCommand)}, want: InfoDiff,
			wantObserved: noContainerSecondSMC, wantSMCs: 2},
		{name: "accepts", want: InfoDiff, wantObserved: noContainerAccepted,
			wantDetail: []string{"Registration Accept, allowed NSSAI none", "state left: the UE was deregistered"}, wantSMCs: 1, wantDeregs: 1},
		{name: "goes on with the unprotected values", opts: []mockamf.Option{mockamf.WithUnservedNSSAI(mockamf.Reject62NSSAI)}, want: InfoDiff,
			wantObserved: "REGISTRATION REJECT, 5GMM cause #62, after the SECURITY MODE COMPLETE without the NAS message container",
			wantDetail:   []string{"rejected NSSAI 01eeeeee (cause 0)", "the core went on with the unprotected values"}, wantSMCs: 1},
		{name: "rejects the missing container", opts: []mockamf.Option{mockamf.WithMissingContainer(mockamf.RejectMissingContainer)}, want: InfoDiff,
			wantObserved: "REGISTRATION REJECT, 5GMM cause #111, after the SECURITY MODE COMPLETE without the NAS message container", wantSMCs: 1},
		{name: "refuses the initial message", opts: []mockamf.Option{mockamf.WithRejectNonCleartextIEs()}, want: InfoDiff,
			wantObserved: "initial message refused before the SECURITY MODE COMMAND (Registration Reject, 5GMM cause #95)"},
		{name: "says nothing", opts: []mockamf.Option{mockamf.WithUnservedNSSAI(mockamf.IgnoreNSSAI)}, want: InfoDiff,
			wantObserved: noContainerUnanswered, wantDetail: []string{"within 500ms"}, wantSMCs: 1},
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
			reg.Register(nasInitialMsgNoContainer{wait: 500 * time.Millisecond})
			results := reg.Run(ctx, env, 20*time.Second)
			r := results[0]
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Fatalf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			for _, w := range append(tc.wantDetail, informationalNote, "requires Rel-15 or later; TS 24.501 4.4.6 a)") {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if sum := Summarize(results); sum.OK() != (tc.want != Error) || sum.Fail != 0 || sum.Pass != 0 {
				t.Errorf("summary %+v: only an ERROR may gate", sum)
			}
			if got := amf.SecurityModeCommands(); got != tc.wantSMCs {
				t.Errorf("mock sent %d Security Mode Commands, want %d", got, tc.wantSMCs)
			}
			if tc.wantSMCs > 0 {
				// With no container the initial message stands as the request.
				initial, final := amf.InitialRegistrationRequests(), amf.RegistrationRequests()
				if len(initial) != 1 || string(initial[0]) != string(final[0]) {
					t.Errorf("the mock took a request from a container: initial %x, final %x", initial, final)
				}
				if _, found, _ := nas.NonCleartextRegistrationIE(initial[0]); !found || !strings.Contains(string(initial[0]), "\xee\xee\xee") {
					t.Errorf("initial message %x, want non-cleartext IEs with the unserved S-NSSAI", initial[0])
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

// judgeNonCleartext: an accepted registration is INFO; a Reject, before or
// after the Security Mode Command, a release and no answer are INFO-DIFF.
func TestJudgeNonCleartext(t *testing.T) {
	tests := []struct {
		name         string
		o            initialNASOutcome
		want         Verdict
		wantObserved string
	}{
		{name: "accepted", o: initialNASOutcome{smcs: 1, registered: true, accept: &nasMessage.RegistrationAccept{}}, want: Info, wantObserved: nonCleartextAccepted},
		{name: "refused before the SMC", o: initialNASOutcome{reject: rejectPlain(95), rejectBeforeSMC: true}, want: InfoDiff,
			wantObserved: "Registration Reject, 5GMM cause #95, before the SECURITY MODE COMMAND"},
		{name: "rejected after the SMC", o: initialNASOutcome{smcs: 1, reject: rejectPlain(111)}, want: InfoDiff,
			wantObserved: "Registration Reject, 5GMM cause #111, after the SECURITY MODE COMPLETE"},
		{name: "released", o: initialNASOutcome{smcs: 1, released: true}, want: InfoDiff, wantObserved: nonCleartextReleased},
		{name: "no answer", o: initialNASOutcome{noAnswer: true}, want: InfoDiff, wantObserved: nonCleartextUnanswered},
		{name: "authentication reject", o: initialNASOutcome{authReject: true}, want: Error, wantObserved: "Authentication Reject"},
		{name: "setup failed", o: initialNASOutcome{err: errors.New("dial")}, want: Error, wantObserved: "UE setup failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if v, observed, detail := judgeNonCleartext(tc.o, initialNASWait); v != tc.want || observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
		})
	}
}

// NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED against the mock AMF over real
// SCTP, as a suite run scores it: a mock that registers the UE is INFO, one
// that refuses the initial message INFO-DIFF, and neither gates. The
// initial message is the complete request, the container holds the same,
// and the UE is deregistered.
func TestInitialMsgNonCleartextAgainstMock(t *testing.T) {
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noUE         bool
		want         Verdict
		wantObserved string
		wantDetail   []string
		wantDeregs   int64
	}{
		{name: "accepts", want: Info, wantObserved: nonCleartextAccepted,
			wantDetail: []string{"Registration Accept, allowed NSSAI none", "state left: the UE was deregistered", "a fresh NG Setup afterwards completes"}, wantDeregs: 1},
		{name: "refuses the initial message", opts: []mockamf.Option{mockamf.WithRejectNonCleartextIEs()}, want: InfoDiff,
			wantObserved: "Registration Reject, 5GMM cause #95, before the SECURITY MODE COMMAND",
			wantDetail:   []string{"state left: the association was closed with the UE not registered"}},
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
			reg.Register(nasInitialMsgNonCleartextAccepted{wait: 500 * time.Millisecond})
			results := reg.Run(ctx, env, 20*time.Second)
			r := results[0]
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Fatalf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			for _, w := range append(tc.wantDetail, informationalNote) {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if sum := Summarize(results); sum.OK() != (tc.want != Error) || sum.Fail != 0 || sum.Pass != 0 {
				t.Errorf("summary %+v: only an ERROR may gate", sum)
			}
			if !tc.noUE {
				initial := amf.InitialRegistrationRequests()
				if len(initial) != 1 {
					t.Fatalf("mock received %d Registration Requests, want 1", len(initial))
				}
				if _, found, _ := nas.NonCleartextRegistrationIE(initial[0]); !found {
					t.Errorf("initial message %x has cleartext IEs only", initial[0])
				}
				if final := amf.RegistrationRequests(); tc.want == Info && string(final[0]) != string(initial[0]) {
					t.Errorf("container request %x differs from the initial message %x", final[0], initial[0])
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

// The two reporting checks are informational, on descriptive references
// alone.
func TestInitialMsgInformationalChecks(t *testing.T) {
	for _, c := range []Test{nasInitialMsgNoContainer{}, nasInitialMsgNonCleartextAccepted{}} {
		if err := ValidateTest(c); err != nil {
			t.Error(err)
		}
		if !IsInformational(c) || VerdictClass(c) != Info || MinRelease(c) != 15 || ExpectedDuration(c, DefaultPerTestTimeout) != 20*time.Second {
			t.Errorf("%s: informational %v, class %s, min %s, up to %s", c.ID(), IsInformational(c), VerdictClass(c), MinRelease(c), ExpectedDuration(c, DefaultPerTestTimeout))
		}
		for _, r := range c.SpecRefs() {
			if r.Strength != Descriptive {
				t.Errorf("%s: %s %s is cited as %s", c.ID(), r.Spec, r.Clause, r.Strength)
			}
		}
	}
}
