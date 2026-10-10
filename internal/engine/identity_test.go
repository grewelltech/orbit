package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
)

func TestInheritIdentity(t *testing.T) {
	serving := gnb.Config{ID: 1, Name: "src", MCC: "001", MNC: "01", TAC: 5, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	tests := []struct {
		name    string
		target  gnb.Config
		want    gnb.Config
		wantErr string
	}{
		{name: "nothing set: all inherited", target: gnb.Config{ID: 2, Name: "tgt"},
			want: gnb.Config{ID: 2, Name: "tgt", MCC: "001", MNC: "01", TAC: 5, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}},
		{name: "explicit plmn kept", target: gnb.Config{ID: 2, MCC: "208", MNC: "93"},
			want: gnb.Config{ID: 2, MCC: "208", MNC: "93", TAC: 5, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}},
		{name: "explicit tac kept", target: gnb.Config{ID: 2, TAC: 9},
			want: gnb.Config{ID: 2, MCC: "001", MNC: "01", TAC: 9, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}},
		{name: "explicit slice kept", target: gnb.Config{ID: 2, Slices: []gnb.SNSSAI{{SST: 2}}},
			want: gnb.Config{ID: 2, MCC: "001", MNC: "01", TAC: 5, Slices: []gnb.SNSSAI{{SST: 2}}}},
		{name: "half a plmn is not inherited and fails", target: gnb.Config{ID: 2, MCC: "208"}, wantErr: "mnc"},
		{name: "invalid explicit mnc fails", target: gnb.Config{ID: 2, MCC: "001", MNC: "1"}, wantErr: "mnc"},
		{name: "invalid explicit sd fails", target: gnb.Config{ID: 2, Slices: []gnb.SNSSAI{{SST: 1, SD: "xyz"}}}, wantErr: "sd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := inheritIdentity(tc.target, serving)
			if tc.wantErr != "" {
				if err == nil || !errors.Is(err, ErrInvalidTarget) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want ErrInvalidTarget naming %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("inheritIdentity: %v", err)
			}
			if got.MCC != tc.want.MCC || got.MNC != tc.want.MNC || got.TAC != tc.want.TAC || len(got.Slices) != len(tc.want.Slices) || got.Slices[0] != tc.want.Slices[0] {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			// The inherited slice list is a copy, not the serving config's.
			if len(tc.target.Slices) == 0 {
				got.Slices[0].SST = 99
				if serving.Slices[0].SST == 99 {
					t.Error("inherited slices alias the serving gNB's slice")
				}
			}
		})
	}
}

// The inherit step runs inside Handover / XnHandover on the UE's serving gNB:
// a target with an invalid identity is refused with ErrInvalidTarget before
// anything is dialled, and a target with no identity picks up the serving
// gNB's (so the failure that follows is the dial, not a missing PLMN).
func TestHandoverInheritsFromServingGNB(t *testing.T) {
	amf, cfg, ki, opc := newBindTestAMF(t)
	d := &fakeN2Dialer{amf: amf}
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.dial = d.dial
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const supi = "208930100000003"
	if _, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, bindTestUE(t, supi, ki, opc)); err != nil {
		t.Fatal(err)
	}

	for _, ho := range []struct {
		name string
		call func(ctx context.Context, supi string, t GNBEndpoint) (time.Duration, error)
	}{{"n2", m.Handover}, {"xn", m.XnHandover}} {
		t.Run(ho.name+" invalid target", func(t *testing.T) {
			_, err := ho.call(ctx, supi, GNBEndpoint{Config: gnb.Config{ID: 2, Name: "tgt", MCC: "001", MNC: "1"}, AMFAddr: "127.0.0.1:1"})
			if !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("err = %v, want ErrInvalidTarget", err)
			}
		})
		t.Run(ho.name+" inherits", func(t *testing.T) {
			// Empty identity: inherited from the serving gNB, so validation
			// passes and the procedure reaches the dial, which fails on the
			// unroutable AMF (a bad bind makes it fail fast and deterministic).
			_, err := ho.call(ctx, supi, GNBEndpoint{Config: gnb.Config{ID: 2, Name: "tgt"}, AMFAddr: "127.0.0.1:1", BindAddr: "10.0.0.1:0:0"})
			if err == nil {
				t.Fatal("handover to an unroutable AMF succeeded")
			}
			if errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("inherited identity rejected: %v", err)
			}
			if !strings.Contains(err.Error(), "invalid local address") {
				t.Errorf("err = %v, want the dial failure (identity accepted)", err)
			}
		})
	}
}
