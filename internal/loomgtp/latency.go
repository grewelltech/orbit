package loomgtp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/bgrewell/loom/core/latency"

	"github.com/bgrewell/orbit/internal/datapath"
)

// Probe is the GTP-U send+receive the latency bridge needs; *datapath.Tunnel
// satisfies it.
type Probe interface {
	SendUplink(innerIP []byte) error
	ReadDownlink(timeout time.Duration) ([]byte, error)
}

// tunnelPinger implements loom's latency.Pinger by sending an ICMP echo up the
// GTP-U tunnel and timing the matching reply on the downlink. All the
// statistics (jitter/loss/percentiles) are loom's — this only supplies the
// tunnel transport for the probe.
type tunnelPinger struct {
	probe     Probe
	ueIP, dst net.IP
	id        uint16
}

func (p *tunnelPinger) Ping(ctx context.Context, seq uint64) (time.Duration, error) {
	s := uint16(seq)
	req, err := datapath.BuildICMPEchoRequest(p.ueIP, p.dst, p.id, s, []byte("orbit-lat"))
	if err != nil {
		return 0, err
	}
	start := time.Now()
	if err := p.probe.SendUplink(req); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(time.Second)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, context.DeadlineExceeded
		}
		inner, err := p.probe.ReadDownlink(remaining)
		if err != nil {
			return 0, err // timeout/read error → loom classifies as loss
		}
		if _, ok := datapath.MatchICMPEchoReply(inner, p.id, s); ok {
			return time.Since(start), nil
		}
		// downlink traffic that isn't our reply — keep reading until deadline.
	}
}

// LatencyConfig parameterises an RTT/jitter/loss probe over the tunnel.
type LatencyConfig struct {
	Probe   Probe
	UEIP    net.IP
	Target  string        // destination IPv4 (ICMP; no port)
	Probes  int           // number of echoes (default 20)
	Spacing time.Duration // between echoes (default 50ms)
	Timeout time.Duration // per echo (default 1s)
}

// LatencyResult is loom's latency summary measured over the GTP-U tunnel.
type LatencyResult struct {
	Sent, Received, Lost           uint64
	LossPct                        float64
	Min, Max, Mean, StdDev, Jitter time.Duration
}

// RunLatency probes the target over the tunnel and returns loom's RTT/jitter/
// loss summary. ORBIT supplies only the tunnel transport (an ICMP-over-N3
// pinger); loom's Sampler drives the probes and Summarize computes the stats.
func RunLatency(ctx context.Context, cfg LatencyConfig) (LatencyResult, error) {
	dst := net.ParseIP(cfg.Target)
	if dst == nil || dst.To4() == nil {
		return LatencyResult{}, fmt.Errorf("latency target must be an IPv4 address, got %q", cfg.Target)
	}
	if cfg.UEIP == nil {
		return LatencyResult{}, errors.New("UEIP is required")
	}
	probes := cfg.Probes
	if probes <= 0 {
		probes = 20
	}
	spacing := cfg.Spacing
	if spacing <= 0 {
		spacing = 50 * time.Millisecond
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = time.Second
	}

	sampler := &latency.Sampler{
		Pinger:   &tunnelPinger{probe: cfg.Probe, ueIP: cfg.UEIP, dst: dst, id: 0xB1A5},
		Probes:   probes,
		Spacing:  spacing,
		Timeout:  timeout,
		Interval: time.Hour, // one batch; cancelled after it emits
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var all []latency.Result
	done := make(chan struct{})
	go func() {
		sampler.Run(rctx, func(b []latency.Result) {
			all = append(all, b...)
			cancel()
		})
		close(done)
	}()
	<-done

	s := latency.Summarize(all)
	return LatencyResult{
		Sent: s.Sent, Received: s.Received, Lost: s.Lost, LossPct: s.LossPct,
		Min: s.Min, Max: s.Max, Mean: s.Mean, StdDev: s.StdDev, Jitter: s.Jitter,
	}, nil
}
