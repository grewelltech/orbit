package loomgtp

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
)

// countingUplink stands in for the GTP-U tunnel: it validates that each inner
// packet is a well-formed UDP datagram sourced from the UE IP to the target,
// and tallies payload bytes/packets.
type countingUplink struct {
	packets, bytes atomic.Int64
	ueIP           net.IP
	dstPort        uint16
	t              *testing.T
}

func (u *countingUplink) SendUplink(inner []byte) error {
	payload, from, ok := datapath.ExtractUDPPayload(inner, u.dstPort)
	if !ok {
		u.t.Error("uplink got a packet that is not a valid inner UDP datagram to the target port")
		return nil
	}
	if !from.IP.Equal(u.ueIP) {
		u.t.Errorf("packet sourced from %v, want UE IP %v", from.IP, u.ueIP)
	}
	u.packets.Add(1)
	u.bytes.Add(int64(len(payload)))
	return nil
}

// TestRunFlowOverTunnel drives a real loom UDP flow through the bridge over a
// loopback tunnel and checks loom's throughput accounting matches the packets
// that actually went up, each correctly sourced from the UE IP.
func TestRunFlowOverTunnel(t *testing.T) {
	up := &countingUplink{ueIP: net.ParseIP("192.168.100.5"), dstPort: 9999, t: t}
	res, err := RunFlow(context.Background(), Config{
		Uplink:     up,
		UEIP:       up.ueIP,
		Target:     "10.0.0.9:9999",
		PacketSize: 200,
		Rate:       "10Mbps",
		Duration:   300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Packets == 0 {
		t.Fatal("loom sent no packets over the tunnel")
	}
	if int64(res.Packets) != up.packets.Load() {
		t.Errorf("loom counted %d packets, the tunnel saw %d", res.Packets, up.packets.Load())
	}
	if int64(res.Bytes) != up.bytes.Load() {
		t.Errorf("loom counted %d payload bytes, the tunnel saw %d", res.Bytes, up.bytes.Load())
	}
	// 10Mbps for ~300ms ≈ 375 KB ≈ ~1875 × 200B packets — sanity, not exact.
	if res.Mbps < 5 || res.Mbps > 15 {
		t.Errorf("throughput %.1f Mbps far from the 10Mbps offered", res.Mbps)
	}
	t.Logf("loom→GTP-U: %d packets, %d payload bytes, %.1f Mbps over %v",
		res.Packets, res.Bytes, res.Mbps, res.Duration.Round(time.Millisecond))
}

func TestRunFlowValidatesConfig(t *testing.T) {
	if _, err := RunFlow(context.Background(), Config{Uplink: &countingUplink{t: t}, Target: "bad"}); err == nil {
		t.Fatal("expected error on malformed target")
	}
}

// echoProbe turns each uplink ICMP echo request into a downlink echo reply
// pushed onto a demux-style ICMP ring (the lane the pinger now consumes),
// optionally dropping every dropEvery-th probe to model loss.
type echoProbe struct {
	ring      *datapath.Ring
	seq       int
	dropEvery int
}

func newEchoProbe(dropEvery int) *echoProbe {
	return &echoProbe{ring: datapath.NewRing(16), dropEvery: dropEvery}
}

func (e *echoProbe) SendUplink(inner []byte) error {
	e.seq++
	if e.dropEvery > 0 && e.seq%e.dropEvery == 0 {
		return nil // dropped: no reply
	}
	r := make([]byte, len(inner))
	copy(r, inner)
	var tmp [4]byte // swap IP src/dst so the reply comes "from" the target
	copy(tmp[:], r[12:16])
	copy(r[12:16], r[16:20])
	copy(r[16:20], tmp[:])
	ihl := int(r[0]&0x0F) * 4
	r[ihl] = 0 // ICMP echo reply
	e.ring.Push(r, time.Now())
	return nil
}

func TestRunLatencyOverTunnel(t *testing.T) {
	p := newEchoProbe(0)
	res, err := RunLatency(context.Background(), LatencyConfig{
		Uplink: p, RX: p.ring, UEIP: net.ParseIP("192.168.100.5"), Target: "10.0.0.9",
		Probes: 10, Spacing: time.Millisecond, Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 10 || res.Received != 10 || res.Lost != 0 {
		t.Fatalf("sent/recv/lost = %d/%d/%d, want 10/10/0", res.Sent, res.Received, res.Lost)
	}
	t.Logf("latency over tunnel: min %v mean %v max %v jitter %v loss %.0f%%",
		res.Min, res.Mean, res.Max, res.Jitter, res.LossPct)
}

func TestRunLatencyReportsLoss(t *testing.T) {
	p := newEchoProbe(2)
	res, err := RunLatency(context.Background(), LatencyConfig{
		Uplink: p, RX: p.ring, UEIP: net.ParseIP("192.168.100.5"), Target: "10.0.0.9",
		Probes: 10, Spacing: time.Millisecond, Timeout: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Lost != 5 || res.LossPct != 50 {
		t.Fatalf("lost/lossPct = %d/%.0f, want 5/50", res.Lost, res.LossPct)
	}
}

// failingUplink answers like echoProbe, but fails the sends for which fail
// returns an error (call is 1-based).
type failingUplink struct {
	*echoProbe
	calls int
	fail  func(call int) error
}

func (u *failingUplink) SendUplink(inner []byte) error {
	u.calls++
	if err := u.fail(u.calls); err != nil {
		return err
	}
	return u.echoProbe.SendUplink(inner)
}

// RunLatency counts what left: a send that fails is not sent, not lost, and
// loss is over the probes sent. A send failing because the session is gone
// stops the run there — the rest are not sent, promptly, rather than spent
// one per spacing — and says why; a transient send failure, or a reply that
// never comes, does not stop it. The caller cancelling stops it too.
func TestRunLatencyCountsUnsentProbes(t *testing.T) {
	gone := SessionGone(errors.New("UE x has no open N3 data path"))
	transient := errors.New("sendto: no buffer space available")
	tests := []struct {
		name                          string
		probes, dropEvery             int
		timeout                       time.Duration
		fail                          func(call int) error
		cancelAfter                   time.Duration
		sent, received, lost, notSent uint64
		lossPct                       float64
		notSentErr, stopped           string
		promptly                      bool
	}{
		{name: "session gone after 4", probes: 50, fail: func(c int) error {
			if c > 4 {
				return gone
			}
			return nil
		}, sent: 4, received: 4, notSent: 46, notSentErr: gone.Error(), stopped: gone.Error(), promptly: true},
		{name: "transient send errors", probes: 10, fail: func(c int) error {
			if c == 3 || c == 7 {
				return transient
			}
			return nil
		}, sent: 8, received: 8, notSent: 2, notSentErr: transient.Error()},
		{name: "dropped replies are loss", probes: 10, dropEvery: 2, timeout: 60 * time.Millisecond,
			fail: func(int) error { return nil }, sent: 10, received: 5, lost: 5, lossPct: 50},
		{name: "loss is over the probes sent", probes: 100, dropEvery: 2, timeout: 60 * time.Millisecond, fail: func(c int) error {
			if c > 4 {
				return gone
			}
			return nil
		}, sent: 4, received: 2, lost: 2, lossPct: 50, notSent: 96, notSentErr: gone.Error(), stopped: gone.Error(), promptly: true},
		{name: "caller cancels", probes: 1000, fail: func(int) error { return nil }, cancelAfter: 50 * time.Millisecond,
			stopped: context.Canceled.Error(), promptly: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			timeout := tc.timeout
			if timeout == 0 {
				timeout = 200 * time.Millisecond
			}
			up := &failingUplink{echoProbe: newEchoProbe(tc.dropEvery), fail: tc.fail}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelAfter > 0 {
				time.AfterFunc(tc.cancelAfter, cancel)
			}
			const spacing = 10 * time.Millisecond
			start := time.Now()
			res, err := RunLatency(ctx, LatencyConfig{
				Uplink: up, RX: up.ring, UEIP: net.ParseIP("192.168.100.5"), Target: "10.0.0.9",
				Probes: tc.probes, Spacing: spacing, Timeout: timeout,
			})
			took := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
			if tc.cancelAfter > 0 {
				// How many left depends on the clock; the accounting must add up.
				if res.Sent == 0 || res.Sent+res.NotSent != uint64(tc.probes) || res.Lost != 0 {
					t.Errorf("sent/notSent/lost = %d/%d/%d, want some sent and the rest not sent", res.Sent, res.NotSent, res.Lost)
				}
			} else if res.Sent != tc.sent || res.Received != tc.received || res.Lost != tc.lost || res.NotSent != tc.notSent || res.LossPct != tc.lossPct {
				t.Errorf("sent/received/lost/notSent/loss%% = %d/%d/%d/%d/%.0f, want %d/%d/%d/%d/%.0f",
					res.Sent, res.Received, res.Lost, res.NotSent, res.LossPct,
					tc.sent, tc.received, tc.lost, tc.notSent, tc.lossPct)
			}
			if tc.cancelAfter == 0 && res.NotSentErr != tc.notSentErr {
				t.Errorf("NotSentErr = %q, want %q", res.NotSentErr, tc.notSentErr)
			}
			if res.Stopped != tc.stopped {
				t.Errorf("Stopped = %q, want %q", res.Stopped, tc.stopped)
			}
			if tc.promptly && took > time.Duration(tc.probes)*spacing/2 {
				t.Errorf("took %v: a stopped run spent its remaining probes", took)
			}
			if res.Elapsed <= 0 || res.Elapsed > took {
				t.Errorf("Elapsed = %v, want within the %v the call took", res.Elapsed, took)
			}
			if want := float64(res.Sent) / res.Elapsed.Seconds(); res.Rate != want {
				t.Errorf("Rate = %v, want sent/elapsed %v", res.Rate, want)
			}
		})
	}
}

// SessionGone keeps the error's text and chain and adds ErrSessionGone; nil
// stays nil.
func TestSessionGone(t *testing.T) {
	if SessionGone(nil) != nil {
		t.Error("SessionGone(nil) != nil")
	}
	err := SessionGone(net.ErrClosed)
	if !errors.Is(err, ErrSessionGone) || !errors.Is(err, net.ErrClosed) || err.Error() != net.ErrClosed.Error() {
		t.Errorf("SessionGone(net.ErrClosed) = %v: want both in its chain and the closed text", err)
	}
}
