package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// joinClient builds a JoinService client against the API server.
func joinClient(url *string) orbitv1connect.JoinServiceClient {
	h, guard := apiClient(*url)
	return orbitv1connect.NewJoinServiceClient(h, *url, guard)
}

// newJoinCmd issues the join tokens an app host's one-liner presents to
// set up an N6 responder (ADR-0009 §1).
func newJoinCmd(serverURL *string) *cobra.Command {
	cmd := &cobra.Command{Use: "join", Short: "Issue, list and revoke the tokens that set up an app host as an N6 responder"}
	cmd.AddCommand(newJoinCreateCmd(serverURL), newJoinListCmd(serverURL), newJoinRevokeCmd(serverURL))
	return cmd
}

func newJoinCreateCmd(serverURL *string) *cobra.Command {
	var (
		name, upfN6, n6Subnet, mgmt string
		uePools                     []string
		ttl                         time.Duration
	)
	cmd := &cobra.Command{
		Use:   "create -t <target>",
		Short: "Issue a join token and print the one-liner to run on the app host",
		Long: "Issue a join token (the server needs TLS) and print the one-liner to run on the app\n" +
			"host, as root. The token is in the one-liner's URL: anyone who sees it before it\n" +
			"expires can use it, and it can show up in shell history. Paste it only on the\n" +
			"intended host, keep --ttl short, and revoke a token you do not use (orbit join\n" +
			"revoke <id>). The UE pools, the UPF's N6 address and its N6 subnet come from the\n" +
			"target (-t) or the flags.",
		Example: "  orbit join create -t lab1 --name app1\n" +
			"  orbit join create --ue-pool 10.250.0.0/16 --upf-n6 198.51.100.1 --n6-subnet 198.51.100.0/24 --mgmt ran1:8412",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ttl < 0 || ttl%time.Second != 0 {
				return fmt.Errorf("--ttl %s: a whole number of seconds", ttl)
			}
			res, err := joinClient(serverURL).CreateJoinToken(cmd.Context(), connect.NewRequest(&orbitv1.CreateJoinTokenRequest{
				Name: name, TtlSeconds: uint32(ttl / time.Second),
				Spec: &orbitv1.JoinSpec{UePools: uePools, UpfN6: upfN6, N6Subnet: n6Subnet, ManagementAddress: mgmt},
			}))
			if err != nil {
				return err
			}
			m := res.Msg
			t := m.GetIssued()
			who := t.GetName()
			if who == "" {
				who = "the app host's hostname"
			}
			expires := time.Unix(0, t.GetExpiresUnixNano())
			// stdout is the one-liner alone (scripts capture it); the rest
			// goes to stderr.
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "join token %s for responder %s, expires %s (in %s).\n"+
				"Run this on the app host only; revoke an unused token with `orbit join revoke %s`:\n",
				t.GetId(), who, expires.Format(time.RFC3339), time.Until(expires).Round(time.Second), t.GetId())
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), m.GetOneLiner())
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "the responder's name (default: the app host's hostname)")
	f.DurationVar(&ttl, "ttl", 0, "how long the token lasts (default 10m, at most 1h)")
	f.StringSliceVar(&uePools, "ue-pool", nil, "a UE address pool (CIDR) the app host routes back through the UPF; repeat for more (target: core.ue_pools)")
	f.StringVar(&upfN6, "upf-n6", "", "the UPF's N6 address, the return route's next hop (target: core.upf_n6)")
	f.StringVar(&n6Subnet, "n6-subnet", "", "the UPF's N6 subnet (CIDR), which the app host's firewall opens to (target: core.n6_subnet)")
	f.StringVar(&mgmt, "mgmt", "", "the address the app host reaches this server at, host[:port] (default: the server's listen address)")
	return cmd
}

func newJoinListCmd(serverURL *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the pending join tokens (never their secrets)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := joinClient(serverURL).ListJoinTokens(cmd.Context(), connect.NewRequest(&orbitv1.ListJoinTokensRequest{}))
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tNAME\tEXPIRES\tMGMT\tUE POOLS")
			for _, t := range res.Msg.GetTokens() {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.GetId(), dash(t.GetName()),
					time.Unix(0, t.GetExpiresUnixNano()).Format(time.RFC3339), t.GetSpec().GetManagementAddress(),
					strings.Join(t.GetSpec().GetUePools(), ","))
			}
			return tw.Flush()
		},
	}
}

func newJoinRevokeCmd(serverURL *string) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Cancel a pending join token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := joinClient(serverURL).RevokeJoinToken(cmd.Context(), connect.NewRequest(&orbitv1.RevokeJoinTokenRequest{Id: args[0]})); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "revoked join token %s\n", args[0])
			return nil
		},
	}
}
