package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/plan"
	"github.com/bgrewell/orbit/internal/target"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// newLoadCmd is the attach storm with per-procedure latency KPIs. It runs on
// the API server by default, as a plan built from its flags and target
// (ADR-0008 §6), waiting for the end unless --detach. --local drives the
// storm in this process instead, with no server: the capacity bench, whose
// numbers are integration capacity, bounded by the core under test.
func newLoadCmd(serverURL *string, o options) *cobra.Command {
	var (
		amf, baseIMSI, ki, opc   string
		gnbName, gnbN3, rampSpec string
		arrival                  string
		binds                    []string
		count, conc, gnbCount    int
		gnbID, gnbBits           uint32
		id                       netIdentity
		rate, sloSuccess         float64
		sloRegP99, sloAttachP99  time.Duration
		duration, sampleInterval time.Duration
		progressEvery            time.Duration
		withPDU                  bool
		tf                       thinFlags
		gates                    []string
	)
	cmd := &cobra.Command{
		Use:   "load",
		Short: "Rate-controlled attach storm against the core with latency KPIs",
		Long: "Run a rate-controlled attach storm against the core and report per-procedure latency.\n" +
			"It runs on the orbit server (`orbit serve`) and waits for the end, exiting with the\n" +
			"result: 0 passed, 1 a gate failed, 2 did not complete, 3 could not start. --detach\n" +
			"starts it and returns. --local runs it in this process with no server: the capacity\n" +
			"bench. The core, keys and addresses come from the flags or a target (-t).",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !tf.local {
				if cmd.Flags().Changed("progress-every") && tf.detach {
					return exitWith(ExitCouldNotStart, errors.New("--progress-every follows the run; --detach does not"))
				}
				return thinLoad(cmd, serverURL, o, gates, tf.detach, progressEvery)
			}
			if tf.detach || len(gates) > 0 {
				return exitWith(ExitCouldNotStart, errors.New("--detach and --gate run on the server; with --local, gate with --slo-*"))
			}
			// Flags or $ORBIT_KI/$ORBIT_OPC, validated before anything else
			// is parsed so a credentials problem is the first thing reported.
			ki, opc, err := resolveCreds(ki, opc)
			if err != nil {
				return err
			}
			if err := id.resolve(true); err != nil {
				return err
			}
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
			bindAddrs, err := engine.ExpandBindAddrs(binds, gnbCount)
			if err != nil {
				return fmt.Errorf("--bind: %w", err)
			}
			if note := sharedBindNote(binds, gnbCount); note != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), note)
			}
			// Named as a plan names them (and the server's load): one gNB
			// by --gnb-name itself, several <name>-1 .. <name>-n.
			var gnbs []engine.GNBSpec
			for n := 0; n < gnbCount; n++ {
				name := gnbName
				if gnbCount > 1 {
					name = fmt.Sprintf("%s-%d", gnbName, n+1)
				}
				gnbs = append(gnbs, engine.GNBSpec{
					AMFAddr:  amf,
					BindAddr: bindAddrs[n],
					Config:   id.gnbConfig(gnbID+uint32(n), int(gnbBits), name),
				})
			}
			n3, err := gnbN3Flag(gnbN3)
			if err != nil {
				return err
			}
			spec := engine.LoadSpec{
				GNBs: gnbs, BaseIMSI: baseIMSI, Count: count, MCC: id.MCC, MNC: id.MNC,
				Ki: kiB, OPc: opcB, Concurrency: conc, Rate: curve,
				Duration: duration, SampleEvery: sampleInterval,
			}
			if withPDU {
				spec.PDUSession = &ue.PDUSessionParams{PDUSessionID: 1, SST: uint8(id.SST), SD: id.SD, DNN: id.DNN}
				spec.GNBN3Addr = n3
			}
			fmt.Fprintf(cmd.OutOrStdout(), "network: %s\n", id)
			// Bad input is refused before the run (exit 3), not reported as
			// a run that did not complete.
			if err := spec.Validate(); err != nil {
				return err
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
				return exitWith(ExitDidNotComplete, err)
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
					return gateFailed("SLO breached")
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&amf, "amf", "", "AMF N2 address (host:port)")
	f.StringSliceVar(&binds, "bind", nil, bindFlagHelp+"; repeat or comma-separate for one per gNB (one value is shared by every gNB)")
	f.StringVar(&baseIMSI, "base-imsi", "", baseIMSIFlagHelp)
	f.IntVar(&count, "count", 100, "number of UEs to attach")
	f.StringVar(&ki, "ki", "", kiFlagHelp)
	f.StringVar(&opc, "opc", "", opcFlagHelp)
	addNetIdentityFlags(f, &id, withDNN())
	f.IntVar(&conc, "concurrency", 64, "max attaches in flight")
	f.Float64Var(&rate, "rate", 0, "offered attach rate (attaches/sec; 0 = concurrency-bound)")
	f.StringVar(&rampSpec, "ramp", "", "linear ramp start:end:seconds (overrides --rate)")
	f.StringVar(&arrival, "arrival", "constant", "arrival process for the offered rate: constant (evenly spaced) or poisson (exponential inter-arrivals)")
	f.IntVar(&gnbCount, "gnb-count", 1, "number of gNBs to mux UEs across")
	f.Uint32Var(&gnbID, "gnb-id", 1, "base gNB ID")
	f.Uint32Var(&gnbBits, "gnb-id-bits", 24, "gNB ID bit length")
	f.StringVar(&gnbName, "gnb-name", "orbit-gnb", "gNB name prefix")
	f.BoolVar(&withPDU, "pdu-session", false, "establish a PDU session per UE")
	f.StringVar(&gnbN3, "gnb-n3", "", "gNB N3 address (with --pdu-session)")
	f.Float64Var(&sloSuccess, "slo-min-success", 0, "SLO: minimum success rate (0-1); breach exits non-zero")
	f.DurationVar(&sloRegP99, "slo-reg-p99", 0, "SLO: max registration P99 (e.g. 500ms)")
	f.DurationVar(&sloAttachP99, "slo-attach-p99", 0, "SLO: max attach P99")
	f.DurationVar(&duration, "duration", 0, "soak: run for this long (e.g. 5m), cycling --count's SUPIs (attempts can outnumber --count)")
	f.DurationVar(&sampleInterval, "sample-interval", 0, "soak: resource-sample cadence (e.g. 10s)")
	f.DurationVar(&progressEvery, "progress-every", 5*time.Second, "print live progress to stderr at this cadence (0 = off)")
	tf.add(f, "run the storm in this process against the core, with no server: the capacity bench")
	f.StringArrayVar(&gates, "gate", nil, gateFlagHelp)
	// Ki/OPc and the PLMN are not marked required: they may come from the
	// environment ($ORBIT_KI/$ORBIT_OPC, $ORBIT_MCC/$ORBIT_MNC).
	for _, r := range []string{"amf", "base-imsi"} {
		_ = cmd.MarkFlagRequired(r)
	}
	return cmd
}

// sharedBindNote is the one-line stderr note for a load run whose gNBs all
// share one N2 source address: the AMF then tells them apart by gNB ID only.
// That is the case for a single --bind given for several gNBs, and for no
// --bind at all, since every unbound gNB dials from the same route-selected
// address with its own ephemeral port. Empty when there is nothing to say.
func sharedBindNote(binds []string, gnbCount int) string {
	if len(binds) > 1 || gnbCount < 2 {
		return ""
	}
	src := "the route-selected address"
	if len(binds) == 1 {
		src = binds[0]
	}
	return fmt.Sprintf("note: %d gNBs share one N2 source (%s); the AMF distinguishes them by gNB ID only (pass %d --bind addresses for one each)",
		gnbCount, src, gnbCount)
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

// printLoadReport writes the report. Every N2 source is one ip:port, bound
// or route-selected, so the sources are printed as the kernel reported them.
func printLoadReport(w io.Writer, rep load.Report) {
	if line := soakLine(uint32(rep.Attempted), uint32(rep.DistinctSUPIs)); line != "" {
		defer func() { _, _ = fmt.Fprintln(w, line) }()
	}
	fmt.Fprintf(w, "load: %d/%d attached in %s (%.1f attach/s)\n",
		rep.Succeeded, rep.Attempted, rep.Duration.Round(time.Millisecond), rep.AchievedRate)
	if len(rep.N2Sources) > 0 {
		_, _ = fmt.Fprintf(w, "  n2-source: %s\n", strings.Join(rep.N2Sources, " "))
	}
	for _, name := range []string{"registration", "pdu_session", "attach"} {
		s, ok := rep.Latencies[name]
		if !ok {
			continue
		}
		_, _ = fmt.Fprint(w, latencyLine(name, uint64(s.Count), round(s.P50), round(s.P90), round(s.P99), round(s.P999), round(s.Max)))
	}
	if len(rep.Resources) > 0 {
		first, last := rep.Resources[0], rep.Resources[len(rep.Resources)-1]
		fmt.Fprintf(w, "  resources: goroutines %d→%d, RSS %dMB→%dMB over %d samples\n",
			first.Goroutines, last.Goroutines, first.RSSBytes>>20, last.RSSBytes>>20, len(rep.Resources))
	}
}

// latencyLine is one procedure's line in a load report: its sample count and
// percentiles. printLoadReport and printLoadReportProto both print through
// it, so --local and server output compare line by line.
func latencyLine(name string, n uint64, p50, p90, p99, p999, peak string) string {
	return fmt.Sprintf("  %-13s n %-6d P50 %-8s P90 %-8s P99 %-8s P99.9 %-8s max %s\n", name, n, p50, p90, p99, p999, peak)
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

// thinLoad runs the storm the flags describe on the server: the flags'
// plan (thinPlan), one population named load, with --gate's gates beside
// the --slo-* ones. It prints the network, progress on stderr while it
// waits, and the report with its gates.
func thinLoad(cmd *cobra.Command, serverURL *string, o options, gates []string, detach bool, progressEvery time.Duration) error {
	var extra []target.Setting
	for _, g := range gates {
		s, err := plan.ParseGate(g)
		if err != nil {
			return exitWith(ExitCouldNotStart, err)
		}
		extra = append(extra, s...)
	}
	base := []target.Setting{{Path: "workloads[load].name", Value: "load", Source: target.CommandDefaults(cmd.CommandPath())}}
	req, err := thinPlan(cmd, o, base, extra)
	if err != nil {
		return exitWith(ExitCouldNotStart, err)
	}
	if err := applyServerURL(cmd, serverURL); err != nil {
		return err
	}
	var prev *orbitv1.LoadProgress
	return thinRun(cmd, *serverURL, req, detach, waitOpts{
		started: func() {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "network: %s\n", planNetwork(req.GetPlan().GetTarget()))
		},
		progressEvery: progressEvery,
		progress: func(g *orbitv1.GetRunResponse) {
			if p := g.GetLoadProgress(); p != nil {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), formatLoadProgressProto(p, prev))
				prev = p
			}
		},
		render: func(w io.Writer, r *orbitv1.GetRunReportResponse) {
			if l := runLoadReport(r); l != nil {
				printLoadReportProto(w, l)
			}
		},
	})
}

// planNetwork is a plan target's identity as the network line prints it.
func planNetwork(t *orbitv1.ResolvedTarget) string {
	core := t.GetCore()
	snssai := "1"
	if sl := core.GetSlices(); len(sl) > 0 {
		snssai = fmt.Sprint(sl[0].GetSst())
		if sl[0].GetSd() != "" {
			snssai += "/" + sl[0].GetSd()
		}
	}
	s := fmt.Sprintf("plmn=%s/%s tac=%d snssai=%s", core.GetPlmn().GetMcc(), core.GetPlmn().GetMnc(), core.GetTac(), snssai)
	if core.GetDnn() != "" {
		s += " dnn=" + core.GetDnn()
	}
	return s
}

// runLoadReport is a finished run's population report: the run's own, or
// its one workload's; nil when it has none.
func runLoadReport(r *orbitv1.GetRunReportResponse) *orbitv1.LoadReport {
	if l := r.GetLoad(); l != nil {
		return l
	}
	for _, w := range r.GetWorkloads() {
		if l := w.GetLoad(); l != nil {
			return l
		}
	}
	return nil
}

// printLoadReportProto is printLoadReport for a server run's report.
func printLoadReportProto(w io.Writer, rep *orbitv1.LoadReport) {
	_, _ = fmt.Fprintf(w, "load: %d/%d attached in %s (%.1f attach/s)\n", rep.GetSucceeded(), rep.GetAttempted(),
		(time.Duration(rep.GetDurationMs()) * time.Millisecond).String(), rep.GetAchievedRate())
	byName := map[string]*orbitv1.ProcedureLatency{}
	for _, l := range rep.GetLatency() {
		byName[l.GetProcedure()] = l
	}
	for _, name := range []string{"registration", "pdu_session", "attach"} {
		l, ok := byName[name]
		if !ok {
			continue
		}
		_, _ = fmt.Fprint(w, latencyLine(name, l.GetCount(), msDuration64(l.GetP50Ms()), msDuration64(l.GetP90Ms()),
			msDuration64(l.GetP99Ms()), msDuration64(l.GetP999Ms()), msDuration64(l.GetMaxMs())))
	}
	if rs := rep.GetResources(); len(rs) > 0 {
		first, last := rs[0], rs[len(rs)-1]
		_, _ = fmt.Fprintf(w, "  resources: goroutines %d→%d, RSS %dMB→%dMB over %d samples\n",
			first.GetGoroutines(), last.GetGoroutines(), first.GetRssBytes()>>20, last.GetRssBytes()>>20, len(rs))
	}
}

// msDuration64 is a latency in milliseconds as printLoadReport rounds it.
func msDuration64(ms float64) string {
	return round(time.Duration(ms * float64(time.Millisecond)))
}

// formatLoadProgressProto is formatLoadProgress for a server run's
// progress, the interval rate from the previous line's (nil: none).
func formatLoadProgressProto(s, prev *orbitv1.LoadProgress) string {
	var now float64
	if d := s.GetElapsedMs() - prev.GetElapsedMs(); d > 0 {
		now = float64(s.GetSucceeded()-prev.GetSucceeded()) / (float64(d) / 1000)
	}
	line := fmt.Sprintf("load: %s  %d ok / %d attempted  %d failed  %.1f attach/s (avg %.1f)",
		(time.Duration(s.GetElapsedMs()) * time.Millisecond).Round(time.Second), s.GetSucceeded(), s.GetAttempted(),
		s.GetFailed(), now, s.GetAchievedRate())
	for _, l := range s.GetLatency() {
		if l.GetProcedure() == "attach" {
			line += fmt.Sprintf("  attach P50 %s P99 %s", msDuration64(l.GetP50Ms()), msDuration64(l.GetP99Ms()))
		}
	}
	return line
}
