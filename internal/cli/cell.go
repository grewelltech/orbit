package cli

import (
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

func newCellCmd(serverURL *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cell",
		Short: "gNB / cell procedures",
	}

	var (
		amf     string
		local   string
		bind    string
		gnbID   uint32
		gnbBits uint32
		name    string
		id      netIdentity
		timeout time.Duration
		hold    time.Duration
	)
	ngsetup := &cobra.Command{
		Use:   "ngsetup",
		Short: "Run one NG Setup exchange against an AMF",
		Long: "Opens an SCTP association to the AMF, sends NG Setup Request\n" +
			"(TS 38.413 §8.7.1), and reports the AMF's answer. The association is\n" +
			"closed as soon as the answer arrives unless --hold keeps it open, which\n" +
			"lets a capture show the AMF's SCTP heartbeat and anything it sends\n" +
			"unprompted. ngsetup is a probe, not a persistent gNB: ue register opens\n" +
			"its own association.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := id.resolve(true); err != nil {
				return err
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be greater than 0 (it bounds the dial and the NG Setup exchange)")
			}
			timeoutMs, err := msDuration("--timeout", timeout)
			if err != nil {
				return err
			}
			if hold < 0 {
				return fmt.Errorf("--hold must not be negative")
			}
			holdMs, err := msDuration("--hold", hold)
			if err != nil {
				return err
			}
			res, err := cellClient(serverURL).RunNGSetup(cmd.Context(),
				connect.NewRequest(&orbitv1.RunNGSetupRequest{
					AmfAddress:   amf,
					LocalAddress: ngsetupLocalAddress(cmd.Flags().Changed("bind"), bind, local),
					Gnb:          id.gnbProto(gnbID, gnbBits, name),
					TimeoutMs:    timeoutMs,
					HoldMs:       holdMs,
				}))
			if err != nil {
				return err
			}
			m := res.Msg
			fmt.Fprintf(cmd.OutOrStdout(), "network: %s\n", id)
			if m.GetAccepted() {
				fmt.Fprintf(cmd.OutOrStdout(), "NG Setup accepted by AMF %q (reply PPID %d)\n",
					m.GetAmfName(), m.GetReplyPpid())
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "NG Setup rejected: %s\n", m.GetCause())
			}
			if src := m.GetLocalAddress(); src != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  n2-source=%s\n", src)
			}
			if hold > 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  held: %s\n", time.Duration(m.GetHeldMs())*time.Millisecond)
			}
			return nil
		},
	}
	ngsetup.Flags().StringVar(&amf, "amf", "", "AMF N2 address (host:port)")
	ngsetup.Flags().DurationVar(&timeout, "timeout", 10*time.Second,
		"bound on the SCTP dial plus the NG Setup exchange (the server default when omitted elsewhere is also 10s)")
	ngsetup.Flags().DurationVar(&hold, "hold", 0,
		"keep the association open this long after a successful NG Setup, then shut it down cleanly (0 = close at once; max 10m)")
	ngsetup.Flags().StringVar(&bind, "bind", "", bindFlagHelp)
	ngsetup.Flags().StringVar(&local, "local", "", "local bind address (host:port)")
	_ = ngsetup.Flags().MarkDeprecated("local", "use --bind")
	ngsetup.Flags().Uint32Var(&gnbID, "gnb-id", 1, "gNB identifier")
	ngsetup.Flags().Uint32Var(&gnbBits, "gnb-id-bits", 24, "gNB identifier bit length (22-32)")
	ngsetup.Flags().StringVar(&name, "name", "orbit-gnb", "RAN node name")
	addNetIdentityFlags(ngsetup.Flags(), &id)
	_ = ngsetup.MarkFlagRequired("amf")

	cmd.AddCommand(ngsetup)
	return cmd
}

// ngsetupLocalAddress picks the N2 source for `cell ngsetup`: --bind when it
// was given (even as an explicit "" meaning the route-selected source), else the
// deprecated --local. The deprecated flag never overrides the current one.
func ngsetupLocalAddress(bindSet bool, bind, local string) string {
	if bindSet {
		return bind
	}
	return local
}
