package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/scenario"
	"github.com/bgrewell/orbit/internal/target"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// newRunCmd runs a declarative YAML scenario against the ORBIT API server. The
// step runner is an ordinary API client, so a scenario drives exactly the same
// operations as the individual `ue` commands, without the flag repetition.
// A `kind: fleet` file selects the population mode (ADR-0004) instead.
func newRunCmd(serverURL *string, o options) *cobra.Command {
	var (
		metricsListen string
		pf            planFlags
		detach, local bool
	)
	cmd := &cobra.Command{
		Use:   "run <plan-or-scenario.yaml>",
		Short: "Run a run file (kind: run) or a declarative YAML scenario against the ORBIT API server",
		Long: "Run a declarative YAML scenario. A step scenario declares the core, gNBs,\n" +
			"and UEs, then an ordered `steps` list (register, ping, traffic, latency,\n" +
			"handover, deregister, wait). A `kind: fleet` scenario generates a topology and\n" +
			"a UE population running continuous behaviours, on the server: orbit run waits\n" +
			"for its end and exits with its result (--detach returns at start; --local,\n" +
			"deprecated, runs it in this process). The core, keys and addresses\n" +
			"come from a target: -t, $ORBIT_TARGET, or the file's target: (docs/targets.md);\n" +
			"a file may instead set its own core/credentials. ${ENV} references expand from\n" +
			"the environment.\n\n" +
			"A `kind: run` file is a run plan (docs/run-files.md): its workloads, gates\n" +
			"and stop condition go to the server, and orbit run waits for the run's end\n" +
			"and exits with its result (0 passed, 1 a gate failed, 2 did not complete,\n" +
			"3 could not start); --detach starts it and returns. --gate and --set add to\n" +
			"or override the file's fields, --explain shows the plan without starting it.\n" +
			"See docs/USAGE.md.",
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
			if kind == "run" {
				if local {
					return errors.New("--local applies to a fleet scenario; a run file runs on the server")
				}
				return runPlanFile(cmd, serverURL, o, args[0], data, pf, detach)
			}
			refused := []string{"set"} // a run file's
			if kind != "fleet" {
				refused = append(refused, "gate", "slo-reg-p99", "slo-attach-p99", "slo-min-success", "detach", "local")
			} else if local {
				refused = append(refused, "gate", "slo-reg-p99", "slo-attach-p99", "slo-min-success", "detach")
			}
			with := ""
			if local {
				with = " with --local"
			}
			for _, f := range refused {
				if cmd.Flags().Changed(f) {
					what := "a run file (kind: run)"
					if f != "set" {
						what = "a run file or a fleet scenario on the server"
					}
					return fmt.Errorf("--%s applies to %s, not a %s scenario%s", f, what, orDefault(kind, "step"), with)
				}
			}
			if cmd.Flags().Changed("metrics-listen") && !local {
				return errors.New("--metrics-listen serves an in-process fleet's cohort gauges and needs --local; a fleet on the server does not serve them yet")
			}
			t, err := scenarioTarget(cmd, args[0], data, o)
			if err != nil {
				return err
			}
			if kind == "fleet" && local {
				// The in-process fleet drives the engine here and never
				// builds a client, so it must not read or report
				// --server/$ORBIT_SERVER.
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), localFleetDeprecated)
				return runFleet(cmd, data, metricsListen, t)
			}
			if kind == "fleet" {
				return thinFleet(cmd, serverURL, data, t, pf.gateFlags, detach)
			}
			if err := applyServerURL(cmd, serverURL); err != nil {
				return err
			}

			sc, err := scenario.ParseWithTarget(data, t)
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
	consumesTarget(cmd)
	cmd.Flags().StringVar(&metricsListen, "metrics-listen", "",
		"with --local: serve Prometheus /metrics on this address during the fleet run (live orbit_fleet_app_* cohort gauges; empty = disabled)")
	pf.add(cmd.Flags())
	cmd.Flags().BoolVar(&detach, "detach", false, "run file or fleet: "+detachHelp)
	cmd.Flags().BoolVar(&local, "local", false, "fleet: run it in this process, not on the server (deprecated: goes after this release)")
	return cmd
}

// runFleet validates a fleet scenario, prints the generated topology and
// population, and runs the attach phase (bringing up one association per gNB and
// attaching the fleet across them at the offered rate). Continuous mobility and
// traffic behaviours run on the attached fleet — added by later chunks.
func runFleet(cmd *cobra.Command, data []byte, metricsListen string, t *target.Target) error {
	f, err := scenario.ParseFleetWithTarget(data, t)
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
	if t := f.Behaviors.Traffic; t != nil {
		var parts []string
		if b := t.Bulk; b != nil {
			s := fmt.Sprintf("%d bulk UEs → %s", b.UEs, b.Peer)
			if b.UplinkRate != "" {
				s += " UL " + b.UplinkRate + "/UE"
			}
			if b.DownlinkRate != "" {
				s += " DL " + b.DownlinkRate + "/UE"
			}
			parts = append(parts, s)
		}
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
		if len(parts) > 0 {
			fmt.Fprintf(out, "  traffic:  %s\n", strings.Join(parts, ", "))
		}
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
	// Zero deps: this is the local, in-process path — it prints the final
	// report, has no telemetry or event subscriber, and owns its own N3
	// sockets because there is no server Manager to share them with.
	rep, err := engine.RunFleet(cmd.Context(), log, spec, beh, engine.FleetDeps{})
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

// thinFleet runs a fleet scenario on the server, as start-fleet sends it
// (fleetRequest) with the gate flags' gates, waiting for its end unless
// detach and printing the fleet's report.
func thinFleet(cmd *cobra.Command, serverURL *string, data []byte, t *target.Target, gf gateFlags, detach bool) error {
	gates, err := gf.gates(cmd)
	if err != nil {
		return err
	}
	req, err := fleetRequest(data, t, "", "", "", "normal", gates)
	if err != nil {
		return exitWith(ExitCouldNotStart, err)
	}
	if err := applyServerURL(cmd, serverURL); err != nil {
		return err
	}
	return thinRun(cmd, *serverURL, req, detach, waitOpts{
		render: func(w io.Writer, r *orbitv1.GetRunReportResponse) {
			if f := runFleetReport(r); f != nil {
				printFleetRunReport(w, f)
			}
		},
	})
}

// runFleetReport is a finished run's fleet report: the run's own, or its
// one workload's; nil when it has none.
func runFleetReport(r *orbitv1.GetRunReportResponse) *orbitv1.FleetReport {
	if f := r.GetFleet(); f != nil {
		return f
	}
	for _, w := range r.GetWorkloads() {
		if f := w.GetFleet(); f != nil {
			return f
		}
	}
	return nil
}
