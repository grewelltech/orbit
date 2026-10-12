package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"

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
// PASS only for it, DEVIATION when the core survives without it, FAIL on a
// reset.
func TestClassifyUnknownUE(t *testing.T) {
	tests := []struct {
		name       string
		resp       *ngapType.NGAPPDU
		err        error
		want       Verdict
		wantDetail string
	}{
		{name: "error indication", resp: pduErrorIndication(), want: Pass},
		{name: "another PDU", resp: pduDownlinkNAS(), want: Deviation, wantDetail: "instead of an ERROR INDICATION"},
		{name: "silent drop", err: context.DeadlineExceeded, want: Deviation, wantDetail: "no ERROR INDICATION within the wait"},
		{name: "association reset", err: errReset, want: Fail, wantDetail: "connection reset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := classifyUnknownUE(tc.resp, tc.err)
			if v != tc.want || observed == "" || !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("= %s %q %q, want %s with detail containing %q", v, observed, detail, tc.want, tc.wantDetail)
			}
		})
	}
}

// NGAP-NGSETUP-MISSING-TALIST asserts the NG SETUP FAILURE 10.3.5 requires:
// PASS for it (Criticality Diagnostics recorded either way), DEVIATION when
// the core survives without it (including an ERROR INDICATION), FAIL for
// acceptance or a reset.
func TestClassifyMissingTAList(t *testing.T) {
	tests := []struct {
		name       string
		resp       *ngapType.NGAPPDU
		err        error
		want       Verdict
		wantDetail string
	}{
		{name: "setup failure with criticality diagnostics", resp: pduNGSetupFailure(true), want: Pass, wantDetail: "Criticality Diagnostics present"},
		{name: "setup failure without criticality diagnostics", resp: pduNGSetupFailure(false), want: Pass, wantDetail: "Criticality Diagnostics absent"},
		{name: "accepted", resp: pduNGSetupResponse(), want: Fail, wantDetail: "accepted an NG Setup"},
		{name: "error indication", resp: pduErrorIndication(), want: Deviation, wantDetail: "instead of NG SETUP FAILURE"},
		{name: "another PDU", resp: pduDownlinkNAS(), want: Deviation, wantDetail: "instead of NG SETUP FAILURE"},
		{name: "silent drop", err: context.DeadlineExceeded, want: Deviation, wantDetail: "no NG SETUP FAILURE within the wait"},
		{name: "association reset", err: errReset, want: Fail, wantDetail: "connection reset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := classifyMissingTAList(tc.resp, tc.err)
			if v != tc.want || observed == "" || !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("= %s %q %q, want %s with detail containing %q", v, observed, detail, tc.want, tc.wantDetail)
			}
		})
	}
}

// A FAIL-class check may report DEVIATION: runOne leaves it as it is.
func TestRunOneFailClassMayDeviate(t *testing.T) {
	c := provTest{id: "X", refs: []SpecRef{goodRef()}, class: Fail, result: Result{Verdict: Deviation}}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	if res := runOne(context.Background(), c, Env{}, 0); res.Verdict != Deviation {
		t.Errorf("verdict = %s, want DEVIATION", res.Verdict)
	}
}
