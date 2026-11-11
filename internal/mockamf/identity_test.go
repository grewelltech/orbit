package mockamf_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// The engine's UE registers against a mock that asks for its identity: the
// IMEISV in the Security Mode Complete when the command requests it
// (TS 24.501 5.4.2.3), beside the complete Registration Request in the NAS
// message container, and an Identity Response to a protected Identity
// Request for any type (5.4.3.3). Before NAS security it answers only a
// request for the SUCI (4.4.4.2).
func TestIdentification(t *testing.T) {
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
	// 0010010000750000, the IMEISV derived from the SUPI, and the IMEI
	// 001001000075009.
	derived := []byte{0x05, 0x10, 0x00, 0x01, 0x00, 0x70, 0x05, 0x00, 0xf0}
	derivedIMEI := []byte{0x0b, 0x10, 0x00, 0x01, 0x00, 0x70, 0x05, 0x90}
	// 4370816125816151, configured.
	configured := []byte{0x45, 0x73, 0x80, 0x61, 0x21, 0x85, 0x61, 0x51, 0xf1}
	none := []byte{0x00}

	tests := []struct {
		name          string
		opts          []mockamf.Option
		imeisv        string   // Identity.IMEISV
		wantSMCIMEISV []byte   // the Security Mode Complete's IMEISV, nil for none
		wantResponses [][]byte // the Identity Responses, in order
	}{
		{name: "nothing asked"},
		{name: "IMEISV requested in the Security Mode Command", opts: []mockamf.Option{mockamf.WithSMCIMEISVRequest()}, wantSMCIMEISV: derived},
		{name: "configured IMEISV", opts: []mockamf.Option{mockamf.WithSMCIMEISVRequest()}, imeisv: "4370816125816151", wantSMCIMEISV: configured},
		{name: "Identity Request for the IMEISV", opts: []mockamf.Option{mockamf.WithIdentityRequest(ue.IdentityTypeIMEISV)}, wantResponses: [][]byte{derived}},
		{name: "Identity Request for the IMEI", opts: []mockamf.Option{mockamf.WithIdentityRequest(ue.IdentityTypeIMEI)}, wantResponses: [][]byte{derivedIMEI}},
		{name: "Identity Request for the SUCI", opts: []mockamf.Option{mockamf.WithIdentityRequest(ue.IdentityTypeSUCI)}, wantResponses: [][]byte{suci}},
		{name: "Identity Request for a 5G-GUTI not yet assigned", opts: []mockamf.Option{mockamf.WithIdentityRequest(ue.IdentityTypeGUTI)}, wantResponses: [][]byte{none}},
		{name: "Identity Request for an EUI-64", opts: []mockamf.Option{mockamf.WithIdentityRequest(7)}, wantResponses: [][]byte{none}},
		{name: "IMEISV in the Security Mode Complete, then asked again",
			opts:          []mockamf.Option{mockamf.WithSMCIMEISVRequest(), mockamf.WithIdentityRequest(ue.IdentityTypeIMEISV)},
			wantSMCIMEISV: derived, wantResponses: [][]byte{derived}},
		{name: "unprotected Identity Request for the SUCI", opts: []mockamf.Option{mockamf.WithPlainIdentityRequest(ue.IdentityTypeSUCI)}, wantResponses: [][]byte{suci}},
		{name: "unprotected Identity Request for the IMEISV is not answered", opts: []mockamf.Option{mockamf.WithPlainIdentityRequest(ue.IdentityTypeIMEISV)}},
		{name: "unprotected Identity Request for the IMEI is not answered", opts: []mockamf.Option{mockamf.WithPlainIdentityRequest(ue.IdentityTypeIMEI)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, append([]mockamf.Option{mockamf.WithRejectNonCleartextIEs()}, tc.opts...)...)
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
			ueID := id
			ueID.IMEISV = tc.imeisv
			sess, err := engine.Attach(ctx, conn, gnbCfg, engine.UEConfig{
				Identity: ueID, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
				RequestedNSSAI: []ue.SNSSAI{{SST: 1, SD: "010203"}},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if err != nil || !sess.Result.Registered {
				t.Fatalf("attach: %v", err)
			}

			smc := amf.SecurityModeCompleteIMEISVs()
			if len(smc) != 1 || !bytes.Equal(smc[0], tc.wantSMCIMEISV) || (smc[0] == nil) != (tc.wantSMCIMEISV == nil) {
				t.Errorf("Security Mode Complete IMEISV = %x, want %x", smc, tc.wantSMCIMEISV)
			}
			got := amf.IdentityResponses()
			if len(got) != len(tc.wantResponses) {
				t.Fatalf("Identity Responses = %x, want %x", got, tc.wantResponses)
			}
			for i := range got {
				if !bytes.Equal(got[i], tc.wantResponses[i]) {
					t.Errorf("Identity Response %d = %x, want %x", i, got[i], tc.wantResponses[i])
				}
			}
			// The container still holds the complete request: the mock
			// reads the Requested NSSAI only from it.
			reqs := amf.RegistrationRequests()
			if len(reqs) != 1 {
				t.Fatalf("mock saw %d Registration Requests", len(reqs))
			}
			if m, err := nas.DecodePlain(reqs[0]); err != nil || m.RegistrationRequest == nil || m.RequestedNSSAI == nil {
				t.Errorf("complete Registration Request at the mock lacks the Requested NSSAI: %x (%v)", reqs[0], err)
			}
			if amf.EncodeErrors() != 0 {
				t.Errorf("mock dropped %d downlink PDUs that did not encode", amf.EncodeErrors())
			}
		})
	}
}
