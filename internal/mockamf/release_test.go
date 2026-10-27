package mockamf_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// SkipNoCommonIntegrity answers a UE offering only NIA0 with a plain
// Registration Accept (7e 00 42) and no Security Mode Command, and the
// engine's cleanup deregisters the UE in the clear, then asks for a release
// if the core sends none: the mock releases on the request by default, on
// the deregistration with WithReleaseOnDeregistration, and never with
// WithIgnoreUEContextReleaseRequest. The cleanup runs to its own bound
// even when the attach's context ends as the Accept arrives. With 128-NIA2
// in common the option changes nothing.
func TestSkipNoCommonIntegrity(t *testing.T) {
	skip := mockamf.WithNoCommonIntegrity(mockamf.SkipNoCommonIntegrity)
	nia0Only, err := ue.NewSecurityCapability([]uint8{nas.NEA0, nas.NEA1, nas.NEA2}, []uint8{nas.NIA0})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		opts      []mockamf.Option
		normal    bool // the default UE, which offers 128-NIA2
		cancel    bool // the attach's context ends as the plain Accept arrives
		wantSteps []string
	}{
		{name: "released on request", opts: []mockamf.Option{skip, mockamf.WithGUTI()},
			wantSteps: []string{"plain switch-off Deregistration Request sent", "no UE Context Release Command within 100ms", "UE Context Release Request sent", "the core released the UE context (UE Context Release Complete sent)"}},
		{name: "no GUTI: deregistered by SUCI", opts: []mockamf.Option{skip},
			wantSteps: []string{"plain switch-off Deregistration Request sent", "UE Context Release Request sent", "the core released the UE context"}},
		{name: "released on deregistration", opts: []mockamf.Option{skip, mockamf.WithGUTI(), mockamf.WithReleaseOnDeregistration()},
			wantSteps: []string{"plain switch-off Deregistration Request sent", "the core released the UE context (UE Context Release Complete sent)"}},
		{name: "never released", opts: []mockamf.Option{skip, mockamf.WithIgnoreUEContextReleaseRequest()},
			wantSteps: []string{"UE Context Release Request sent", "no UE Context Release Command within 100ms; the association is closed with the UE context still held"}},
		{name: "context ended at the Accept", opts: []mockamf.Option{skip, mockamf.WithGUTI()}, cancel: true,
			wantSteps: []string{"plain switch-off Deregistration Request sent", "no UE Context Release Command within 100ms", "UE Context Release Request sent", "the core released the UE context (UE Context Release Complete sent)"}},
		{name: "NIA2 in common", opts: []mockamf.Option{skip}, normal: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf := testAMFWith(t, tc.opts...)
			gnbCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
			ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
			opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }()
			if ng, err := gnb.NGSetup(ctx, conn, gnbCfg); err != nil || !ng.Accepted {
				t.Fatalf("NG Setup: %v (accepted=%v)", err, ng)
			}
			supi := "208930100007500"
			id, err := ue.ParseIdentity(supi, "208", "93", "0")
			if err != nil {
				t.Fatal(err)
			}
			rec := &recorder{}
			actx, acancel := context.WithCancel(ctx)
			defer acancel()
			onDownlink := rec.add
			if tc.cancel {
				onDownlink = func(ev engine.DownlinkEvent) {
					rec.add(ev)
					if ev.NASType == f5nas.MsgTypeRegistrationAccept {
						acancel()
					}
				}
			}
			cfg := engine.UEConfig{
				Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
				Probe: &engine.AttachProbe{OnDownlink: onDownlink, CleanUpPlainAccept: 100 * time.Millisecond},
			}
			if !tc.normal {
				cfg.SecurityCapabilities = &nia0Only
			}
			_, err = engine.Attach(actx, conn, gnbCfg, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if tc.normal {
				if err != nil {
					t.Fatalf("attach with 128-NIA2 in common: %v", err)
				}
				if n, _ := rec.count(f5nas.MsgTypeSecurityModeCommand); n != 1 {
					t.Errorf("%d Security Mode Commands, want 1", n)
				}
				return
			}
			var pa *engine.PlainAcceptError
			if !errors.As(err, &pa) {
				t.Fatalf("attach = %v, want a *PlainAcceptError", err)
			}
			got := strings.Join(pa.Steps, "; ")
			for _, w := range tc.wantSteps {
				if !strings.Contains(got, w) {
					t.Errorf("steps %q lack %q", got, w)
				}
			}
			if n, _ := rec.count(f5nas.MsgTypeSecurityModeCommand); n != 0 {
				t.Errorf("%d Security Mode Commands, want none", n)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			var accept []byte
			for _, ev := range rec.evs {
				if ev.NASType == f5nas.MsgTypeRegistrationAccept {
					accept = ev.NAS
				}
			}
			if !bytes.HasPrefix(accept, []byte{0x7e, 0x00, 0x42}) {
				t.Errorf("Registration Accept = %x, want a plain one starting 7e0042", accept)
			}
		})
	}
}
