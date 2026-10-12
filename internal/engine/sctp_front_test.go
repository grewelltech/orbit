package engine

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
)

// sctpFront puts the pipe-only mock AMF behind a real loopback SCTP
// listener (sctpfront.Listen), so the engine's genuine dial path is
// exercised against it. The test is skipped when the host has no kernel
// SCTP.
func sctpFront(t *testing.T, amf *mockamf.AMF) string {
	t.Helper()
	return sctpfront.Listen(t, amf)
}

// With no bind addresses, every gNB in a fleet dials from the route-selected
// source: one address per gNB, the same IP for all (they route to the same
// AMF), and a distinct ephemeral port each, as the kernel bound them.
func TestNewFleetUnboundOneSourcePerGNB(t *testing.T) {
	amf, cfg, _, _ := newBindTestAMF(t)
	addr := sctpFront(t, amf)
	cfg2 := cfg
	cfg2.ID, cfg2.Name = 2, "orbit-gnb-2"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f, err := NewFleet(ctx, []GNBSpec{{Config: cfg, AMFAddr: addr}, {Config: cfg2, AMFAddr: addr}}, log)
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	defer f.Close()

	srcs := f.N2Sources()
	if len(srcs) != 2 {
		t.Fatalf("N2Sources = %v, want one per gNB", srcs)
	}
	hosts := map[string]int{}
	ports := map[string]int{}
	for _, s := range srcs {
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			t.Fatalf("N2 source %q is not a single host:port: %v", s, err)
		}
		if host != "127.0.0.1" {
			t.Errorf("N2 source %q is not the route-selected loopback address", s)
		}
		if port == "0" {
			t.Errorf("N2 source %q has no real port", s)
		}
		hosts[host]++
		ports[port]++
	}
	if len(hosts) != 1 {
		t.Errorf("sources use %d different IPs, want 1: %v", len(hosts), srcs)
	}
	if len(ports) != 2 {
		t.Errorf("sources share a port, want distinct ephemeral ports: %v", srcs)
	}
}
