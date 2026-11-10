package mockamf_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// An uplink PDU that does not decode ends its association by default and
// leaves the mock serving others; the options answer it with an ERROR
// INDICATION (cause Protocol transfer-syntax-error), drop it, or bring the
// whole mock down.
func TestMockUndecodablePDU(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	cfg := gnb.Config{ID: 1, Name: "g", MCC: "208", MNC: "93", TAC: 7, Slices: []gnb.SNSSAI{{SST: 1}}}
	tests := []struct {
		name       string
		opts       []mockamf.Option
		wantErrInd bool
		wantSilent bool // no answer, association still up
		wantServes bool // a second association still completes NG Setup
	}{
		{name: "closes the association by default", wantServes: true},
		{name: "error indication", opts: []mockamf.Option{mockamf.WithUndecodablePDU(mockamf.ErrorIndicationOnUndecodable)}, wantErrInd: true, wantServes: true},
		{name: "ignored", opts: []mockamf.Option{mockamf.WithUndecodablePDU(mockamf.IgnoreUndecodable)}, wantSilent: true, wantServes: true},
		{name: "crashes", opts: []mockamf.Option{mockamf.WithUndecodablePDU(mockamf.CrashOnUndecodable)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }()
			if ng, err := gnb.NGSetup(ctx, conn, cfg); err != nil || !ng.Accepted {
				t.Fatalf("NGSetup: %+v, %v", ng, err)
			}
			// An NGAP PDU cut off inside its header.
			if err := conn.WriteNGAP(0, []byte{0x00, 0x23}); err != nil {
				t.Fatal(err)
			}
			rctx, rcancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer rcancel()
			pdu, err := gnb.ReadPDU(rctx, conn)
			switch {
			case tc.wantErrInd:
				if err != nil {
					t.Fatalf("no ERROR INDICATION: %v", err)
				}
				im := pdu.InitiatingMessage
				if im == nil || im.ProcedureCode.Value != ngapType.ProcedureCodeErrorIndication || im.Value.ErrorIndication == nil {
					t.Fatalf("answer is not an ERROR INDICATION: %+v", pdu)
				}
				ies := im.Value.ErrorIndication.ProtocolIEs.List
				if len(ies) != 1 || ies[0].Value.Cause == nil || ies[0].Value.Cause.Present != ngapType.CausePresentProtocol ||
					ies[0].Value.Cause.Protocol.Value != ngapType.CauseProtocolPresentTransferSyntaxError {
					t.Errorf("ERROR INDICATION IEs = %+v, want only the cause Protocol transfer-syntax-error", ies)
				}
			case tc.wantSilent:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("answer %+v, err %v; want nothing and the association up", pdu, err)
				}
			default:
				if err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("answer %+v, err %v; want the association closed", pdu, err)
				}
			}

			second := amf.Connect(ctx)
			defer func() { _ = second.Close() }()
			sctx, scancel := context.WithTimeout(ctx, time.Second)
			defer scancel()
			ng, err := gnb.NGSetup(sctx, second, cfg)
			if served := err == nil && ng.Accepted; served != tc.wantServes {
				t.Errorf("a second association's NG Setup: %+v, %v; want served %v", ng, err, tc.wantServes)
			}
			if n := amf.EncodeErrors(); n != 0 {
				t.Errorf("mock dropped %d answers that failed to encode", n)
			}
		})
	}
}
