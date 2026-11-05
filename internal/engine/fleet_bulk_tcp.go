package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bgrewell/loom/core/netpath"
	"github.com/bgrewell/loom/core/units"
)

// bulkTCPWriteBuf is the write block for the uplink sender. It is independent
// of the UDP packet size: TCP is a byte stream, and a larger block amortises
// the per-write netstack cost without changing what crosses the wire.
const bulkTCPWriteBuf = 32 * 1024

// runFleetBulkTCP is the TCP variant of runFleetBulk (fleet_bulk.go). Both
// directions ride each UE's gVisor netstack — the same path the http cohort
// uses — so real TCP congestion control, retransmission and RTT are exercised
// across the GTP-U tunnel:
//
//   - uplink: each UE dials a loom TCP sink on N6 and streams at the rate;
//   - downlink: each UE listens in its netstack and a loom TCP sender on N6
//     streams toward it.
//
// It returns the per-direction goodput and the number of UEs with at least one
// active flow.
func runFleetBulkTCP(ctx context.Context, log *slog.Logger, pool *fleetAgentPool, cfg FleetBulkTraffic,
	members []*fleetUE, duration time.Duration, live *FleetLiveStats) bulkResult {
	a, err := pool.acquire(ctx, cfg.Peer, cfg.Token)
	if err != nil {
		log.Warn("fleet bulk traffic: N6 agent unavailable", "peer", cfg.Peer, "err", err)
		return bulkResult{}
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
			return bulkResult{}
		}
	}

	// One TCP sink on N6 accepts every UE's uplink connection; its ephemeral
	// port gives the senders a real target.
	uplinkTarget := ""
	receiverID := ""
	if cfg.UplinkRate != "" {
		receiverID, uplinkTarget, err = configureBulkReceiver(ctx, pool, a, dataHost, packetSize, "tcp")
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
	started := 0
	for _, fu := range members {
		ueIP := net.ParseIP(fu.sess.Result.PDUAddress)
		if ueIP == nil {
			log.Warn("fleet bulk traffic: UE has no PDU address", "supi", fu.sess.SUPI)
			continue
		}
		nw, err := fu.sess.tcpNetwork(ueIP)
		if err != nil {
			log.Warn("fleet bulk traffic: UE netstack unavailable", "supi", fu.sess.SUPI, "err", err)
			continue
		}
		active := false

		if uplinkTarget != "" {
			active = true
			wg.Add(1)
			go func(nw netpath.Network, supi string) {
				defer wg.Done()
				n, err := bulkTCPSend(ctx, nw, uplinkTarget, cfg.UplinkRate, duration, pool.tun.rpcTimeout)
				if err != nil {
					log.Warn("fleet bulk traffic: uplink flow failed", "supi", supi, "err", err)
				}
				upBytes.Add(n)
			}(nw, fu.sess.SUPI)
		}

		if cfg.DownlinkRate != "" {
			ln, err := nw.Listen("tcp", fmt.Sprintf(":%d", bulkDownlinkPort))
			if err != nil {
				log.Warn("fleet bulk traffic: downlink listen failed", "supi", fu.sess.SUPI, "err", err)
			} else {
				target := net.JoinHostPort(fu.sess.Result.PDUAddress, fmt.Sprint(bulkDownlinkPort))
				id, serr := configureBulkSender(ctx, pool, a, target, cfg.DownlinkRate, packetSize, duration, "tcp")
				if serr != nil {
					log.Warn("fleet bulk traffic: downlink sender setup failed", "supi", fu.sess.SUPI, "err", serr)
					_ = ln.Close()
				} else {
					remoteIDs = append(remoteIDs, id)
					active = true
					wg.Add(1)
					go func(ln net.Listener, supi string) {
						defer wg.Done()
						downBytes.Add(bulkTCPDrain(ctx, ln, duration+pool.tun.serverSlack))
					}(ln, fu.sess.SUPI)
				}
			}
		}

		if active {
			started++
			live.TrafficFlowStarted()
			live.MarkCohortFlow(fu.sess.SUPI, "bulk", "bulk", cfg.Peer)
		}
	}

	wg.Wait()
	for _, id := range remoteIDs {
		pool.destroyFlow(a, id)
	}
	return bulkResult{Flows: started, UplinkBytes: upBytes.Load(), DownlinkBytes: downBytes.Load()}
}

// bulkTCPSend dials target through the UE's netstack and writes a paced byte
// stream for duration. It returns the bytes written (the uplink goodput).
func bulkTCPSend(ctx context.Context, nw netpath.Network, target, rate string,
	duration time.Duration, dialTimeout time.Duration) (uint64, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := nw.DialContext(dctx, "tcp", target)
	if err != nil {
		return 0, fmt.Errorf("dial %s: %w", target, err)
	}
	defer conn.Close()
	return pacedWrite(ctx, conn, rate, duration)
}

// pacedWrite writes to conn at the given bit rate for duration, returning the
// bytes written. It paces in short slices so the stream tracks the target rate
// rather than bursting, and stops on ctx cancellation or a write error.
func pacedWrite(ctx context.Context, conn net.Conn, rate string, duration time.Duration) (uint64, error) {
	bps, err := units.ParseRate(rate)
	if err != nil || bps <= 0 {
		return 0, fmt.Errorf("bulk tcp uplink rate %q is not a positive bit rate", rate)
	}
	const tick = 20 * time.Millisecond
	perTick := uint64(bps) / 8 * uint64(tick) / uint64(time.Second)
	if perTick == 0 {
		perTick = 1
	}
	buf := make([]byte, bulkTCPWriteBuf)
	deadline := time.Now().Add(duration)
	t := time.NewTicker(tick)
	defer t.Stop()
	var total uint64
	for {
		var sent uint64
		for sent < perTick {
			n := perTick - sent
			if n > uint64(len(buf)) {
				n = uint64(len(buf))
			}
			_ = conn.SetWriteDeadline(time.Now().Add(4 * tick))
			m, werr := conn.Write(buf[:n])
			total += uint64(m)
			sent += uint64(m)
			if werr != nil {
				// A deadline timeout under backpressure is expected pacing, not
				// a failure; anything else ends the flow.
				var ne net.Error
				if errors.As(werr, &ne) && ne.Timeout() {
					break
				}
				return total, werr
			}
		}
		select {
		case <-ctx.Done():
			return total, nil
		case <-t.C:
			if !time.Now().Before(deadline) {
				return total, nil
			}
		}
	}
}

// bulkTCPDrain accepts the N6 sender's connection on ln and discards every
// byte, returning the count (the downlink goodput). It closes ln after the run
// window so a never-arriving connection cannot wedge Accept.
func bulkTCPDrain(ctx context.Context, ln net.Listener, window time.Duration) uint64 {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(window):
		case <-done:
		}
		_ = ln.Close()
	}()
	conn, err := ln.Accept()
	if err != nil {
		return 0
	}
	defer conn.Close()
	n, _ := io.Copy(io.Discard, conn)
	return uint64(n)
}
