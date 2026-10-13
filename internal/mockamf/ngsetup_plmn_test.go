package mockamf_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// The mock rejects an NG Setup whose Supported TA List names none of its
// PLMNs with NG SETUP FAILURE, cause unknown PLMN (TS 38.413 8.7.1.4),
// accepts its own PLMN, and with AcceptUnknownPLMN accepts anyway.
func TestMockNGSetupUnknownPLMN(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	gnbIn := func(mcc, mnc string) gnb.Config {
		return gnb.Config{ID: 1, Name: "g", MCC: mcc, MNC: mnc, TAC: 7, Slices: []gnb.SNSSAI{{SST: 1}}}
	}
	tests := []struct {
		name       string
		accept     bool
		cfg        gnb.Config
		wantAccept bool
	}{
		{name: "served PLMN", cfg: gnbIn("208", "93"), wantAccept: true},
		{name: "unserved PLMN", cfg: gnbIn("999", "99")},
		{name: "unserved PLMN, accepting anyway", accept: true, cfg: gnbIn("999", "99"), wantAccept: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, AcceptUnknownPLMN: tc.accept})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }()
			ng, err := gnb.NGSetup(ctx, conn, tc.cfg)
			if err != nil {
				t.Fatalf("NGSetup: %v", err)
			}
			if ng.Accepted != tc.wantAccept {
				t.Fatalf("accepted = %v, want %v (cause %q)", ng.Accepted, tc.wantAccept, ng.Cause)
			}
			if !tc.wantAccept && !strings.Contains(strings.ToLower(ng.Cause), "misc") {
				t.Errorf("cause = %q, want a Misc cause", ng.Cause)
			}
		})
	}
}
