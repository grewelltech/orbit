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

// NGAP-NG-SETUP reports the answer to a valid NG SETUP REQUEST: the
// response 8.7.1.2 describes is INFO; a rejection violates nothing and is
// WARN for availability; anything that is neither is a DEVIATION from the
// should of 8.7.1.3. Every outcome but the response reports the NG Setup as
// not completed.
func TestClassifyNGSetup(t *testing.T) {
	tests := []struct {
		name         string
		resp         *ngapType.NGAPPDU
		err          error
		want         Outcome
		wantObserved string
		wantDetail   string
	}{
		{name: "response", resp: pduNGSetupResponse(), want: ngSetupAccepted, wantObserved: "NG SETUP RESPONSE", wantDetail: "accepted 1.234s after the NG SETUP REQUEST"},
		{name: "failure", resp: pduNGSetupFailure(false), want: ngSetupRejected, wantObserved: "NG SETUP FAILURE",
			wantDetail: "rejected 1.234s after the NG SETUP REQUEST, cause absent; TS 38.413 8.7.1.3 lets an AMF that cannot accept the setup reject it"},
		{name: "another PDU", resp: pduErrorIndication(), want: ngSetupOtherPDU, wantObserved: "InitiatingMessage(procedure 9)",
			wantDetail: "neither NG SETUP RESPONSE nor the NG SETUP FAILURE of 8.7.1.3"},
		{name: "no answer", err: context.DeadlineExceeded, want: ngSetupUnanswered, wantObserved: "no answer; association survived",
			wantDetail: "within " + ngSetupWait.String() + " of the NG SETUP REQUEST (absence inferred within this bound)"},
		{name: "association lost", err: io.EOF, want: ngSetupAssociationLost, wantObserved: "association reset", wantDetail: "the association was lost 1.234s after the NG SETUP REQUEST (EOF)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, observed, detail := classifyNGSetup(tc.resp, tc.err, 1234*time.Millisecond)
			if covers(o).ID != tc.want.ID || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("= %s %q %q, want %s %q with detail containing %q", o.ID, observed, detail, tc.want.ID, tc.wantObserved, tc.wantDetail)
			}
			if completed := o.NotCompleted == ""; completed != (tc.want.ID == ngSetupAccepted.ID) {
				t.Errorf("%s: NotCompleted = %q; only the response completes the NG Setup", o.ID, o.NotCompleted)
			}
		})
	}
}

// The declaration: an INFO for the described answer, a WARN for
// availability, and DEVIATIONs that cite the should; the check owns the NG
// Setup and needs no procedure first.
func TestNGSetupCheckDeclaration(t *testing.T) {
	c := ngSetupCompletes{}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	if got := OwnsOf(c); len(got) != 1 || got[0] != ProcNGSetup || len(NeedsOf(c).Procedures) != 0 {
		t.Errorf("owns %v, needs %v; want the NG Setup owned and nothing needed", got, NeedsOf(c).Procedures)
	}
	if WorstVerdict(c) != Deviation || ngSetupAccepted.Verdict != Info || ngSetupRejected.Verdict != Warn || ngSetupRejected.Criterion != CriterionAvailability {
		t.Errorf("worst %s, accepted %s, rejected %s (%s)", WorstVerdict(c), ngSetupAccepted.Verdict, ngSetupRejected.Verdict, ngSetupRejected.Criterion)
	}
}

// Against the mock AMF over real SCTP, as a suite run scores it: an AMF
// that serves the presented PLMN accepts (INFO), one that does not rejects
// (WARN, with the cause), and the association is closed afterwards.
func TestNGSetupCheckAgainstMock(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	tests := []struct {
		name         string
		mcc, mnc     string
		want         Verdict
		wantOutcome  string
		wantObserved string
		wantDetail   []string
	}{
		{name: "accepts", mcc: "208", mnc: "93", want: Info, wantOutcome: "accepted", wantObserved: "NG SETUP RESPONSE",
			wantDetail: []string{"presented gNB ID 1024, PLMN 208/93, TAC 1"}},
		{name: "rejects", mcc: "001", mnc: "01", want: Warn, wantOutcome: "rejected", wantObserved: "NG SETUP FAILURE",
			wantDetail: []string{"cause Misc unknown-PLMN-or-SNPN", "no requirement violated; WARN for availability impact", "presented gNB ID 1024, PLMN 001/01, TAC 1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203"})
			if err != nil {
				t.Fatal(err)
			}
			front := sctpfront.Start(t, amf)
			env := Env{AMFAddr: front.Addr,
				GNB: gnb.Config{ID: 0x400, IDBits: 24, Name: "orbit-conf", MCC: tc.mcc, MNC: tc.mnc, TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}}
			res := runOne(context.Background(), ngSetupCompletes{}, env, 10*time.Second)
			if res.Verdict != tc.want || res.Outcome != tc.wantOutcome || res.Observed != tc.wantObserved {
				t.Fatalf("= %s %s %q (%s), want %s %s %q", res.Verdict, res.Outcome, res.Observed, res.Detail, tc.want, tc.wantOutcome, tc.wantObserved)
			}
			for _, w := range append(tc.wantDetail, "requires Rel-15 or later; TS 38.413 8.7.1.2") {
				if !strings.Contains(res.Detail, w) {
					t.Errorf("detail %q lacks %q", res.Detail, w)
				}
			}
			if notCompleted := res.NotCompleted == ProcNGSetup; notCompleted != (tc.want == Warn) {
				t.Errorf("NotCompleted = %q for %s", res.NotCompleted, tc.want)
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
