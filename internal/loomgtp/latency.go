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

// ICMPRx is the downlink side of the probe: an ICMP subscription on the
// session's N3 demux (datapath.UERx.SubscribeICMP). *datapath.Ring satisfies
// it. The probe never reads the tunnel socket directly — the Demux's reader
// goroutine is the socket's only reader (design §6), so the probe and media
// lanes share one downlink.
type ICMPRx interface {
	Read(timeout time.Duration) (datapath.Frame, error)
}

// ErrSessionGone marks an uplink send error that means the UE's session is
// gone (deregistered, or its data path torn down): no later probe can leave,
// so RunLatency stops rather than spending the remaining probes. An Uplink
// marks such an error with SessionGone; any other send error is transient
// and the run goes on.
var ErrSessionGone = errors.New("UE session is gone")

// SessionGone marks err as ErrSessionGone, keeping its text and its chain
// (errors.Is(SessionGone(net.ErrClosed), net.ErrClosed) still holds). A nil
// err stays nil.
func SessionGone(err error) error {
	if err == nil {
		return nil
	}
	return goneError{err}
}

type goneError struct{ err error }

func (e goneError) Error() string   { return e.err.Error() }
func (e goneError) Unwrap() []error { return []error{ErrSessionGone, e.err} }

// sendError is a probe that never left: building or sending its uplink
// failed. It keeps a send failure apart from a read error after a successful
// send, which is loss.
type sendError struct{ err error }

func (e sendError) Error() string { return "send: " + e.err.Error() }
func (e sendError) Unwrap() error { return e.err }

// tunnelPinger implements loom's latency.Pinger by sending an ICMP echo up the
// GTP-U tunnel and timing the matching reply on its demuxed ICMP lane. All
// the statistics (jitter/loss/percentiles) are loom's — this only supplies
// the tunnel transport for the probe.
//
// It also records every probe's outcome, because loom's Sampler drops a
// batch cut short by cancellation and the run is cancelled when the session
// goes: the probes that left are the ones summarised, those that did not are
// counted apart. Ping is called from the sampler goroutine only, and the
// record is read after it exits.
type tunnelPinger struct {
	up        Uplink
	rx        ICMPRx
	ueIP, dst net.IP
	id        uint16

	stop    context.CancelFunc // ends the run when the session is gone
	sent    []latency.Result   // probes whose uplink send succeeded
	sendErr error              // the first send error, if any
	stopped error              // the send error that ended the run, if any
}

func (p *tunnelPinger) Ping(ctx context.Context, seq uint64) (time.Duration, error) {
	rtt, err := p.ping(ctx, seq)
	var se sendError
	switch {
	case errors.As(err, &se):
		if p.sendErr == nil {
			p.sendErr = se.err
		}
		if errors.Is(se.err, ErrSessionGone) && p.stopped == nil {
			p.stopped = se.err
			p.stop()
		}
	case err == nil:
		p.sent = append(p.sent, latency.Result{Seq: seq, State: latency.StateOK, RTT: rtt})
	default:
		p.sent = append(p.sent, latency.Result{Seq: seq, State: latency.StateTimeout, Err: err})
	}
	return rtt, err
}

// readSlice bounds each wait for a reply, so a cancel ends a probe within
// about this long rather than at its timeout.
const readSlice = 50 * time.Millisecond

func (p *tunnelPinger) ping(ctx context.Context, seq uint64) (time.Duration, error) {
	s := uint16(seq)
	req, err := datapath.BuildICMPEchoRequest(p.ueIP, p.dst, p.id, s, []byte("orbit-lat"))
	if err != nil {
		return 0, sendError{err}
	}
	start := time.Now()
	if err := p.up.SendUplink(req); err != nil {
		return 0, sendError{err}
	}
	deadline := time.Now().Add(time.Second)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	for {
		// The caller's cancel cuts the wait (#169): the read runs in slices
		// of at most readSlice, and ctx is checked between them.
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, context.DeadlineExceeded
		}
		f, err := p.rx.Read(min(remaining, readSlice))
		if errors.Is(err, context.DeadlineExceeded) {
			continue // this slice ran out; the loop checks ctx and the deadline
		}
		if err != nil {
			return 0, err // closed after a successful send → loss
		}
		if _, ok := datapath.MatchICMPEchoReply(f.Payload, p.id, s); ok {
			// Time the reply at the demux reader's socket-read stamp, not at
			// ring dequeue: behind a shared demux the probe's frames can wait
			// in the ring (and this goroutine can wake late) while media
			// flows — exactly when inflating the RTT would matter most.
			if !f.Arrival.IsZero() {
				return f.Arrival.Sub(start), nil
			}
			return time.Since(start), nil
		}
		// ICMP traffic that isn't our reply — keep reading until deadline.
	}
}

// LatencyConfig parameterises an RTT/jitter/loss probe over the tunnel.
type LatencyConfig struct {
	Uplink  Uplink // GTP-U uplink (tunnel or UE flow)
	RX      ICMPRx // demuxed downlink ICMP lane (UERx.SubscribeICMP)
	UEIP    net.IP
	Target  string        // destination IPv4 (ICMP; no port)
	Probes  int           // number of echoes (default 20)
	Spacing time.Duration // pause after each reply or timeout (default 50ms)
	Timeout time.Duration // per echo (default 1s)
}

// LatencyResult is loom's latency summary measured over the GTP-U tunnel.
// Sent counts the probes whose uplink send succeeded; Lost those of them
// with no reply in time, and LossPct is Lost over Sent. NotSent counts the
// probes that never left — a failed send, or the remainder of a run that
// stopped — with NotSentErr the first send error. Stopped is why the run
// ended before its probes were spent ("" when it ran them all). Elapsed is
// the run's wall time and Rate the probes sent per second over it.
type LatencyResult struct {
	Sent, Received, Lost, NotSent  uint64
	LossPct                        float64
	Min, Max, Mean, StdDev, Jitter time.Duration
	NotSentErr, Stopped            string
	Elapsed                        time.Duration
	Rate                           float64
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
	if cfg.Uplink == nil || cfg.RX == nil {
		return LatencyResult{}, errors.New("Uplink and RX (demux ICMP lane) are required")
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

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &tunnelPinger{up: cfg.Uplink, rx: cfg.RX, ueIP: cfg.UEIP, dst: dst, id: 0xB1A5, stop: cancel}
	sampler := &latency.Sampler{
		Pinger:   p,
		Probes:   probes,
		Spacing:  spacing,
		Timeout:  timeout,
		Interval: time.Hour, // one batch; cancelled after it emits
	}
	start := time.Now()
	done := make(chan struct{})
	go func() {
		sampler.Run(rctx, func([]latency.Result) { cancel() })
		close(done)
	}()
	<-done
	return summarize(p, probes, time.Since(start), ctx.Err()), nil
}

// summarize turns the pinger's record into the result: loom's statistics
// over the probes that left, the rest counted as not sent, and why the run
// stopped early — the session going, or the caller's cancellation (ctxErr).
func summarize(p *tunnelPinger, probes int, elapsed time.Duration, ctxErr error) LatencyResult {
	s := latency.Summarize(p.sent)
	res := LatencyResult{
		Sent: s.Sent, Received: s.Received, Lost: s.Lost, LossPct: s.LossPct,
		Min: s.Min, Max: s.Max, Mean: s.Mean, StdDev: s.StdDev, Jitter: s.Jitter,
		Elapsed: elapsed,
	}
	if n := uint64(probes); n > s.Sent {
		res.NotSent = n - s.Sent
	}
	if p.sendErr != nil {
		res.NotSentErr = p.sendErr.Error()
	}
	switch {
	case p.stopped != nil:
		res.Stopped = p.stopped.Error()
	case ctxErr != nil && res.NotSent > 0:
		res.Stopped = ctxErr.Error()
	}
	if elapsed > 0 {
		res.Rate = float64(s.Sent) / elapsed.Seconds()
	}
	return res
}
