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
// with its cause, another PDU or nothing (both unanswered, told apart in
// the detail), or a lost association.
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
		{name: "another PDU", resp: pduErrorIndication(), want: "second NG Setup unanswered",
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

// Only an accepted second NG Setup with the first association kept is INFO;
// every other combination is INFO-DIFF, and Observed names both halves.
func TestJudgeDuplicate(t *testing.T) {
	tests := []struct {
		outcome      string
		kept         bool
		want         Verdict
		wantObserved string
	}{
		{duplicateAccepted, true, Info, "second NG Setup accepted; first association kept"},
		{duplicateAccepted, false, infoDiffers, "second NG Setup accepted; first association dropped"},
		{duplicateRejected, true, infoDiffers, "second NG Setup rejected (NG SETUP FAILURE); first association kept"},
		{duplicateRejected, false, infoDiffers, "second NG Setup rejected (NG SETUP FAILURE); first association dropped"},
		{duplicateUnanswered, true, infoDiffers, "second NG Setup unanswered; first association kept"},
		{duplicateUnanswered, false, infoDiffers, "second NG Setup unanswered; first association dropped"},
		{duplicateReset, true, infoDiffers, "second association reset; first association kept"},
		{duplicateReset, false, infoDiffers, "second association reset; first association dropped"},
	}
	for _, tc := range tests {
		t.Run(tc.wantObserved, func(t *testing.T) {
			if v, observed := judgeDuplicate(tc.outcome, tc.kept); v != tc.want || observed != tc.wantObserved {
				t.Errorf("= %s %q, want %s %q", v, observed, tc.want, tc.wantObserved)
			}
		})
	}
}

// The check is informational: registered on descriptive references alone.
func TestDuplicateRANNodeIsInformational(t *testing.T) {
	c := duplicateRANNodeID{}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	if !IsInformational(c) || VerdictClass(c) != Info || MinRelease(c) != 15 {
		t.Errorf("informational %v, class %s, min %s", IsInformational(c), VerdictClass(c), MinRelease(c))
	}
	if n := NeedsOf(c); n.UEs != 0 || len(n.GNBRoles) != 2 {
		t.Errorf("needs = %+v, want no UE and the check's own and the liveness probe's gNB IDs", n)
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
// associations is INFO; one that rejects it, one that accepts it and drops
// the first association and one that leaves it unanswered are INFO-DIFF,
// and none of them fails the gate. One that stops answering did not
// survive: FAIL, and the gate closes. The mock sees the same gNB
// ID twice, and both associations close afterwards.
func TestDuplicateRANNodeAgainstMock(t *testing.T) {
	tests := []struct {
		name         string
		answer       mockamf.DuplicateRANNodeAnswer
		want         Verdict
		wantObserved string
		wantDetail   []string
		wantSetups   int // the two of the check, and the liveness probe's from a running core
		wantAlive    bool
	}{
		{name: "accepts both", answer: mockamf.AcceptDuplicateRANNode, want: Info,
			wantObserved: "second NG Setup accepted; first association kept",
			wantDetail:   []string{"NG SETUP RESPONSE", "the first association did not end within 300ms", "a fresh NG Setup afterwards completes"}, wantSetups: 3, wantAlive: true},
		{name: "rejects the duplicate", answer: mockamf.RejectDuplicateRANNode, want: Info,
			wantObserved: "second NG Setup rejected (NG SETUP FAILURE); first association kept",
			wantDetail:   []string{"NG SETUP FAILURE, cause Misc 5", "a fresh NG Setup afterwards completes"}, wantSetups: 3, wantAlive: true},
		{name: "replaces the first association", answer: mockamf.ReplaceDuplicateRANNode, want: Info,
			wantObserved: "second NG Setup accepted; first association dropped",
			wantDetail:   []string{"the first association ended", "a fresh NG Setup afterwards completes"}, wantSetups: 3, wantAlive: true},
		{name: "leaves the duplicate unanswered", answer: mockamf.IgnoreDuplicateRANNode, want: Info,
			wantObserved: "second NG Setup unanswered; first association kept",
			wantDetail:   []string{"no NG SETUP RESPONSE or FAILURE within 300ms", "a fresh NG Setup afterwards completes"}, wantSetups: 3, wantAlive: true},
		{name: "stops answering", answer: mockamf.CrashOnDuplicateRANNode, want: Crash,
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
			if res.Verdict != tc.want || res.Observed != tc.wantObserved {
				t.Fatalf("= %s %q (%s), want %s %q", res.Verdict, res.Observed, res.Detail, tc.want, tc.wantObserved)
			}
			for _, w := range append(tc.wantDetail, "gNB ID 1536 presented from 127.0.0.1:", " and then from 127.0.0.2:", informationalNote,
				"requires Rel-15 or later; TS 38.413 8.7.1.2") {
				if !strings.Contains(res.Detail, w) {
					t.Errorf("detail %q lacks %q", res.Detail, w)
				}
			}
			crashed := tc.want == Crash
			if sum := Summarize(results); sum.OK() == crashed || (sum.Crash == 1) != crashed || sum.Deviation != 0 || sum.Pass != 0 || sum.Fail != 0 || sum.Crash+sum.Info != 1 {
				t.Errorf("summary %+v: an informational check gates only for a core that did not survive, and never counts as a pass", sum)
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

// Without two source addresses the check SKIPs before it opens anything;
// an AMF it cannot reach is an ERROR, which gates as any ERROR does.
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
		wantObserved string
		wantDetail   string
		wantSetups   int
	}{
		{name: "no second address", env: noSecond, want: Skip, wantObserved: "no second N2 source address configured", wantDetail: "ran.n2_source_ips"},
		{name: "the second address is the first", env: same, want: Skip, wantObserved: "no second N2 source address configured"},
		{name: "the second address is not on this host", env: notLocal, want: Skip, wantObserved: "second N2 source address is not on this host",
			wantDetail: "192.0.2.77 cannot be bound here", wantSetups: 1},
		{name: "stimulus cannot be built", env: noSlices, want: Error, wantObserved: "stimulus could not be built"},
		{name: "AMF unreachable", env: unreachable, want: Error, wantObserved: "setup failed"},
	}
	seen := 0
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runOne(context.Background(), duplicateRANNodeID{setupWait: 300 * time.Millisecond, firstWait: 300 * time.Millisecond}, tc.env, 5*time.Second)
			if res.Verdict != tc.want || res.Observed != tc.wantObserved || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Errorf("= %s %q (%s), want %s %q with detail containing %q", res.Verdict, res.Observed, res.Detail, tc.want, tc.wantObserved, tc.wantDetail)
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
