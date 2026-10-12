package engine

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// The #114 mechanism, pinned: a Subscription built from the NAI spelling
// derives KAMF from "imsi-..." and fails the NAS integrity check at the
// Security Mode Command, while the same SUPI normalised to digits registers.
// The server normalises every SUPI before building a Subscription, so the
// first row is the state this change removes, not a path that still exists.
func TestRegisterSubscriptionSUPIFormDecidesIntegrity(t *testing.T) {
	const nai = "imsi-208930100000001"
	tests := []struct {
		name     string
		subSUPI  string
		wantErr  string
		wantSUPI string
	}{
		{name: "raw NAI subscription fails integrity", subSUPI: nai, wantErr: "integrity"},
		{name: "normalised subscription registers", subSUPI: "208930100000001", wantSUPI: "208930100000001"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, cfg, ki, opc := newBindTestAMF(t)
			d := &fakeN2Dialer{amf: amf}
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.dial = d.dial
			id, err := ue.ParseIdentity(nai, "208", "93", "0") // either spelling parses
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"},
				UEConfig{Identity: id, Sub: auth.Subscription{SUPI: tc.subSUPI, Ki: ki, OPc: opc}})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.wantErr) {
					t.Fatalf("err = %v, want an integrity failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
			if !res.Registered || res.SUPI != tc.wantSUPI {
				t.Errorf("result = registered %v SUPI %q, want registered with %q", res.Registered, res.SUPI, tc.wantSUPI)
			}
		})
	}
}

// incIMSI accepts the base IMSI in either spelling and generates digits-only
// SUPIs; a malformed base is the shared normalisation error.
func TestIncIMSIAcceptsBothForms(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		i       int
		want    string
		wantErr string
	}{
		{name: "bare", base: "208930100000000", i: 7, want: "208930100000007"},
		{name: "nai", base: "imsi-208930100000000", i: 7, want: "208930100000007"},
		{name: "short base pads to 15", base: "imsi-20893", i: 1, want: "000000000020894"},
		{name: "malformed", base: "IMSI-208930100000000", wantErr: "want 5-15 digits or imsi-<digits>"},
		{name: "empty", base: "", wantErr: "want 5-15 digits or imsi-<digits>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := incIMSI(tc.base, tc.i)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("incIMSI(%q, %d): %v", tc.base, tc.i, err)
			}
			if got != tc.want {
				t.Errorf("incIMSI(%q, %d) = %q, want %q", tc.base, tc.i, got, tc.want)
			}
		})
	}
}
