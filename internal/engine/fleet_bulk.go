package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	loomv1 "github.com/bgrewell/loom/api/loomv1"
	"github.com/bgrewell/loom/core/units"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/bgrewell/orbit/internal/loomgtp"
)

// FleetBulkTraffic drives constant-rate load through a fixed number of
// non-mobile UEs. Rates are per UE and either direction may be omitted.
// Transport selects UDP (the default) or TCP; TCP rides each UE's gVisor
// netstack so the transport's congestion control, retransmission and RTT are
// exercised across the GTP-U tunnel.
type FleetBulkTraffic struct {
	UEs          int
	UplinkRate   string
	DownlinkRate string
	Peer         string
	Token        string
	PeerDataIP   string
	PacketSize   int
	Transport    string // "" or "udp" (default) | "tcp"
}

// isTCP reports whether this bulk workload runs over TCP.
func (c FleetBulkTraffic) isTCP() bool { return c.Transport == "tcp" }

// bulkResult is a bulk run's outcome: UEs with at least one active flow and
// the goodput each direction carried (counted where the bytes land — the UE's
// uplink send and downlink receive).
type bulkResult struct {
	Flows         int
	UplinkBytes   uint64
	DownlinkBytes uint64
}

// Bytes is the total goodput across both directions.
func (r bulkResult) Bytes() uint64 { return r.UplinkBytes + r.DownlinkBytes }

const bulkDownlinkPort = 5001

// maxBulkPacketSize is the largest inner UDP payload fleet bulk traffic may
// request. Fleet bulk rides the GTP-U tunnel in both directions (uplink through
// each UE's tunnel, downlink from the N6 responder to the PDU address through
// the core), so its payload is bounded by the tunnel's inner MTU, not by any
// NIC. The inner MTU is loomgtp.DefaultInnerMTU (1400); subtracting the inner
// IPv4+UDP headers (28) leaves 1372, and this ceiling keeps headroom under that
// for stacked encapsulation. Jumbo-sized payloads only help on direct,
// non-tunnel datapaths, which clamp against their own egress MTU instead.
const maxBulkPacketSize = 1350

// resolveBulkPacketSize maps a requested inner UDP payload size to the size a
// tunnel bulk flow will use. Zero (the default) means "auto": the largest the
// GTP-U tunnel safely carries. A NIC's MTU is deliberately not consulted here
// because it does not bound inner tunnel payloads.
func resolveBulkPacketSize(requested int) int {
	if requested <= 0 {
		return maxBulkPacketSize
	}
	return requested
}

// ValidateBulkPacketSize rejects a negative or over-MTU bulk payload size. Zero
// is valid and selects the auto-detected default (see resolveBulkPacketSize).
func ValidateBulkPacketSize(n int) error {
	if n < 0 || n > maxBulkPacketSize {
		return fmt.Errorf("bulk packet size must be between 1 and %d bytes (or 0 to auto-detect the largest the tunnel carries)", maxBulkPacketSize)
	}
	return nil
}

func validateFleetBulk(cfg FleetBulkTraffic) error {
	if cfg.UEs == 0 && cfg.UplinkRate == "" && cfg.DownlinkRate == "" && cfg.Peer == "" {
		return nil
	}
	if cfg.UEs < 1 {
		return fmt.Errorf("bulk traffic needs at least one UE")
	}
	if cfg.Peer == "" {
		return fmt.Errorf("bulk traffic needs an N6 loomd peer")
	}
	if cfg.UplinkRate == "" && cfg.DownlinkRate == "" {
		return fmt.Errorf("bulk traffic needs an uplink rate, downlink rate, or both")
	}
	for direction, value := range map[string]string{"uplink": cfg.UplinkRate, "downlink": cfg.DownlinkRate} {
		if value == "" {
			continue
		}
		if rate, err := units.ParseRate(value); err != nil || rate <= 0 {
			return fmt.Errorf("bulk %s rate %q is not a positive bit rate", direction, value)
		}
	}
	if err := ValidateBulkPacketSize(cfg.PacketSize); err != nil {
		return err
	}
	switch cfg.Transport {
	case "", "udp", "tcp":
	default:
		return fmt.Errorf("bulk transport %q is not udp or tcp", cfg.Transport)
	}
	return nil
}

// runFleetBulk drives uplink locally through each UE's tunnel and asks the N6
// loom agent to drive downlink toward each UE's PDU address. The members are
// reserved by runFleetBehaviors, so bulk does not overlap mobility or apps.
func runFleetBulk(ctx context.Context, log *slog.Logger, pool *fleetAgentPool, cfg FleetBulkTraffic,
	members []*fleetUE, duration time.Duration, live *FleetLiveStats) bulkResult {
	if len(members) == 0 || pool == nil || cfg.Peer == "" {
		return bulkResult{}
	}
	if cfg.isTCP() {
		return runFleetBulkTCP(ctx, log, pool, cfg, members, duration, live)
	}
	a, err := pool.acquire(ctx, cfg.Peer, cfg.Token)
	if err != nil {
		log.Warn("fleet bulk traffic: N6 agent unavailable", "peer", cfg.Peer, "err", err)
		return bulkResult{}
	}
	defer pool.release(a)

	packetSize := resolveBulkPacketSize(cfg.PacketSize)
	dataHost := cfg.PeerDataIP
	if dataHost == "" {
		dataHost, _, err = net.SplitHostPort(cfg.Peer)
		if err != nil {
			log.Warn("fleet bulk traffic: invalid N6 peer", "peer", cfg.Peer, "err", err)
			return bulkResult{}
		}
	}

	// One receiver is enough for every uplink sender. Its returned ephemeral
	// port gives the traffic a real N6 sink instead of relying on an arbitrary
	// closed UDP port.
	uplinkTarget := ""
	receiverID := ""
	if cfg.UplinkRate != "" {
		receiverID, uplinkTarget, err = configureBulkReceiver(ctx, pool, a, dataHost, packetSize, "udp")
		if err != nil {
			log.Warn("fleet bulk traffic: uplink receiver setup failed", "peer", cfg.Peer, "err", err)
		}
	}
	if receiverID != "" {
		defer pool.destroyFlow(a, receiverID)
	}

	var wg sync.WaitGroup
	var upBytes, downBytes atomic.Uint64
	remoteIDs := make([]string, 0, len(members))
	downlinkBase := make(map[*fleetUE]uint64, len(members))
	started := 0
	for _, fu := range members {
		if _, _, err := fu.sess.dataplane(); err != nil {
			log.Warn("fleet bulk traffic: UE data path unavailable", "supi", fu.sess.SUPI, "err", err)
			continue
		}
		_, _, _, dlBytes := fu.sess.Totals()
		downlinkBase[fu] = dlBytes

		active := false
		if uplinkTarget != "" {
			active = true
			wg.Add(1)
			go func(fu *fleetUE) {
				defer wg.Done()
				res, err := loomgtp.RunFlow(ctx, loomgtp.Config{
					Uplink: fu.sess, UEIP: net.ParseIP(fu.sess.Result.PDUAddress),
					Target: uplinkTarget, PacketSize: packetSize, Rate: cfg.UplinkRate, Duration: duration,
				})
				if err != nil {
					log.Warn("fleet bulk traffic: uplink flow failed", "supi", fu.sess.SUPI, "err", err)
					return
				}
				upBytes.Add(res.Bytes)
			}(fu)
		}

		if cfg.DownlinkRate != "" {
			target := net.JoinHostPort(fu.sess.Result.PDUAddress, fmt.Sprint(bulkDownlinkPort))
			id, err := configureBulkSender(ctx, pool, a, target, cfg.DownlinkRate, packetSize, duration, "udp")
			if err != nil {
				log.Warn("fleet bulk traffic: downlink sender setup failed", "supi", fu.sess.SUPI, "err", err)
			} else {
				remoteIDs = append(remoteIDs, id)
				active = true
			}
		}
		if active {
			started++
			live.TrafficFlowStarted()
			live.MarkCohortFlow(fu.sess.SUPI, "bulk", "bulk", cfg.Peer)
		}
	}

	// A downlink-only run has no local flow goroutine to hold this worker open.
	if cfg.UplinkRate == "" {
		<-ctx.Done()
	}
	wg.Wait()
	for _, id := range remoteIDs {
		pool.destroyFlow(a, id)
	}
	for fu, before := range downlinkBase {
		_, _, _, after := fu.sess.Totals()
		if after >= before {
			downBytes.Add(after - before)
		}
	}
	return bulkResult{Flows: started, UplinkBytes: upBytes.Load(), DownlinkBytes: downBytes.Load()}
}

func configureBulkReceiver(ctx context.Context, pool *fleetAgentPool, a *fleetAgent,
	host string, packetSize int, datapath string) (string, string, error) {
	cfg, err := bulkConfigure(ctx, pool, a, &loomv1.FlowSpec{
		Role: loomv1.FlowRole_FLOW_ROLE_RECEIVER, Datapath: datapath, PacketSize: uint32(packetSize),
	})
	if err != nil {
		return "", "", err
	}
	if cfg.GetDataPort() == 0 {
		pool.destroyFlow(a, cfg.GetFlowId())
		return "", "", fmt.Errorf("loomd at %s advertised no %s receiver port", a.addr, datapath)
	}
	return cfg.GetFlowId(), net.JoinHostPort(host, fmt.Sprint(cfg.GetDataPort())), nil
}

func configureBulkSender(ctx context.Context, pool *fleetAgentPool, a *fleetAgent,
	target, rate string, packetSize int, duration time.Duration, datapath string) (string, error) {
	cfg, err := bulkConfigure(ctx, pool, a, &loomv1.FlowSpec{
		Generator: "stream", Payload: "random", Datapath: datapath, Target: target,
		PacketSize: uint32(packetSize), Rate: rate, Duration: durationpb.New(duration + pool.tun.serverSlack),
	})
	if err != nil {
		return "", err
	}
	return cfg.GetFlowId(), nil
}

func bulkConfigure(ctx context.Context, pool *fleetAgentPool, a *fleetAgent,
	spec *loomv1.FlowSpec) (*loomv1.ConfigureResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, pool.tun.rpcTimeout)
	defer cancel()
	cfg, err := a.client.Configure(rctx, &loomv1.ConfigureRequest{Flow: spec})
	if err != nil {
		return nil, err
	}
	if cfg.GetFlowId() == "" {
		return nil, fmt.Errorf("loomd at %s returned an empty flow id", a.addr)
	}
	if _, err := a.client.Start(rctx, &loomv1.StartRequest{
		FlowId: cfg.GetFlowId(), ReportIntervalNanos: pool.tun.sampleInterval.Nanoseconds(),
	}); err != nil {
		pool.destroyFlow(a, cfg.GetFlowId())
		return nil, err
	}
	return cfg, nil
}
