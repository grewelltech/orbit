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

// FleetBulkTraffic drives constant-rate UDP load through a fixed number of
// non-mobile UEs. Rates are per UE and either direction may be omitted.
type FleetBulkTraffic struct {
	UEs          int
	UplinkRate   string
	DownlinkRate string
	Peer         string
	Token        string
	PeerDataIP   string
	PacketSize   int
}

const bulkDownlinkPort = 5001

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
	if cfg.PacketSize < 0 || cfg.PacketSize > 1350 {
		return fmt.Errorf("bulk packet size must be between 1 and 1350 bytes (or 0 for the default)")
	}
	return nil
}

// runFleetBulk drives uplink locally through each UE's tunnel and asks the N6
// loom agent to drive downlink toward each UE's PDU address. The members are
// reserved by runFleetBehaviors, so bulk does not overlap mobility or apps.
func runFleetBulk(ctx context.Context, log *slog.Logger, pool *fleetAgentPool, cfg FleetBulkTraffic,
	members []*fleetUE, duration time.Duration, live *FleetLiveStats) (int, uint64) {
	if len(members) == 0 || pool == nil || cfg.Peer == "" {
		return 0, 0
	}
	a, err := pool.acquire(ctx, cfg.Peer, cfg.Token)
	if err != nil {
		log.Warn("fleet bulk traffic: N6 agent unavailable", "peer", cfg.Peer, "err", err)
		return 0, 0
	}
	defer pool.release(a)

	packetSize := cfg.PacketSize
	if packetSize == 0 {
		packetSize = 1200
	}
	dataHost := cfg.PeerDataIP
	if dataHost == "" {
		dataHost, _, err = net.SplitHostPort(cfg.Peer)
		if err != nil {
			log.Warn("fleet bulk traffic: invalid N6 peer", "peer", cfg.Peer, "err", err)
			return 0, 0
		}
	}

	// One receiver is enough for every uplink sender. Its returned ephemeral
	// port gives the traffic a real N6 sink instead of relying on an arbitrary
	// closed UDP port.
	uplinkTarget := ""
	receiverID := ""
	if cfg.UplinkRate != "" {
		receiverID, uplinkTarget, err = configureBulkReceiver(ctx, pool, a, dataHost, packetSize)
		if err != nil {
			log.Warn("fleet bulk traffic: uplink receiver setup failed", "peer", cfg.Peer, "err", err)
		}
	}
	if receiverID != "" {
		defer pool.destroyFlow(a, receiverID)
	}

	var wg sync.WaitGroup
	var bytes atomic.Uint64
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
				bytes.Add(res.Bytes)
			}(fu)
		}

		if cfg.DownlinkRate != "" {
			target := net.JoinHostPort(fu.sess.Result.PDUAddress, fmt.Sprint(bulkDownlinkPort))
			id, err := configureBulkSender(ctx, pool, a, target, cfg.DownlinkRate, packetSize, duration)
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
			bytes.Add(after - before)
		}
	}
	return started, bytes.Load()
}

func configureBulkReceiver(ctx context.Context, pool *fleetAgentPool, a *fleetAgent,
	host string, packetSize int) (string, string, error) {
	cfg, err := bulkConfigure(ctx, pool, a, &loomv1.FlowSpec{
		Role: loomv1.FlowRole_FLOW_ROLE_RECEIVER, Datapath: "udp", PacketSize: uint32(packetSize),
	})
	if err != nil {
		return "", "", err
	}
	if cfg.GetDataPort() == 0 {
		pool.destroyFlow(a, cfg.GetFlowId())
		return "", "", fmt.Errorf("loomd at %s advertised no UDP receiver port", a.addr)
	}
	return cfg.GetFlowId(), net.JoinHostPort(host, fmt.Sprint(cfg.GetDataPort())), nil
}

func configureBulkSender(ctx context.Context, pool *fleetAgentPool, a *fleetAgent,
	target, rate string, packetSize int, duration time.Duration) (string, error) {
	cfg, err := bulkConfigure(ctx, pool, a, &loomv1.FlowSpec{
		Generator: "stream", Payload: "random", Datapath: "udp", Target: target,
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
