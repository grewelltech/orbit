package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// ueCapability: zero keeps the default (nil); a set bitmap is advertised,
// the other taking its default; NIA0 is refused.
func TestUECapability(t *testing.T) {
	tests := []struct {
		name    string
		ia, ea  uint8
		want    []byte // EA octet, IA octet; nil for the default
		wantErr bool
	}{
		{name: "default"},
		{name: "integrity only", ia: 0x70, want: []byte{0x80, 0x70}},
		{name: "ciphering only", ea: 0xe0, want: []byte{0xe0, 0x20}},
		{name: "both", ia: 0x40, ea: 0x20, want: []byte{0x20, 0x40}},
		{name: "NIA0", ia: 0xa0, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ueCapability(UESpec{NASIntegrity: tc.ia, NASCiphering: tc.ea})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if tc.want == nil {
				if c != nil {
					t.Errorf("= %x, want the default (nil)", c.Buffer)
				}
				return
			}
			if c == nil || c.GetLen() != 2 || string(c.Buffer[:2]) != string(tc.want) {
				t.Errorf("= %+v, want %x", c, tc.want)
			}
		})
	}
}

// A UE-level check runs unchanged under a non-default pair: the harness
// advertises 128-NIA1 and NEA2, the mock selects them, and
// NAS-PDU-UNKNOWN-DNN reads the SMF's Reject through the ciphered context.
func TestHarnessAdvertisesNASAlgorithms(t *testing.T) {
	k, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	amf, err := mockamf.New(mockamf.Config{
		Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	}, mockamf.WithGUTI(), mockamf.WithIntegrityOrder(nas.NIA1), mockamf.WithCipheringOrder(nas.NEA2),
		mockamf.WithUnknownDNN(stimulusDNN, mockamf.RejectDNN, 0))
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	env := ueEnv(front.Addr, "", "127.0.0.1")
	env.UE.NASIntegrity, env.UE.NASCiphering = 0x80>>nas.NIA1, 0x80>>nas.NEA2
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := runOne(ctx, nasPDUUnknownDNN{registerWait: 3 * time.Second, answerWait: 300 * time.Millisecond}, env, 15*time.Second)
	if r.Verdict != Pass || !strings.Contains(r.Detail, "UE deregistered") {
		t.Errorf("= %s %q (%s), want PASS with the UE deregistered", r.Verdict, r.Observed, r.Detail)
	}
}
