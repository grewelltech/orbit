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

// The mock's answers to an NG SETUP REQUEST whose gNB ID a live association
// already holds: by default both are accepted and coexist; configured, it
// rejects the second, replaces the first, leaves the second unanswered, or
// goes down. Another gNB ID is never a duplicate, and an ID is free again
// once its association has ended.
func TestMockDuplicateRANNodeID(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	cfg := gnb.Config{ID: 7, Name: "g", MCC: "208", MNC: "93", TAC: 7, Slices: []gnb.SNSSAI{{SST: 1}}}
	other := cfg
	other.ID = 8
	type outcome int
	const (
		accepted outcome = iota
		rejected
		unanswered
		closed
	)
	tests := []struct {
		name           string
		answer         mockamf.DuplicateRANNodeAnswer
		want           outcome
		wantCause      string
		wantFirstEnded bool
		wantDead       bool
	}{
		{name: "accepted by default", answer: mockamf.AcceptDuplicateRANNode, want: accepted},
		{name: "rejected", answer: mockamf.RejectDuplicateRANNode, want: rejected, wantCause: "misc(5)"},
		{name: "replaces the first", answer: mockamf.ReplaceDuplicateRANNode, want: accepted, wantFirstEnded: true},
		{name: "ignored", answer: mockamf.IgnoreDuplicateRANNode, want: unanswered},
		{name: "crash", answer: mockamf.CrashOnDuplicateRANNode, want: closed, wantDead: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1}, mockamf.WithDuplicateRANNodeID(tc.answer))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			setup := func(conn gnb.Transport, c gnb.Config) (*gnb.NGSetupResult, error) {
				sctx, scancel := context.WithTimeout(ctx, 300*time.Millisecond)
				defer scancel()
				return gnb.NGSetup(sctx, conn, c)
			}
			first := amf.Connect(ctx)
			defer func() { _ = first.Close() }()
			if ng, err := setup(first, cfg); err != nil || !ng.Accepted {
				t.Fatalf("first NG Setup: %+v, %v", ng, err)
			}
			// Another gNB ID is no duplicate, whatever the option.
			third := amf.Connect(ctx)
			if ng, err := setup(third, other); err != nil || !ng.Accepted {
				t.Fatalf("NG Setup with another gNB ID: %+v, %v", ng, err)
			}
			_ = third.Close()

			second := amf.Connect(ctx)
			defer func() { _ = second.Close() }()
			ng, err := setup(second, cfg)
			var got outcome
			switch {
			case err == nil && ng.Accepted:
				got = accepted
			case err == nil:
				got = rejected
			case errors.Is(err, context.DeadlineExceeded):
				got = unanswered
			default:
				got = closed
			}
			if got != tc.want {
				t.Fatalf("duplicate NG Setup: outcome %d (%+v, %v), want %d", got, ng, err, tc.want)
			}
			if tc.wantCause != "" && ng.Cause != tc.wantCause {
				t.Errorf("cause = %q, want %q", ng.Cause, tc.wantCause)
			}

			// The first association: ended only when the mock replaces it.
			done := make(chan error, 1)
			go func() {
				_, _, _, err := first.ReadMsg(make([]byte, 1024))
				done <- err
			}()
			select {
			case err := <-done:
				if !tc.wantFirstEnded {
					t.Errorf("the first association ended (%v)", err)
				}
			case <-time.After(300 * time.Millisecond):
				if tc.wantFirstEnded {
					t.Error("the first association is still up")
				}
			}

			// Once both have ended the ID is free: a running mock accepts
			// it again as a first presentation.
			_ = first.Close()
			_ = second.Close()
			deadline := time.Now().Add(3 * time.Second)
			for {
				again := amf.Connect(ctx)
				ng, err := setup(again, cfg)
				_ = again.Close()
				if tc.wantDead {
					if err == nil {
						t.Errorf("a crashed mock answered a later NG Setup: %+v", ng)
					}
					break
				}
				if err == nil && ng.Accepted {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the gNB ID was not free after both associations ended: %+v, %v", ng, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
