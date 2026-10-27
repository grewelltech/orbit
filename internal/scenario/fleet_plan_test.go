package scenario

import (
	"errors"
	"testing"
)

// A bit rate is typed only when the plan writes it back as the same text.
func TestTypedBitRate(t *testing.T) {
	tests := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"", 0, true},
		{"8Mbps", 8_000_000, true},
		{"1Gbps", 1_000_000_000, true},
		{"500Kbps", 500_000, true},
		{"1500bps", 1500, true},
		{"1000Kbps", 0, false},
		{"8 Mbps", 0, false},
		{"0Mbps", 0, false},
		{"1.5Mbps", 0, false},
		{"fast", 0, false},
	}
	for _, tc := range tests {
		got, err := typedBitRate(tc.in)
		if tc.ok != (err == nil) || got != tc.want || err != nil && !errors.Is(err, ErrNotTyped) {
			t.Errorf("typedBitRate(%q) = %d, %v; want %d, ok %t", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// Behaviours a plan cannot carry exactly are ErrNotTyped; the rest type.
func TestBehaviorsProto(t *testing.T) {
	tests := []struct {
		name string
		b    Behaviors
		ok   bool
	}{
		{"none", Behaviors{}, true},
		{"mobility", Behaviors{Mobility: &MobilityBehavior{Model: "random_walk", Speed: "3m/s", Handover: "xn"}}, true},
		{"a speed written another way", Behaviors{Mobility: &MobilityBehavior{Speed: "3.0m/s"}}, false},
		{"an unknown handover", Behaviors{Mobility: &MobilityBehavior{Handover: "XN"}}, false},
		{"a cohort", Behaviors{Traffic: &TrafficBehavior{Mix: []TrafficShare{{App: "http", Share: 1, StartAfter: "6m"}}}}, true},
		{"a share both profile and app", Behaviors{Traffic: &TrafficBehavior{Mix: []TrafficShare{{App: "http", Profile: "web"}}}}, false},
		{"a profile with app fields", Behaviors{Traffic: &TrafficBehavior{Mix: []TrafficShare{{Profile: "web", Peer: "x:1"}}}}, false},
		{"a sub-millisecond start", Behaviors{Traffic: &TrafficBehavior{Mix: []TrafficShare{{App: "http", StartAfter: "10us"}}}}, false},
		{"latency", Behaviors{Latency: &LatencyBehavior{Target: "192.0.2.1", Interval: "2s", Timeout: "1s", UEs: 4}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.b.Proto()
			if tc.ok != (err == nil) || err != nil && !errors.Is(err, ErrNotTyped) {
				t.Errorf("Proto() err = %v, want ok %t", err, tc.ok)
			}
		})
	}
}
