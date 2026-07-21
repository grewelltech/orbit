package cli

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/scenario"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// newRunCmd runs a declarative YAML scenario against the ORBIT API server. The
// step runner is an ordinary API client, so a scenario drives exactly the same
// operations as the individual `ue` commands, without the flag repetition.
// A `kind: fleet` file selects the population mode (ADR-0004) instead.
func newRunCmd(serverURL *string) *cobra.Command {
	var metricsListen string
	cmd := &cobra.Command{
		Use:   "run <scenario.yaml>",
		Short: "Run a declarative YAML scenario against the ORBIT API server",
		Long: "Run a declarative YAML scenario. A step scenario declares the core, gNBs,\n" +
			"and UEs, then an ordered `steps` list (register, ping, traffic, latency,\n" +
			"handover, deregister, wait). A `kind: fleet` scenario generates a topology and\n" +
			"a UE population running continuous behaviours. ${ENV} references expand from the\n" +
			"environment, so secrets like Ki/OPc stay out of the file. See docs/USAGE.md.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			kind, err := scenario.PeekKind(data)
			if err != nil {
				return err
			}
			if kind == "fleet" {
				return runFleet(cmd, data, metricsListen)
			}

			sc, err := scenario.Parse(data)
			if err != nil {
				return err
			}
			runner, err := scenario.NewRunner(sc, ueClient(serverURL), cmd.OutOrStdout())
			if err != nil {
				return err
			}
			return runner.Run(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&metricsListen, "metrics-listen", "",
		"serve Prometheus /metrics on this address during a fleet run (live orbit_fleet_app_* cohort gauges; empty = disabled)")
	return cmd
}

// runFleet validates a fleet scenario, prints the generated topology and
// population, and runs the attach phase (bringing up one association per gNB and
// attaching the fleet across them at the offered rate). Continuous mobility and
// traffic behaviours run on the attached fleet — added by later chunks.
func runFleet(cmd *cobra.Command, data []byte, metricsListen string) error {
	f, err := scenario.ParseFleet(data)
	if err != nil {
		return err
	}
	gnbs := f.GenGNBs()
	ues := f.GenFleet(gnbs)
	out := cmd.OutOrStdout()

	name := f.Name
	if name == "" {
		name = "fleet"
	}
	fmt.Fprintf(out, "▶ %s\n", name)
	fmt.Fprintf(out, "  core:     %s  PLMN %s/%s\n", f.Core.AMF, f.Core.PLMN.MCC, f.Core.PLMN.MNC)
	fmt.Fprintf(out, "  topology: %d gNBs (ids %d–%d), grid, source IPs %s\n",
		len(gnbs), gnbs[0].GNB.ID, gnbs[len(gnbs)-1].GNB.ID, ipSummary(f.Topology.GNBs.SourceIPs, len(gnbs)))
	fmt.Fprintf(out, "  fleet:    %d UEs (%s…), %s across gNBs",
		len(ues), ues[0].SUPI, distOr(f.Fleet.Distribution))
	if f.Fleet.AttachRate != "" {
		fmt.Fprintf(out, ", attach %s", f.Fleet.AttachRate)
	}
	if f.Fleet.PDUSession {
		fmt.Fprint(out, ", PDU sessions")
	}
	fmt.Fprintln(out)
	if m := f.Behaviors.Mobility; m != nil {
		fmt.Fprintf(out, "  mobility: %s @ %s, %s handover\n", m.Model, orDefault(m.Speed, "?"), orDefault(m.Handover, "xn"))
	}
	if t := f.Behaviors.Traffic; t != nil && len(t.Mix) > 0 {
		var parts []string
		for _, m := range t.Mix {
			var s string
			if m.App != "" {
				s = fmt.Sprintf("%.0f%% %s app → %s", m.Share*100, m.App, m.Peer)
			} else {
				s = fmt.Sprintf("%.0f%% %s", m.Share*100, m.Profile)
				if m.Rate != "" {
					s += " (" + m.Rate + ")"
				}
			}
			parts = append(parts, s)
		}
		fmt.Fprintf(out, "  traffic:  %s\n", strings.Join(parts, ", "))
	}
	if f.Run.Duration != "" {
		fmt.Fprintf(out, "  run:      %s\n", f.Run.Duration)
	}

	// Attach phase. The spec/behaviours build is shared with the RunService so
	// the CLI and server produce identical runs from the same scenario.
	ki, err := auth.ParseHexKey("Ki", f.Credentials.Ki)
	if err != nil {
		return err
	}
	opc, err := auth.ParseHexKey("OPc", f.Credentials.OPc)
	if err != nil {
		return err
	}
	spec, beh, err := scenario.BuildFleetRun(f, ki, opc)
	if err != nil {
		return err
	}
	fgnbs := spec.GNBs

	// Optional live Prometheus surface for the run: the per-cohort
	// orbit_fleet_app_* distribution gauges on /metrics.
	if metricsListen != "" && len(beh.Apps) > 0 {
		reg := observability.NewRegistry()
		beh.AppMetricsReg = reg
		msrv := &http.Server{
			Addr:              metricsListen,
			Handler:           promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := msrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(cmd.ErrOrStderr(), "metrics listener %s: %v\n", metricsListen, err)
			}
		}()
		defer msrv.Close()
		fmt.Fprintf(out, "  metrics:  http://%s/metrics\n", metricsListen)
	}

	fmt.Fprintf(out, "\nattaching %d UEs across %d gNBs", spec.Count, len(fgnbs))
	if beh.Duration > 0 {
		fmt.Fprintf(out, ", then running behaviours for %s", beh.Duration)
	}
	fmt.Fprint(out, "…\n\n")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rep, err := engine.RunFleet(cmd.Context(), log, spec, beh)
	if err != nil {
		return err
	}
	printFleetReport(out, rep)
	return nil
}

func printFleetReport(out io.Writer, r engine.FleetReport) {
	fmt.Fprintf(out, "attach:       %d/%d in %s\n", r.Attached, r.Attached+r.AttachFailed,
		r.AttachElapsed.Round(time.Millisecond))
	if r.Handovers+r.HandoverErr > 0 {
		fmt.Fprintf(out, "handovers:    %d ok, %d failed\n", r.Handovers, r.HandoverErr)
	}
	if r.TrafficFlows > 0 {
		fmt.Fprintf(out, "traffic:      %d flow(s) (per UE, shared N3 socket per gNB), %.1f MB total\n",
			r.TrafficFlows, float64(r.TrafficBytes)/1e6)
	}
	for _, c := range r.AppCohorts {
		line := fmt.Sprintf("app %-10s %s: %d UE(s), %d server(s)", c.Name, c.App, c.UEs, c.Servers)
		if c.Failed > 0 {
			line += fmt.Sprintf(", %d failed", c.Failed)
		}
		if c.Err != "" {
			fmt.Fprintf(out, "%s — %s\n", line, c.Err)
			continue
		}
		if q := c.MOS; q != nil {
			line += fmt.Sprintf(", MOS p5/p50/p95 %.2f/%.2f/%.2f", q.P5, q.P50, q.P95)
		}
		if q := c.TTFBMs; q != nil {
			// Across-member quantiles of each member's MEDIAN TTFB
			// (FleetAppCohortReport.TTFBMs) — label it so, or the p95 here
			// reads as a request-level tail like the single-UE ttfb-p95
			// (the 95th-percentile UE's median sits far below that).
			line += fmt.Sprintf(", TTFB per-UE-median p5/p50/p95 %.1f/%.1f/%.1f ms", q.P5, q.P50, q.P95)
		}
		if q := c.GoodputMbps; q != nil {
			line += fmt.Sprintf(", goodput %.2f/%.2f/%.2f Mbps", q.P5, q.P50, q.P95)
		}
		if q := c.StallTimeMs; q != nil {
			line += fmt.Sprintf(", stall %.0f/%.0f/%.0f ms", q.P5, q.P50, q.P95)
		}
		if q := c.RebufferRatio; q != nil {
			line += fmt.Sprintf(", rebuffer %.3f/%.3f/%.3f", q.P5, q.P50, q.P95)
		}
		fmt.Fprintln(out, line)
	}
	fmt.Fprintf(out, "deregistered: %d\n", r.Deregistered)
}

func ipSummary(ips []string, n int) string {
	if len(ips) == 0 {
		return "(none)"
	}
	if n <= 2 || len(ips) <= 2 {
		return strings.Join(ips[:min(n, len(ips))], ", ")
	}
	return fmt.Sprintf("%s … %s", ips[0], ips[n-1])
}

func distOr(d string) string {
	if d == "" {
		return "even"
	}
	return d
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
