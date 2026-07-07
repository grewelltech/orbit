package cli

import (
	"github.com/spf13/cobra"

	"github.com/bgrewell/orbit/internal/scenario"
)

// newRunCmd runs a declarative YAML scenario against the ORBIT API server. The
// runner is an ordinary API client, so a scenario drives exactly the same
// operations as the individual `ue` commands, without the flag repetition.
func newRunCmd(serverURL *string) *cobra.Command {
	return &cobra.Command{
		Use:   "run <scenario.yaml>",
		Short: "Run a declarative YAML scenario against the ORBIT API server",
		Long: "Run a declarative YAML scenario. The file declares the core, gNBs, and UEs\n" +
			"once, then an ordered `steps` list (register, ping, traffic, latency, handover,\n" +
			"deregister, wait) references them. ${ENV} references expand from the environment,\n" +
			"so secrets like Ki/OPc stay out of the file. See docs/USAGE.md.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := scenario.Load(args[0])
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
