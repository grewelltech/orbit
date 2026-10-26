package engine

import (
	"context"
	"io"
	"testing"
	"time"

	loomv1 "github.com/bgrewell/loom/api/loomv1"
)

// The bulk control path uses stock loom roles: one shared receiver for uplink
// and a paced sender per downlink UE. Exercise the real agent protocol so role,
// datapath, target, and lifecycle mistakes fail before reaching a testbed.
func TestBulkRemoteFlows(t *testing.T) {
	addr := startLoomAgent(t, "test")
	pool := newFleetAgentPool(appTuning{sampleInterval: 20 * time.Millisecond, rpcTimeout: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := pool.acquire(ctx, addr, "")
	if err != nil {
		t.Fatalf("acquire agent: %v", err)
	}
	defer pool.release(a)

	receiverID, target, err := configureBulkReceiver(ctx, pool, a, "127.0.0.1", 512)
	if err != nil {
		t.Fatalf("configure receiver: %v", err)
	}
	defer pool.destroyFlow(a, receiverID)

	senderID, err := configureBulkSender(ctx, pool, a, target, "5Mbps", 512, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("configure sender: %v", err)
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
		t.Fatal("bulk receiver observed no traffic")
	}
}

func TestValidateFleetBulk(t *testing.T) {
	if err := validateFleetBulk(FleetBulkTraffic{}); err != nil {
		t.Fatalf("disabled bulk config: %v", err)
	}
	if err := validateFleetBulk(FleetBulkTraffic{UEs: 10, Peer: "n6:9551", DownlinkRate: "50Mbps"}); err != nil {
		t.Fatalf("valid downlink config: %v", err)
	}
	for _, cfg := range []FleetBulkTraffic{
		{UEs: 0, Peer: "n6:9551", UplinkRate: "1Mbps"},
		{UEs: 1, UplinkRate: "1Mbps"},
		{UEs: 1, Peer: "n6:9551"},
		{UEs: 1, Peer: "n6:9551", DownlinkRate: "nope"},
		{UEs: 1, Peer: "n6:9551", UplinkRate: "1Mbps", PacketSize: 1400},
	} {
		if err := validateFleetBulk(cfg); err == nil {
			t.Errorf("validateFleetBulk(%+v) succeeded, want error", cfg)
		}
	}
}
