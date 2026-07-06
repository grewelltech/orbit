package cli

import (
	"fmt"
	"io"
	"log/slog"
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
		count, conc, gnbCount             int
		gnbID, gnbBits, tac, sst          uint32
		rate, sloSuccess                  float64
		sloRegP99, sloAttachP99           time.Duration
		duration, sampleInterval          time.Duration
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
			curve, err := parseRate(rate, rampSpec)
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

			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			rep, err := engine.RunLoad(cmd.Context(), log, spec)
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
	for _, r := range []string{"amf", "base-imsi", "ki", "opc", "mcc", "mnc"} {
		_ = cmd.MarkFlagRequired(r)
	}
	return cmd
}

// parseRate turns the --rate / --ramp flags into a load.Rate (nil = unbounded).
func parseRate(rate float64, ramp string) (load.Rate, error) {
	if ramp != "" {
		parts := strings.Split(ramp, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("--ramp must be start:end:seconds, got %q", ramp)
		}
		start, err1 := strconv.ParseFloat(parts[0], 64)
		end, err2 := strconv.ParseFloat(parts[1], 64)
		secs, err3 := strconv.ParseFloat(parts[2], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Errorf("invalid --ramp %q", ramp)
		}
		return load.LinearRamp{Start: start, End: end, Over: time.Duration(secs * float64(time.Second))}, nil
	}
	if rate > 0 {
		return load.Constant{RPS: rate}, nil
	}
	return nil, nil
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
