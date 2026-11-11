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
	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// Every answer to the valid update: the ACKNOWLEDGE is INFO; a FAILURE, an
// ERROR INDICATION, no answer and a lost association are each a WARN for
// availability with the issue's message.
func TestClassifyRANConfigAck(t *testing.T) {
	failure := pduRANConfigUpdateFailure()
	failure.UnsuccessfulOutcome.Value.RANConfigurationUpdateFailure.ProtocolIEs.List = []ngapType.RANConfigurationUpdateFailureIEs{{
		Id: ngapType.ProtocolIEID{Value: ngapType.ProtocolIEIDCause},
		Value: ngapType.RANConfigurationUpdateFailureIEsValue{Present: ngapType.RANConfigurationUpdateFailureIEsPresentCause,
			Cause: &ngapType.Cause{Present: ngapType.CausePresentMisc, Misc: &ngapType.CauseMisc{Value: ngapType.CauseMiscPresentUnspecified}}},
	}}
	tests := []struct {
		name         string
		resp         *ngapType.NGAPPDU
		err          error
		want         Outcome
		wantObserved string
		wantDetail   string
	}{
		{name: "acknowledge", resp: pduRANConfigUpdateAcknowledge(), want: ranConfigAcked, wantObserved: "RAN CONFIGURATION UPDATE ACKNOWLEDGE",
			wantDetail: "acknowledged 1.234s after the RAN CONFIGURATION UPDATE"},
		{name: "failure with a cause", resp: failure, want: ranConfigRejected, wantObserved: "RAN CONFIGURATION UPDATE FAILURE",
			wantDetail: "answered with RAN CONFIGURATION UPDATE FAILURE, cause Misc 5, 1.234s after"},
		{name: "failure without a cause", resp: pduRANConfigUpdateFailure(), want: ranConfigRejected, wantObserved: "RAN CONFIGURATION UPDATE FAILURE",
			wantDetail: "cause absent"},
		{name: "error indication", resp: pduErrorIndication(), want: ranConfigErrorIndicated, wantObserved: "ERROR INDICATION",
			wantDetail: "answered with ERROR INDICATION, cause absent, 1.234s after"},
		{name: "no answer", err: context.DeadlineExceeded, want: ranConfigNoAnswer, wantObserved: "no answer; association survived",
			wantDetail: "no RAN CONFIGURATION UPDATE ACKNOWLEDGE or FAILURE within 5s of the RAN CONFIGURATION UPDATE (absence inferred within this bound)"},
		{name: "association lost", err: io.EOF, want: ranConfigAssociationLost, wantObserved: "association reset",
			wantDetail: "the association was lost 1.234s after the RAN CONFIGURATION UPDATE (EOF)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, observed, detail := classifyRANConfigAck(tc.resp, tc.err, 1234*time.Millisecond, ranConfigAckWait)
			if covers(o).ID != tc.want.ID || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("= %s %q %q, want %s %q with detail containing %q", o.ID, observed, detail, tc.want.ID, tc.wantObserved, tc.wantDetail)
			}
			if want := o.ID != ranConfigAcked.ID; (o.Verdict == Warn && o.Criterion == CriterionAvailability) != want {
				t.Errorf("%s: %s (%s); every answer but the ACKNOWLEDGE is WARN for availability", o.ID, o.Verdict, o.Criterion)
			}
			if (o.Verdict == Warn) != strings.Contains(detail, ranConfigAckText) {
				t.Errorf("detail %q: the issue's message belongs on every outcome but the ACKNOWLEDGE", detail)
			}
		})
	}
	if ranConfigAckText != "no RAN CONFIGURATION UPDATE ACKNOWLEDGE (class-1 procedure, Table 8.1-1). TS 38.413 8.7.2.2 (descriptive; Rel-15 onward). Likely cause: AMF does not implement RAN Configuration Update." {
		t.Errorf("the message differs from the issue's: %q", ranConfigAckText)
	}
}

// scriptedConn is a Transport that hands out queued PDUs and then err.
type scriptedConn struct {
	pdus []*ngapType.NGAPPDU
	err  error
}

func (s *scriptedConn) WriteNGAP(uint16, []byte) error { return nil }
func (s *scriptedConn) Close() error                   { return nil }
func (s *scriptedConn) ReadMsg([]byte) ([]byte, uint16, uint32, error) {
	if len(s.pdus) == 0 {
		return nil, 0, 0, s.err
	}
	p := s.pdus[0]
	s.pdus = s.pdus[1:]
	b, err := ngap.Encode(*p)
	return b, 0, sctp.PPIDNGAP, err
}

// PDUs of other procedures are passed over and counted; the first outcome
// of the update, or an ERROR INDICATION, ends the read.
func TestReadRANConfigOutcome(t *testing.T) {
	tests := []struct {
		name       string
		pdus       []*ngapType.NGAPPDU
		err        error
		wantAck    bool
		wantPassed int
		wantErr    error
	}{
		{name: "acknowledge at once", pdus: []*ngapType.NGAPPDU{pduRANConfigUpdateAcknowledge()}, wantAck: true},
		{name: "another PDU first", pdus: []*ngapType.NGAPPDU{pduNGSetupResponse(), pduRANConfigUpdateAcknowledge()}, wantAck: true, wantPassed: 1},
		{name: "only another PDU, then the deadline", pdus: []*ngapType.NGAPPDU{pduNGSetupResponse()}, err: context.DeadlineExceeded,
			wantPassed: 1, wantErr: context.DeadlineExceeded},
		{name: "association lost", err: io.EOF, wantErr: io.EOF},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, passed, err := readRANConfigOutcome(context.Background(), &scriptedConn{pdus: tc.pdus, err: tc.err})
			if passed != tc.wantPassed || (tc.wantErr == nil) != (err == nil) || tc.wantErr != nil && !strings.Contains(err.Error(), tc.wantErr.Error()) {
				t.Fatalf("passed %d err %v, want %d and %v", passed, err, tc.wantPassed, tc.wantErr)
			}
			if got := err == nil && gnb.IsRANConfigurationUpdateAcknowledge(resp); got != tc.wantAck {
				t.Errorf("acknowledge = %v, want %v", got, tc.wantAck)
			}
		})
	}
}

// The check cites descriptive text only: its worst outcome is a WARN, the
// acknowledge is INFO, and 8.7.2.2 is the primary reference.
func TestRANConfigAckDeclaration(t *testing.T) {
	c := ranConfigUpdateAck{}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	if WorstVerdict(c) != Warn || ranConfigAcked.Verdict != Info || MinRelease(c) != 15 {
		t.Errorf("worst %s, acknowledged %s, min %s", WorstVerdict(c), ranConfigAcked.Verdict, MinRelease(c))
	}
	for _, r := range c.SpecRefs() {
		if r.Strength != Descriptive || !r.Primary {
			t.Errorf("%s %s is cited as %s, primary %v", r.Spec, r.Clause, r.Strength, r.Primary)
		}
	}
	if n := NeedsOf(c); len(n.Procedures) != 1 || n.Procedures[0] != ProcNGSetup {
		t.Errorf("needs %v, want the NG Setup", n.Procedures)
	}
}

// Against the mock AMF over real SCTP, as a suite run scores it: an AMF
// that acknowledges is INFO; one that answers with FAILURE and one that
// withholds any answer are WARN for availability. One that stops answering
// did not survive: CRASH. The update restates the Supported TA List, and the
// check's associations close afterwards.
func TestRANConfigAckAgainstMock(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	tests := []struct {
		name         string
		answer       mockamf.RANConfigUpdateAnswer
		want         Verdict
		wantOutcome  string
		wantObserved string
		wantDetail   []string
		wantSetups   int
		wantAlive    bool
	}{
		{name: "acknowledges", answer: mockamf.AcknowledgeRANConfigUpdate, want: Info, wantOutcome: "acknowledged", wantObserved: "RAN CONFIGURATION UPDATE ACKNOWLEDGE",
			wantDetail: []string{"acknowledged", "restating the NG Setup's Supported TA List (TAC 1, PLMN 208/93)"}, wantSetups: 1, wantAlive: true},
		{name: "answers with failure", answer: mockamf.FailRANConfigUpdate, want: Warn, wantOutcome: "failure", wantObserved: "RAN CONFIGURATION UPDATE FAILURE",
			wantDetail: []string{"cause Misc 5", ranConfigAckText, "a fresh NG Setup afterwards completes", "no requirement violated; WARN for availability impact"}, wantSetups: 2, wantAlive: true},
		{name: "withholds the answer", answer: mockamf.IgnoreRANConfigUpdate, want: Warn, wantOutcome: "no-answer", wantObserved: "no answer; association survived",
			wantDetail: []string{"within 300ms", ranConfigAckText, "a fresh NG Setup afterwards completes"}, wantSetups: 2, wantAlive: true},
		{name: "stops answering", answer: mockamf.CrashOnRANConfigUpdate, want: Crash, wantOutcome: OutcomeCrash,
			wantObserved: "AMF unresponsive after the message (association reset)",
			wantDetail:   []string{ranConfigAckText, "core did not survive the RAN CONFIGURATION UPDATE: the AMF no longer completes a fresh NG Setup"}, wantSetups: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203"}, mockamf.WithRANConfigUpdate(tc.answer))
			if err != nil {
				t.Fatal(err)
			}
			front := sctpfront.Start(t, amf)
			env := Env{AMFAddr: front.Addr, GNB: gnb.Config{ID: 0x500, IDBits: 24, Name: "orbit-conf", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}}
			reg := &Registry{}
			reg.Register(ranConfigUpdateAck{wait: 300 * time.Millisecond})
			results := reg.Run(context.Background(), env, 10*time.Second)
			res := results[0]
			if res.Verdict != tc.want || res.Outcome != tc.wantOutcome || res.Observed != tc.wantObserved {
				t.Fatalf("= %s %s %q (%s), want %s %s %q", res.Verdict, res.Outcome, res.Observed, res.Detail, tc.want, tc.wantOutcome, tc.wantObserved)
			}
			for _, w := range append(tc.wantDetail, "requires Rel-15 or later; TS 38.413 8.7.2.2") {
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

			updates := amf.RANConfigUpdates()
			if len(updates) != 1 || len(updates[0].IEs) != 1 || !updates[0].HasIE(ngapType.ProtocolIEIDSupportedTAList) {
				t.Errorf("mock received updates %+v, want one carrying only the Supported TA List", updates)
			}
			if setups := amf.Setups(); len(setups) != tc.wantSetups {
				t.Errorf("mock saw %d NG Setups, want %d", len(setups), tc.wantSetups)
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

// What stops the check before the update gates: a stimulus that cannot be
// built is ERROR, and an AMF that does not complete the check's NG Setup is
// BLOCKED on it.
func TestRANConfigAckSetupErrors(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	good := gnb.Config{ID: 0x500, IDBits: 24, Name: "orbit-conf", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	noSlices := good
	noSlices.Slices = nil
	tests := []struct {
		name         string
		env          Env
		want         Verdict
		wantObserved string
	}{
		{name: "stimulus cannot be built", env: Env{AMFAddr: front.Addr, GNB: noSlices}, want: Error, wantObserved: "stimulus could not be built"},
		{name: "AMF unreachable", env: Env{AMFAddr: "127.0.0.1:1", GNB: good}, want: Blocked, wantObserved: "setup failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runOne(context.Background(), ranConfigUpdateAck{wait: 300 * time.Millisecond}, tc.env, 5*time.Second)
			if res.Verdict != tc.want || res.Observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", res.Verdict, res.Observed, res.Detail, tc.want, tc.wantObserved)
			}
			if tc.want == Blocked && res.BlockedOn != ProcNGSetup {
				t.Errorf("blocked on %q, want the NG Setup", res.BlockedOn)
			}
			if Summarize([]Result{res}).OK() {
				t.Errorf("%s did not gate", res.Verdict)
			}
			if n := len(amf.RANConfigUpdates()); n != 0 {
				t.Errorf("mock received %d RAN CONFIGURATION UPDATEs, want none", n)
			}
		})
	}
}
