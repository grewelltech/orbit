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
// misses the shall and is FAIL, with its own outcome for a silent drop,
// another PDU, acceptance and a reset; the silent-drop detail names
// ngSetupWait.
func TestClassifyMissingTAList(t *testing.T) {
	tests := []struct {
		name         string
		resp         *ngapType.NGAPPDU
		err          error
		want         Outcome
		wantObserved string
		wantDetail   string
		wantSurvived bool
	}{
		{name: "setup failure with criticality diagnostics", resp: pduNGSetupFailure(true), want: missingTAListRejected, wantObserved: "NG SETUP FAILURE (rejected)", wantDetail: "Criticality Diagnostics present", wantSurvived: true},
		{name: "setup failure without criticality diagnostics", resp: pduNGSetupFailure(false), want: missingTAListRejected, wantObserved: "NG SETUP FAILURE (rejected)", wantDetail: "Criticality Diagnostics absent", wantSurvived: true},
		{name: "accepted", resp: pduNGSetupResponse(), want: missingTAListAccepted, wantObserved: "NG SETUP RESPONSE (accepted!)", wantDetail: "accepted an NG Setup", wantSurvived: true},
		{name: "error indication", resp: pduErrorIndication(), want: missingTAListOtherPDU, wantObserved: "InitiatingMessage(procedure 9)",
			wantDetail: "1.234s after the NG SETUP REQUEST instead of NG SETUP FAILURE", wantSurvived: true},
		{name: "another PDU", resp: pduDownlinkNAS(), want: missingTAListOtherPDU, wantObserved: "InitiatingMessage(procedure 4)", wantDetail: "instead of NG SETUP FAILURE", wantSurvived: true},
		{name: "silent drop", err: context.DeadlineExceeded, want: missingTAListSilent, wantObserved: "silently dropped; association survived",
			wantDetail: "no NG SETUP FAILURE within " + ngSetupWait.String() + " of the NG SETUP REQUEST (absence inferred within this bound)", wantSurvived: true},
		{name: "association reset", err: errReset, want: missingTAListReset, wantObserved: "association reset", wantDetail: "connection reset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, observed, detail, survived := classifyMissingTAList(tc.resp, tc.err, 1234*time.Millisecond)
			if covers(o).ID != tc.want.ID || o.Verdict != tc.want.Verdict || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) || survived != tc.wantSurvived {
				t.Errorf("= %s (%s) %q %q survived=%v, want %s (%s) %q with detail containing %q, survived=%v",
					o.ID, o.Verdict, observed, detail, survived, tc.want.ID, tc.want.Verdict, tc.wantObserved, tc.wantDetail, tc.wantSurvived)
			}
		})
	}
	for _, o := range []Outcome{missingTAListAccepted, missingTAListOtherPDU, missingTAListSilent, missingTAListReset} {
		if o.Verdict != Fail || len(o.Refs) != 1 || o.Refs[0] != missingTAListRef {
			t.Errorf("%s: %s citing %v, want FAIL citing the shall of 10.3.5", o.ID, o.Verdict, o.Refs)
		}
	}
}

// runOne holds every result to the check's declaration: a check that
// declares no outcomes cannot report a verdict on a core, and a result whose
// verdict is not its outcome's is an ERROR.
func TestRunOneHoldsResultsToTheDeclaration(t *testing.T) {
	shall := SpecRef{Spec: "TS 38.413", Clause: "10.6", Strength: Shall, Since: 15, WordingUnchanged: true, Primary: true}
	tests := []struct {
		name       string
		test       Test
		want       Verdict
		wantDetail string
	}{
		{name: "a declared FAIL", test: provTest{id: "X", refs: []SpecRef{shall}, result: Result{Verdict: Fail}}, want: Fail},
		{name: "a check that declares nothing", test: undeclaredTest{id: "X", refs: []SpecRef{shall}}, want: Error,
			wantDetail: "without one of the check's declared outcomes"},
		{name: "a DEVIATION from a check citing no should", test: provTest{id: "X", refs: []SpecRef{shall}, result: Result{Verdict: Deviation}}, want: Error,
			wantDetail: "which the check neither declares nor has from its needs"},
		{name: "an outcome with another verdict", test: provTest{id: "X", refs: []SpecRef{shall}, result: Result{Verdict: Pass, Outcome: verdictOutcomeID(Fail)}}, want: Error,
			wantDetail: "it is declared FAIL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runOne(context.Background(), tc.test, Env{}, time.Second)
			if res.Verdict != tc.want || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Errorf("= %s (%s), want %s with detail containing %q", res.Verdict, res.Detail, tc.want, tc.wantDetail)
			}
		})
	}
}
