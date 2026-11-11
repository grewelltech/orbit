package conformance

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// The second NG Setup's outcome by what answered it: a RESPONSE, a FAILURE
// with its cause, another PDU, nothing, or a lost association.
func TestClassifyDuplicateSetup(t *testing.T) {
	tests := []struct {
		name       string
		resp       *ngapType.NGAPPDU
		err        error
		want       string
		wantDetail string
	}{
		{name: "response", resp: pduNGSetupResponse(), want: "second NG Setup accepted", wantDetail: "NG SETUP RESPONSE 1.234s after the second NG SETUP REQUEST"},
		{name: "failure", resp: pduNGSetupFailure(false), want: "second NG Setup rejected (NG SETUP FAILURE)", wantDetail: "NG SETUP FAILURE, cause "},
		{name: "another PDU", resp: pduErrorIndication(), want: "second NG Setup answered with another PDU",
			wantDetail: "answered with InitiatingMessage(procedure 9) 1.234s after the second NG SETUP REQUEST, neither NG SETUP RESPONSE nor NG SETUP FAILURE"},
		{name: "nothing", err: context.DeadlineExceeded, want: "second NG Setup unanswered",
			wantDetail: "no NG SETUP RESPONSE or FAILURE within 4s of the second NG SETUP REQUEST (absence inferred within this bound)"},
		{name: "association lost", err: io.EOF, want: "second association reset", wantDetail: "the second association was lost 1.234s after its NG SETUP REQUEST (EOF)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := classifyDuplicateSetup(tc.resp, tc.err, 1234*time.Millisecond, duplicateSetupWait)
			if got != tc.want || !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("= %q %q, want %q with detail containing %q", got, detail, tc.want, tc.wantDetail)
			}
		})
	}
}

// The first association is up when its read times out or yields a PDU, and
// ended when the read fails.
func TestClassifyFirstAssociation(t *testing.T) {
	tests := []struct {
		name       string
		pdu        *ngapType.NGAPPDU
		err        error
		wantKept   bool
		wantDetail string
	}{
		{name: "quiet", err: context.DeadlineExceeded, wantKept: true, wantDetail: "did not end within 2s (watched at transport level; no PDU was sent on it)"},
		{name: "a PDU arrives", pdu: pduErrorIndication(), wantKept: true, wantDetail: "is up and the AMF sent InitiatingMessage(procedure 9) on it"},
		{name: "ended", err: io.EOF, wantDetail: "the first association ended (EOF)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kept, detail := classifyFirstAssociation(tc.pdu, tc.err, duplicateFirstWait)
			if kept != tc.wantKept || !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("= %v %q, want %v with detail containing %q", kept, detail, tc.wantKept, tc.wantDetail)
			}
		})
	}
}

// An answer to the duplicate is INFO with the first association kept and
// WARN for availability without it; no answer, another PDU and a lost second
// association are DEVIATIONs from the should of 8.7.1.3 either way.
// Observed names both halves.
func TestJudgeDuplicate(t *testing.T) {
	tests := []struct {
		kind         string
		kept         bool
		want         Outcome
		wantObserved string
	}{
		{duplicateAccepted, true, duplicateAcceptedKept, "second NG Setup accepted; first association kept"},
		{duplicateAccepted, false, duplicateAcceptedDropped, "second NG Setup accepted; first association dropped"},
		{duplicateRejected, true, duplicateRejectedKept, "second NG Setup rejected (NG SETUP FAILURE); first association kept"},
		{duplicateRejected, false, duplicateRejectedDropped, "second NG Setup rejected (NG SETUP FAILURE); first association dropped"},
		{duplicateUnanswered, true, duplicateNoAnswer, "second NG Setup unanswered; first association kept"},
		{duplicateUnanswered, false, duplicateNoAnswer, "second NG Setup unanswered; first association dropped"},
		{duplicateOtherPDU, true, duplicateOtherAnswer, "second NG Setup answered with another PDU; first association kept"},
		{duplicateOtherPDU, false, duplicateOtherAnswer, "second NG Setup answered with another PDU; first association dropped"},
		{duplicateReset, true, duplicateSecondLost, "second association reset; first association kept"},
		{duplicateReset, false, duplicateSecondLost, "second association reset; first association dropped"},
	}
	for _, tc := range tests {
		t.Run(tc.wantObserved, func(t *testing.T) {
			if o, observed := judgeDuplicate(tc.kind, tc.kept); covers(o).ID != tc.want.ID || o.Verdict != tc.want.Verdict || observed != tc.wantObserved {
				t.Errorf("= %s (%s) %q, want %s (%s) %q", o.ID, o.Verdict, observed, tc.want.ID, tc.want.Verdict, tc.wantObserved)
			}
		})
	}
}

// The declaration: INFO for an answer with the first association kept, WARN
// for availability when it is dropped, DEVIATIONs citing the should of
// 8.7.1.3; 8.7.1.2 is the primary reference, and the check needs its own NG
// Setup and a second N2 source address.
func TestDuplicateRANNodeDeclaration(t *testing.T) {
	c := duplicateRANNodeID{}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	if WorstVerdict(c) != Deviation || MinRelease(c) != 15 {
		t.Errorf("worst %s, min %s", WorstVerdict(c), MinRelease(c))
	}
	for _, o := range []Outcome{duplicateAcceptedDropped, duplicateRejectedDropped} {
		if o.Verdict != Warn || o.Criterion != CriterionAvailability {
			t.Errorf("%s: %s (%s), want WARN for availability", o.ID, o.Verdict, o.Criterion)
		}
	}
	for _, r := range c.SpecRefs() {
		if r.Primary != (r.Key() == duplicateSuccessRef) {
			t.Errorf("%s: primary %v; 8.7.1.2 is the primary reference", r.Key(), r.Primary)
		}
	}
	if n := NeedsOf(c); n.UEs != 0 || len(n.GNBRoles) != 2 || !n.SecondN2Source || len(n.Procedures) != 1 || n.Procedures[0] != ProcNGSetup {
		t.Errorf("needs = %+v, want no UE, the check's own and the liveness probe's gNB IDs, a second N2 source and the NG Setup", n)
	}
}

// A second source exists only when SecondBind is set and names another IP
// than the first associations leave from.
func TestEnvHasSecondSource(t *testing.T) {
	tests := []struct {
		name         string
		bind, second string
		want         bool
	}{
		{name: "none"},
		{name: "another address", bind: "127.0.0.1", second: "127.0.0.2", want: true},
		{name: "another address, ports written", bind: "127.0.0.1:0", second: "127.0.0.2:0", want: true},
		{name: "the same address", bind: "127.0.0.1", second: "127.0.0.1:0"},
		{name: "unbound first, second is the route-selected address", second: "127.0.0.1"},
		{name: "unbound first, second is another", second: "127.0.0.2", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := Env{AMFAddr: "127.0.0.1:38412", Bind: tc.bind, SecondBind: tc.second}
			if got := env.HasSecondSource(); got != tc.want {
				t.Errorf("HasSecondSource = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := (Env{AMFAddr: "127.0.0.1:38412"}).DialSecond(context.Background()); err == nil || !strings.Contains(err.Error(), "no second N2 source address configured") {
		t.Errorf("DialSecond without SecondBind: err = %v", err)
	}
}

func duplicateMock(t *testing.T, opts ...mockamf.Option) (*mockamf.AMF, *sctpfront.Front) {
	t.Helper()
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return amf, sctpfront.Start(t, amf)
}

func duplicateEnv(amfAddr string) Env {
	return Env{AMFAddr: amfAddr, Bind: "127.0.0.1", SecondBind: "127.0.0.2",
		GNB: gnb.Config{ID: 0x600, IDBits: 24, Name: "orbit-conf", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}}
}

// Against the mock AMF over real SCTP, from two loopback addresses, as a
// suite run scores it. An AMF that accepts the duplicate and keeps both
// associations, and one that rejects it, are INFO; one that accepts it and
// drops the first association is WARN for availability; one that leaves it
// unanswered is a DEVIATION from 8.7.1.3. One that stops answering did not
// survive: CRASH. The mock sees the same gNB ID twice, and both
// associations close afterwards.
func TestDuplicateRANNodeAgainstMock(t *testing.T) {
	tests := []struct {
		name         string
		answer       mockamf.DuplicateRANNodeAnswer
		want         Verdict
		wantOutcome  string
		wantObserved string
		wantDetail   []string
		wantSetups   int // the two of the check, and the liveness probe's from a running core
		wantAlive    bool
	}{
		{name: "accepts both", answer: mockamf.AcceptDuplicateRANNode, want: Info, wantOutcome: "accepted",
			wantObserved: "second NG Setup accepted; first association kept",
			wantDetail:   []string{"NG SETUP RESPONSE", "the first association did not end within 300ms", "a fresh NG Setup afterwards completes"}, wantSetups: 3, wantAlive: true},
		{name: "rejects the duplicate", answer: mockamf.RejectDuplicateRANNode, want: Info, wantOutcome: "rejected",
			wantObserved: "second NG Setup rejected (NG SETUP FAILURE); first association kept",
			wantDetail:   []string{"NG SETUP FAILURE, cause Misc 5", "a fresh NG Setup afterwards completes"}, wantSetups: 3, wantAlive: true},
		{name: "replaces the first association", answer: mockamf.ReplaceDuplicateRANNode, want: Warn, wantOutcome: "accepted-first-dropped",
			wantObserved: "second NG Setup accepted; first association dropped",
			wantDetail:   []string{"the first association ended", "a fresh NG Setup afterwards completes", "no requirement violated; WARN for availability impact"}, wantSetups: 3, wantAlive: true},
		{name: "leaves the duplicate unanswered", answer: mockamf.IgnoreDuplicateRANNode, want: Deviation, wantOutcome: "unanswered",
			wantObserved: "second NG Setup unanswered; first association kept",
			wantDetail: []string{"no NG SETUP RESPONSE or FAILURE within 300ms", "a fresh NG Setup afterwards completes",
				"does not follow TS 38.413 8.7.1.3 (should)"}, wantSetups: 3, wantAlive: true},
		{name: "stops answering", answer: mockamf.CrashOnDuplicateRANNode, want: Crash, wantOutcome: OutcomeCrash,
			wantObserved: "AMF unresponsive after the message (second association reset; first association kept)",
			wantDetail:   []string{"the second association was lost", "core did not survive the duplicate NG SETUP REQUEST: the AMF no longer completes a fresh NG Setup"}, wantSetups: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, front := duplicateMock(t, mockamf.WithDuplicateRANNodeID(tc.answer))
			env := duplicateEnv(front.Addr)
			reg := &Registry{}
			reg.Register(duplicateRANNodeID{setupWait: 300 * time.Millisecond, firstWait: 300 * time.Millisecond})
			results := reg.Run(context.Background(), env, 10*time.Second)
			res := results[0]
			if res.Verdict != tc.want || res.Outcome != tc.wantOutcome || res.Observed != tc.wantObserved {
				t.Fatalf("= %s %s %q (%s), want %s %s %q", res.Verdict, res.Outcome, res.Observed, res.Detail, tc.want, tc.wantOutcome, tc.wantObserved)
			}
			for _, w := range append(tc.wantDetail, "gNB ID 1536 presented from 127.0.0.1:", " and then from 127.0.0.2:",
				"requires Rel-15 or later; TS 38.413 8.7.1.2") {
				if !strings.Contains(res.Detail, w) {
					t.Errorf("detail %q lacks %q", res.Detail, w)
				}
			}
			sum := Summarize(results)
			if sum.OK() == GateDefault.Gates(tc.want) {
				t.Errorf("summary %+v: under the default gate, OK = %v for a %s", sum, sum.OK(), tc.want)
			}
			if sum.Gate = GateSpec; sum.Count(tc.want) != 1 || sum.OK() == (tc.want == Crash) {
				t.Errorf("summary %+v: want one %s, gating only for a crash under the spec gate", sum, tc.want)
			}

			setups := amf.Setups()
			if len(setups) != tc.wantSetups {
				t.Fatalf("mock saw %d NG Setups, want %d", len(setups), tc.wantSetups)
			}
			if setups[0].GNBID != 0x600 || setups[1].GNBID != 0x600 {
				t.Errorf("mock saw gNB IDs %#x and %#x, want the same ID 0x600 twice", setups[0].GNBID, setups[1].GNBID)
			}
			if len(setups) == 3 && setups[2].GNBID == 0x600 {
				t.Errorf("the liveness probe presented the duplicated gNB ID %#x", setups[2].GNBID)
			}
			deadline := time.Now().Add(3 * time.Second)
			for front.Active() != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if n := front.Active(); n != 0 {
				t.Errorf("%d associations still open after the check", n)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if alive, err := env.Alive(ctx); err != nil || alive != tc.wantAlive {
				t.Errorf("liveness after the check: alive %v, err %v; want alive %v", alive, err, tc.wantAlive)
			}
			if n := amf.EncodeErrors(); n != 0 {
				t.Errorf("mock AMF dropped %d PDUs that failed to encode", n)
			}
		})
	}
}

// Without two source addresses the check SKIPs before it opens anything. A
// second address this host does not hold is the configuration's ERROR, as is
// a stimulus that cannot be built; an AMF that does not complete the check's
// own NG Setup is BLOCKED on the NG Setup. ERROR and BLOCKED gate.
func TestDuplicateRANNodeSkipsAndErrors(t *testing.T) {
	amf, front := duplicateMock(t)
	noSecond := duplicateEnv(front.Addr)
	noSecond.SecondBind = ""
	same := duplicateEnv(front.Addr)
	same.SecondBind = "127.0.0.1:0"
	notLocal := duplicateEnv(front.Addr)
	notLocal.SecondBind = "192.0.2.77"
	unreachable := duplicateEnv("127.0.0.1:1")
	noSlices := duplicateEnv(front.Addr)
	noSlices.GNB.Slices = nil
	tests := []struct {
		name         string
		env          Env
		want         Verdict
		wantOutcome  string
		wantObserved string
		wantDetail   string
		wantSetups   int
	}{
		{name: "no second address", env: noSecond, want: Skip, wantOutcome: "skip-no-second-n2-source-address",
			wantObserved: "no second N2 source address configured", wantDetail: "ran.n2_source_ips"},
		{name: "the second address is the first", env: same, want: Skip, wantOutcome: "skip-no-second-n2-source-address",
			wantObserved: "no second N2 source address configured"},
		{name: "the second address is not on this host", env: notLocal, want: Error, wantOutcome: OutcomeError,
			wantObserved: "second N2 source address is not on this host", wantDetail: "192.0.2.77 cannot be bound here", wantSetups: 1},
		{name: "stimulus cannot be built", env: noSlices, want: Error, wantOutcome: OutcomeError, wantObserved: "stimulus could not be built"},
		{name: "AMF unreachable", env: unreachable, want: Blocked, wantOutcome: "blocked-ng-setup", wantObserved: "setup failed"},
	}
	seen := 0
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runOne(context.Background(), duplicateRANNodeID{setupWait: 300 * time.Millisecond, firstWait: 300 * time.Millisecond}, tc.env, 5*time.Second)
			if res.Verdict != tc.want || res.Outcome != tc.wantOutcome || res.Observed != tc.wantObserved || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Errorf("= %s %s %q (%s), want %s %s %q with detail containing %q", res.Verdict, res.Outcome, res.Observed, res.Detail, tc.want, tc.wantOutcome, tc.wantObserved, tc.wantDetail)
			}
			if got := Summarize([]Result{res}).OK(); got != (tc.want == Skip) {
				t.Errorf("gate OK = %v for %s", got, tc.want)
			}
			seen += tc.wantSetups
			if n := len(amf.Setups()); n != seen {
				t.Errorf("mock saw %d NG Setups so far, want %d", n, seen)
			}
			deadline := time.Now().Add(3 * time.Second)
			for front.Active() != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if n := front.Active(); n != 0 {
				t.Errorf("%d associations still open after the check", n)
			}
		})
	}
}
