package cli

import (
	"fmt"

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
	)
	ngsetup := &cobra.Command{
		Use:   "ngsetup",
		Short: "Run one NG Setup exchange against an AMF",
		Long: "Opens an SCTP association to the AMF, sends NG Setup Request\n" +
			"(TS 38.413 §8.7.1), and reports the AMF's answer.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := id.resolve(true); err != nil {
				return err
			}
			res, err := cellClient(serverURL).RunNGSetup(cmd.Context(),
				connect.NewRequest(&orbitv1.RunNGSetupRequest{
					AmfAddress:   amf,
					LocalAddress: ngsetupLocalAddress(cmd.Flags().Changed("bind"), bind, local),
					Gnb:          id.gnbProto(gnbID, gnbBits, name),
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
			return nil
		},
	}
	ngsetup.Flags().StringVar(&amf, "amf", "", "AMF N2 address (host:port)")
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
// was given (even as an explicit "" meaning kernel default), else the
// deprecated --local. The deprecated flag never overrides the current one.
func ngsetupLocalAddress(bindSet bool, bind, local string) string {
	if bindSet {
		return bind
	}
	return local
}
