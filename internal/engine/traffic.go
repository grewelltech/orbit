package engine

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/bgrewell/orbit/internal/loomgtp"
)

// TrafficResult is the throughput of a UE data-plane flow.
type TrafficResult struct {
	Bytes, Packets uint64
	Mbps           float64
	Duration       time.Duration
}

// Traffic runs a loom-generated UDP flow from a registered UE over its N3 data
// path to target ("host:port"), at the given rate (loom rate string, e.g.
// "50Mbps"; "" = unlimited) and packet size for the duration, returning
// throughput. Requires an active PDU session and a gNB N3 address (the data
// path must be reachable from the UPF — run from the RAN node).
func (m *Manager) Traffic(ctx context.Context, supi, target, rate string, packetSize int, duration time.Duration) (*TrafficResult, error) {
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("UE %s is not registered", supi)
	}
	if !sess.Result.SessionActive {
		return nil, fmt.Errorf("UE %s has no active PDU session", supi)
	}
	if sess.gnbN3 == "" {
		return nil, fmt.Errorf("UE %s registered without a gNB N3 address; data path disabled", supi)
	}
	tun, err := sess.tunnel()
	if err != nil {
		return nil, err
	}
	res, err := loomgtp.RunFlow(ctx, loomgtp.Config{
		Uplink:     tun,
		UEIP:       net.ParseIP(sess.Result.PDUAddress),
		Target:     target,
		PacketSize: packetSize,
		Rate:       rate,
		Duration:   duration,
	})
	if err != nil {
		return nil, err
	}
	return &TrafficResult{Bytes: res.Bytes, Packets: res.Packets, Mbps: res.Mbps, Duration: res.Duration}, nil
}

// LatencyStats is loom's RTT/jitter/loss summary over a UE's N3 data path.
type LatencyStats struct {
	Sent, Received, Lost           uint64
	LossPct                        float64
	Min, Max, Mean, StdDev, Jitter time.Duration
}

// Latency probes target ("host", an IPv4) from a registered UE over its N3
// data path with ICMP echoes and returns loom's RTT/jitter/loss summary.
// Requires an active PDU session and a gNB N3 address.
func (m *Manager) Latency(ctx context.Context, supi, target string, probes int, spacing, timeout time.Duration) (*LatencyStats, error) {
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("UE %s is not registered", supi)
	}
	if !sess.Result.SessionActive {
		return nil, fmt.Errorf("UE %s has no active PDU session", supi)
	}
	if sess.gnbN3 == "" {
		return nil, fmt.Errorf("UE %s registered without a gNB N3 address; data path disabled", supi)
	}
	tun, err := sess.tunnel()
	if err != nil {
		return nil, err
	}
	res, err := loomgtp.RunLatency(ctx, loomgtp.LatencyConfig{
		Probe:   tun,
		UEIP:    net.ParseIP(sess.Result.PDUAddress),
		Target:  target,
		Probes:  probes,
		Spacing: spacing,
		Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}
	return &LatencyStats{
		Sent: res.Sent, Received: res.Received, Lost: res.Lost, LossPct: res.LossPct,
		Min: res.Min, Max: res.Max, Mean: res.Mean, StdDev: res.StdDev, Jitter: res.Jitter,
	}, nil
}
