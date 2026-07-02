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
		gnbID   uint32
		gnbBits uint32
		name    string
		mcc     string
		mnc     string
		tac     uint32
		sst     uint32
		sd      string
	)
	ngsetup := &cobra.Command{
		Use:   "ngsetup",
		Short: "Run one NG Setup exchange against an AMF",
		Long: "Opens an SCTP association to the AMF, sends NG Setup Request\n" +
			"(TS 38.413 §8.7.1), and reports the AMF's answer.",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := cellClient(serverURL).RunNGSetup(cmd.Context(),
				connect.NewRequest(&orbitv1.RunNGSetupRequest{
					AmfAddress:   amf,
					LocalAddress: local,
					Gnb: &orbitv1.GnbConfig{
						Id:     gnbID,
						IdBits: gnbBits,
						Name:   name,
						Mcc:    mcc,
						Mnc:    mnc,
						Tac:    tac,
						Slices: []*orbitv1.Snssai{{Sst: sst, Sd: sd}},
					},
				}))
			if err != nil {
				return err
			}
			m := res.Msg
			if m.GetAccepted() {
				fmt.Fprintf(cmd.OutOrStdout(), "NG Setup accepted by AMF %q (reply PPID %d)\n",
					m.GetAmfName(), m.GetReplyPpid())
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "NG Setup rejected: %s\n", m.GetCause())
			}
			return nil
		},
	}
	ngsetup.Flags().StringVar(&amf, "amf", "", "AMF N2 address (host:port)")
	ngsetup.Flags().StringVar(&local, "local", "", "local bind address (host:port)")
	ngsetup.Flags().Uint32Var(&gnbID, "gnb-id", 1, "gNB identifier")
	ngsetup.Flags().Uint32Var(&gnbBits, "gnb-id-bits", 24, "gNB identifier bit length (22-32)")
	ngsetup.Flags().StringVar(&name, "name", "orbit-gnb", "RAN node name")
	ngsetup.Flags().StringVar(&mcc, "mcc", "", "mobile country code (3 digits)")
	ngsetup.Flags().StringVar(&mnc, "mnc", "", "mobile network code (2-3 digits)")
	ngsetup.Flags().Uint32Var(&tac, "tac", 1, "tracking area code (24-bit)")
	ngsetup.Flags().Uint32Var(&sst, "sst", 1, "slice/service type")
	ngsetup.Flags().StringVar(&sd, "sd", "", "slice differentiator (6 hex digits)")
	_ = ngsetup.MarkFlagRequired("amf")
	_ = ngsetup.MarkFlagRequired("mcc")
	_ = ngsetup.MarkFlagRequired("mnc")

	cmd.AddCommand(ngsetup)
	return cmd
}
