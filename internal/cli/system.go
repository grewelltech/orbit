package cli

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

func newSystemCmd(serverURL *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Inspect the running ORBIT server",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "info",
		Short: "Show server version and runtime info",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := systemClient(serverURL).GetInfo(cmd.Context(),
				connect.NewRequest(&orbitv1.GetInfoRequest{}))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "server version: %s\ngo version:     %s\n",
				res.Msg.GetVersion(), res.Msg.GetGoVersion())
			return nil
		},
	})
	return cmd
}
