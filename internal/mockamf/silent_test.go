package mockamf_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// WithSilentAfterNGSetup answers the NG Setup and nothing after it: a RAN
// Configuration Update the default mock acknowledges goes unanswered.
func TestMockSilentAfterNGSetup(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	cfg := gnb.Config{ID: 1, Name: "g", MCC: "208", MNC: "93", TAC: 7, Slices: []gnb.SNSSAI{{SST: 1}}}
	tests := []struct {
		name   string
		opts   []mockamf.Option
		answer bool
	}{
		{name: "default", opts: []mockamf.Option{mockamf.WithRANConfigUpdate(mockamf.AcknowledgeRANConfigUpdate)}, answer: true},
		{name: "silent", opts: []mockamf.Option{mockamf.WithRANConfigUpdate(mockamf.AcknowledgeRANConfigUpdate), mockamf.WithSilentAfterNGSetup()}},
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
			ng, err := gnb.NGSetup(ctx, conn, cfg)
			if err != nil || !ng.Accepted {
				t.Fatalf("NGSetup: %+v, %v", ng, err)
			}
			pdu, err := gnb.BuildRANConfigurationUpdate()
			if err != nil {
				t.Fatal(err)
			}
			wctx, wcancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer wcancel()
			_, err = gnb.RANConfigurationUpdate(wctx, conn, pdu)
			if answered := err == nil; answered != tc.answer {
				t.Errorf("answered = %v (%v), want %v", answered, err, tc.answer)
			}
			if !tc.answer && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err = %v, want the wait to run out", err)
			}
		})
	}
}
