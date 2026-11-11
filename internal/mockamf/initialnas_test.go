package mockamf_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// The engine's UE registers against a mock that rejects non-cleartext IEs
// in the initial NAS message (TS 24.501 4.4.6): the Initial UE Message
// carries cleartext IEs only, and the mock reads the 5GMM capability and
// Requested NSSAI from the Security Mode Complete's NAS message container.
// A Registration Request sent complete as the initial NAS message is
// rejected with 5GMM cause #95 by that mock and answered with an
// Authentication Request by the default one.
func TestInitialNASCleartextIEs(t *testing.T) {
	const supi = "208930100007500"
	gnbCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	id, err := ue.ParseIdentity(supi, "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	suci, err := id.EncodeNullSUCI()
	if err != nil {
		t.Fatal(err)
	}
	nssai, err := ue.BuildRequestedNSSAI([]ue.SNSSAI{{SST: 1, SD: "010203"}})
	if err != nil {
		t.Fatal(err)
	}
	complete, err := ue.BuildRegistrationRequest(suci, ue.SecurityCapability(), nssai)
	if err != nil {
		t.Fatal(err)
	}
	cleartext, err := ue.CleartextRegistrationRequest(complete)
	if err != nil {
		t.Fatal(err)
	}
	strict := []mockamf.Option{mockamf.WithRejectNonCleartextIEs()}

	tests := []struct {
		name string
		opts []mockamf.Option
		// initial, when set, is sent by hand as the initial NAS message;
		// otherwise the engine's UE attaches.
		initial     []byte
		wantAnswer  uint8 // the message type answering initial
		wantCause   uint8 // the 5GMM cause of a Registration Reject answer
		wantInitial []byte
	}{
		{name: "engine UE, strict mock", opts: strict, wantInitial: cleartext},
		{name: "engine UE, default mock", wantInitial: cleartext},
		{name: "complete request as the initial message, strict mock", opts: strict, initial: complete,
			wantAnswer: f5nas.MsgTypeRegistrationReject, wantCause: nasMessage.Cause5GMMSemanticallyIncorrectMessage, wantInitial: complete},
		{name: "complete request as the initial message, default mock", initial: complete,
			wantAnswer: f5nas.MsgTypeAuthenticationRequest, wantInitial: complete},
		{name: "cleartext request by hand, strict mock", opts: strict, initial: cleartext,
			wantAnswer: f5nas.MsgTypeAuthenticationRequest, wantInitial: cleartext},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }() // test teardown
			if ng, err := gnb.NGSetup(ctx, conn, gnbCfg); err != nil || !ng.Accepted {
				t.Fatalf("NG Setup: %v (%+v)", err, ng)
			}

			if tc.initial == nil {
				sess, err := engine.Attach(ctx, conn, gnbCfg, engine.UEConfig{
					Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
					RequestedNSSAI: []ue.SNSSAI{{SST: 1, SD: "010203"}},
				}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
				if err != nil || !sess.Result.Registered {
					t.Fatalf("attach: %v", err)
				}
				// The mock holds the complete request only if it deciphered
				// the Security Mode Complete and found it in the container.
				got := amf.RegistrationRequests()
				if len(got) != 1 || !bytes.Equal(got[0], complete) {
					t.Errorf("complete Registration Request at the mock = %x, want %x", got, complete)
				}
			} else {
				pdu, err := gnb.BuildInitialUEMessage(gnbCfg, 1, tc.initial)
				if err != nil {
					t.Fatal(err)
				}
				if err := gnb.SendPDU(conn, 1, pdu); err != nil {
					t.Fatal(err)
				}
				answer, err := gnb.ReadPDU(ctx, conn)
				if err != nil {
					t.Fatal(err)
				}
				dl, err := gnb.ParseDownlink(answer)
				if err != nil {
					t.Fatal(err)
				}
				m, err := nas.DecodePlain(dl.NASPDU)
				if err != nil {
					t.Fatal(err)
				}
				if mt := m.GmmHeader.GetMessageType(); mt != tc.wantAnswer {
					t.Fatalf("answer message type = %#x, want %#x", mt, tc.wantAnswer)
				}
				if m.RegistrationReject != nil && m.RegistrationReject.GetCauseValue() != tc.wantCause {
					t.Errorf("Registration Reject cause = #%d, want #%d", m.RegistrationReject.GetCauseValue(), tc.wantCause)
				}
			}
			if got := amf.InitialRegistrationRequests(); len(got) != 1 || !bytes.Equal(got[0], tc.wantInitial) {
				t.Errorf("initial NAS message at the mock = %x, want %x", got, tc.wantInitial)
			}
			if amf.EncodeErrors() != 0 {
				t.Errorf("mock dropped %d downlink PDUs that did not encode", amf.EncodeErrors())
			}
		})
	}
}
