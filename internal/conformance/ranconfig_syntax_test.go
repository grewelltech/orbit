package conformance

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// The stimulus is the well-formed RAN CONFIGURATION UPDATE carrying one
// Supported TA List, cut off after the Supported TA Item's TAC with the two
// lengths restated: the same bytes whatever the PLMN and slices, which sit
// after the cut. The well-formed message decodes and the stimulus does not.
func TestTruncatedTAListUpdate(t *testing.T) {
	tests := []struct {
		name string
		cfg  gnb.Config
		want string
	}{
		{name: "TAC 1", cfg: gnb.Config{ID: 1, MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}},
			want: "0023000c000001006600050000000001"},
		{name: "another TAC and PLMN, several slices", cfg: gnb.Config{ID: 1, MCC: "001", MNC: "001", TAC: 0xABCDEF,
			Slices: []gnb.SNSSAI{{SST: 1}, {SST: 2, SD: "0a0b0c"}, {SST: 3, SD: "0d0e0f"}}},
			want: "0023000c000001006600050000abcdef"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := truncatedTAListUpdate(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != tc.want {
				t.Fatalf("stimulus = %x, want %s", got, tc.want)
			}
			if pdu, err := ngap.Decode(got); err == nil {
				t.Errorf("the stimulus decodes (%+v); it must be a transfer syntax error", pdu)
			}

			one := tc.cfg
			one.Slices = one.Slices[:1]
			pdu, err := gnb.BuildRANConfigurationUpdate(gnb.WithSupportedTAList(one))
			if err != nil {
				t.Fatal(err)
			}
			valid, err := ngap.Encode(pdu)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ngap.Decode(valid); err != nil {
				t.Fatalf("the well-formed message does not decode: %v", err)
			}
			// Only the two length octets differ from the well-formed
			// message's first bytes.
			for i := range got {
				if i != rcuMessageLen && i != rcuIELen && got[i] != valid[i] {
					t.Errorf("octet %d = %#02x, the well-formed message has %#02x", i, got[i], valid[i])
				}
			}
			if len(valid) <= len(got) {
				t.Errorf("the stimulus (%d octets) is not shorter than the well-formed message (%d)", len(got), len(valid))
			}
		})
	}
}

// A configuration the Supported TA List cannot be built from is an error,
// not a stimulus.
func TestTruncatedTAListUpdateErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  gnb.Config
	}{
		{name: "no slice", cfg: gnb.Config{ID: 1, MCC: "208", MNC: "93", TAC: 1}},
		{name: "bad PLMN", cfg: gnb.Config{ID: 1, MCC: "2", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if b, err := truncatedTAListUpdate(tc.cfg); err == nil {
				t.Errorf("stimulus %x built from an invalid configuration", b)
			}
		})
	}
}

func pduErrorIndicationCause(c *ngapType.Cause) *ngapType.NGAPPDU {
	pdu := pduErrorIndication()
	if c != nil {
		ie := ngapType.ErrorIndicationIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDCause
		ie.Value.Present = ngapType.ErrorIndicationIEsPresentCause
		ie.Value.Cause = c
		pdu.InitiatingMessage.Value.ErrorIndication.ProtocolIEs.List = append(pdu.InitiatingMessage.Value.ErrorIndication.ProtocolIEs.List, ie)
	}
	return pdu
}

func pduRANConfigUpdateFailure() *ngapType.NGAPPDU {
	return &ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentUnsuccessfulOutcome,
		UnsuccessfulOutcome: &ngapType.UnsuccessfulOutcome{ProcedureCode: ngapType.ProcedureCode{Value: ngapType.ProcedureCodeRANConfigurationUpdate},
			Value: ngapType.UnsuccessfulOutcomeValue{Present: ngapType.UnsuccessfulOutcomePresentRANConfigurationUpdateFailure,
				RANConfigurationUpdateFailure: &ngapType.RANConfigurationUpdateFailure{}}}}
}

func pduRANConfigUpdateAcknowledge() *ngapType.NGAPPDU {
	return &ngapType.NGAPPDU{Present: ngapType.NGAPPDUPresentSuccessfulOutcome,
		SuccessfulOutcome: &ngapType.SuccessfulOutcome{ProcedureCode: ngapType.ProcedureCode{Value: ngapType.ProcedureCodeRANConfigurationUpdate},
			Value: ngapType.SuccessfulOutcomeValue{Present: ngapType.SuccessfulOutcomePresentRANConfigurationUpdateAcknowledge,
				RANConfigurationUpdateAcknowledge: &ngapType.RANConfigurationUpdateAcknowledge{}}}}
}

// Every reaction to the malformed update: an ERROR INDICATION is PASS with
// its cause recorded; every other reaction with the association up is
// DEVIATION with the issue's message, Observed keeping them apart; a lost
// association is FAIL and not survived.
func TestClassifyRANConfigSyntax(t *testing.T) {
	syntax := &ngapType.Cause{Present: ngapType.CausePresentProtocol, Protocol: &ngapType.CauseProtocol{Value: ngapType.CauseProtocolPresentTransferSyntaxError}}
	other := &ngapType.Cause{Present: ngapType.CausePresentProtocol, Protocol: &ngapType.CauseProtocol{Value: ngapType.CauseProtocolPresentSemanticError}}
	misc := &ngapType.Cause{Present: ngapType.CausePresentMisc, Misc: &ngapType.CauseMisc{Value: ngapType.CauseMiscPresentUnspecified}}
	timeout := errors.Join(errors.New("waiting for NGAP PDU"), context.DeadlineExceeded)
	tests := []struct {
		name         string
		resp         *ngapType.NGAPPDU
		err          error
		want         Verdict
		wantObserved string
		wantDetail   string
		wantSurvived bool
	}{
		{name: "error indication, transfer syntax cause", resp: pduErrorIndicationCause(syntax), want: Pass,
			wantObserved: "ERROR INDICATION", wantDetail: "cause Protocol transfer-syntax-error (recorded, not asserted)", wantSurvived: true},
		{name: "error indication, another protocol cause", resp: pduErrorIndicationCause(other), want: Pass,
			wantObserved: "ERROR INDICATION", wantDetail: "cause Protocol 4 (recorded, not asserted)", wantSurvived: true},
		{name: "error indication, misc cause", resp: pduErrorIndicationCause(misc), want: Pass,
			wantObserved: "ERROR INDICATION", wantDetail: "cause Misc 5", wantSurvived: true},
		{name: "error indication without a cause", resp: pduErrorIndicationCause(nil), want: Pass,
			wantObserved: "ERROR INDICATION", wantDetail: "cause absent", wantSurvived: true},
		{name: "failure", resp: pduRANConfigUpdateFailure(), want: Deviation,
			wantObserved: "RAN CONFIGURATION UPDATE FAILURE", wantDetail: "answered with RAN CONFIGURATION UPDATE FAILURE 20ms after", wantSurvived: true},
		{name: "acknowledge", resp: pduRANConfigUpdateAcknowledge(), want: Deviation,
			wantObserved: "RAN CONFIGURATION UPDATE ACKNOWLEDGE (accepted!)", wantDetail: ranConfigSyntaxText, wantSurvived: true},
		{name: "another PDU", resp: pduDownlinkNAS(), want: Deviation,
			wantObserved: "InitiatingMessage(procedure 4)", wantDetail: ranConfigSyntaxText, wantSurvived: true},
		{name: "silence", err: timeout, want: Deviation,
			wantObserved: "silently dropped; association survived", wantDetail: "no ERROR INDICATION within 3s of the malformed RAN CONFIGURATION UPDATE (absence inferred within this bound)", wantSurvived: true},
		{name: "association lost", err: io.EOF, want: Fail,
			wantObserved: "association reset", wantDetail: "the association was lost 20ms after the malformed RAN CONFIGURATION UPDATE (EOF)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail, survived := classifyRANConfigSyntax(tc.resp, tc.err, 20*time.Millisecond, ranConfigSyntaxWait)
			if v != tc.want || observed != tc.wantObserved || survived != tc.wantSurvived || !strings.Contains(detail, tc.wantDetail) {
				t.Fatalf("= %s %q survived %v (%s), want %s %q survived %v with detail containing %q",
					v, observed, survived, detail, tc.want, tc.wantObserved, tc.wantSurvived, tc.wantDetail)
			}
			if (v != Pass) != strings.HasSuffix(detail, ranConfigSyntaxText) {
				t.Errorf("detail %q: the issue's message ends every outcome but PASS", detail)
			}
		})
	}
}

// Against the mock AMF over real SCTP, scored as a suite run scores it: an
// AMF that sends the ERROR INDICATION PASSes; one that drops the PDU, and
// one that drops the association but keeps running, DEVIATE (the check cites
// no shall); one that stops answering FAILs. The check's associations close
// afterwards.
func TestRANConfigSyntaxAgainstMock(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	tests := []struct {
		name         string
		answer       mockamf.UndecodableAnswer
		want         Verdict
		wantRun      Verdict // what Run returns before the registry scores it
		wantObserved string
		wantDetail   string
		wantAlive    bool
	}{
		{name: "error indication", answer: mockamf.ErrorIndicationOnUndecodable, want: Pass, wantRun: Pass,
			wantObserved: "ERROR INDICATION", wantDetail: "cause Protocol transfer-syntax-error", wantAlive: true},
		{name: "withholds the error indication", answer: mockamf.IgnoreUndecodable, want: Deviation, wantRun: Deviation,
			wantObserved: "silently dropped; association survived", wantDetail: "no ERROR INDICATION within 300ms", wantAlive: true},
		{name: "drops the association", answer: mockamf.CloseOnUndecodable, want: Deviation, wantRun: Fail,
			wantObserved: "association reset", wantDetail: "a fresh NG Setup still completes, so the AMF dropped the association and kept running", wantAlive: true},
		{name: "stops answering", answer: mockamf.CrashOnUndecodable, want: Fail, wantRun: Fail,
			wantObserved: ObservedAMFUnresponsive + " (association reset)", wantDetail: "the core did not survive the malformed RAN CONFIGURATION UPDATE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := Env{GNB: gnb.Config{ID: 0x400, IDBits: 24, Name: "orbit-conf", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}}
			check := ranConfigUpdateTransferSyntax{wait: 300 * time.Millisecond}
			for _, scored := range []bool{false, true} {
				amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
					MCC: "208", MNC: "93", SST: 1, SD: "010203"}, mockamf.WithUndecodablePDU(tc.answer))
				if err != nil {
					t.Fatal(err)
				}
				front := sctpfront.Start(t, amf)
				env.AMFAddr = front.Addr
				var res Result
				want := tc.wantRun
				if scored {
					res, want = runOne(context.Background(), check, env, 10*time.Second), tc.want
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					res = check.Run(ctx, env)
					cancel()
				}
				if res.Verdict != want || res.Observed != tc.wantObserved || !strings.Contains(res.Detail, tc.wantDetail) {
					t.Fatalf("scored %v: = %s %q (%s), want %s %q with detail containing %q", scored, res.Verdict, res.Observed, res.Detail, want, tc.wantObserved, tc.wantDetail)
				}
				if (res.Verdict == Deviation || tc.answer == mockamf.CloseOnUndecodable) != strings.Contains(res.Detail, ranConfigSyntaxText) {
					t.Errorf("scored %v: detail %q: the issue's message belongs on the outcomes without an ERROR INDICATION from a running core", scored, res.Detail)
				}
				deadline := time.Now().Add(3 * time.Second)
				for front.Active() != 0 && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if n := front.Active(); n != 0 {
					t.Errorf("scored %v: %d associations still open after the check", scored, n)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				alive, err := env.Alive(ctx)
				cancel()
				if err != nil || alive != tc.wantAlive {
					t.Errorf("scored %v: liveness after the check: alive %v, err %v; want alive %v", scored, alive, err, tc.wantAlive)
				}
				if n := amf.EncodeErrors(); n != 0 {
					t.Errorf("mock AMF dropped %d PDUs that failed to encode", n)
				}
			}
		})
	}
}

// What stops the check before the stimulus is an ERROR: a gNB identity the
// stimulus cannot be built from, an AMF that rejects the NG Setup, and one
// that cannot be dialled.
func TestRANConfigSyntaxSetupErrors(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	served := gnb.Config{ID: 0x400, IDBits: 24, Name: "orbit-conf", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	unserved := served
	unserved.MCC, unserved.MNC = "001", "01"
	noSlice := served
	noSlice.Slices = nil
	tests := []struct {
		name         string
		env          Env
		wantObserved string
		wantDetail   string
	}{
		{name: "no slice configured", env: Env{AMFAddr: front.Addr, GNB: noSlice}, wantObserved: "stimulus could not be built", wantDetail: "at least one supported S-NSSAI"},
		{name: "NG Setup rejected", env: Env{AMFAddr: front.Addr, GNB: unserved}, wantObserved: "setup failed", wantDetail: "NG Setup rejected"},
		{name: "AMF not reachable", env: Env{AMFAddr: "127.0.0.1:1", GNB: served}, wantObserved: "setup failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runOne(context.Background(), ranConfigUpdateTransferSyntax{wait: 300 * time.Millisecond}, tc.env, 5*time.Second)
			if res.Verdict != Error || res.Observed != tc.wantObserved || !strings.Contains(res.Detail, tc.wantDetail) {
				t.Fatalf("= %s %q (%s), want ERROR %q with detail containing %q", res.Verdict, res.Observed, res.Detail, tc.wantObserved, tc.wantDetail)
			}
		})
	}
	if got := amf.RANConfigUpdates(); len(got) != 0 {
		t.Errorf("the mock received %d RAN CONFIGURATION UPDATEs from checks that stopped before the stimulus", len(got))
	}
}
