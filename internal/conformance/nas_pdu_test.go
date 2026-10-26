package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// gsmPDU builds a plain 5GSM message of type mt for session 1: a Reject or
// a STATUS with cause, or a bare header for any other type.
func gsmPDU(t *testing.T, mt, cause uint8) []byte {
	t.Helper()
	m := f5nas.NewMessage()
	m.GsmMessage = f5nas.NewGsmMessage()
	m.GsmHeader.SetMessageType(mt)
	switch mt {
	case f5nas.MsgTypePDUSessionEstablishmentReject:
		r := nasMessage.NewPDUSessionEstablishmentReject(mt)
		r.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSSessionManagementMessage)
		r.SetPDUSessionID(1)
		r.SetMessageType(mt)
		r.SetCauseValue(cause)
		m.PDUSessionEstablishmentReject = r
	case f5nas.MsgTypeStatus5GSM:
		s := nasMessage.NewStatus5GSM(mt)
		s.SetExtendedProtocolDiscriminator(nasMessage.Epd5GSSessionManagementMessage)
		s.SetPDUSessionID(1)
		s.SetMessageType(mt)
		s.SetCauseValue(cause)
		m.Status5GSM = s
	default:
		return []byte{nasMessage.Epd5GSSessionManagementMessage, 1, 0, mt}
	}
	b, err := m.PlainNasEncode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// readSM names each 5GSM message and reads the cause of a Reject or STATUS.
func TestReadSM(t *testing.T) {
	tests := []struct {
		name      string
		sm        []byte
		wantKind  smKind
		wantName  string
		wantCause uint8
	}{
		{name: "reject #27", sm: gsmPDU(t, f5nas.MsgTypePDUSessionEstablishmentReject, 27), wantKind: smReject,
			wantName: "PDU Session Establishment Reject, 5GSM cause #27 missing or unknown DNN", wantCause: 27},
		{name: "reject #31", sm: gsmPDU(t, f5nas.MsgTypePDUSessionEstablishmentReject, 31), wantKind: smReject,
			wantName: "PDU Session Establishment Reject, 5GSM cause #31", wantCause: 31},
		{name: "status #96", sm: gsmPDU(t, f5nas.MsgTypeStatus5GSM, 96), wantKind: smStatus, wantName: "5GSM STATUS, 5GSM cause #96", wantCause: 96},
		{name: "accept", sm: gsmPDU(t, f5nas.MsgTypePDUSessionEstablishmentAccept, 0), wantKind: smAccept, wantName: "PDU Session Establishment Accept"},
		{name: "request", sm: gsmPDU(t, f5nas.MsgTypePDUSessionEstablishmentRequest, 0), wantKind: smRequest, wantName: "PDU Session Establishment Request"},
		{name: "other type", sm: gsmPDU(t, 0xcb, 0), wantKind: smOther, wantName: "5GSM message type 0xcb"},
		{name: "not 5GSM", sm: []byte{0x7e, 0x00, 0x68, 0x01}, wantKind: smNot5GSM, wantName: "no 5GSM message"},
		{name: "too short", sm: []byte{0x2e, 1}, wantKind: smNot5GSM, wantName: "no 5GSM message"},
		{name: "truncated reject", sm: []byte{0x2e, 1, 0, f5nas.MsgTypePDUSessionEstablishmentReject}, wantKind: smUndecodable, wantName: "PDU Session Establishment Reject"},
		{name: "truncated status", sm: []byte{0x2e, 1, 0, f5nas.MsgTypeStatus5GSM}, wantKind: smUndecodable, wantName: "5GSM STATUS"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := readSM(tc.sm)
			if got.kind != tc.wantKind || got.name != tc.wantName || got.cause != tc.wantCause || (got.kind == smUndecodable) != (got.decodeErr != nil) {
				t.Errorf("readSM = %+v, want kind %d %q cause %d", got, tc.wantKind, tc.wantName, tc.wantCause)
			}
		})
	}
}

// Every verdict branch from a constructed outcome.
func TestJudgeDNN(t *testing.T) {
	sent := []byte{0x2e, 1, 0, 0xc1, 0xff, 0xff, 0x91}
	reject := func(c uint8) []byte { return gsmPDU(t, f5nas.MsgTypePDUSessionEstablishmentReject, c) }
	ans := func(payload []byte, mm uint8) dnnOutcome {
		return dnnOutcome{sent: sent, registered: true, answer: &dlN1SM{payload: payload, mmCause: mm}}
	}
	tests := []struct {
		name         string
		o            dnnOutcome
		want         Verdict
		wantObserved string
		wantDetail   []string
		notDetail    []string
	}{
		{name: "registration failure", o: dnnOutcome{err: errors.New("AMF rejected registration")}, want: Error,
			wantObserved: "registration or harness failure", wantDetail: []string{"AMF rejected registration"}},
		{name: "integrity failure", o: dnnOutcome{sent: sent, decodeErr: fmt.Errorf("%w: mac 1 != 2", nas.ErrIntegrity), decodeRaw: []byte{0x7e, 0x02}}, want: Error,
			wantObserved: "DL NAS Transport failed integrity check", wantDetail: []string{"NAS 7e02"}},
		{name: "undecodable", o: dnnOutcome{sent: sent, decodeErr: errors.New("nas plain decode"), decodeRaw: []byte{0x7e, 0x02}}, want: Error,
			wantObserved: "answer did not decode", wantDetail: []string{"NAS 7e02", "codec covers about Rel-15/16"}},
		{name: "accepted", o: dnnOutcome{sent: sent, registered: true, accepted: true}, want: Skip, wantObserved: acceptedObserved},
		{name: "silence", o: dnnOutcome{sent: sent, registered: true, noAnswer: true}, want: Fail, wantObserved: "nothing back within 10s",
			wantDetail: []string{"(got nothing within 10s)", "#90/#91", "5.4.5.2.5 a) 1), 5.4.5.3.2 (case e) of 5.4.5.3.1)", "Likely cause: the request is dropped when no SMF is selected"}},
		{name: "returned #91", o: ans(sent, 91), want: Pass, wantObserved: "un-forwarded request returned, 5GMM cause #91 DNN not supported or not subscribed in the slice",
			wantDetail: []string{"path (B)"}},
		{name: "returned #90", o: ans(sent, 90), want: Pass, wantObserved: "un-forwarded request returned, 5GMM cause #90 payload was not forwarded"},
		{name: "#91 with a Reject payload", o: ans(reject(91), 91), want: Fail, wantObserved: "5GMM cause #91 DNN not supported or not subscribed in the slice with a payload that is not the request",
			wantDetail: []string{"Likely cause: AMF returns its own Reject in the payload container instead of the un-forwarded message.", "carrying PDU Session Establishment Reject, 5GSM cause #91"}},
		{name: "#90 with another payload", o: ans(gsmPDU(t, f5nas.MsgTypeStatus5GSM, 111), 90), want: Fail,
			wantDetail: []string{"Likely cause: AMF returns a payload other than the un-forwarded 5GSM message."}},
		{name: "reject #27", o: ans(reject(27), 0), want: Pass, wantObserved: "PDU Session Establishment Reject, 5GSM cause #27 missing or unknown DNN",
			wantDetail: []string{"path (A)", "5GMM cause absent", "does not tell which network function rejected"}, notDetail: []string{"not #27"}},
		{name: "reject #31", o: ans(reject(31), 0), want: Pass, wantDetail: []string{"the cause is not #27", "typical"}},
		{name: "reject with another 5GMM cause", o: ans(reject(27), 22), want: Pass, wantDetail: []string{"5GMM cause #22"}},
		{name: "undecodable reject", o: ans([]byte{0x2e, 1, 0, f5nas.MsgTypePDUSessionEstablishmentReject}, 0), want: Error,
			wantObserved: "answer did not decode", wantDetail: []string{"payload 2e0100c3"}},
		{name: "plain accept", o: ans(gsmPDU(t, f5nas.MsgTypePDUSessionEstablishmentAccept, 0), 0), want: Skip, wantObserved: acceptedObserved,
			wantDetail: []string{"plain DL NAS Transport", "without a PDU Session Resource Setup Request"}},
		{name: "returned with #22", o: ans(sent, 22), want: Skip, wantObserved: congestionObserved,
			wantDetail: []string{"5GMM cause #22", "no Back-off timer value IE", "from V16.3.0", "re-run"}},
		{name: "returned with #67 and a back-off", o: dnnOutcome{sent: sent, registered: true, answer: &dlN1SM{payload: sent, mmCause: 67, backoff: true}},
			want: Skip, wantObserved: congestionObserved, wantDetail: []string{"5GMM cause #67", "and a Back-off timer value IE"}},
		{name: "returned with #69", o: ans(sent, 69), want: Skip, wantObserved: congestionObserved},
		{name: "#22 with another payload", o: ans(reject(27), 22), want: Pass, wantDetail: []string{"5GMM cause #22"}},
		{name: "returned with #9", o: ans(sent, 9), want: Error, wantDetail: []string{"5GMM cause #9", "not a cause 5.4.5.3.2 assigns to a routing failure"}},
		{name: "status #96", o: ans(gsmPDU(t, f5nas.MsgTypeStatus5GSM, 96), 0), want: Fail, wantObserved: "unexpected answer: 5GSM STATUS, 5GSM cause #96",
			wantDetail: []string{"check the stimulus", "#90/#91"}},
		{name: "status #26", o: ans(gsmPDU(t, f5nas.MsgTypeStatus5GSM, 26), 0), want: Fail, notDetail: []string{"check the stimulus"}},
		{name: "request returned without a cause", o: ans(sent, 0), want: Fail, wantObserved: "unexpected answer: PDU Session Establishment Request"},
		{name: "not 5GSM", o: ans([]byte{1, 2, 3}, 0), want: Fail, wantObserved: "unexpected answer: no 5GSM message"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeDNN(tc.o, dnnAnswerWait)
			if v != tc.want || (tc.wantObserved != "" && observed != tc.wantObserved) {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
			for _, w := range tc.notDetail {
				if strings.Contains(detail, w) {
					t.Errorf("detail %q has %q", detail, w)
				}
			}
		})
	}
}

// pointsAtStimulus picks out #43 and the protocol-error causes.
func TestPointsAtStimulus(t *testing.T) {
	for c, want := range map[uint8]bool{43: true, 95: true, 96: true, 101: true, 111: true, 27: false, 94: false, 102: false} {
		if got := pointsAtStimulus(c); got != want {
			t.Errorf("pointsAtStimulus(%d) = %v, want %v", c, got, want)
		}
	}
}

type nopUPF struct{}

func (nopUPF) SessionUp(mockamf.SessionInfo)   {}
func (nopUPF) PathSwitched(mockamf.PathSwitch) {}

// Every branch against the mock AMF over a real SCTP listener: each answer
// the mock can give an unknown DNN, an accepting core with a UPF, silence,
// an Authentication Reject, and no UE. A UE that registered is deregistered
// and no association stays open; one the mock gave no 5G-GUTI cannot be, and
// the detail says so.
func TestUnknownDNNCheckAgainstMock(t *testing.T) {
	dnn := func(a mockamf.DNNAnswer, cause uint8) []mockamf.Option {
		return []mockamf.Option{mockamf.WithUnknownDNN(stimulusDNN, a, cause)}
	}
	const dereg = "UE deregistered"
	const dropped = "association was dropped without a deregistration"
	tests := []struct {
		name         string
		opts         []mockamf.Option
		upf          bool
		noGUTI       bool // the mock assigns no 5G-GUTI, so the deregistration fails
		noUE         bool
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "reject #27", opts: dnn(mockamf.RejectDNN, 0), want: Pass,
			wantObserved: "PDU Session Establishment Reject, 5GSM cause #27 missing or unknown DNN", wantDetail: []string{"path (A)", dereg}},
		{name: "reject #31", opts: dnn(mockamf.RejectDNN, 31), want: Pass,
			wantObserved: "PDU Session Establishment Reject, 5GSM cause #31", wantDetail: []string{"the cause is not #27", dereg}},
		{name: "returned #91", opts: dnn(mockamf.ReturnDNN, 0), want: Pass,
			wantObserved: "un-forwarded request returned, 5GMM cause #91 DNN not supported or not subscribed in the slice", wantDetail: []string{"path (B)", dereg}},
		{name: "returned #90", opts: dnn(mockamf.ReturnDNN, 90), want: Pass,
			wantObserved: "un-forwarded request returned, 5GMM cause #90 payload was not forwarded", wantDetail: []string{dereg}},
		{name: "#91 with an AMF-built Reject", opts: dnn(mockamf.ReturnWrongPayloadDNN, 0), want: Fail,
			wantObserved: "5GMM cause #91 DNN not supported or not subscribed in the slice with a payload that is not the request",
			wantDetail:   []string{"Likely cause: AMF returns its own Reject in the payload container instead of the un-forwarded message.", dereg}},
		{name: "#91 with another payload", opts: dnn(mockamf.ReturnOtherPayloadDNN, 0), want: Fail,
			wantObserved: "5GMM cause #91 DNN not supported or not subscribed in the slice with a payload that is not the request",
			wantDetail:   []string{"Likely cause: AMF returns a payload other than the un-forwarded 5GSM message.", dereg}},
		{name: "congestion, back-off", opts: dnn(mockamf.CongestionDNN, 0), want: Skip,
			wantObserved: congestionObserved, wantDetail: []string{"5GMM cause #22", "and a Back-off timer value IE", dereg}},
		{name: "congestion #69, no back-off", opts: dnn(mockamf.ReturnDNN, 69), want: Skip,
			wantObserved: congestionObserved, wantDetail: []string{"5GMM cause #69", "no Back-off timer value IE", dereg}},
		{name: "returned with #9", opts: dnn(mockamf.ReturnDNN, 9), want: Error,
			wantObserved: "request returned with 5GMM cause #9", wantDetail: []string{"not a cause 5.4.5.3.2 assigns to a routing failure", dereg}},
		{name: "5GSM STATUS #96", opts: dnn(mockamf.StatusDNN, 0), want: Fail,
			wantObserved: "unexpected answer: 5GSM STATUS, 5GSM cause #96", wantDetail: []string{"check the stimulus", dereg}},
		{name: "plain accept", opts: dnn(mockamf.PlainAcceptDNN, 0), want: Skip, wantObserved: acceptedObserved,
			wantDetail: []string{"without a PDU Session Resource Setup Request", dereg}},
		{name: "accepted with a UPF", upf: true, want: Skip, wantObserved: acceptedObserved,
			wantDetail: []string{"established a PDU session", dereg}},
		{name: "undecodable answer", opts: dnn(mockamf.UndecodableDNN, 0), want: Error,
			wantObserved: "answer did not decode", wantDetail: []string{"codec covers about Rel-15/16", dropped}},
		{name: "bad MAC", opts: dnn(mockamf.BadMACDNN, 0), want: Error,
			wantObserved: "DL NAS Transport failed integrity check", wantDetail: []string{"NAS 7e02", dropped}},
		{name: "silence", opts: dnn(mockamf.IgnoreDNN, 0), want: Fail,
			wantObserved: "nothing back within 300ms", wantDetail: []string{"(got nothing within 300ms)", dereg}},
		{name: "deregistration fails", opts: dnn(mockamf.RejectDNN, 0), noGUTI: true, want: Pass,
			wantObserved: "PDU Session Establishment Reject, 5GSM cause #27 missing or unknown DNN",
			wantDetail:   []string{"deregistration failed (no 5G-GUTI", "the association was dropped"}},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error,
			wantObserved: "registration or harness failure"},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, _ := auth.ParseHexKey("Ki", testKi)
			opc, _ := auth.ParseHexKey("OPc", testOPc)
			cfg := mockamf.Config{
				Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}
			if tc.upf {
				cfg.UPF, cfg.UPFAddress = nopUPF{}, "127.0.0.1"
			}
			opts := tc.opts
			if !tc.noGUTI {
				opts = append([]mockamf.Option{mockamf.WithGUTI()}, opts...)
			}
			amf, err := mockamf.New(cfg, opts...)
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
			r := runOne(ctx, nasPDUUnknownDNN{registerWait: 3 * time.Second, answerWait: 300 * time.Millisecond}, env, 15*time.Second)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
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
			deadline := time.Now().Add(3 * time.Second)
			for front.Active() != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if front.Active() != 0 {
				t.Errorf("%d associations still open after the check", front.Active())
			}
		})
	}
}

// The stimulus is the harness's PDU session with only the DNN replaced, so
// it rides the run's slice.
func TestUnknownDNNStimulusSlice(t *testing.T) {
	h, err := NewUEHarness(ueEnv("127.0.0.1:1", "", "127.0.0.1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := h.pdu
	p.DNN = stimulusDNN
	if p.SST != 1 || p.SD != "010203" || p.PDUSessionID != 1 {
		t.Errorf("stimulus session %+v, want session 1 on SST 1 SD 010203", p)
	}
	if _, err := ue.BuildPDUSessionEstablishmentRequest(p); err != nil {
		t.Error(err)
	}
}
