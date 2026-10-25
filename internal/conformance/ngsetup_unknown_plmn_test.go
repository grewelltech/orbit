package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// pduNGSetupResponseServing is an NG SETUP RESPONSE whose PLMN Support List
// names plmn.
func pduNGSetupResponseServing(plmn [3]byte) *ngapType.NGAPPDU {
	pdu := pduNGSetupResponse()
	pdu.SuccessfulOutcome.Value.NGSetupResponse.ProtocolIEs.List = append(
		pdu.SuccessfulOutcome.Value.NGSetupResponse.ProtocolIEs.List, ngapType.NGSetupResponseIEs{
			Id: ngapType.ProtocolIEID{Value: ngapType.ProtocolIEIDPLMNSupportList},
			Value: ngapType.NGSetupResponseIEsValue{Present: ngapType.NGSetupResponseIEsPresentPLMNSupportList,
				PLMNSupportList: &ngapType.PLMNSupportList{List: []ngapType.PLMNSupportItem{
					{PLMNIdentity: ngapType.PLMNIdentity{Value: aper.OctetString(plmn[:])}}}}}})
	return pdu
}

func pduNGSetupFailureCause(c ngapType.Cause) *ngapType.NGAPPDU {
	pdu := pduNGSetupFailure(false)
	pdu.UnsuccessfulOutcome.Value.NGSetupFailure.ProtocolIEs.List = append(
		pdu.UnsuccessfulOutcome.Value.NGSetupFailure.ProtocolIEs.List, ngapType.NGSetupFailureIEs{
			Id:    ngapType.ProtocolIEID{Value: ngapType.ProtocolIEIDCause},
			Value: ngapType.NGSetupFailureIEsValue{Present: ngapType.NGSetupFailureIEsPresentCause, Cause: &c}})
	return pdu
}

// NG SETUP FAILURE is PASS whatever its cause, which is recorded; anything
// else misses the 8.7.1.4 shall and is FAIL, with Observed keeping
// acceptance, another PDU, a silent drop and a reset apart.
func TestClassifyUnknownPLMN(t *testing.T) {
	unknownPLMN := ngapType.Cause{Present: ngapType.CausePresentMisc, Misc: &ngapType.CauseMisc{Value: ngapType.CauseMiscPresentUnknownPLMN}}
	other := ngapType.Cause{Present: ngapType.CausePresentProtocol, Protocol: &ngapType.CauseProtocol{Value: 2}}
	presented, _ := ngap.EncodePLMN("999", "99")
	served, _ := ngap.EncodePLMN("208", "93")
	tests := []struct {
		name         string
		resp         *ngapType.NGAPPDU
		err          error
		want         Verdict
		wantObserved string
		wantDetail   string
		wantSurvived bool
	}{
		{name: "failure, unknown PLMN", resp: pduNGSetupFailureCause(unknownPLMN), want: Pass, wantObserved: "NG SETUP FAILURE (rejected)",
			wantDetail: "cause Misc unknown-PLMN-or-SNPN (recorded, not asserted)", wantSurvived: true},
		{name: "failure, another cause", resp: pduNGSetupFailureCause(other), want: Pass, wantObserved: "NG SETUP FAILURE (rejected)",
			wantDetail: "cause Protocol 2", wantSurvived: true},
		{name: "failure, no cause", resp: pduNGSetupFailure(false), want: Pass, wantObserved: "NG SETUP FAILURE (rejected)",
			wantDetail: "cause absent", wantSurvived: true},
		{name: "accepted", resp: pduNGSetupResponse(), want: Fail, wantObserved: "NG SETUP RESPONSE (accepted!)",
			wantDetail: unknownPLMNFailText, wantSurvived: true},
		{name: "accepted, serving another PLMN", resp: pduNGSetupResponseServing(served), want: Fail, wantObserved: "NG SETUP RESPONSE (accepted!)",
			wantDetail: unknownPLMNFailText, wantSurvived: true},
		{name: "accepted, serving the stimulus PLMN", resp: pduNGSetupResponseServing(presented), want: Error, wantObserved: "AMF serves the stimulus PLMN",
			wantDetail: "the AMF serves the stimulus PLMN 999/99; this check cannot run against it", wantSurvived: true},
		{name: "error indication", resp: pduErrorIndication(), want: Fail, wantObserved: "InitiatingMessage(procedure 9)",
			wantDetail: "1.234s after the NG SETUP REQUEST instead of NG SETUP FAILURE", wantSurvived: true},
		{name: "silent drop", err: context.DeadlineExceeded, want: Fail, wantObserved: "silently dropped; association survived",
			wantDetail: "no NG SETUP FAILURE within " + ngSetupWait.String(), wantSurvived: true},
		{name: "association reset", err: errReset, want: Fail, wantObserved: "association reset", wantDetail: "connection reset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail, survived := classifyUnknownPLMN(tc.resp, tc.err, 1234*time.Millisecond, presented, "999/99")
			if v != tc.want || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) || survived != tc.wantSurvived {
				t.Errorf("= %s %q %q survived=%v, want %s %q with detail containing %q, survived=%v",
					v, observed, detail, survived, tc.want, tc.wantObserved, tc.wantDetail, tc.wantSurvived)
			}
		})
	}
}

// The stimulus never presents the run's own PLMN.
func TestUnservedPLMN(t *testing.T) {
	for _, tc := range []struct{ mcc, mnc, wantMCC, wantMNC string }{
		{"001", "01", "999", "99"},
		{"999", "99", "998", "98"},
		{"999", "999", "999", "99"},
	} {
		mcc, mnc := unservedPLMN(gnb.Config{MCC: tc.mcc, MNC: tc.mnc})
		if mcc != tc.wantMCC || mnc != tc.wantMNC {
			t.Errorf("served %s/%s: presented %s/%s, want %s/%s", tc.mcc, tc.mnc, mcc, mnc, tc.wantMCC, tc.wantMNC)
		}
	}
}

// Against the mock AMF over real SCTP: a core that rejects the unserved PLMN
// PASSes, one that accepts it anyway FAILs with the issue's message, and
// either way the check's associations close and the liveness probe (with
// the served PLMN) succeeds.
func TestNGSetupUnknownPLMNAgainstMock(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	tests := []struct {
		name         string
		accept       bool
		extra        []mockamf.PLMN
		want         Verdict
		wantObserved string
		wantDetail   string
	}{
		{name: "rejects", want: Pass, wantObserved: "NG SETUP FAILURE (rejected)", wantDetail: "cause Misc unknown-PLMN-or-SNPN"},
		{name: "accepts anyway", accept: true, want: Fail, wantObserved: "NG SETUP RESPONSE (accepted!)", wantDetail: unknownPLMNFailText},
		// A core that also serves the stimulus PLMN cannot be tested: ERROR,
		// never a false FAIL.
		{name: "serves the stimulus PLMN too", extra: []mockamf.PLMN{{MCC: "999", MNC: "99"}}, want: Error,
			wantObserved: "AMF serves the stimulus PLMN", wantDetail: "the AMF serves the stimulus PLMN 999/99"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203", AcceptUnknownPLMN: tc.accept, ExtraPLMNs: tc.extra})
			if err != nil {
				t.Fatal(err)
			}
			front := sctpfront.Start(t, amf)
			env := Env{AMFAddr: front.Addr,
				GNB: gnb.Config{ID: 0x400, IDBits: 24, Name: "orbit-conf", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}}
			res := runOne(context.Background(), ngSetupUnknownPLMN{}, env, 10*time.Second)
			if res.Verdict != tc.want || res.Observed != tc.wantObserved || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Fatalf("= %s %q (%s), want %s %q", res.Verdict, res.Observed, res.Detail, tc.want, tc.wantObserved)
			}
			if !strings.Contains(res.Detail, "presented PLMN 999/99, TAC 257 (served: 208/93, TAC 1)") {
				t.Errorf("detail does not say what was presented: %q", res.Detail)
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

// A probe identity the AMF does not serve (a mistyped --mcc/--mnc) is the
// operator's problem, not a crash: the AMF answers the liveness probe with
// NG SETUP FAILURE, and both NG Setup checks report ERROR "liveness probe
// could not run", which gates and cannot be listed, never "AMF
// unresponsive".
func TestNGSetupChecksMistypedPLMNIsError(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	env := Env{AMFAddr: front.Addr,
		GNB: gnb.Config{ID: 0x400, IDBits: 24, Name: "orbit-conf", MCC: "001", MNC: "01", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}}
	for _, c := range []Test{ngSetupUnknownPLMN{}, ngSetupMissingMandatoryIE{}} {
		t.Run(c.ID(), func(t *testing.T) {
			res := runOne(context.Background(), c, env, 10*time.Second)
			if res.Verdict != Error || !strings.HasPrefix(res.Observed, ObservedProbeFailed) ||
				!strings.Contains(res.Detail, "liveness probe rejected by the AMF") || !strings.Contains(res.Detail, "check --mcc/--mnc/--tac/--sd") {
				t.Errorf("= %s %q (%s), want ERROR from the rejected probe", res.Verdict, res.Observed, res.Detail)
			}
		})
	}
}
