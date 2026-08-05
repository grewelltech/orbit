package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// resolveCreds returns validated Ki/OPc, falling back to $ORBIT_KI/$ORBIT_OPC,
// so every run-starting subcommand acquires and validates credentials the same
// way — and fails locally with a clear message rather than sending empty or
// malformed keys the server rejects opaquely.
func resolveCreds(ki, opc string) (string, string, error) {
	if ki == "" {
		ki = os.Getenv("ORBIT_KI")
	}
	if opc == "" {
		opc = os.Getenv("ORBIT_OPC")
	}
	if ki == "" || opc == "" {
		return "", "", fmt.Errorf("Ki and OPc are required (--ki/--opc or $ORBIT_KI/$ORBIT_OPC)")
	}
	if _, err := auth.ParseHexKey("Ki", ki); err != nil {
		return "", "", err
	}
	if _, err := auth.ParseHexKey("OPc", opc); err != nil {
		return "", "", err
	}
	return ki, opc, nil
}

// msDuration converts a duration to milliseconds for a uint32 proto field,
// rejecting values that would overflow (~49.7 days) rather than wrapping to a
// tiny soak that ends immediately.
func msDuration(name string, d time.Duration) (uint32, error) {
	ms := d.Milliseconds()
	if ms < 0 || ms > math.MaxUint32 {
		return 0, fmt.Errorf("%s %s is out of range (max ~49 days)", name, d)
	}
	return uint32(ms), nil
}

// runsClient builds a RunService client against the API server.
func runsClient(url *string) orbitv1connect.RunServiceClient {
	return orbitv1connect.NewRunServiceClient(http.DefaultClient, *url)
}

// newRunsCmd manages server-side runs (ADR-0005): the CLI is a client of the
// RunService, not an in-process orchestrator. A run started here executes on
// the server and outlives this command.
func newRunsCmd(serverURL *string) *cobra.Command {
	cmd := &cobra.Command{Use: "runs", Short: "Start and observe server-side test runs"}
	cmd.AddCommand(newRunsListCmd(serverURL))
	cmd.AddCommand(newRunsGetCmd(serverURL))
	cmd.AddCommand(newRunsStopCmd(serverURL))
	cmd.AddCommand(newRunsWatchCmd(serverURL))
	cmd.AddCommand(newRunsEventsCmd(serverURL))
	cmd.AddCommand(newRunsStartLoadCmd(serverURL))
	cmd.AddCommand(newRunsStartFleetCmd(serverURL))
	return cmd
}

func newRunsListCmd(serverURL *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List active and recent runs (newest first)",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := runsClient(serverURL).ListRuns(cmd.Context(), connect.NewRequest(&orbitv1.ListRunsRequest{}))
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "RUN-ID\tKIND\tSTATE\tNAME\tAGE")
			for _, r := range res.Msg.GetRuns() {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					r.GetRunId(), runKindLabel(r.GetKind()), runStateLabel(r.GetState()),
					orDash(r.GetName()), runAge(r))
			}
			return w.Flush()
		},
	}
}

func newRunsGetCmd(serverURL *string) *cobra.Command {
	return &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show a run's state, plus live progress while it is running",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := runsClient(serverURL).GetRun(cmd.Context(),
				connect.NewRequest(&orbitv1.GetRunRequest{RunId: args[0]}))
			if err != nil {
				return err
			}
			r := res.Msg.GetRun()
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s  %s  %s\n", r.GetRunId(), runKindLabel(r.GetKind()), runStateLabel(r.GetState()))
			if r.GetError() != "" {
				fmt.Fprintf(out, "  error: %s\n", r.GetError())
			}
			if p := res.Msg.GetLoadProgress(); p != nil {
				fmt.Fprintf(out, "  %d ok / %d attempted, %d failed  %.1f attach/s\n",
					p.GetSucceeded(), p.GetAttempted(), p.GetFailed(), p.GetAchievedRate())
				printProcedureLatency(out, p.GetLatency())
			}
			if p := res.Msg.GetFleetProgress(); p != nil {
				printFleetProgress(out, p, false)
			}
			return nil
		},
	}
}

func newRunsStopCmd(serverURL *string) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <run-id>",
		Short: "Request cancellation of a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := runsClient(serverURL).StopRun(cmd.Context(),
				connect.NewRequest(&orbitv1.StopRunRequest{RunId: args[0]}))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", res.Msg.GetRun().GetRunId(), runStateLabel(res.Msg.GetRun().GetState()))
			return nil
		},
	}
}

// newRunsWatchCmd tails a run's live telemetry to the terminal — the CLI
// equivalent of the dashboard's live view.
func newRunsWatchCmd(serverURL *string) *cobra.Command {
	return &cobra.Command{
		Use:   "watch [run-id]",
		Short: "Tail a run's live progress (default: the active run)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := runsClient(serverURL)
			runID := ""
			if len(args) == 1 {
				runID = args[0]
			} else {
				id, err := activeRunID(cmd.Context(), c)
				if err != nil {
					return err
				}
				runID = id
			}

			stream, err := c.RunTelemetry(cmd.Context(),
				connect.NewRequest(&orbitv1.RunTelemetryRequest{RunId: runID, IntervalMs: 1000}))
			if err != nil {
				return err
			}
			defer stream.Close()
			out := cmd.OutOrStdout()
			for stream.Receive() {
				f := stream.Msg()
				elapsed := time.Duration(f.GetElapsedMs()) * time.Millisecond
				line := fmt.Sprintf("%s  %s", elapsed.Round(time.Second), runStateLabel(f.GetState()))
				if p := f.GetLoad(); p != nil {
					line += fmt.Sprintf("  %d ok / %d attempted  %d failed  %.1f attach/s",
						p.GetSucceeded(), p.GetAttempted(), p.GetFailed(), p.GetAchievedRate())
				}
				if p := f.GetFleet(); p != nil {
					line += fmt.Sprintf("  %d attached  ul %s  dl %s",
						p.GetAttached(), bitsPerSec(p.GetUplinkBps()), bitsPerSec(p.GetDownlinkBps()))
					if l := p.GetUpLatency(); l != nil {
						line += fmt.Sprintf("  up-rtt p50 %.2fms p99 %.2fms", l.GetP50Ms(), l.GetP99Ms())
						if lost := p.GetUpProbesLost(); lost > 0 {
							line += fmt.Sprintf(" (%d/%d lost)", lost, p.GetUpProbes())
						}
					}
					if p.GetHandovers() > 0 || p.GetHandoverErrors() > 0 {
						line += fmt.Sprintf("  ho %d/%d err", p.GetHandovers(), p.GetHandoverErrors())
					}
					for _, c := range p.GetCohorts() {
						if m := c.GetMos(); m != nil {
							line += fmt.Sprintf("  %s MOS %.2f", c.GetName(), m.GetP50())
						} else if g := c.GetGoodputMbps(); g != nil {
							line += fmt.Sprintf("  %s %.1f Mbps", c.GetName(), g.GetP50())
						}
					}
					if n := p.GetFlowsTotal(); n > 0 {
						line += fmt.Sprintf("  %d flows", n)
						// The busiest flow, so a per-UE figure is visible
						// without opening the dashboard.
						if f := p.GetFlows(); len(f) > 0 {
							line += fmt.Sprintf(" (top %s ul %s dl %s)", f[0].GetSupi(),
								bitsPerSec(f[0].GetUplinkBps()), bitsPerSec(f[0].GetDownlinkBps()))
						}
					}
				}
				fmt.Fprintln(out, line)
			}
			return stream.Err()
		},
	}
}

// eventsFlagHelp documents the --events flag on every run-starting command.
const eventsFlagHelp = "event detail: \"normal\" (failures, mobility, traffic and phase milestones) " +
	"or \"verbose\" (adds every UE lifecycle transition; use on small runs)"

// eventVerbosityFlag maps the --events flag onto the wire enum, refusing an
// unknown value rather than silently defaulting: an operator who asked for
// detail and got the default would read the resulting quiet stream as
// "nothing happened".
func eventVerbosityFlag(s string) (orbitv1.EventVerbosity, error) {
	switch s {
	case "", "normal":
		return orbitv1.EventVerbosity_EVENT_VERBOSITY_NORMAL, nil
	case "verbose":
		return orbitv1.EventVerbosity_EVENT_VERBOSITY_VERBOSE, nil
	default:
		return 0, fmt.Errorf("unknown --events %q (want \"normal\" or \"verbose\")", s)
	}
}

// newRunsEventsCmd tails a run's discrete events — the CLI view of what the
// dashboard's event stream shows.
func newRunsEventsCmd(serverURL *string) *cobra.Command {
	var fromSeq uint64
	cmd := &cobra.Command{
		Use:   "events [run-id]",
		Short: "Tail a run's event stream (default: the active run)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := runsClient(serverURL)
			runID := ""
			if len(args) == 1 {
				runID = args[0]
			} else {
				id, err := activeRunID(cmd.Context(), c)
				if err != nil {
					return err
				}
				runID = id
			}
			stream, err := c.RunEvents(cmd.Context(),
				connect.NewRequest(&orbitv1.RunEventsRequest{RunId: runID, FromSeq: fromSeq}))
			if err != nil {
				return err
			}
			defer stream.Close()
			out := cmd.OutOrStdout()
			first := true
			for stream.Receive() {
				ev := stream.Msg()
				// The ring is bounded, so say plainly when the resume point
				// pointed into evicted history rather than showing a silent gap.
				if first {
					if d := ev.GetDroppedBefore(); d > 0 {
						fmt.Fprintf(out, "(%d earlier events already evicted from the ring)\n", d)
					}
					first = false
				}
				fmt.Fprintf(out, "%s  %-5s %-8s %s%s\n",
					time.Unix(0, ev.GetUnixNano()).Format("15:04:05.000"),
					severityLabel(ev.GetSeverity()), ev.GetKind(),
					supiPrefix(ev.GetSupi()), ev.GetMessage())
			}
			return stream.Err()
		},
	}
	cmd.Flags().Uint64Var(&fromSeq, "from-seq", 0, "resume from this sequence number (0 = oldest retained)")
	return cmd
}

// severityLabel renders the wire severity for a fixed-width column.
func severityLabel(s orbitv1.EventSeverity) string {
	switch s {
	case orbitv1.EventSeverity_EVENT_SEVERITY_WARN:
		return "WARN"
	case orbitv1.EventSeverity_EVENT_SEVERITY_ERROR:
		return "ERROR"
	default:
		return "INFO"
	}
}

// supiPrefix renders a run-scoped event's empty SUPI as nothing rather than a
// stray separator.
func supiPrefix(supi string) string {
	if supi == "" {
		return ""
	}
	return supi + "  "
}

// printFleetProgress renders a fleet run's live aggregates. Rates print only
// when a stream derived them (a single-shot Get cannot), so a zero there is
// never mistaken for an idle data path.
func printFleetProgress(out io.Writer, p *orbitv1.FleetProgress, rates bool) {
	fmt.Fprintf(out, "  %d attached, %d failed", p.GetAttached(), p.GetAttachFailed())
	if p.GetTrafficFlows() > 0 {
		fmt.Fprintf(out, "  %d flows", p.GetTrafficFlows())
	}
	if p.GetAppSessions() > 0 {
		fmt.Fprintf(out, "  %d app", p.GetAppSessions())
	}
	if p.GetHandovers() > 0 || p.GetHandoverErrors() > 0 {
		fmt.Fprintf(out, "  %d handovers (%d failed)", p.GetHandovers(), p.GetHandoverErrors())
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  N3 total    ul %s / %d pkts   dl %s / %d pkts\n",
		byteCount(p.GetUplinkBytes()), p.GetUplinkPackets(),
		byteCount(p.GetDownlinkBytes()), p.GetDownlinkPackets())
	if rates {
		fmt.Fprintf(out, "  N3 rate     ul %s   dl %s\n",
			bitsPerSec(p.GetUplinkBps()), bitsPerSec(p.GetDownlinkBps()))
	}
	// Control-plane procedures: attach-phase ones during the attach, handovers
	// for the rest of a mobility run — so this is not blank once the fleet is up.
	printProcedureLatency(out, p.GetLatency())
	if l := p.GetUpLatency(); l != nil {
		fmt.Fprintf(out, "  UP RTT      p50 %.2f  p90 %.2f  p99 %.2f  max %.2f ms  (%d probes",
			l.GetP50Ms(), l.GetP90Ms(), l.GetP99Ms(), l.GetMaxMs(), p.GetUpProbes())
		if lost := p.GetUpProbesLost(); lost > 0 {
			fmt.Fprintf(out, ", %d lost", lost)
		}
		fmt.Fprintln(out, ")")
	}
	for _, c := range p.GetCohorts() {
		failed := ""
		if f := c.GetFailed(); f > 0 {
			failed = fmt.Sprintf(" (%d FAILED)", f)
		}
		fmt.Fprintf(out, "  %-12s %-6s %d UEs%s %s%s\n", c.GetName(), c.GetApp(), c.GetUes(), failed,
			(time.Duration(c.GetElapsedMs()) * time.Millisecond).Round(time.Second), cohortQualityLine(c))
		printFarEnd(out, c.GetFarEnd())
	}
	if n := p.GetFlowsTotal(); n > 0 {
		fmt.Fprintf(out, "  flows       %d carrying traffic", n)
		if shown := p.GetFlowsReported(); shown < n {
			fmt.Fprintf(out, " (showing the %d busiest)", shown)
		}
		fmt.Fprintln(out)
		for _, fl := range p.GetFlows() {
			fmt.Fprintf(out, "    %-18s %-6s %-10s ul %s  dl %s\n",
				fl.GetSupi(), fl.GetApp(), orDash(fl.GetCohort()),
				bitsPerSec(fl.GetUplinkBps()), bitsPerSec(fl.GetDownlinkBps()))
		}
	}
	for _, g := range p.GetPerGnb() {
		fmt.Fprintf(out, "  %-16s %d attached\n", g.GetGnb(), g.GetSucceeded())
	}
}

// printFarEnd renders the N6 agent's independent view, or why there isn't one.
// The two figures are deliberately not divided into a ratio: N3 counts
// encapsulated inner-IP bytes and this counts application payload, so a
// quotient would read ~1.04 with nothing wrong. It is here to be COMPARED by
// eye — a far end reporting far less than the tunnel carried, or nothing at
// all, is the signal.
func printFarEnd(out io.Writer, fe *orbitv1.FarEndView) {
	if fe == nil {
		return
	}
	if !fe.GetAvailable() {
		if r := fe.GetReason(); r != "" {
			fmt.Fprintf(out, "               N6 far end: %s\n", r)
		}
		return
	}
	line := fmt.Sprintf("               N6 far end: %s, %s (app payload)",
		bitsPerSec(fe.GetBitsPerSec()), byteCountCLI(fe.GetBytes()))
	if r := fe.GetRequests(); r > 0 {
		line += fmt.Sprintf("  %d requests", r)
		if e := fe.GetErrors(); e > 0 {
			line += fmt.Sprintf(", %d errors", e)
		}
	}
	fmt.Fprintln(out, line)
}

// byteCountCLI renders a byte total for a report line.
func byteCountCLI(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// cohortQualityLine renders whichever quality families the cohort's app
// produced. An absent family prints nothing rather than a zero.
func cohortQualityLine(c *orbitv1.CohortProgress) string {
	var b strings.Builder
	q := func(label string, v *orbitv1.Quantiles, unit string, prec int) {
		if v == nil {
			return
		}
		fmt.Fprintf(&b, "  %s p5/p50/p95 %.*f/%.*f/%.*f%s", label,
			prec, v.GetP5(), prec, v.GetP50(), prec, v.GetP95(), unit)
	}
	q("MOS", c.GetMos(), "", 2)
	q("TTFB", c.GetTtfbMs(), "ms", 1)
	q("goodput", c.GetGoodputMbps(), "Mbps", 2)
	q("bitrate", c.GetBitrateKbps(), "kbps", 0)
	q("stall", c.GetStallTimeMs(), "ms", 0)
	q("rebuffer", c.GetRebufferRatio(), "", 3)
	q("startup", c.GetStartupMs(), "ms", 0)
	return b.String()
}

// bitsPerSec renders a bit rate in the unit that keeps it readable.
func bitsPerSec(bps float64) string {
	switch {
	case bps >= 1e9:
		return fmt.Sprintf("%.2f Gbps", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.2f Mbps", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.1f kbps", bps/1e3)
	default:
		return fmt.Sprintf("%.0f bps", bps)
	}
}

// byteCount renders a cumulative byte total in the unit that keeps it readable.
func byteCount(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// activeRunID returns the id of the newest active run, or an error if none.
func activeRunID(ctx context.Context, c orbitv1connect.RunServiceClient) (string, error) {
	res, err := c.ListRuns(ctx, connect.NewRequest(&orbitv1.ListRunsRequest{}))
	if err != nil {
		return "", err
	}
	for _, r := range res.Msg.GetRuns() {
		if r.GetState() == orbitv1.RunState_RUN_STATE_RUNNING || r.GetState() == orbitv1.RunState_RUN_STATE_PENDING {
			return r.GetRunId(), nil
		}
	}
	return "", fmt.Errorf("no active run; pass a run id (see `orbit runs list`)")
}

func newRunsStartLoadCmd(serverURL *string) *cobra.Command {
	var (
		name, amf, baseIMSI, ki, opc, mcc, mnc string
		events                                 string
		gnbName, sd, gnbN3, dnn                string
		count, gnbCount                        int
		gnbID, gnbBits, tac, sst               uint32
		rate                                   float64
		ramp                                   string
		concurrency                            uint32
		duration, sampleInterval               time.Duration
		withPDU                                bool
	)
	cmd := &cobra.Command{
		Use:   "start-load",
		Short: "Start a rate-controlled attach storm on the server",
		RunE: func(cmd *cobra.Command, args []string) error {
			ki, opc, err := resolveCreds(ki, opc)
			if err != nil {
				return err
			}
			durMs, err := msDuration("--duration", duration)
			if err != nil {
				return err
			}
			sampleMs, err := msDuration("--sample-interval", sampleInterval)
			if err != nil {
				return err
			}
			var gnbs []*orbitv1.GnbConfig
			if gnbCount < 1 {
				gnbCount = 1
			}
			for n := 0; n < gnbCount; n++ {
				gnbs = append(gnbs, &orbitv1.GnbConfig{
					Id: gnbID + uint32(n), IdBits: gnbBits, Name: fmt.Sprintf("%s-%d", gnbName, n),
					Mcc: mcc, Mnc: mnc, Tac: tac,
					Slices: []*orbitv1.Snssai{{Sst: sst, Sd: sd}},
				})
			}
			spec := &orbitv1.LoadRunSpec{
				AmfAddress: amf, Gnbs: gnbs, BaseImsi: baseIMSI, Count: uint32(count),
				Credentials: &orbitv1.Credentials{Ki: ki, Opc: opc},
				Rate:        rate, Concurrency: concurrency,
				DurationMs: durMs, SampleIntervalMs: sampleMs,
			}
			// Same ramp syntax as `orbit load`: start:end:seconds.
			if ramp != "" {
				start, end, secs, err := parseRampSpec(ramp)
				if err != nil {
					return err
				}
				spec.RampStart, spec.RampEnd, spec.RampSeconds = start, end, uint32(secs)
			}
			if withPDU {
				spec.PduSession = &orbitv1.PDUSession{PduSessionId: 1, Sst: sst, Sd: sd, Dnn: dnn}
				spec.GnbN3Addr = gnbN3
			}
			verbosity, err := eventVerbosityFlag(events)
			if err != nil {
				return err
			}
			res, err := runsClient(serverURL).StartRun(cmd.Context(), connect.NewRequest(&orbitv1.StartRunRequest{
				Name:           name,
				Spec:           &orbitv1.StartRunRequest_Load{Load: spec},
				EventVerbosity: verbosity,
			}))
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.Msg.GetRun().GetRunId())
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "run label")
	f.StringVar(&events, "events", "normal", eventsFlagHelp)
	f.StringVar(&amf, "amf", "", "AMF N2 address (host:port)")
	f.StringVar(&baseIMSI, "base-imsi", "", "first SUPI/IMSI; each UE increments it")
	f.IntVar(&count, "count", 100, "number of UEs to attach")
	f.StringVar(&ki, "ki", "", "subscriber key Ki (32 hex; default $ORBIT_KI)")
	f.StringVar(&opc, "opc", "", "operator key OPc (32 hex; default $ORBIT_OPC)")
	f.StringVar(&mcc, "mcc", "", "MCC")
	f.StringVar(&mnc, "mnc", "", "MNC")
	f.Uint32Var(&concurrency, "concurrency", 64, "max attaches in flight")
	f.Float64Var(&rate, "rate", 0, "offered attach rate (attaches/sec; 0 = concurrency-bound)")
	f.StringVar(&ramp, "ramp", "", "linear ramp start:end:seconds (overrides --rate)")
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
	f.DurationVar(&duration, "duration", 0, "soak: run for this long instead of --count")
	f.DurationVar(&sampleInterval, "sample-interval", 0, "soak: resource-sample cadence")
	// Ki/OPc are not marked required: they may come from the environment.
	for _, r := range []string{"amf", "base-imsi", "mcc", "mnc"} {
		_ = cmd.MarkFlagRequired(r)
	}
	return cmd
}

func newRunsStartFleetCmd(serverURL *string) *cobra.Command {
	var name, ki, opc, events string
	cmd := &cobra.Command{
		Use:   "start-fleet <scenario.yaml>",
		Short: "Start a fleet run on the server from a fleet scenario file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			// Credentials come from flags or $ORBIT_KI/$ORBIT_OPC, validated
			// locally; never the scenario's ${ENV}, which the server does not
			// expand (secret safety).
			ki, opc, err := resolveCreds(ki, opc)
			if err != nil {
				return err
			}
			verbosity, err := eventVerbosityFlag(events)
			if err != nil {
				return err
			}
			res, err := runsClient(serverURL).StartRun(cmd.Context(), connect.NewRequest(&orbitv1.StartRunRequest{
				Name: name,
				Spec: &orbitv1.StartRunRequest_Fleet{Fleet: &orbitv1.FleetRunSpec{
					ScenarioYaml: string(data),
					Credentials:  &orbitv1.Credentials{Ki: ki, Opc: opc},
				}},
				EventVerbosity: verbosity,
			}))
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.Msg.GetRun().GetRunId())
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "run label")
	cmd.Flags().StringVar(&events, "events", "normal", eventsFlagHelp)
	cmd.Flags().StringVar(&ki, "ki", "", "subscriber key Ki (32 hex; default $ORBIT_KI)")
	cmd.Flags().StringVar(&opc, "opc", "", "operator key OPc (32 hex; default $ORBIT_OPC)")
	return cmd
}

// --- rendering helpers ---

func runKindLabel(k orbitv1.RunKind) string {
	switch k {
	case orbitv1.RunKind_RUN_KIND_LOAD:
		return "load"
	case orbitv1.RunKind_RUN_KIND_FLEET:
		return "fleet"
	default:
		return "?"
	}
}

func runStateLabel(s orbitv1.RunState) string {
	name := orbitv1.RunState_name[int32(s)]
	if name == "" {
		return "UNSPECIFIED"
	}
	// Strip the RUN_STATE_ prefix for readability.
	return strings.TrimPrefix(name, "RUN_STATE_")
}

func runAge(r *orbitv1.Run) string {
	if r.GetStartedUnixNano() == 0 {
		return "-"
	}
	start := time.Unix(0, r.GetStartedUnixNano())
	end := time.Now()
	if r.GetEndedUnixNano() != 0 {
		end = time.Unix(0, r.GetEndedUnixNano())
	}
	return end.Sub(start).Round(time.Second).String()
}

func printProcedureLatency(out io.Writer, lats []*orbitv1.ProcedureLatency) {
	for _, l := range lats {
		fmt.Fprintf(out, "  %-13s P50 %.1f  P99 %.1f  max %.1f ms\n",
			l.GetProcedure(), l.GetP50Ms(), l.GetP99Ms(), l.GetMaxMs())
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
