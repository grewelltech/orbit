package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bgrewell/orbit/internal/scenario"
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
				return runFleetPlan(cmd, data)
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

// runFleetPlan validates a fleet scenario and prints the generated topology and
// population. Execution (attach + continuous mobility/traffic) lands in a
// follow-up; this lets an operator author and check a fleet run first.
func runFleetPlan(cmd *cobra.Command, data []byte) error {
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
	fmt.Fprintf(out, "▶ %s (plan)\n", name)
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
	fmt.Fprintln(out, "\nnote: fleet execution is not wired yet — this validates and plans the scenario.")
	return nil
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
