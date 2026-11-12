package engine

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	loomv1 "github.com/bgrewell/loom/api/loomv1"
	"google.golang.org/protobuf/types/known/durationpb"
)

// DownlinkTrigger is something on the N6 side of the core that can send
// downlink traffic to a UE's PDU address without the UE asking for it: what
// makes a core page a CM-IDLE UE.
type DownlinkTrigger interface {
	// SendDownlink sends at least one packet from N6 towards ueIP and
	// returns once it has been asked for; delivery is not confirmed.
	SendDownlink(ctx context.Context, ueIP netip.Addr) error
}

// DownlinkTriggerFunc adapts a function to DownlinkTrigger.
type DownlinkTriggerFunc func(ctx context.Context, ueIP netip.Addr) error

// SendDownlink calls f.
func (f DownlinkTriggerFunc) SendDownlink(ctx context.Context, ueIP netip.Addr) error {
	return f(ctx, ueIP)
}

// ResponderDownlink is the DownlinkTrigger of a live run: a loom responder
// on the N6 network (`orbit responder`, or loomd), asked over its control
// address to send a short UDP flow to the UE's address.
type ResponderDownlink struct {
	// Peer is the responder's control address (host:port) and Token its
	// bearer token.
	Peer, Token string
	// Port is the UDP port at the UE the flow is sent to; 0 is 5001.
	// Nothing needs to listen there.
	Port int
}

// The responder's flow: a second of small datagrams at a low rate. One flow is one trigger for a UPF that
// de-duplicates its downlink data notifications.
const (
	triggerFlowDuration   = time.Second
	triggerFlowRate       = "64kbps"
	triggerFlowPacketSize = 128
	defaultTriggerPort    = 5001
)

// SendDownlink starts the flow on the responder and returns once the
// responder has accepted it. The flow ends by its own duration.
func (r ResponderDownlink) SendDownlink(ctx context.Context, ueIP netip.Addr) error {
	if r.Peer == "" {
		return fmt.Errorf("no responder configured")
	}
	pool := newFleetAgentPool(appTuning{})
	a, err := pool.acquire(ctx, r.Peer, r.Token)
	if err != nil {
		return fmt.Errorf("responder %s: %w", r.Peer, err)
	}
	defer pool.release(a)
	port := r.Port
	if port == 0 {
		port = defaultTriggerPort
	}
	target := net.JoinHostPort(ueIP.String(), strconv.Itoa(port))
	if _, err := bulkConfigure(ctx, pool, a, &loomv1.FlowSpec{
		Generator: "stream", Payload: "random", Datapath: "udp", Target: target,
		PacketSize: triggerFlowPacketSize, Rate: triggerFlowRate, Duration: durationpb.New(triggerFlowDuration),
	}); err != nil {
		return fmt.Errorf("responder %s: downlink flow to %s: %w", r.Peer, target, err)
	}
	return nil
}
