package engine

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/loomgtp"
)

// FleetLatencyProbe configures the fleet's user-plane latency sampling: ICMP
// echoes sent over sampled UEs' own N3 data paths, so the figure is the
// tunnel's round trip rather than the management network's.
//
// It samples rather than sweeps. A probe opens a data path and an ICMP lane on
// the UE it measures, which at population scale is real cost for a number that
// does not vary per UE once the path is shared — a handful of UEs on each gNB
// characterises the tunnel, and the rest carry traffic undisturbed.
type FleetLatencyProbe struct {
	// Target is the IPv4 address to echo (no port). Empty disables probing.
	Target string
	// Interval between probe rounds (default 1s).
	Interval time.Duration
	// UEs is how many UEs to sample per round (default 4, capped at the
	// fleet's size). Sampling walks the population so the sample rotates.
	UEs int
	// Timeout bounds one echo (default 1s).
	Timeout time.Duration
}

func (p FleetLatencyProbe) withDefaults() FleetLatencyProbe {
	if p.Interval <= 0 {
		p.Interval = time.Second
	}
	if p.UEs <= 0 {
		p.UEs = 4
	}
	if p.Timeout <= 0 {
		p.Timeout = time.Second
	}
	return p
}

// runFleetLatencyProbe samples user-plane RTT until ctx ends, recording each
// echo into live. It rotates through the population so the sample is not
// pinned to the same few UEs for a whole soak, and records a timeout as a lost
// probe rather than as a very large latency.
//
// Probes are issued one echo at a time: the histogram then holds real
// individual round trips, where recording a multi-probe mean would report
// percentiles over averages and hide the tail the percentiles exist to show.
func runFleetLatencyProbe(ctx context.Context, ues []*fleetUE, cfg FleetLatencyProbe,
	live *FleetLiveStats, log *slog.Logger) {
	cfg = cfg.withDefaults()
	if cfg.Target == "" || len(ues) == 0 {
		return
	}
	n := min(cfg.UEs, len(ues))

	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	// next rotates the starting offset each round.
	next := 0
	var warnOnce sync.Once
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			fu := ues[(next+i)%len(ues)]
			wg.Add(1)
			go func(fu *fleetUE) {
				defer wg.Done()
				rtt, err := probeUELatency(ctx, fu, cfg)
				switch {
				case ctx.Err() != nil:
					// Shutdown, not a measurement: recording it would end
					// every run with a burst of phantom loss.
				case err != nil:
					live.RecordUPLatency(0, true)
					warnOnce.Do(func() {
						log.Warn("fleet latency probe failed; reported as probe loss",
							"supi", fu.sess.SUPI, "target", cfg.Target, "err", err)
					})
				default:
					live.RecordUPLatency(rtt, false)
				}
			}(fu)
		}
		wg.Wait()
		next = (next + n) % len(ues)
	}
}

// probeUELatency sends one ICMP echo over this UE's data path and returns its
// round trip. A probe that times out returns lost=true via a nil error and a
// zero RTT from loom's summary, which the caller records as loss.
func probeUELatency(ctx context.Context, fu *fleetUE, cfg FleetLatencyProbe) (time.Duration, error) {
	sess := fu.sess
	if !sess.Result.SessionActive || sess.gnbN3 == "" {
		return 0, errNoDataPath
	}
	// Opening the data path here is what makes the UE's downlink countable
	// too: a UE carrying only synthetic traffic has no lane until now.
	_, rx, err := sess.dataplane()
	if err != nil {
		return 0, err
	}
	ring := rx.SubscribeICMP()
	defer rx.UnsubscribeICMP(ring)

	pctx, cancel := context.WithTimeout(ctx, cfg.Timeout+time.Second)
	defer cancel()
	res, err := loomgtp.RunLatency(pctx, loomgtp.LatencyConfig{
		Uplink:  sess,
		RX:      ring,
		UEIP:    net.ParseIP(sess.Result.PDUAddress),
		Target:  cfg.Target,
		Probes:  1,
		Spacing: 0,
		Timeout: cfg.Timeout,
	})
	if err != nil {
		return 0, err
	}
	if res.Received == 0 {
		return 0, errProbeLost
	}
	return res.Mean, nil
}

// errNoDataPath and errProbeLost are recorded as probe loss rather than
// surfaced: a UE without a session, or an echo that did not come back, is a
// data point about the path, not a failure of the run.
var (
	errNoDataPath = fleetProbeError("UE has no active PDU session or gNB N3 address")
	errProbeLost  = fleetProbeError("no echo reply within the probe timeout")
)

type fleetProbeError string

func (e fleetProbeError) Error() string { return string(e) }
