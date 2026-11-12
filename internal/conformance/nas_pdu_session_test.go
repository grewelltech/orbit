package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// Every branch of the classifier from a constructed outcome.
func TestClassifyPDUSession(t *testing.T) {
	sent := []byte{0x2e, 1, 0, 0xc1, 0xff, 0xff, 0x91}
	ans := func(payload []byte, mm uint8) dnnOutcome {
		return dnnOutcome{sent: sent, registered: true, answer: &dlN1SM{payload: payload, mmCause: mm}}
	}
	tests := []struct {
		name         string
		o            dnnOutcome
		want         Outcome
		wantObserved string
		wantDetail   []string
	}{
		{name: "established", o: dnnOutcome{sent: sent, registered: true, acceptedNAS: true, accepted: true}, want: pduSessionEstablished,
			wantObserved: "PDU session established"},
		{name: "rejected", o: ans(gsmPDU(t, f5nas.MsgTypePDUSessionEstablishmentReject, 27), 0), want: pduSessionRejected,
			wantObserved: "PDU Session Establishment Reject, 5GSM cause #27 missing or unknown DNN", wantDetail: []string{"no requirement is violated"}},
		{name: "not forwarded", o: ans(sent, 91), want: pduSessionNotForwarded,
			wantObserved: "request not forwarded, 5GMM cause #91 DNN not supported or not subscribed in the slice"},
		{name: "a reject with a 5GMM cause is not forwarded", o: ans(gsmPDU(t, f5nas.MsgTypePDUSessionEstablishmentReject, 27), 90), want: pduSessionNotForwarded},
		{name: "accept without N2", o: dnnOutcome{sent: sent, registered: true, acceptedNAS: true, noUserPlane: true}, want: pduSessionNoUserPlane,
			wantDetail: []string{"no PDU SESSION RESOURCE SETUP REQUEST for the session within 10s"}},
		{name: "integrity failure", o: dnnOutcome{sent: sent, decodeErr: fmt.Errorf("%w: mac", nas.ErrIntegrity), decodeRaw: []byte{0x7e, 0x02}, decodeType: f5nas.MsgTypeDLNASTransport},
			want: pduSessionIntegrityFailed, wantObserved: "downlink NAS after the request failed integrity check (DL NAS Transport)"},
		{name: "undecodable downlink", o: dnnOutcome{sent: sent, decodeErr: errors.New("nas plain decode"), decodeRaw: []byte{0x7e, 0x02}}, want: classifiedError,
			wantDetail: []string{"codec covers about Rel-15/16"}},
		{name: "unanswered", o: dnnOutcome{sent: sent, registered: true, noAnswer: true}, want: pduSessionUnanswered, wantObserved: "no answer within 10s",
			wantDetail: []string{"shall reject it (TS 24.501 6.4.1.4.1)"}},
		{name: "unanswered after a 5GMM STATUS", o: dnnOutcome{sent: sent, registered: true, noAnswer: true, mmStatus: true, mmStatusC: 111}, want: pduSessionUnanswered,
			wantDetail: []string{"a 5GMM STATUS with 5GMM cause #111 came"}},
		{name: "released", o: dnnOutcome{sent: sent, released: true}, want: pduSessionReleased, wantObserved: "UE released after the request, with no answer"},
		{name: "5GSM STATUS", o: ans(gsmPDU(t, f5nas.MsgTypeStatus5GSM, 96), 0), want: pduSessionOtherAnswer, wantObserved: "unexpected answer: 5GSM STATUS, 5GSM cause #96"},
		{name: "request returned without a cause", o: ans(sent, 0), want: pduSessionOtherAnswer, wantObserved: "unexpected answer: PDU Session Establishment Request"},
		{name: "undecodable reject", o: ans([]byte{0x2e, 1, 0, f5nas.MsgTypePDUSessionEstablishmentReject}, 0), want: classifiedError, wantObserved: "answer did not decode"},
		{name: "registration timed out", o: dnnOutcome{regTimedOut: true, registerWait: 3 * time.Second}, want: classifiedBlocked(ProcRegistration),
			wantObserved: "registration did not reach the PDU Session Establishment Request", wantDetail: []string{"did not register within 3s"}},
		{name: "harness failure", o: dnnOutcome{err: errors.New("x")}, want: classifiedError, wantObserved: "registration or harness failure"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, observed, detail := classifyPDUSession(tc.o, pduSessionAnswerWait)
			if o.ID != covers(tc.want).ID || o.Verdict != tc.want.Verdict || (tc.wantObserved != "" && observed != tc.wantObserved) {
				t.Errorf("= %s %s %q (%s), want %s %s %q", o.ID, o.Verdict, observed, detail, tc.want.ID, tc.want.Verdict, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
		})
	}
}

// Every outcome the mock AMF can produce, over a real SCTP listener: a
// session set up on N2, a reject and a send-back of the run's own DNN,
// an Accept with no N2 setup, a bad MAC, no answer
// (WithPDUSessionAnswers(0)), a 5GSM STATUS, an undecodable answer, an
// Authentication Reject (BLOCKED on the registration) and no UE. The
// outcomes that mean the session did not complete carry NotCompleted, a
// UE that registered is deregistered, and no association stays open.
func TestPDUSessionCheckAgainstMock(t *testing.T) {
	own := func(a mockamf.DNNAnswer, cause uint8) []mockamf.Option {
		return []mockamf.Option{mockamf.WithUnknownDNN("internet", a, cause)}
	}
	const dereg = "UE deregistered"
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noUE         bool
		want         Verdict
		wantOutcome  string
		wantObserved string
		wantDetail   []string
		notCompleted bool
	}{
		{name: "established", want: Pass, wantOutcome: "established", wantObserved: "PDU session established",
			wantDetail: []string{`DNN "internet", slice SST 1 SD "010203"`, dereg}},
		{name: "rejected", opts: own(mockamf.RejectDNN, 26), want: Warn, wantOutcome: "rejected", notCompleted: true,
			wantObserved: "PDU Session Establishment Reject, 5GSM cause #26", wantDetail: []string{"WARN for availability impact", dereg}},
		{name: "not forwarded", opts: own(mockamf.ReturnDNN, 0), want: Warn, wantOutcome: "not-forwarded", notCompleted: true,
			wantObserved: "request not forwarded, 5GMM cause #91 DNN not supported or not subscribed in the slice", wantDetail: []string{dereg}},
		{name: "accept without N2", opts: own(mockamf.PlainAcceptDNN, 0), want: Warn, wantOutcome: "no-user-plane", notCompleted: true,
			wantObserved: "PDU SESSION ESTABLISHMENT ACCEPT without the session's setup on N2", wantDetail: []string{dereg}},
		{name: "bad MAC", opts: own(mockamf.BadMACDNN, 0), want: Warn, wantOutcome: "integrity-failed", notCompleted: true,
			wantObserved: "downlink NAS after the request failed integrity check (DL NAS Transport)", wantDetail: []string{"WARN for interoperability impact", dereg}},
		{name: "unanswered", opts: []mockamf.Option{mockamf.WithPDUSessionAnswers(0)}, want: Fail, wantOutcome: "unanswered", notCompleted: true,
			wantObserved: "no answer within 300ms", wantDetail: []string{"violates TS 24.501 6.4.1.3 (shall), TS 24.501 6.4.1.4.1", "absence inferred", dereg}},
		{name: "5GSM STATUS", opts: own(mockamf.StatusDNN, 0), want: Fail, wantOutcome: "other-answer", notCompleted: true,
			wantObserved: "unexpected answer: 5GSM STATUS, 5GSM cause #96", wantDetail: []string{dereg}},
		{name: "undecodable answer", opts: own(mockamf.UndecodableDNN, 0), want: Error, wantOutcome: OutcomeError,
			wantObserved: "downlink NAS after the request did not decode (DL NAS Transport)", wantDetail: []string{dereg}},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Blocked, wantOutcome: blockedID(ProcRegistration),
			wantObserved: "UE registration failed"},
		{name: "registration unanswered", opts: []mockamf.Option{mockamf.WithNoAuthenticationRequest()}, want: Blocked, wantOutcome: blockedID(ProcRegistration),
			wantObserved: "registration did not reach the PDU Session Establishment Request", wantDetail: []string{"did not register within 3s"}},
		{name: "no UE", noUE: true, want: Skip, wantOutcome: skipInputID(InputUE), wantObserved: "no UE configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, _ := auth.ParseHexKey("Ki", testKi)
			opc, _ := auth.ParseHexKey("OPc", testOPc)
			cfg := mockamf.Config{
				Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203", UPF: nopUPF{}, UPFAddress: "127.0.0.1",
			}
			amf, err := mockamf.New(cfg, append([]mockamf.Option{mockamf.WithGUTI()}, tc.opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			front := sctpfront.Start(t, amf)
			env := ueEnv(front.Addr, "", "127.0.0.1")
			if tc.noUE {
				env.UE = UESpec{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r := runOne(ctx, nasPDUSessionEstablishment{registerWait: 3 * time.Second, answerWait: 300 * time.Millisecond}, env, 15*time.Second)
			if r.Verdict != tc.want || r.Outcome != tc.wantOutcome || r.Observed != tc.wantObserved {
				t.Errorf("= %s %s %q (%s), want %s %s %q", r.Verdict, r.Outcome, r.Observed, r.Detail, tc.want, tc.wantOutcome, tc.wantObserved)
			}
			if got := r.NotCompleted == ProcPDUSession; got != tc.notCompleted || (!got && r.NotCompleted != "") {
				t.Errorf("NotCompleted %q, want the PDU session reported as not completed: %v", r.NotCompleted, tc.notCompleted)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if amf.EncodeErrors() != 0 {
				t.Errorf("mock dropped %d downlink PDUs that did not encode", amf.EncodeErrors())
			}
			assertNoAssociations(t, front)
		})
	}
}

// The check owns the PDU session establishment, needs the NG Setup and the
// registration with one UE, and the preflight establishes a session for it.
func TestPDUSessionCheckIdentity(t *testing.T) {
	c := testByID(t, "NAS-PDU-SESSION-ESTABLISHMENT")
	if got := OwnsOf(c); len(got) != 1 || got[0] != ProcPDUSession {
		t.Errorf("owns %v, want [%s]", got, ProcPDUSession)
	}
	n := NeedsOf(c)
	if n.UEs != 1 || fmt.Sprint(n.Procedures) != fmt.Sprint([]Procedure{ProcNGSetup, ProcRegistration}) {
		t.Errorf("needs %+v, want one UE after the NG Setup and the registration", n)
	}
	if !usesPDUSession(c) {
		t.Error("the preflight would not establish a PDU session for the owner")
	}
	if err := ValidateTest(c); err != nil {
		t.Error(err)
	}
	if got := NewRegistry().OwnerOf(ProcPDUSession); got != c.ID() {
		t.Errorf("owner of %s = %q", ProcPDUSession, got)
	}
	if WorstVerdict(c) != Fail || MinRelease(c) != 15 {
		t.Errorf("worst verdict %s, min release %s; want FAIL and Rel-15", WorstVerdict(c), MinRelease(c))
	}
}

// A per-test timeout that ends before the answer wait is an ERROR, not a
// FAIL.
func TestPDUSessionCheckCutShort(t *testing.T) {
	k, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	amf, err := mockamf.New(mockamf.Config{
		Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203", UPF: nopUPF{}, UPFAddress: "127.0.0.1",
	}, mockamf.WithGUTI(), mockamf.WithPDUSessionAnswers(0))
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	env := ueEnv(front.Addr, "", "127.0.0.1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := nasPDUSessionEstablishment{registerWait: 3 * time.Second, answerWait: time.Minute}.Run(ctx, env)
	if r.Verdict != Error || !strings.Contains(r.Detail, "--per-test-timeout") {
		t.Errorf("= %s %q (%s), want ERROR naming the per-test timeout", r.Verdict, r.Observed, r.Detail)
	}
	assertNoAssociations(t, front)
}
