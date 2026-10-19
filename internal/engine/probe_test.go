package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// nasMessageType reads a plain message's type at offset 2 and a protected
// one's inner type at offset 9 when integrity-only or ciphered under NEA0;
// a ciphered message under another algorithm, and anything it cannot read,
// is 0.
func TestNASMessageType(t *testing.T) {
	prot := func(sht byte) []byte { return []byte{0x7e, sht, 1, 2, 3, 4, 0, 0x7e, 0x00, 0x42, 0x01} }
	tests := []struct {
		name string
		raw  []byte
		nea0 bool
		want uint8
	}{
		{name: "plain", raw: []byte{0x7e, 0x00, 0x56, 0x01}, want: f5nas.MsgTypeAuthenticationRequest},
		{name: "integrity only", raw: prot(0x01), want: f5nas.MsgTypeRegistrationAccept},
		{name: "integrity only, new context", raw: prot(0x03), want: f5nas.MsgTypeRegistrationAccept},
		{name: "ciphered under NEA0", raw: prot(0x02), nea0: true, want: f5nas.MsgTypeRegistrationAccept},
		{name: "ciphered under NEA0, new context", raw: prot(0x04), nea0: true, want: f5nas.MsgTypeRegistrationAccept},
		{name: "ciphered under another algorithm", raw: prot(0x02), want: 0},
		{name: "ciphered, new context, another algorithm", raw: prot(0x04), want: 0},
		{name: "too short", raw: []byte{0x7e, 0x00}, want: 0},
		{name: "protected but truncated", raw: []byte{0x7e, 0x01, 1, 2, 3, 4, 0, 0x7e}, want: 0},
		{name: "5GSM, not 5GMM", raw: []byte{0x2e, 0x01, 0x01, 0xc2}, want: 0},
		{name: "unknown security header type", raw: prot(0x0c), nea0: true, want: 0},
		{name: "protected with an unreadable inner message", raw: []byte{0x7e, 0x01, 1, 2, 3, 4, 0, 0x99, 0x00, 0x42}, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nasMessageType(tc.raw, tc.nea0); got != tc.want {
				t.Errorf("nasMessageType = %#x, want %#x", got, tc.want)
			}
		})
	}
}

func TestProcedureName(t *testing.T) {
	for code, want := range map[int64]string{4: "DownlinkNASTransport", 14: "InitialContextSetup", 41: "UEContextRelease", 29: "PDUSessionResourceSetup", 99: "procedure 99"} {
		if got := procedureName(code); got != want {
			t.Errorf("procedureName(%d) = %q, want %q", code, got, want)
		}
	}
}

// A probe withholds at the reaction: the UE stops where the withheld reply
// would have been sent, emitting no later state event, while OnDownlink
// sees every downlink with its NAS type. With T3560 on the mock, the release
// ends the attach with ErrReleasedByCore. A nil probe is the ordinary attach.
func TestAttachProbe(t *testing.T) {
	tests := []struct {
		name       string
		probe      bool
		withhold   uint8
		opts       []mockamf.Option
		wantStates []string
		wantNAS    []uint8 // downlink NAS types the probe saw, in order
		wantErr    error
	}{
		{name: "nil probe registers", wantStates: []string{StateRegistering, StateAuthenticated, StateSecurityEstablished, StateRegistered}},
		{name: "probe withholding nothing registers", probe: true,
			wantStates: []string{StateRegistering, StateAuthenticated, StateSecurityEstablished, StateRegistered},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeSecurityModeCommand, f5nas.MsgTypeRegistrationAccept}},
		{name: "authentication response withheld", probe: true, withhold: f5nas.MsgTypeAuthenticationResponse,
			wantStates: []string{StateRegistering}, wantNAS: []uint8{f5nas.MsgTypeAuthenticationRequest}, wantErr: context.DeadlineExceeded},
		{name: "security mode complete withheld", probe: true, withhold: f5nas.MsgTypeSecurityModeComplete,
			wantStates: []string{StateRegistering, StateAuthenticated},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeSecurityModeCommand}, wantErr: context.DeadlineExceeded},
		{name: "registration complete withheld", probe: true, withhold: f5nas.MsgTypeRegistrationComplete,
			wantStates: []string{StateRegistering, StateAuthenticated, StateSecurityEstablished},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeSecurityModeCommand, f5nas.MsgTypeRegistrationAccept},
			wantErr:    context.DeadlineExceeded},
		{name: "released by the core", probe: true, withhold: f5nas.MsgTypeAuthenticationResponse,
			opts:       []mockamf.Option{mockamf.WithT3560(30*time.Millisecond, 1)},
			wantStates: []string{StateRegistering},
			wantNAS:    []uint8{f5nas.MsgTypeAuthenticationRequest, f5nas.MsgTypeAuthenticationRequest, 0}, wantErr: ErrReleasedByCore},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
			opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			conn := amf.Connect(ctx)
			defer func() { _ = conn.Close() }()
			cfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
			if ng, err := gnb.NGSetup(ctx, conn, cfg); err != nil || !ng.Accepted {
				t.Fatalf("NG Setup: %v", err)
			}
			const supi = "208930100007501"
			id, err := ue.ParseIdentity(supi, "208", "93", "0")
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var states []string
			var nasTypes []uint8
			ucfg := UEConfig{Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc}}
			if tc.probe {
				ucfg.Probe = &AttachProbe{OnDownlink: func(ev DownlinkEvent) {
					mu.Lock()
					nasTypes = append(nasTypes, ev.NASType)
					mu.Unlock()
					if ev.NASType != 0 && len(ev.NAS) == 0 {
						t.Errorf("event %+v has a NAS type but no NAS", ev)
					}
				}}
				if tc.withhold != 0 {
					ucfg.Probe.Withhold = map[uint8]bool{tc.withhold: true}
				}
			}
			_, err = Attach(ctx, conn, cfg, ucfg, slog.New(slog.NewTextHandler(io.Discard, nil)), func(ev StateEvent) {
				mu.Lock()
				states = append(states, ev.State)
				mu.Unlock()
			})
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Errorf("Attach err = %v, want %v", err, tc.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if fmt.Sprint(states) != fmt.Sprint(tc.wantStates) {
				t.Errorf("states %v, want %v", states, tc.wantStates)
			}
			if !slices.Equal(nasTypes, tc.wantNAS) {
				t.Errorf("probe saw NAS types %v, want %v", nasTypes, tc.wantNAS)
			}
		})
	}
}
