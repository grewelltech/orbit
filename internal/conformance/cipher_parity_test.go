package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// Every UE-level check gives the same verdict and Observed when the core
// ciphers NAS with NEA2 as when it selects NEA0: the UE advertises NEA0 and
// NEA2 and the mock prefers NEA2. Checks read the NAS type from the
// deciphered message (NASEvent.MessageType), never from a ciphered header.
// The T3550 row is the reviewer's reproduction of #188 MEDIUM-1.
func TestUELevelChecksUnderNEA2(t *testing.T) {
	const T, Tmax = 50 * time.Millisecond, 200 * time.Millisecond
	tests := []struct {
		name   string
		check  Test
		opts   []mockamf.Option
		net24  int // > 0: run over the mock pair with a stub UPF on this /24
		upfOpt mockupf.Options
	}{
		{name: "NAS-T3560-AUTH-RETRANSMIT", check: nasT3560AuthRetransmit{t3560: T, tmax: Tmax, authWait: 100 * time.Millisecond},
			opts: []mockamf.Option{mockamf.WithT3560(T, 4)}},
		{name: "NAS-T3550-ACCEPT-RETRANSMIT", check: nasT3550AcceptRetransmit{t3550: T, tmax: Tmax, acceptWait: 100 * time.Millisecond},
			opts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithT3550(T, 4)}},
		{name: "NAS-PDU-UNKNOWN-DNN", check: nasPDUUnknownDNN{registerWait: 3 * time.Second, answerWait: 300 * time.Millisecond},
			opts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithUnknownDNN(stimulusDNN, mockamf.RejectDNN, 0)}},
		{name: "NAS-SMC-NO-NIA0-REPLAYED-CAPS", check: nasSMCNoNIA0{wait: 300 * time.Millisecond}},
		// A real Reject #62 with the stimulus in the Rejected NSSAI IE, so
		// the row reaches the IE walk on the deciphered message.
		{name: "NAS-REG-REJECTED-NSSAI", check: nasRegRejectedNSSAI{wait: 500 * time.Millisecond},
			opts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithUnservedNSSAI(mockamf.Reject62NSSAI)}},
		{name: "GTPU-END-MARKER-ON-PATH-SWITCH", check: endMarkerOnPathSwitch{window: 300 * time.Millisecond},
			net24: 68, upfOpt: mockupf.Options{EndMarker: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := func(nea2 bool, net24 int) Result {
				opts := append([]mockamf.Option(nil), tc.opts...)
				if nea2 {
					opts = append(opts, mockamf.WithCipheringOrder(nas.NEA2, nas.NEA0))
				}
				var env Env
				if tc.net24 > 0 {
					c := newMockCore(t, net24, tc.upfOpt, testKi, opts...)
					env = ueEnv(c.front.Addr, "", c.n3)
				} else {
					k, _ := auth.ParseHexKey("Ki", testKi)
					opc, _ := auth.ParseHexKey("OPc", testOPc)
					amf, err := mockamf.New(mockamf.Config{
						Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
						MCC: "208", MNC: "93", SST: 1, SD: "010203",
					}, opts...)
					if err != nil {
						t.Fatal(err)
					}
					env = ueEnv(sctpfront.Start(t, amf).Addr, "", "127.0.0.1")
				}
				if nea2 {
					env.UE.NASCiphering = 0x80>>nas.NEA0 | 0x80>>nas.NEA2
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				return runOne(ctx, tc.check, env, 20*time.Second)
			}
			base := run(false, tc.net24)
			if base.Verdict != Pass {
				t.Fatalf("under NEA0: %s %q (%s), want PASS", base.Verdict, base.Observed, base.Detail)
			}
			got := run(true, tc.net24+1)
			if got.Verdict != base.Verdict || got.Observed != base.Observed {
				t.Errorf("under NEA2: %s %q (%s); under NEA0: %s %q", got.Verdict, got.Observed, got.Detail, base.Verdict, base.Observed)
			}
		})
	}
}
