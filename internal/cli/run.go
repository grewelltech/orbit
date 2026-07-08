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
	"github.com/bgrewell/orbit/internal/scenario"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// newRunCmd runs a declarative YAML scenario against the ORBIT API server. The
// step runner is an ordinary API client, so a scenario drives exactly the same
// operations as the individual `ue` commands, without the flag repetition.
// A `kind: fleet` file selects the population mode (ADR-0004) instead.
func newRunCmd(serverURL *string) *cobra.Command {
	return &cobra.Command{
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
				return runFleet(cmd, data)
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
}

// runFleet validates a fleet scenario, prints the generated topology and
// population, and runs the attach phase (bringing up one association per gNB and
// attaching the fleet across them at the offered rate). Continuous mobility and
// traffic behaviours run on the attached fleet — added by later chunks.
func runFleet(cmd *cobra.Command, data []byte) error {
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
			s := fmt.Sprintf("%.0f%% %s", m.Share*100, m.Profile)
			if m.Rate != "" {
				s += " (" + m.Rate + ")"
			}
			parts = append(parts, s)
		}
		fmt.Fprintf(out, "  traffic:  %s\n", strings.Join(parts, ", "))
	}
	if f.Run.Duration != "" {
		fmt.Fprintf(out, "  run:      %s\n", f.Run.Duration)
	}

	// Attach phase.
	ki, err := auth.ParseHexKey("Ki", f.Credentials.Ki)
	if err != nil {
		return err
	}
	opc, err := auth.ParseHexKey("OPc", f.Credentials.OPc)
	if err != nil {
		return err
	}
	rateRPS, err := parseAttachRate(f.Fleet.AttachRate)
	if err != nil {
		return err
	}

	fgnbs := make([]engine.FleetGNB, len(gnbs))
	for i, pg := range gnbs {
		fgnbs[i] = engine.FleetGNB{
			Config: gnb.Config{
				ID: pg.GNB.ID, Name: pg.GNB.Name,
				MCC: f.Core.PLMN.MCC, MNC: f.Core.PLMN.MNC, TAC: f.Core.TAC,
				Slices: []gnb.SNSSAI{{SST: uint8(f.Core.Slice.SST), SD: f.Core.Slice.SD}},
			},
			BindAddr: pg.GNB.Bind,
			N3Addr:   pg.GNB.N3,
		}
	}
	spec := engine.FleetRunSpec{
		AMFAddr: f.Core.AMF, GNBs: fgnbs,
		BaseIMSI: f.Fleet.SUPIBase, Count: f.Fleet.Count,
		MCC: f.Core.PLMN.MCC, MNC: f.Core.PLMN.MNC,
		Ki: ki, OPc: opc, RateRPS: rateRPS, Concurrency: 64,
	}
	if f.Fleet.PDUSession {
		spec.PDUSession = &ue.PDUSessionParams{
			PDUSessionID: 1, SST: uint8(f.Core.Slice.SST), SD: f.Core.Slice.SD, DNN: f.Core.DNN,
		}
	}

	var beh engine.FleetBehaviors
	if f.Run.Duration != "" {
		d, err := time.ParseDuration(f.Run.Duration)
		if err != nil {
			return fmt.Errorf("run.duration %q: %w", f.Run.Duration, err)
		}
		beh.Duration = d
	}
	if f.Behaviors.Mobility != nil {
		beh.MobileUEs = f.Fleet.Count / 2
		if beh.MobileUEs < 1 {
			beh.MobileUEs = 1
		}
		beh.HandoverEvery = 15 * time.Second
	}
	if t := f.Behaviors.Traffic; t != nil && len(t.Mix) > 0 && f.Fleet.PDUSession {
		beh.Traffic = true
		beh.TrafficRate = t.Mix[0].Rate
		if beh.TrafficRate == "" {
			beh.TrafficRate = "10Mbps"
		}
		beh.TrafficTarget = "8.8.8.8:9999"
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

// parseAttachRate turns "10/s" (or "10") into an attaches/sec rate; empty = 0.
func parseAttachRate(s string) (float64, error) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "/s")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	r, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("attach_rate %q: %w", s, err)
	}
	if r < 0 {
		return 0, nil
	}
	return r, nil
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
