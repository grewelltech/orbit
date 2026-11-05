package engine

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	loomv1 "github.com/bgrewell/loom/api/loomv1"
)

// The N6 side of bulk-over-TCP uses loom's stock tcp datapath: a TCP sink for
// uplink and a paced TCP sender for downlink. Exercise the real agent protocol
// so a datapath/role/target mistake fails here, not on a testbed.
func TestBulkRemoteFlowsTCP(t *testing.T) {
	addr := startLoomAgent(t, "test")
	pool := newFleetAgentPool(appTuning{sampleInterval: 20 * time.Millisecond, rpcTimeout: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := pool.acquire(ctx, addr, "")
	if err != nil {
		t.Fatalf("acquire agent: %v", err)
	}
	defer pool.release(a)

	receiverID, target, err := configureBulkReceiver(ctx, pool, a, "127.0.0.1", 512, "tcp")
	if err != nil {
		t.Fatalf("configure tcp receiver: %v", err)
	}
	defer pool.destroyFlow(a, receiverID)

	senderID, err := configureBulkSender(ctx, pool, a, target, "5Mbps", 512, 200*time.Millisecond, "tcp")
	if err != nil {
		t.Fatalf("configure tcp sender: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	pool.stopFlow(a, senderID)
	pool.stopFlow(a, receiverID)

	stream, err := a.client.StreamTelemetry(ctx, &loomv1.TelemetryRequest{FlowId: receiverID})
	if err != nil {
		t.Fatalf("receiver telemetry: %v", err)
	}
	var bytes uint64
	for {
		sample, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("receive telemetry: %v", err)
		}
		bytes = sample.GetBytes()
	}
	pool.destroyFlow(a, senderID)
	if bytes == 0 {
		t.Fatal("bulk tcp receiver observed no traffic")
	}
}

// pacedWrite tracks the requested rate (loosely, to survive CI jitter) and
// stops at the duration rather than running on.
func TestPacedWriteRateAndStop(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, c)
		c.Close()
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	const dur = 300 * time.Millisecond
	start := time.Now()
	// 8 Mbps = 1 MB/s → ~300 KB over 300 ms.
	n, err := pacedWrite(context.Background(), conn, "8Mbps", dur)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("pacedWrite: %v", err)
	}
	if elapsed > dur+time.Second {
		t.Fatalf("pacedWrite ran %v, well past the %v window", elapsed, dur)
	}
	// Expect ~300 KB; accept a wide band so the test is about correctness, not
	// precise shaping.
	if n < 120_000 || n > 750_000 {
		t.Fatalf("pacedWrite sent %d bytes, outside the expected ~300 KB band", n)
	}
}

func TestPacedWriteRejectsBadRate(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	if _, err := pacedWrite(context.Background(), c1, "not-a-rate", 10*time.Millisecond); err == nil {
		t.Fatal("expected an error for an unparseable rate")
	}
}

// bulkTCPDrain counts every byte the sender delivers.
func TestBulkTCPDrainCounts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	const want = 64 * 1024
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		_, _ = c.Write(make([]byte, want))
		c.Close()
	}()
	got := bulkTCPDrain(context.Background(), ln, 2*time.Second)
	if got != want {
		t.Fatalf("bulkTCPDrain counted %d bytes, want %d", got, want)
	}
}

// bulkTCPDrain must not wedge when no connection ever arrives: the window
// closes the listener and Accept returns.
func TestBulkTCPDrainWindowCloses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	start := time.Now()
	got := bulkTCPDrain(context.Background(), ln, 100*time.Millisecond)
	if got != 0 {
		t.Fatalf("expected 0 bytes with no sender, got %d", got)
	}
	if time.Since(start) > time.Second {
		t.Fatal("bulkTCPDrain did not return promptly after its window")
	}
}

func TestValidateFleetBulkTransport(t *testing.T) {
	base := FleetBulkTraffic{UEs: 1, UplinkRate: "5Mbps", Peer: "10.0.0.1:7000"}
	for _, tc := range []struct {
		transport string
		ok        bool
	}{
		{"", true},
		{"udp", true},
		{"tcp", true},
		{"sctp", false},
		{"TCP", false},
	} {
		cfg := base
		cfg.Transport = tc.transport
		err := validateFleetBulk(cfg)
		if tc.ok && err != nil {
			t.Errorf("transport %q: unexpected error %v", tc.transport, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("transport %q: expected an error", tc.transport)
		}
	}
}
