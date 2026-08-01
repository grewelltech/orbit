package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// newLoadCmd is a direct-drive load generator: it runs the attach storm
// in-process against the core (not through the API server), reporting
// per-procedure latency KPIs. Numbers here are integration-capacity, bounded
// by the core under test.
func newLoadCmd() *cobra.Command {
	var (
		amf, baseIMSI, ki, opc, mcc, mnc  string
		gnbName, sd, dnn, gnbN3, rampSpec string
		arrival                           string
		count, conc, gnbCount             int
		gnbID, gnbBits, tac, sst          uint32
		rate, sloSuccess                  float64
		sloRegP99, sloAttachP99           time.Duration
		duration, sampleInterval          time.Duration
		progressEvery                     time.Duration
		withPDU                           bool
	)
	cmd := &cobra.Command{
		Use:   "load",
		Short: "Rate-controlled attach storm against the core with latency KPIs",
		RunE: func(cmd *cobra.Command, args []string) error {
			kiB, err := auth.ParseHexKey("Ki", ki)
			if err != nil {
				return err
			}
			opcB, err := auth.ParseHexKey("OPc", opc)
			if err != nil {
				return err
			}
			curve, err := parseRateArrival(rate, rampSpec, arrival)
			if err != nil {
				return err
			}
			if gnbCount < 1 {
				gnbCount = 1
			}
			var gnbs []engine.GNBSpec
			for n := 0; n < gnbCount; n++ {
				gnbs = append(gnbs, engine.GNBSpec{
					AMFAddr: amf,
					Config: gnb.Config{
						ID: gnbID + uint32(n), IDBits: int(gnbBits), Name: fmt.Sprintf("%s-%d", gnbName, n),
						MCC: mcc, MNC: mnc, TAC: tac, Slices: []gnb.SNSSAI{{SST: uint8(sst), SD: sd}},
					},
				})
			}
			spec := engine.LoadSpec{
				GNBs: gnbs, BaseIMSI: baseIMSI, Count: count, MCC: mcc, MNC: mnc,
				Ki: kiB, OPc: opcB, Concurrency: conc, Rate: curve,
				Duration: duration, SampleEvery: sampleInterval,
			}
			if withPDU {
				spec.PDUSession = &ue.PDUSessionParams{PDUSessionID: 1, SST: uint8(sst), SD: sd, DNN: dnn}
				spec.GNBN3Addr = gnbN3
			}

			// Live progress: a load run is silent for minutes otherwise, and
			// an operator watching a soak needs to see it degrade as it happens
			// rather than at the end. Progress goes to stderr so stdout stays
			// the machine-readable report.
			live := load.NewLiveStats()
			spec.Observer = live
			stopProgress := startLoadProgress(cmd.ErrOrStderr(), live, progressEvery)

			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			rep, err := engine.RunLoad(cmd.Context(), log, spec)
			stopProgress()
			if err != nil {
				return err
			}
			printLoadReport(cmd.OutOrStdout(), rep)

			slo := load.SLO{MinSuccessRate: sloSuccess, Latency: map[string]load.LatencyBound{}}
			if sloRegP99 > 0 {
				slo.Latency["registration"] = load.LatencyBound{P99: sloRegP99}
			}
			if sloAttachP99 > 0 {
				slo.Latency["attach"] = load.LatencyBound{P99: sloAttachP99}
			}
			if !slo.Empty() {
				v := slo.Evaluate(rep)
				printVerdict(cmd.OutOrStdout(), v)
				if !v.Pass {
					return fmt.Errorf("SLO breached")
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&amf, "amf", "", "AMF N2 address (host:port)")
	f.StringVar(&baseIMSI, "base-imsi", "", "first SUPI/IMSI; each UE increments it")
	f.IntVar(&count, "count", 100, "number of UEs to attach")
	f.StringVar(&ki, "ki", "", "subscriber key Ki (32 hex)")
	f.StringVar(&opc, "opc", "", "operator key OPc (32 hex)")
	f.StringVar(&mcc, "mcc", "", "MCC")
	f.StringVar(&mnc, "mnc", "", "MNC")
	f.IntVar(&conc, "concurrency", 64, "max attaches in flight")
	f.Float64Var(&rate, "rate", 0, "offered attach rate (attaches/sec; 0 = concurrency-bound)")
	f.StringVar(&rampSpec, "ramp", "", "linear ramp start:end:seconds (overrides --rate)")
	f.StringVar(&arrival, "arrival", "constant", "arrival process for the offered rate: constant (evenly spaced) or poisson (exponential inter-arrivals)")
	f.IntVar(&gnbCount, "gnb-count", 1, "number of gNBs to mux UEs across")
	f.Uint32Var(&gnbID, "gnb-id", 1, "base gNB ID")
	f.Uint32Var(&gnbBits, "gnb-id-bits", 24, "gNB ID bit length")
	f.StringVar(&gnbName, "gnb-name", "orbit-gnb", "gNB name prefix")
	f.Uint32Var(&tac, "tac", 1, "TAC")
	f.Uint32Var(&sst, "sst", 1, "slice SST")
	f.StringVar(&sd, "sd", "", "slice SD (6 hex)")
	f.BoolVar(&withPDU, "pdu-session", false, "establish a PDU session per UE")
	f.StringVar(&dnn, "dnn", "internet", "DNN (with --pdu-session)")
	f.StringVar(&gnbN3, "gnb-n3", "", "gNB N3 address (with --pdu-session)")
	f.Float64Var(&sloSuccess, "slo-min-success", 0, "SLO: minimum success rate (0-1); breach exits non-zero")
	f.DurationVar(&sloRegP99, "slo-reg-p99", 0, "SLO: max registration P99 (e.g. 500ms)")
	f.DurationVar(&sloAttachP99, "slo-attach-p99", 0, "SLO: max attach P99")
	f.DurationVar(&duration, "duration", 0, "soak: run for this long instead of --count (e.g. 5m)")
	f.DurationVar(&sampleInterval, "sample-interval", 0, "soak: resource-sample cadence (e.g. 10s)")
	f.DurationVar(&progressEvery, "progress-every", 5*time.Second, "print live progress to stderr at this cadence (0 = off)")
	for _, r := range []string{"amf", "base-imsi", "ki", "opc", "mcc", "mnc"} {
		_ = cmd.MarkFlagRequired(r)
	}
	return cmd
}

// parseRate turns the --rate / --ramp flags into a load.Rate (nil = unbounded).
func parseRate(rate float64, ramp string) (load.Rate, error) {
	return parseRateArrival(rate, ramp, "")
}

// parseRateArrival turns --rate/--ramp into a load.Rate and, when arrival is
// "poisson", wraps it so inter-arrival times are exponentially distributed
// about that mean instead of evenly spaced. seed 0 leaves the sequence
// nondeterministic; ORBIT_LOAD_SEED pins it for a reproducible run.
func parseRateArrival(rate float64, ramp, arrival string) (load.Rate, error) {
	curve, err := parseRateCurve(rate, ramp)
	if err != nil || curve == nil {
		return curve, err
	}
	switch arrival {
	case "", "constant", "uniform":
		return curve, nil
	case "poisson":
		var seed uint64
		if v := os.Getenv("ORBIT_LOAD_SEED"); v != "" {
			n, perr := strconv.ParseUint(v, 10, 64)
			if perr != nil {
				return nil, fmt.Errorf("ORBIT_LOAD_SEED must be a uint64, got %q", v)
			}
			seed = n
		}
		return load.NewPoisson(curve, seed), nil
	default:
		return nil, fmt.Errorf("--arrival must be constant or poisson, got %q", arrival)
	}
}

func parseRateCurve(rate float64, ramp string) (load.Rate, error) {
	if ramp != "" {
		start, end, secs, err := parseRampSpec(ramp)
		if err != nil {
			return nil, err
		}
		return load.LinearRamp{Start: start, End: end, Over: time.Duration(secs * float64(time.Second))}, nil
	}
	if rate > 0 {
		return load.Constant{RPS: rate}, nil
	}
	return nil, nil
}

// parseRampSpec parses a "start:end:seconds" ramp string. Shared so the
// in-process `orbit load` and the server-driven `orbit runs start-load` accept
// exactly the same ramp syntax.
func parseRampSpec(s string) (start, end, seconds float64, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("--ramp must be start:end:seconds, got %q", s)
	}
	e1 := parseFloatInto(parts[0], &start)
	e2 := parseFloatInto(parts[1], &end)
	e3 := parseFloatInto(parts[2], &seconds)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, 0, fmt.Errorf("invalid --ramp %q", s)
	}
	return start, end, seconds, nil
}

func parseFloatInto(s string, dst *float64) error {
	v, err := strconv.ParseFloat(s, 64)
	if err == nil {
		*dst = v
	}
	return err
}

func printLoadReport(w io.Writer, rep load.Report) {
	fmt.Fprintf(w, "load: %d/%d attached in %s (%.1f attach/s)\n",
		rep.Succeeded, rep.Attempted, rep.Duration.Round(time.Millisecond), rep.AchievedRate)
	for _, name := range []string{"registration", "pdu_session", "attach"} {
		s, ok := rep.Latencies[name]
		if !ok {
			continue
		}
		fmt.Fprintf(w, "  %-13s P50 %-8s P99 %-8s P99.9 %-8s max %s\n",
			name, round(s.P50), round(s.P99), round(s.P999), round(s.Max))
	}
	if len(rep.Resources) > 0 {
		first, last := rep.Resources[0], rep.Resources[len(rep.Resources)-1]
		fmt.Fprintf(w, "  resources: goroutines %d→%d, RSS %dMB→%dMB over %d samples\n",
			first.Goroutines, last.Goroutines, first.RSSBytes>>20, last.RSSBytes>>20, len(rep.Resources))
	}
}

func round(d time.Duration) string { return d.Round(100 * time.Microsecond).String() }

func printVerdict(w io.Writer, v load.Verdict) {
	result := "PASS"
	if !v.Pass {
		result = "FAIL"
	}
	fmt.Fprintf(w, "SLO: %s\n", result)
	for _, c := range v.Checks {
		mark := "ok  "
		if !c.Pass {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  [%s] %-20s %s\n", mark, c.Name, c.Detail)
	}
}

// startLoadProgress prints a live progress line every interval until the
// returned stop func is called. It returns a no-op when interval <= 0.
//
// Progress is written to stderr so a caller redirecting stdout still gets a
// clean report. The stop func blocks until the printer has finished, so the
// final report can never interleave with a progress line.
func startLoadProgress(w io.Writer, live *load.LiveStats, interval time.Duration) func() {
	if interval <= 0 || live == nil {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		var prev load.Snapshot
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s := live.Snapshot()
				fmt.Fprintln(w, formatLoadProgress(s, prev))
				prev = s
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// formatLoadProgress renders one progress line. The interval rate comes from
// the difference between successive snapshots — LiveStats reports cumulative
// values only, so each consumer picks its own cadence.
func formatLoadProgress(s, prev load.Snapshot) string {
	var now float64
	if d := s.Elapsed - prev.Elapsed; d > 0 {
		now = float64(s.Succeeded-prev.Succeeded) / d.Seconds()
	}
	line := fmt.Sprintf("load: %s  %d ok / %d attempted  %d failed  %.1f attach/s (avg %.1f)",
		s.Elapsed.Round(time.Second), s.Succeeded, s.Attempted, s.Failed, now, s.AchievedRate)
	if a, ok := s.Latencies["attach"]; ok {
		line += fmt.Sprintf("  attach P50 %s P99 %s", round(a.P50), round(a.P99))
	}
	return line
}
