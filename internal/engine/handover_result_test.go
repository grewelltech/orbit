package engine

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockupf"
)

// A completed handover reports the identity it presented: a target with no
// identity comes back with the serving gNB's PLMN, TAC and slices and its
// own ID and name; a field set explicitly comes back as given (#108). Xn,
// over the mock pair; N2 shares inheritIdentity and the result's shape.
func TestHandoverResultReportsPresentedTarget(t *testing.T) {
	tests := []struct {
		name   string
		net24  int
		target gnb.Config
		want   func(serving gnb.Config) gnb.Config
	}{
		{name: "inherit", net24: 71, target: gnb.Config{ID: 2, Name: "orbit-gnb-2"},
			want: func(s gnb.Config) gnb.Config {
				return gnb.Config{ID: 2, Name: "orbit-gnb-2", MCC: s.MCC, MNC: s.MNC, TAC: s.TAC, Slices: s.Slices}
			}},
		{name: "explicit TAC", net24: 72, target: gnb.Config{ID: 2, Name: "orbit-gnb-2", TAC: 7},
			want: func(s gnb.Config) gnb.Config {
				return gnb.Config{ID: 2, Name: "orbit-gnb-2", MCC: s.MCC, MNC: s.MNC, TAC: 7, Slices: s.Slices}
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockPair(t, tc.net24, mockupf.Options{}, false)
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			const supi = "208930100007500"
			p.register(t, m, supi)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := m.XnHandover(ctx, supi, GNBEndpoint{Config: tc.target, AMFAddr: p.amfAddr, N3Addr: p.tgtN3})
			if err != nil {
				t.Fatalf("XnHandover: %v", err)
			}
			want := tc.want(p.cfg)
			got := res.Target
			if got.ID != want.ID || got.Name != want.Name || got.MCC != want.MCC || got.MNC != want.MNC || got.TAC != want.TAC ||
				len(got.Slices) != len(want.Slices) || got.Slices[0] != want.Slices[0] {
				t.Errorf("Target = %+v, want %+v", got, want)
			}
			if res.Elapsed <= 0 {
				t.Errorf("Elapsed = %s, want > 0", res.Elapsed)
			}
		})
	}
}
