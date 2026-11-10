package conformance

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/testnet"
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
		{name: "integrity failure", o: dnnOutcome{sent: sent, decodeErr: fmt.Errorf("%w: mac 1 != 2", nas.ErrIntegrity), decodeRaw: []byte{0x7e, 0x02}, decodeType: f5nas.MsgTypeDLNASTransport}, want: Error,
			wantObserved: "downlink NAS after the request failed integrity check (DL NAS Transport)", wantDetail: []string{"NAS 7e02"}},
		{name: "integrity failure, header unreadable", o: dnnOutcome{sent: sent, decodeErr: fmt.Errorf("%w: mac 1 != 2", nas.ErrIntegrity), decodeRaw: []byte{0x7e, 0x02}}, want: Error,
			wantObserved: "downlink NAS after the request failed integrity check"},
		{name: "undecodable", o: dnnOutcome{sent: sent, decodeErr: errors.New("nas plain decode"), decodeRaw: []byte{0x7e, 0x02}, decodeType: 0x54}, want: Error,
			wantObserved: "downlink NAS after the request did not decode (5GMM message type 0x54)", wantDetail: []string{"NAS 7e02", "codec covers about Rel-15/16"}},
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
		{name: "request returned without a cause", o: ans(sent, 0), want: Fail, wantObserved: "request returned without a 5GMM cause",
			wantDetail: []string{"Likely cause: AMF returned the request without the 5GMM cause 5.4.5.3.2 requires.", "5GMM cause absent"}},
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
// an Authentication Reject, and no UE. A UE that registered is deregistered,
// on both decode ERRORs too, and no association stays open; one the mock gave no 5G-GUTI cannot be, and
// the detail says so.
func TestUnknownDNNCheckAgainstMock(t *testing.T) {
	dnn := func(a mockamf.DNNAnswer, cause uint8) []mockamf.Option {
		return []mockamf.Option{mockamf.WithUnknownDNN(stimulusDNN, a, cause)}
	}
	const dereg = "UE deregistered"
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
			wantObserved: "downlink NAS after the request did not decode (DL NAS Transport)", wantDetail: []string{"codec covers about Rel-15/16", dereg}},
		{name: "bad MAC", opts: dnn(mockamf.BadMACDNN, 0), want: Error,
			wantObserved: "downlink NAS after the request failed integrity check (DL NAS Transport)", wantDetail: []string{"NAS 7e02", dereg}},
		{name: "returned without a 5GMM cause", opts: dnn(mockamf.ReturnNoCauseDNN, 0), want: Fail,
			wantObserved: "request returned without a 5GMM cause", wantDetail: []string{"5.4.5.3.2 requires", dereg}},
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

// Every verdict branch of the UE-requested release check from a constructed
// outcome: the commands, the downlink before, around and after the release,
// and the traffic notes.
func TestJudgePDURelease(t *testing.T) {
	t0 := time.Unix(1000, 0)
	n3 := netip.MustParseAddrPort("127.0.0.1:2152")
	const teid = 0x2a
	ms := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }
	gpdu := func(at time.Time) datapath.GPDUArrival {
		return datapath.GPDUArrival{Arrival: at, TEID: teid, GNB: "127.0.0.1:2152"}
	}
	both := func() *engine.PDUReleaseResult {
		return &engine.PDUReleaseResult{PDUSessionID: 1, PTI: 1, DLTEID: teid, SentAt: t0,
			NASCommand: true, NASCommandAt: ms(20), CommandPTI: 1, CommandCause: 36, NASInResourceRelease: true, CompleteSent: true,
			ResourceRelease: true, ResourceReleaseAt: ms(20)}
	}
	with := func(f func(*engine.PDUReleaseResult)) *engine.PDUReleaseResult {
		r := both()
		f(r)
		return r
	}
	obs := func(traffic string, gpdus ...datapath.GPDUArrival) PathObservation {
		return PathObservation{OldTEID: teid, SourceN3: n3, GPDUs: gpdus, Traffic: traffic}
	}
	tests := []struct {
		name         string
		res          *engine.PDUReleaseResult
		err          error
		obs          PathObservation
		cut          bool
		want         Verdict
		wantObserved string
		wantDetail   []string
		notDetail    []string
	}{
		{name: "released, no traffic", res: both(), obs: obs(""), want: Pass, wantObserved: "PDU session released on N1 and N2; the released tunnel stayed quiet",
			wantDetail: []string{"PDU Session Release Command after 20ms, inside the PDU SESSION RESOURCE RELEASE COMMAND, 5GSM cause #36, PTI 1 echoed",
				"PDU SESSION RESOURCE RELEASE COMMAND after 20ms", "downlink on TEID 0x2a: 0 G-PDUs before the request, 0 up to 1s after the release, 0 after that",
				"no echo peer (--traffic): the released tunnel was watched for 4s"}},
		{name: "released apart, another PTI", res: with(func(r *engine.PDUReleaseResult) {
			r.NASInResourceRelease, r.NASCommandAt, r.CommandPTI = false, ms(60), 0
		}), obs: obs(""), want: Pass,
			wantDetail: []string{"after 60ms, in a DOWNLINK NAS TRANSPORT", "PTI 0, where the request's is 1 (TS 24.501 6.3.3.2 has the SMF echo it; recorded, not scored)"}},
		{name: "released, downlink before and in the grace", res: both(), obs: obs("192.0.2.7:7", gpdu(ms(-500)), gpdu(ms(-10)), gpdu(ms(30)), gpdu(ms(1020))),
			want: Pass, wantDetail: []string{"2 G-PDUs before the request, 2 up to 1s after the release, 0 after that", "echo flow to 192.0.2.7:7 kept sending uplink"},
			notDetail: []string{"check the echo peer"}},
		{name: "released, traffic that drew no downlink", res: both(), obs: obs("192.0.2.7:7"), want: Pass,
			wantDetail: []string{"no downlink arrived before the release either", "check the echo peer"}},
		{name: "downlink after the grace", res: both(), obs: obs("192.0.2.7:7", gpdu(ms(-10)), gpdu(ms(1021)), gpdu(ms(2520))),
			want: Fail, wantObserved: "2 downlink G-PDUs on the released tunnel after the release",
			wantDetail: []string{pduReleaseFail, "downlink continues: the last G-PDU came 2.5s after the release (TS 23.502 4.3.4.2 step 2a)", "2 after that"}},
		{name: "downlink on another tunnel or socket", res: both(), obs: obs("",
			datapath.GPDUArrival{Arrival: ms(3000), TEID: teid + 1, GNB: "127.0.0.1:2152"},
			datapath.GPDUArrival{Arrival: ms(3000), TEID: teid, GNB: "127.0.0.2:2152"}), want: Pass},
		{name: "the later command sets the release time", res: with(func(r *engine.PDUReleaseResult) { r.ResourceReleaseAt = ms(520) }),
			obs: obs("", gpdu(ms(1500))), want: Pass, wantDetail: []string{"1 up to 1s after the release, 0 after that"}},
		{name: "no answer", res: &engine.PDUReleaseResult{PDUSessionID: 1, PTI: 1, DLTEID: teid, SentAt: t0}, obs: obs(""),
			want: Fail, wantObserved: "no answer to the PDU Session Release Request within 16s",
			wantDetail: []string{pduReleaseFail, "neither a PDU Session Release Command nor a PDU SESSION RESOURCE RELEASE COMMAND came"}},
		{name: "the NGAP command alone", res: with(func(r *engine.PDUReleaseResult) { r.NASCommand, r.CompleteSent = false, false }), obs: obs(""),
			want: Fail, wantObserved: "PDU SESSION RESOURCE RELEASE COMMAND without a PDU Session Release Command",
			wantDetail: []string{pduReleaseFail, "the NGAP command came after 20ms", "within 2s of it (TS 24.501 6.3.3.2)"}},
		{name: "the NAS command alone", res: with(func(r *engine.PDUReleaseResult) { r.ResourceRelease = false }), obs: obs(""),
			want: Fail, wantObserved: "PDU Session Release Command without a PDU SESSION RESOURCE RELEASE COMMAND",
			wantDetail: []string{pduReleaseFail, "no PDU SESSION RESOURCE RELEASE COMMAND listing session 1 came within 2s of it", "TS 23.502 4.3.4.2 step 3"}},
		{name: "rejected", res: &engine.PDUReleaseResult{PDUSessionID: 1, PTI: 1, SentAt: t0, Rejected: true, RejectCause: 43}, obs: obs(""),
			want: Skip, wantObserved: "core rejected the release (PDU Session Release Reject, 5GSM cause #43)", wantDetail: []string{"TS 24.501 6.4.3.4"}},
		{name: "rejected as the time runs out", res: &engine.PDUReleaseResult{PDUSessionID: 1, PTI: 1, SentAt: t0, Rejected: true, RejectCause: 35}, obs: obs(""), cut: true,
			want: Skip, wantObserved: "core rejected the release (PDU Session Release Reject, 5GSM cause #35)"},
		{name: "undecodable downlink, no command", res: &engine.PDUReleaseResult{PDUSessionID: 1, PTI: 1, SentAt: t0, DecodeErr: errors.New("nas plain decode")}, obs: obs(""),
			want: Error, wantObserved: "downlink NAS after the request did not decode", wantDetail: []string{"nas plain decode", "codec covers about Rel-15/16"}},
		{name: "undecodable downlink beside the commands", res: with(func(r *engine.PDUReleaseResult) { r.DecodeErr = errors.New("x") }), obs: obs(""), want: Pass},
		{name: "cut short", res: both(), obs: obs(""), cut: true, want: Error, wantObserved: "timed out before the release was judged",
			wantDetail: []string{"raise --per-test-timeout to at least 43s"}},
		{name: "cut short with no answer", res: &engine.PDUReleaseResult{PDUSessionID: 1, PTI: 1, SentAt: t0}, obs: obs(""), cut: true,
			want: Error, wantObserved: "timed out before the release was judged"},
		{name: "procedure failed", err: errors.New("UE 208930100000011 has no PDU session"), obs: obs(""),
			want: Error, wantObserved: "PDU session release procedure failed", wantDetail: []string{"has no PDU session"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgePDURelease(tc.res, tc.err, tc.obs, tc.cut, nasPDUReleaseUERequested{})
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

// The FAIL message is the one the check's issue fixes, with the clauses the
// archive reading pinned.
func TestPDUReleaseFailMessage(t *testing.T) {
	const want = "PDU session not released on UE request. TS 24.501 6.4.3.3, 6.3.3.2; TS 23.502 4.3.4.2; TS 38.413 8.2.2.2. Likely cause: SMF release path."
	if pduReleaseFail != want {
		t.Errorf("FAIL message %q, want %q", pduReleaseFail, want)
	}
}

// pduReleaseTestCheck is the check with waits short enough for CI and long
// enough for a loaded host: 1s for the second command (two PDUs sent back
// to back over SCTP can arrive some 200ms apart), a 300ms grace and a 700ms
// watch.
var pduReleaseTestCheck = nasPDUReleaseUERequested{registerWait: 5 * time.Second, wait: 500 * time.Millisecond,
	settle: time.Second, grace: 300 * time.Millisecond, watch: 700 * time.Millisecond}

// Every branch over the real-SCTP mock pair: the release with the NAS
// command inside the NGAP command or apart, each command withheld, both
// withheld, a reject, and a UPF that sends on the released tunnel within
// the grace (in flight) or after it (the stub withholding the release). The
// engine answers what it gets, the UPF is told, the UE is deregistered and
// no association stays open.
func TestPDUReleaseCheckAgainstMockPair(t *testing.T) {
	answer := func(a mockamf.PDUReleaseAnswer) []mockamf.Option {
		return []mockamf.Option{mockamf.WithPDUReleaseAnswer(a)}
	}
	tests := []struct {
		name          string
		net24         int
		upf           mockupf.Options
		amf           []mockamf.Option
		traffic       string
		want          Verdict
		wantObserved  string
		wantDetail    []string
		wantCompletes int64 // PDU SESSION RELEASE COMPLETEs
		wantResponses int64 // PDU SESSION RESOURCE RELEASE RESPONSEs
		wantReleases  int   // sessions the UPF was told were released
	}{
		{name: "released", net24: testnet.ConformancePDURelease, want: Pass,
			wantObserved:  "PDU session released on N1 and N2; the released tunnel stayed quiet",
			wantDetail:    []string{"inside the PDU SESSION RESOURCE RELEASE COMMAND, 5GSM cause #36, PTI 1 echoed", "0 after that", "no echo peer (--traffic)"},
			wantCompletes: 1, wantResponses: 1, wantReleases: 1},
		{name: "released, the commands apart", net24: testnet.ConformancePDUReleaseApart, amf: answer(mockamf.ReleaseSeparately), want: Pass,
			wantObserved: "PDU session released on N1 and N2; the released tunnel stayed quiet", wantDetail: []string{"in a DOWNLINK NAS TRANSPORT"},
			wantCompletes: 1, wantResponses: 1, wantReleases: 1},
		{name: "no answer", net24: testnet.ConformancePDUReleaseSilent, amf: answer(mockamf.IgnorePDURelease), want: Fail,
			wantObserved: "no answer to the PDU Session Release Request within 500ms", wantDetail: []string{pduReleaseFail}},
		{name: "the NAS command alone", net24: testnet.ConformancePDUReleaseNAS, amf: answer(mockamf.ReleaseNASOnly), want: Fail,
			wantObserved: "PDU Session Release Command without a PDU SESSION RESOURCE RELEASE COMMAND", wantDetail: []string{pduReleaseFail, "TS 23.502 4.3.4.2 step 3"},
			wantCompletes: 1, wantReleases: 1},
		{name: "the NGAP command alone", net24: testnet.ConformancePDUReleaseNGAP, amf: answer(mockamf.ReleaseNGAPOnly), want: Fail,
			wantObserved: "PDU SESSION RESOURCE RELEASE COMMAND without a PDU Session Release Command", wantDetail: []string{pduReleaseFail, "TS 24.501 6.3.3.2"},
			wantResponses: 1, wantReleases: 1},
		{name: "rejected", net24: testnet.ConformancePDUReleaseReject, amf: answer(mockamf.RejectPDURelease), want: Skip,
			wantObserved: "core rejected the release (PDU Session Release Reject, 5GSM cause #43)", wantDetail: []string{"TS 24.501 6.4.3.4"}},
		{name: "downlink continues", net24: testnet.ConformancePDUReleaseLate, upf: mockupf.Options{GPDUAfterRelease: 600 * time.Millisecond}, want: Fail,
			wantObserved:  "1 downlink G-PDUs on the released tunnel after the release",
			wantDetail:    []string{pduReleaseFail, "downlink continues", "TS 23.502 4.3.4.2 step 2a", "0 up to 300ms after the release, 1 after that"},
			wantCompletes: 1, wantResponses: 1, wantReleases: 1},
		{name: "downlink in flight at the release", net24: testnet.ConformancePDUReleaseGrace, upf: mockupf.Options{GPDUAfterRelease: 20 * time.Millisecond}, want: Pass,
			wantDetail: []string{"1 up to 300ms after the release, 0 after that"}, wantCompletes: 1, wantResponses: 1, wantReleases: 1},
		// The stub UPF does not echo, so the flow reaches it and brings no
		// downlink back; it keeps arriving after the release, on a tunnel
		// the UPF no longer knows.
		{name: "traffic configured", net24: testnet.ConformancePDUReleaseFlow, traffic: "192.0.2.7:7", want: Pass,
			wantDetail:    []string{"echo flow to 192.0.2.7:7 kept sending uplink on the released tunnel", "check the echo peer"},
			wantCompletes: 1, wantResponses: 1, wantReleases: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCore(t, tc.net24, tc.upf, testKi, append([]mockamf.Option{mockamf.WithGUTI()}, tc.amf...)...)
			env := ueEnv(c.front.Addr, "", c.n3)
			env.UE.Traffic = tc.traffic
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r := runOne(ctx, pduReleaseTestCheck, env, 20*time.Second)
			if r.Verdict != tc.want || (tc.wantObserved != "" && r.Observed != tc.wantObserved) {
				t.Fatalf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			for _, w := range append([]string{"UE deregistered"}, tc.wantDetail...) {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if got := c.amf.PDUReleaseRequests(); got != 1 {
				t.Errorf("the mock got %d PDU Session Release Requests, want 1", got)
			}
			if c.amf.PDUReleaseCompletes() != tc.wantCompletes || c.amf.PDUResourceReleaseResponses() != tc.wantResponses {
				t.Errorf("the mock got %d Release Completes and %d NGAP Release Responses, want %d and %d",
					c.amf.PDUReleaseCompletes(), c.amf.PDUResourceReleaseResponses(), tc.wantCompletes, tc.wantResponses)
			}
			if got := c.upf.Releases(); len(got) != tc.wantReleases {
				t.Errorf("the UPF was told of %d releases, want %d", len(got), tc.wantReleases)
			}
			if got := c.amf.Deregistrations(); got != 1 {
				t.Errorf("the mock got %d Deregistration Requests, want 1", got)
			}
			if errs := c.upf.SendErrors(); len(errs) != 0 {
				t.Errorf("the stub UPF failed to send: %v", errs)
			}
			if tc.traffic != "" {
				unknown := 0
				for _, u := range c.upf.Uplink() {
					if !u.Known {
						unknown++
					}
				}
				if unknown == 0 {
					t.Error("no uplink reached the UPF on the released tunnel")
				}
			}
			c.assertClosed(t, env)
		})
	}
}

// Without a UE the check SKIPs; a registration that fails is ERROR before
// any release is sent.
func TestPDUReleaseCheckWithoutASession(t *testing.T) {
	amf, front := deregMock(t, false, mockamf.WithAuthenticationReject())
	env := ueEnv(front.Addr, "", "127.0.0.1")
	noUE := env
	noUE.UE = UESpec{}
	tests := []struct {
		name         string
		env          Env
		want         Verdict
		wantObserved string
	}{
		{name: "no UE", env: noUE, want: Skip, wantObserved: "no UE configured"},
		{name: "registration fails", env: env, want: Error, wantObserved: "UE setup failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			r := runOne(ctx, pduReleaseTestCheck, tc.env, 15*time.Second)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
		})
	}
	if amf.PDUReleaseRequests() != 0 {
		t.Errorf("the mock got %d PDU Session Release Requests", amf.PDUReleaseRequests())
	}
	assertNoAssociations(t, front)
}

// The check's time running out while it waits for the answer is the
// harness's ERROR, and the UE is still deregistered.
func TestPDUReleaseCheckCutShort(t *testing.T) {
	c := newMockCore(t, testnet.ConformancePDUReleaseCut, mockupf.Options{}, testKi, mockamf.WithGUTI(), mockamf.WithPDUReleaseAnswer(mockamf.IgnorePDURelease))
	env := ueEnv(c.front.Addr, "", c.n3)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	check := pduReleaseTestCheck
	check.wait = time.Minute
	r := check.Run(ctx, env)
	if r.Verdict != Error || r.Observed != "timed out before the release was judged" || !strings.Contains(r.Detail, "UE deregistered") {
		t.Errorf("= %s %q (%s), want ERROR for the timeout and a deregistered UE", r.Verdict, r.Observed, r.Detail)
	}
	c.assertClosed(t, env)
}

// The registry stamps the check's identity and its four shall references.
func TestPDUReleaseCheckIdentity(t *testing.T) {
	c := testByID(t, "NAS-PDU-RELEASE-UE-REQUESTED")
	if c.Category() != Procedural || WorstVerdict(c) != Fail || MinRelease(c) != 15 {
		t.Errorf("category %s, worst verdict %s, min release %s", c.Category(), WorstVerdict(c), MinRelease(c))
	}
	var shall []string
	for _, r := range c.SpecRefs() {
		if r.Strength == Shall {
			shall = append(shall, r.Spec+" "+r.Clause)
		}
	}
	if fmt.Sprint(shall) != "[TS 24.501 6.4.3.3 TS 24.501 6.3.3.2 TS 23.502 4.3.4.2 step 2a TS 23.502 4.3.4.2 step 3]" {
		t.Errorf("shall references %v", shall)
	}
	if n := NeedsOf(c); n.UEs != 1 || n.RequiresIdleCore {
		t.Errorf("needs %+v, want one UE and no idle core", n)
	}
	if b, ok := c.(Budgeter); !ok || b.Budget() != 43*time.Second {
		t.Errorf("budget %v, want 43s", c.(Budgeter).Budget())
	}
}
