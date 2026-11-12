package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// nasMessageType reads a plain message's type at offset 2 and a protected
// one's inner type at offset 9 when integrity-only or ciphered under NEA0;
// a ciphered message under another algorithm, and anything it cannot read,
// is 0.
func TestNASMessageType(t *testing.T) {
	prot := func(sht byte) []byte { return []byte{0x7e, sht, 1, 2, 3, 4, 0, 0x7e, 0x00, 0x42, 0x01} }
	tests := []struct {
		name string
		raw  []byte
		nea0 bool
		want uint8
	}{
		{name: "plain", raw: []byte{0x7e, 0x00, 0x56, 0x01}, want: f5nas.MsgTypeAuthenticationRequest},
		{name: "integrity only", raw: prot(0x01), want: f5nas.MsgTypeRegistrationAccept},
		{name: "integrity only, new context", raw: prot(0x03), want: f5nas.MsgTypeRegistrationAccept},
		{name: "ciphered under NEA0", raw: prot(0x02), nea0: true, want: f5nas.MsgTypeRegistrationAccept},
		{name: "ciphered under NEA0, new context", raw: prot(0x04), nea0: true, want: f5nas.MsgTypeRegistrationAccept},
		{name: "ciphered under another algorithm", raw: prot(0x02), want: 0},
		{name: "ciphered, new context, another algorithm", raw: prot(0x04), want: 0},
		{name: "too short", raw: []byte{0x7e, 0x00}, want: 0},
		{name: "protected but truncated", raw: []byte{0x7e, 0x01, 1, 2, 3, 4, 0, 0x7e}, want: 0},
		{name: "5GSM, not 5GMM", raw: []byte{0x2e, 0x01, 0x01, 0xc2}, want: 0},
		{name: "unknown security header type", raw: prot(0x0c), nea0: true, want: 0},
		{name: "protected with an unreadable inner message", raw: []byte{0x7e, 0x01, 1, 2, 3, 4, 0, 0x99, 0x00, 0x42}, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nasMessageType(tc.raw, tc.nea0); got != tc.want {
				t.Errorf("nasMessageType = %#x, want %#x", got, tc.want)
			}
		})
	}
}

func TestProcedureName(t *testing.T) {
	for code, want := range map[int64]string{4: "DownlinkNASTransport", 14: "InitialContextSetup", 41: "UEContextRelease", 29: "PDUSessionResourceSetup", 28: "PDUSessionResourceRelease", 26: "PDUSessionResourceModify", 99: "procedure 99"} {
		if got := procedureName(code); got != want {
			t.Errorf("procedureName(%d) = %q, want %q", code, got, want)
		}
	}
}

// A probe withholds at the reaction: the UE stops where the withheld reply
// would have been sent, emitting no later state event, while OnDownlink
// sees every downlink with its NAS type. With T3560 on the mock, the release
// ends the attach with ErrReleasedByCore. An Authentication Reject ends it
// with ErrAuthenticationRejected unless AfterAuthenticationReject says to
// wait on, and then the release or the context ends it. A nil probe is the
// ordinary attach.
func TestAttachProbe(t *testing.T) {
	yes := true
	tests := []struct {
		name     string
		probe    bool
		withhold uint8
		// afterReject, when set, is what AfterAuthenticationReject answers.
		afterReject *bool
		opts        []mockamf.Option
		wantStates  []string
		wantNAS     []uint8 // downlink NAS types the probe saw, in order
		wantErr     error
	}{
		{name: "nil probe registers", wantStates: []string{StateRegistering, StateAuthenticated, StateSecurityEstablished, StateRegistered}},
		{name: "probe withholding nothing registers", probe: true,
			wantStates: []string{StateRegistering, StateAuthenticated, StateSecurityEstablished, StateRegistered},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeSecurityModeCommand, f5nas.MsgTypeRegistrationAccept}},
		{name: "authentication response withheld", probe: true, withhold: f5nas.MsgTypeAuthenticationResponse,
			wantStates: []string{StateRegistering}, wantNAS: []uint8{f5nas.MsgTypeAuthenticationRequest}, wantErr: context.DeadlineExceeded},
		{name: "security mode complete withheld", probe: true, withhold: f5nas.MsgTypeSecurityModeComplete,
			wantStates: []string{StateRegistering, StateAuthenticated},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeSecurityModeCommand}, wantErr: context.DeadlineExceeded},
		{name: "registration complete withheld", probe: true, withhold: f5nas.MsgTypeRegistrationComplete,
			wantStates: []string{StateRegistering, StateAuthenticated, StateSecurityEstablished},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeSecurityModeCommand, f5nas.MsgTypeRegistrationAccept},
			wantErr:    context.DeadlineExceeded},
		{name: "released by the core", probe: true, withhold: f5nas.MsgTypeAuthenticationResponse,
			opts:       []mockamf.Option{mockamf.WithT3560(30*time.Millisecond, 1)},
			wantStates: []string{StateRegistering},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeAuthenticationRequest, 0}, wantErr: ErrReleasedByCore},
		{name: "an authentication reject ends the attach", probe: true, withhold: f5nas.MsgTypeAuthenticationResponse,
			opts:       []mockamf.Option{mockamf.WithT3560(30*time.Millisecond, 1), mockamf.WithT3560RejectOnLastExpiry()},
			wantStates: []string{StateRegistering},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeAuthenticationReject},
			wantErr:    ErrAuthenticationRejected},
		{name: "an authentication reject the probe declines to wait after", probe: true, withhold: f5nas.MsgTypeAuthenticationResponse, afterReject: new(bool),
			opts:       []mockamf.Option{mockamf.WithT3560(30*time.Millisecond, 1), mockamf.WithT3560RejectOnLastExpiry()},
			wantStates: []string{StateRegistering},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeAuthenticationReject},
			wantErr:    ErrAuthenticationRejected},
		{name: "waiting on after an authentication reject, released", probe: true, withhold: f5nas.MsgTypeAuthenticationResponse, afterReject: &yes,
			opts:       []mockamf.Option{mockamf.WithT3560(30*time.Millisecond, 1), mockamf.WithT3560RejectOnLastExpiry()},
			wantStates: []string{StateRegistering},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeAuthenticationReject, 0},
			wantErr:    ErrReleasedByCore},
		{name: "waiting on after an authentication reject, never released", probe: true, withhold: f5nas.MsgTypeAuthenticationResponse, afterReject: &yes,
			opts:       []mockamf.Option{mockamf.WithT3560(30*time.Millisecond, 1), mockamf.WithT3560RejectOnLastExpiry(), mockamf.WithT3560NoRelease()},
			wantStates: []string{StateRegistering},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeAuthenticationReject},
			wantErr:    context.DeadlineExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
			opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }()
			cfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
			if ng, err := gnb.NGSetup(ctx, conn, cfg); err != nil || !ng.Accepted {
				t.Fatalf("NG Setup: %v", err)
			}
			const supi = "208930100007501"
			id, err := ue.ParseIdentity(supi, "208", "93", "0")
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var states []string
			var nasTypes []uint8
			ucfg := UEConfig{Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc}}
			if tc.probe {
				ucfg.Probe = &AttachProbe{OnDownlink: func(ev DownlinkEvent) {
					mu.Lock()
					nasTypes = append(nasTypes, ev.NASType)
					mu.Unlock()
					if ev.NASType != 0 && len(ev.NAS) == 0 {
						t.Errorf("event %+v has a NAS type but no NAS", ev)
					}
				}}
				if tc.withhold != 0 {
					ucfg.Probe.Withhold = map[uint8]bool{tc.withhold: true}
				}
				if tc.afterReject != nil {
					ucfg.Probe.AfterAuthenticationReject = func() bool { return *tc.afterReject }
				}
			}
			_, err = Attach(ctx, conn, cfg, ucfg, slog.New(slog.NewTextHandler(io.Discard, nil)), func(ev StateEvent) {
				mu.Lock()
				states = append(states, ev.State)
				mu.Unlock()
			})
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Errorf("Attach err = %v, want %v", err, tc.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if fmt.Sprint(states) != fmt.Sprint(tc.wantStates) {
				t.Errorf("states %v, want %v", states, tc.wantStates)
			}
			if !slices.Equal(nasTypes, tc.wantNAS) {
				t.Errorf("probe saw NAS types %v, want %v", nasTypes, tc.wantNAS)
			}
		})
	}
}

// OnSMRequest sees the PDU Session Establishment Request the UE sends, byte
// for byte; OnNAS sees each downlink as the security context decoded it, a
// MAC failure as nas.ErrIntegrity, with the header's message type, and a
// downlink that does not decode no longer ends the attach; Stop ends it,
// returning a REGISTERED UE's session and failing an earlier one with
// ErrProbeStopped.
func TestAttachProbeSMAnswer(t *testing.T) {
	const dnn = "orbit-unknown-dnn"
	p := ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: dnn}
	want, err := ue.BuildPDUSessionEstablishmentRequest(p)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		answer        mockamf.DNNAnswer
		noSMHook      bool
		stopAtAuth    bool // close Stop at the Authentication Request, which is withheld
		wantSMs       int
		wantErr       error // nil: a registered session comes back
		wantNASErr    bool
		wantIntegrity bool // the decode error is nas.ErrIntegrity
		wantReturn    bool // OnNAS saw the request returned
	}{
		{name: "returned, stop after the answer", answer: mockamf.ReturnDNN, wantSMs: 1, wantReturn: true},
		{name: "no OnSMRequest hook", answer: mockamf.ReturnDNN, noSMHook: true, wantReturn: true},
		{name: "bad MAC", answer: mockamf.BadMACDNN, wantSMs: 1, wantNASErr: true, wantIntegrity: true},
		{name: "undecodable", answer: mockamf.UndecodableDNN, wantSMs: 1, wantNASErr: true},
		{name: "stopped before registration", answer: mockamf.ReturnDNN, stopAtAuth: true, wantErr: ErrProbeStopped},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
			opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, mockamf.WithUnknownDNN(dnn, tc.answer, 0))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }()
			cfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
			if ng, err := gnb.NGSetup(ctx, conn, cfg); err != nil || !ng.Accepted {
				t.Fatalf("NG Setup: %v", err)
			}
			const supi = "208930100007501"
			id, err := ue.ParseIdentity(supi, "208", "93", "0")
			if err != nil {
				t.Fatal(err)
			}
			stop := make(chan struct{})
			var once sync.Once
			var mu sync.Mutex
			var sms [][]byte
			var nasErr error
			var nasErrType uint8
			var returned bool
			probe := &AttachProbe{Stop: stop, OnNAS: func(ev NASEvent) {
				mu.Lock()
				defer mu.Unlock()
				if ev.Err != nil {
					nasErr, nasErrType = ev.Err, ev.Type
					once.Do(func() { close(stop) })
					return
				}
				if tr := ev.Msg.DLNASTransport; ev.Msg.GmmMessage != nil && tr != nil {
					returned = slices.Equal(tr.GetPayloadContainerContents(), want)
					once.Do(func() { close(stop) })
				}
			}}
			if !tc.noSMHook {
				probe.OnSMRequest = func(sm []byte) {
					mu.Lock()
					sms = append(sms, sm)
					mu.Unlock()
				}
			}
			if tc.stopAtAuth {
				probe.Withhold = map[uint8]bool{f5nas.MsgTypeAuthenticationResponse: true}
				probe.OnDownlink = func(ev DownlinkEvent) {
					if ev.NASType == f5nas.MsgTypeAuthenticationRequest {
						once.Do(func() { close(stop) })
					}
				}
			}
			sess, err := Attach(ctx, conn, cfg, UEConfig{Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc}, PDUSession: &p, Probe: probe},
				slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			switch {
			case tc.wantErr == nil:
				if err != nil || sess == nil || !sess.Result.Registered || sess.Result.SessionActive {
					t.Fatalf("Attach = %+v, %v; want a registered UE without a session", sess, err)
				}
			case !errors.Is(err, tc.wantErr):
				t.Fatalf("Attach err = %v, want %v", err, tc.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(sms) != tc.wantSMs {
				t.Fatalf("OnSMRequest called %d times, want %d", len(sms), tc.wantSMs)
			}
			for _, sm := range sms {
				if !slices.Equal(sm, want) {
					t.Errorf("OnSMRequest got %x, want %x", sm, want)
				}
			}
			if (nasErr != nil) != tc.wantNASErr || returned != tc.wantReturn {
				t.Errorf("OnNAS err %v, returned %v; want err %v, returned %v", nasErr, returned, tc.wantNASErr, tc.wantReturn)
			}
			if nasErr != nil && (errors.Is(nasErr, nas.ErrIntegrity) != tc.wantIntegrity || nasErrType != f5nas.MsgTypeDLNASTransport) {
				t.Errorf("OnNAS err %v type %#x; want integrity failure %v, type DL NAS Transport", nasErr, nasErrType, tc.wantIntegrity)
			}
		})
	}
}

// The probe's non-conformant UE modes, each off by default. InitialNAS
// replaces the cleartext-only initial message, here with the complete
// Registration Request; OmitNASMessageContainer leaves the container out of
// every Security Mode Complete, a second one included; OmitIMEISV leaves a
// requested IMEISV out. Without them the initial message has cleartext IEs
// only, the Complete carries the complete request, and a requested IMEISV
// is sent.
func TestAttachProbeNonConformantModes(t *testing.T) {
	tests := []struct {
		name          string
		probe         *AttachProbe
		opts          []mockamf.Option
		wantPlainFull bool // the initial NAS message is the complete request
		wantContainer bool
		wantIMEISV    bool
		wantSMCs      int64
		wantErr       error
	}{
		{name: "conformant", probe: &AttachProbe{}, opts: []mockamf.Option{mockamf.WithSMCIMEISVRequest()},
			wantContainer: true, wantIMEISV: true, wantSMCs: 1},
		{name: "complete request as the initial message", probe: &AttachProbe{InitialNAS: func(complete []byte) ([]byte, error) { return complete, nil }},
			wantPlainFull: true, wantContainer: true, wantSMCs: 1},
		{name: "initial message refused for its non-cleartext IEs", probe: &AttachProbe{InitialNAS: func(complete []byte) ([]byte, error) { return complete, nil }},
			opts: []mockamf.Option{mockamf.WithRejectNonCleartextIEs()}, wantPlainFull: true, wantErr: ErrRegistrationRejected},
		{name: "initial message cannot be built", probe: &AttachProbe{InitialNAS: func([]byte) ([]byte, error) { return nil, errors.New("no stimulus") }},
			wantErr: errNoInitial},
		{name: "no container", probe: &AttachProbe{OmitNASMessageContainer: true}, wantSMCs: 1},
		{name: "no container, asked again", probe: &AttachProbe{OmitNASMessageContainer: true},
			opts: []mockamf.Option{mockamf.WithMissingContainer(mockamf.RequestInitialNAS)}, wantSMCs: 2},
		{name: "no container, rejected", probe: &AttachProbe{OmitNASMessageContainer: true},
			opts: []mockamf.Option{mockamf.WithMissingContainer(mockamf.RejectMissingContainer)}, wantSMCs: 1, wantErr: ErrRegistrationRejected},
		{name: "requested IMEISV left out", probe: &AttachProbe{OmitIMEISV: true}, opts: []mockamf.Option{mockamf.WithSMCIMEISVRequest()},
			wantContainer: true, wantSMCs: 1},
		{name: "requested IMEISV left out, identified", probe: &AttachProbe{OmitIMEISV: true},
			opts:          []mockamf.Option{mockamf.WithSMCIMEISVRequest(), mockamf.WithMissingIMEISV(mockamf.IdentifyMissingIMEISV)},
			wantContainer: true, wantSMCs: 1},
		{name: "requested IMEISV left out, rejected", probe: &AttachProbe{OmitIMEISV: true},
			opts:          []mockamf.Option{mockamf.WithSMCIMEISVRequest(), mockamf.WithMissingIMEISV(mockamf.RejectMissingIMEISV)},
			wantContainer: true, wantSMCs: 1, wantErr: ErrRegistrationRejected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
			opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }()
			cfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
			if ng, err := gnb.NGSetup(ctx, conn, cfg); err != nil || !ng.Accepted {
				t.Fatalf("NG Setup: %v", err)
			}
			const supi = "208930100007502"
			id, err := ue.ParseIdentity(supi, "208", "93", "0")
			if err != nil {
				t.Fatal(err)
			}
			ucfg := UEConfig{Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc}, Probe: tc.probe,
				RequestedNSSAI: []ue.SNSSAI{{SST: 1, SD: "010203"}}}
			_, err = Attach(ctx, conn, cfg, ucfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if tc.wantErr == errNoInitial {
				if err == nil || len(amf.InitialRegistrationRequests()) != 0 {
					t.Fatalf("Attach err = %v with %d initial messages sent, want a build error and none", err, len(amf.InitialRegistrationRequests()))
				}
				return
			}
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("Attach err = %v, want %v", err, tc.wantErr)
			}
			initial, final := amf.InitialRegistrationRequests(), amf.RegistrationRequests()
			if len(initial) != 1 || len(final) != 1 {
				t.Fatalf("mock saw %d initial and %d final Registration Requests, want one each", len(initial), len(final))
			}
			_, nonCleartext, err := nas.NonCleartextRegistrationIE(initial[0])
			if err != nil || nonCleartext != tc.wantPlainFull {
				t.Errorf("initial NAS message has non-cleartext IEs: %v (err %v), want %v", nonCleartext, err, tc.wantPlainFull)
			}
			// With a container the mock takes its request, which has the
			// non-cleartext IEs; without one the initial message stands. A
			// complete initial message shows nothing either way.
			if _, finalFull, _ := nas.NonCleartextRegistrationIE(final[0]); !tc.wantPlainFull && finalFull != tc.wantContainer {
				t.Errorf("the mock took a request from a container: %v, want %v", finalFull, tc.wantContainer)
			}
			if tc.wantSMCs > 0 {
				if got := amf.SecurityModeCommands(); got != tc.wantSMCs {
					t.Errorf("mock sent %d Security Mode Commands, want %d", got, tc.wantSMCs)
				}
				imeisvs := amf.SecurityModeCompleteIMEISVs()
				if int64(len(imeisvs)) != tc.wantSMCs {
					t.Fatalf("mock saw %d Security Mode Completes, want %d", len(imeisvs), tc.wantSMCs)
				}
				if got := imeisvs[0] != nil; got != tc.wantIMEISV {
					t.Errorf("Security Mode Complete carried an IMEISV: %v, want %v", got, tc.wantIMEISV)
				}
			}
			if n := amf.EncodeErrors(); n != 0 {
				t.Errorf("mock AMF dropped %d PDUs that failed to encode", n)
			}
		})
	}
}

// errNoInitial marks the row whose InitialNAS fails.
var errNoInitial = errors.New("initial NAS message not built")
