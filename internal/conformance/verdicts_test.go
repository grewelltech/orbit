package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"
)

func pduErrorIndication() *ngapType.NGAPPDU {
	return &ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentInitiatingMessage,
		InitiatingMessage: &ngapType.InitiatingMessage{ProcedureCode: ngapType.ProcedureCode{Value: ngapType.ProcedureCodeErrorIndication},
			Value: ngapType.InitiatingMessageValue{Present: ngapType.InitiatingMessagePresentErrorIndication, ErrorIndication: &ngapType.ErrorIndication{}}}}
}

func pduDownlinkNAS() *ngapType.NGAPPDU {
	return &ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentInitiatingMessage,
		InitiatingMessage: &ngapType.InitiatingMessage{ProcedureCode: ngapType.ProcedureCode{Value: ngapType.ProcedureCodeDownlinkNASTransport},
			Value: ngapType.InitiatingMessageValue{Present: ngapType.InitiatingMessagePresentDownlinkNASTransport, DownlinkNASTransport: &ngapType.DownlinkNASTransport{}}}}
}

func pduNGSetupFailure(withCD bool) *ngapType.NGAPPDU {
	f := &ngapType.NGSetupFailure{}
	if withCD {
		f.ProtocolIEs.List = append(f.ProtocolIEs.List, ngapType.NGSetupFailureIEs{
			Id:    ngapType.ProtocolIEID{Value: ngapType.ProtocolIEIDCriticalityDiagnostics},
			Value: ngapType.NGSetupFailureIEsValue{Present: ngapType.NGSetupFailureIEsPresentCriticalityDiagnostics, CriticalityDiagnostics: &ngapType.CriticalityDiagnostics{}}})
	}
	return &ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentUnsuccessfulOutcome,
		UnsuccessfulOutcome: &ngapType.UnsuccessfulOutcome{ProcedureCode: ngapType.ProcedureCode{Value: ngapType.ProcedureCodeNGSetup},
			Value: ngapType.UnsuccessfulOutcomeValue{Present: ngapType.UnsuccessfulOutcomePresentNGSetupFailure, NGSetupFailure: f}}}
}

func pduNGSetupResponse() *ngapType.NGAPPDU {
	return &ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentSuccessfulOutcome,
		SuccessfulOutcome: &ngapType.SuccessfulOutcome{ProcedureCode: ngapType.ProcedureCode{Value: ngapType.ProcedureCodeNGSetup},
			Value: ngapType.SuccessfulOutcomeValue{Present: ngapType.SuccessfulOutcomePresentNGSetupResponse, NGSetupResponse: &ngapType.NGSetupResponse{}}}}
}

var errReset = errors.New("sctp read: connection reset by peer")

// NGAP-UNKNOWN-UE-SURVIVES asserts the ERROR INDICATION 10.6 requires:
// PASS only for it. Surviving without it (another PDU, a silent drop) misses
// the shall and is FAIL, as a reset is, with Observed keeping the three
// apart. The silent-drop detail names the bound from unknownUEWait, and
// the other-PDU detail says how long after the stimulus it came (#161).
func TestClassifyUnknownUE(t *testing.T) {
	tests := []struct {
		name         string
		resp         *ngapType.NGAPPDU
		err          error
		want         Verdict
		wantObserved string
		wantDetail   string
		wantSurvived bool
	}{
		{name: "error indication", resp: pduErrorIndication(), want: Pass, wantObserved: "ERROR INDICATION", wantSurvived: true},
		{name: "another PDU", resp: pduDownlinkNAS(), want: Fail, wantObserved: "InitiatingMessage(procedure 4)",
			wantDetail: "1.234s after the UPLINK NAS TRANSPORT instead of an ERROR INDICATION", wantSurvived: true},
		{name: "silent drop", err: context.DeadlineExceeded, want: Fail, wantObserved: "silently dropped; association survived",
			wantDetail: "no ERROR INDICATION within " + unknownUEWait.String() + " of the UPLINK NAS TRANSPORT (absence inferred within this bound)", wantSurvived: true},
		{name: "association reset", err: errReset, want: Fail, wantObserved: "association reset", wantDetail: "connection reset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, observed, detail, survived := classifyUnknownUE(tc.resp, tc.err, 1234*time.Millisecond)
			if v := covers(o).Verdict; v != tc.want || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) || survived != tc.wantSurvived {
				t.Errorf("= %s %q %q survived=%v, want %s %q with detail containing %q, survived=%v",
					v, observed, detail, survived, tc.want, tc.wantObserved, tc.wantDetail, tc.wantSurvived)
			}
		})
	}
	if unknownUEWait != 3*time.Second {
		t.Errorf("unknownUEWait = %s; the bound the detail names changed", unknownUEWait)
	}
}

func pduNGResetAcknowledge() *ngapType.NGAPPDU {
	return &ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentSuccessfulOutcome,
		SuccessfulOutcome: &ngapType.SuccessfulOutcome{ProcedureCode: ngapType.ProcedureCode{Value: ngapType.ProcedureCodeNGReset},
			Value: ngapType.SuccessfulOutcomeValue{Present: ngapType.SuccessfulOutcomePresentNGResetAcknowledge, NGResetAcknowledge: &ngapType.NGResetAcknowledge{}}}}
}

// NGAP-NG-RESET-ACK asserts the NG RESET ACKNOWLEDGE 8.7.4.2.2 requires:
// PASS only for it. Another PDU, no answer within ngResetWait and a lost
// association each miss the shall and are FAIL, with their own outcome.
func TestClassifyNGReset(t *testing.T) {
	tests := []struct {
		name         string
		resp         *ngapType.NGAPPDU
		err          error
		want         Outcome
		wantObserved string
		wantDetail   string
	}{
		{name: "acknowledge", resp: pduNGResetAcknowledge(), want: ngResetAcked, wantObserved: "NG RESET ACKNOWLEDGE"},
		{name: "another PDU", resp: pduErrorIndication(), want: ngResetOtherPDU, wantObserved: "InitiatingMessage(procedure 9)", wantDetail: "expected NG RESET ACKNOWLEDGE"},
		{name: "no answer", err: context.DeadlineExceeded, want: ngResetNoAnswer, wantObserved: "no response",
			wantDetail: "no NG RESET ACKNOWLEDGE within " + ngResetWait.String() + " of the NG RESET (absence inferred within this bound)"},
		{name: "association lost", err: errReset, want: ngResetAssociationLost, wantObserved: "association reset", wantDetail: "connection reset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, observed, detail := classifyNGReset(tc.resp, tc.err)
			if covers(o).ID != tc.want.ID || o.Verdict != tc.want.Verdict || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("= %s (%s) %q %q, want %s (%s) %q with detail containing %q",
					o.ID, o.Verdict, observed, detail, tc.want.ID, tc.want.Verdict, tc.wantObserved, tc.wantDetail)
			}
		})
	}
	if ngResetAcked.Verdict != Pass || ngResetNoAnswer.Verdict != Fail || ngResetOtherPDU.Verdict != Fail || ngResetAssociationLost.Verdict != Fail {
		t.Error("the verdicts of the NG RESET outcomes changed")
	}
}

// NGAP-NGSETUP-MISSING-TALIST asserts the NG SETUP FAILURE 10.3.5 requires:
// PASS for it (Criticality Diagnostics recorded either way). Everything else
// misses the shall and is FAIL, with Observed keeping a silent drop, another
// PDU, acceptance and a reset apart; the silent-drop detail names
// ngSetupWait.
func TestClassifyMissingTAList(t *testing.T) {
	tests := []struct {
		name         string
		resp         *ngapType.NGAPPDU
		err          error
		want         Verdict
		wantObserved string
		wantDetail   string
		wantSurvived bool
	}{
		{name: "setup failure with criticality diagnostics", resp: pduNGSetupFailure(true), want: Pass, wantObserved: "NG SETUP FAILURE (rejected)", wantDetail: "Criticality Diagnostics present", wantSurvived: true},
		{name: "setup failure without criticality diagnostics", resp: pduNGSetupFailure(false), want: Pass, wantObserved: "NG SETUP FAILURE (rejected)", wantDetail: "Criticality Diagnostics absent", wantSurvived: true},
		{name: "accepted", resp: pduNGSetupResponse(), want: Fail, wantObserved: "NG SETUP RESPONSE (accepted!)", wantDetail: "accepted an NG Setup", wantSurvived: true},
		{name: "error indication", resp: pduErrorIndication(), want: Fail, wantObserved: "InitiatingMessage(procedure 9)",
			wantDetail: "1.234s after the NG SETUP REQUEST instead of NG SETUP FAILURE", wantSurvived: true},
		{name: "another PDU", resp: pduDownlinkNAS(), want: Fail, wantObserved: "InitiatingMessage(procedure 4)", wantDetail: "instead of NG SETUP FAILURE", wantSurvived: true},
		{name: "silent drop", err: context.DeadlineExceeded, want: Fail, wantObserved: "silently dropped; association survived",
			wantDetail: "no NG SETUP FAILURE within " + ngSetupWait.String() + " of the NG SETUP REQUEST (absence inferred within this bound)", wantSurvived: true},
		{name: "association reset", err: errReset, want: Fail, wantObserved: "association reset", wantDetail: "connection reset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail, survived := classifyMissingTAList(tc.resp, tc.err, 1234*time.Millisecond)
			if v != tc.want || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) || survived != tc.wantSurvived {
				t.Errorf("= %s %q %q survived=%v, want %s %q with detail containing %q, survived=%v",
					v, observed, detail, survived, tc.want, tc.wantObserved, tc.wantDetail, tc.wantSurvived)
			}
		})
	}
}

// The worst verdict follows the references: FAIL when a check cites a shall,
// DEVIATION when it cites only shoulds.
func TestWorstVerdict(t *testing.T) {
	ref := func(s Strength) SpecRef { return SpecRef{Spec: "TS 38.413", Clause: "10.6", Strength: s, Since: 15} }
	tests := []struct {
		name string
		refs []SpecRef
		want Verdict
	}{
		{name: "shall only", refs: []SpecRef{ref(Shall)}, want: Fail},
		{name: "should only", refs: []SpecRef{ref(Should)}, want: Deviation},
		{name: "shall and should", refs: []SpecRef{ref(Should), ref(Shall)}, want: Fail},
		{name: "descriptive and shall", refs: []SpecRef{ref(Descriptive), ref(Shall)}, want: Fail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := WorstVerdict(provTest{id: "X", refs: tc.refs}); got != tc.want {
				t.Errorf("WorstVerdict = %s, want %s", got, tc.want)
			}
		})
	}
}

// runOne enforces the rule whatever Run returns: a FAIL from a check citing
// no shall is a DEVIATION, and a DEVIATION from a check citing no should is
// a FAIL that says why; a check citing a should may deviate.
func TestRunOneVerdictRule(t *testing.T) {
	shall := SpecRef{Spec: "TS 38.413", Clause: "10.6", Strength: Shall, Since: 15, WordingUnchanged: true}
	should := SpecRef{Spec: "TS 38.413", Clause: "10.3.5", Strength: Should, Since: 15, WordingUnchanged: true}
	tests := []struct {
		name       string
		refs       []SpecRef
		run        Verdict
		observed   string
		want       Verdict
		wantDetail string
	}{
		{name: "FAIL from a should-only check", refs: []SpecRef{should}, run: Fail, want: Deviation},
		{name: "a crash on a should-only check stays CRASH", refs: []SpecRef{should}, run: Crash, observed: ObservedAMFUnresponsive + " (x)", want: Crash},
		{name: "a dead UPF on a should-only check stays CRASH", refs: []SpecRef{should}, run: Crash, observed: ObservedUPFUnresponsive, want: Crash},
		{name: "DEVIATION from a shall-only check", refs: []SpecRef{shall}, run: Deviation, want: Fail,
			wantDetail: "scored FAIL: DEVIATION is reserved for a missed should"},
		{name: "DEVIATION from a check citing a should", refs: []SpecRef{shall, should}, run: Deviation, want: Deviation},
		{name: "FAIL from a shall check", refs: []SpecRef{shall}, run: Fail, want: Fail},
		{name: "PASS", refs: []SpecRef{shall}, run: Pass, want: Pass},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := provTest{id: "X", refs: tc.refs, result: Result{Verdict: tc.run, Observed: tc.observed}}
			res := runOne(context.Background(), c, Env{}, time.Second)
			if res.Verdict != tc.want {
				t.Errorf("verdict = %s, want %s", res.Verdict, tc.want)
			}
			if tc.wantDetail != "" && !strings.Contains(res.Detail, tc.wantDetail) {
				t.Errorf("detail %q lacks %q", res.Detail, tc.wantDetail)
			}
		})
	}
}

// shouldOnly are the registered checks that cite a should and no shall, so
// their worst verdict is DEVIATION.
var shouldOnly = map[string]bool{
	"NGAP-RAN-CONFIG-UPDATE-TRANSFER-SYNTAX": true,
	"NGAP-UE-CTX-RELEASE-ON-REQUEST":         true,
}

// informational are the registered checks that cite descriptive references
// only and declare themselves informational, so they are never graded.
var informational = map[string]bool{
	"NGAP-RAN-CONFIG-UPDATE-ACK": true,
	"NGAP-DUPLICATE-RAN-NODE-ID": true,
}

// Every registered check's worst verdict follows what it cites: DEVIATION
// for the should-only checks named in shouldOnly, INFO for the
// informational checks named in informational, FAIL for every other. A
// check that changes class changes this on purpose.
func TestEveryCheckWorstVerdict(t *testing.T) {
	for _, c := range NewRegistry().Tests() {
		if _, ok := declared(c); ok {
			continue // its class is its declared outcomes', in checks.md
		}
		want := Fail
		switch {
		case shouldOnly[c.ID()]:
			want = Deviation
		case informational[c.ID()]:
			want = Info
		}
		if got := WorstVerdict(c); got != want {
			t.Errorf("%s: WorstVerdict = %s, want %s; a check that changes class must update this test", c.ID(), got, want)
		}
		if got := IsInformational(c); got != informational[c.ID()] {
			t.Errorf("%s: IsInformational = %v, want %v; a check that changes class must update this test", c.ID(), got, informational[c.ID()])
		}
	}
}
