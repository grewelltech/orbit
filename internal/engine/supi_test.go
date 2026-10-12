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

// LoadSpec.Validate normalises the base IMSI once and stores it digits-only;
// a malformed base is the shared message with the "base IMSI" prefix.
func TestLoadSpecValidateBaseIMSI(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		want    string
		wantErr string
	}{
		{name: "bare", base: "208930100000000", want: "208930100000000"},
		{name: "nai stored as digits", base: "imsi-208930100000000", want: "208930100000000"},
		{name: "too short", base: "2089", wantErr: `base IMSI: invalid SUPI "2089": want 5-15 digits or imsi-<digits>`},
		{name: "upper-case prefix", base: "IMSI-208930100000000", wantErr: `base IMSI: invalid SUPI "IMSI-208930100000000"`},
		{name: "empty", base: "", wantErr: `base IMSI: invalid SUPI ""`},
		{name: "other PLMN", base: "001010000000001", wantErr: `base IMSI 001010000000001 (UE 1 of 2): supi "001010000000001" does not start with mcc+mnc "20893"`},
		{name: "short base padding breaks the prefix", base: "imsi-20893", wantErr: `base IMSI 20893 (UE 1 of 2): supi "000000000020893" does not start with mcc+mnc "20893"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := LoadSpec{BaseIMSI: tc.base, Count: 2, MCC: "208", MNC: "93"}
			err := spec.Validate()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if spec.BaseIMSI != tc.want {
				t.Errorf("BaseIMSI after Validate = %q, want %q", spec.BaseIMSI, tc.want)
			}
		})
	}
}

// The last generated SUPI is checked as well as the first: a range that
// walks out of the PLMN prefix, or runs past 15 digits, is refused naming
// that UE.
func TestLoadSpecValidateRangeEnd(t *testing.T) {
	tests := []struct {
		name, base, mcc, mnc, want string
	}{
		{name: "walks out of the PLMN", base: "208939999999999", mcc: "208", mnc: "93", want: `(UE 2 of 2): supi "208940000000000" does not start with mcc+mnc "20893"`},
		{name: "runs past 15 digits", base: "999999999999999", mcc: "999", mnc: "99", want: `(UE 2 of 2): invalid SUPI "1000000000000000": want 5-15 digits`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := LoadSpec{BaseIMSI: tc.base, Count: 2, MCC: tc.mcc, MNC: tc.mnc}
			err := spec.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// RunLoad refuses a malformed base IMSI before any association is opened:
// the error is the validation message, not a dial error, and it returns at
// once (the gNB points at a refused loopback port, so a dial attempt would
// have produced "sctp dial ..." instead).
func TestRunLoadRejectsMalformedBaseBeforeDialing(t *testing.T) {
	tests := []struct {
		name, base string
		count      int
		want       string
	}{
		{name: "malformed", base: "2089", count: 1, want: `base IMSI: invalid SUPI "2089"`},
		{name: "other PLMN", base: "001010000000001", count: 2, want: `does not start with mcc+mnc "20893"`},
		{name: "padded short base", base: "imsi-20893", count: 2, want: `does not start with mcc+mnc "20893"`},
		{name: "range walks out of the PLMN", base: "208939999999999", count: 2, want: "(UE 2 of 2)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg, ki, opc := newBindTestAMF(t)
			spec := LoadSpec{
				GNBs:     []GNBSpec{{Config: cfg, AMFAddr: "127.0.0.1:1"}},
				BaseIMSI: tc.base, Count: tc.count, MCC: "208", MNC: "93", Ki: ki, OPc: opc,
			}
			start := time.Now()
			_, err := RunLoad(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), spec)
			if err == nil {
				t.Fatal("RunLoad with a bad base IMSI succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "sctp dial") {
				t.Errorf("a dial was attempted before validation: %v", err)
			}
			if took := time.Since(start); took > time.Second {
				t.Errorf("RunLoad took %s to refuse a bad base", took)
			}
		})
	}
}

// A valid base under the configured PLMN still attaches: the range check
// does not refuse what the old per-UE path accepted.
func TestRunLoadValidBaseStillAttaches(t *testing.T) {
	amf, cfg, ki, opc := newBindTestAMF(t)
	addr := sctpFront(t, amf)
	spec := LoadSpec{
		GNBs:     []GNBSpec{{Config: cfg, AMFAddr: addr}},
		BaseIMSI: "imsi-208930100000001", Count: 2, Concurrency: 2, MCC: "208", MNC: "93", Ki: ki, OPc: opc,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rep, err := RunLoad(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), spec)
	if err != nil {
		t.Fatalf("RunLoad: %v", err)
	}
	if rep.Succeeded != 2 {
		t.Errorf("attached %d/%d, want 2/2", rep.Succeeded, rep.Attempted)
	}
}
