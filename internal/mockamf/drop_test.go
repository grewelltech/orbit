package mockamf_test

import (
	"context"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
)

// DropAssociations ends every association and keeps serving new ones;
// Crash ends them and answers no later NG Setup.
func TestDropAssociationsAndCrash(t *testing.T) {
	cfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	tests := []struct {
		name      string
		crash     bool
		wantSetup bool
	}{
		{name: "drop", wantSetup: true},
		{name: "crash", crash: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf := testAMF(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }()
			if ng, err := gnb.NGSetup(ctx, conn, cfg); err != nil || !ng.Accepted {
				t.Fatalf("NG Setup: %v", err)
			}
			if tc.crash {
				amf.Crash()
			} else {
				amf.DropAssociations()
			}
			buf := make([]byte, 1024)
			if _, _, _, err := conn.ReadMsg(buf); err == nil {
				t.Fatal("the association is still open")
			}
			fresh := amf.Connect(ctx)
			defer func() { _ = fresh.Close() }()
			sctx, scancel := context.WithTimeout(ctx, 300*time.Millisecond)
			defer scancel()
			ng, err := gnb.NGSetup(sctx, fresh, cfg)
			if got := err == nil && ng.Accepted; got != tc.wantSetup {
				t.Errorf("fresh NG Setup accepted %v (%v), want %v", got, err, tc.wantSetup)
			}
		})
	}
}
