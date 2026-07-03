package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

func newUECmd(serverURL *string) *cobra.Command {
	cmd := &cobra.Command{Use: "ue", Short: "Register and manage simulated UEs"}
	cmd.AddCommand(newUERegisterCmd(serverURL))
	cmd.AddCommand(newUEStatusCmd(serverURL))
	cmd.AddCommand(newUEDeregisterCmd(serverURL))
	cmd.AddCommand(newUEListCmd(serverURL))
	cmd.AddCommand(newUEWatchCmd(serverURL))
	return cmd
}

func newUERegisterCmd(serverURL *string) *cobra.Command {
	var (
		amf, supi, ki, opc, rid         string
		name, mcc, mnc, sd, dnn         string
		gnbID, gnbBits, tac, sst, pduID uint32
		withPDU                         bool
	)
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Attach one UE to the core (Registration + optional PDU session)",
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &orbitv1.RegisterRequest{
				AmfAddress: amf,
				Supi:       supi,
				Gnb: &orbitv1.GnbConfig{
					Id: gnbID, IdBits: gnbBits, Name: name,
					Mcc: mcc, Mnc: mnc, Tac: tac,
					Slices: []*orbitv1.Snssai{{Sst: sst, Sd: sd}},
				},
				Credentials:      &orbitv1.Credentials{Ki: ki, Opc: opc},
				RoutingIndicator: rid,
			}
			if withPDU {
				req.PduSession = &orbitv1.PDUSession{PduSessionId: pduID, Sst: sst, Sd: sd, Dnn: dnn}
			}
			res, err := ueClient(serverURL).Register(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			m := res.Msg
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "UE %s registered=%t (AMF-UE-NGAP-ID %d)\n", m.GetSupi(), m.GetRegistered(), m.GetAmfUeNgapId())
			if m.GetSessionActive() {
				fmt.Fprintf(out, "  PDU session: UE IP %s via UPF %s (TEID %d)\n", m.GetPduAddress(), m.GetUpfAddress(), m.GetUpfTeid())
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&amf, "amf", "", "AMF N2 address (host:port)")
	f.StringVar(&supi, "supi", "", "SUPI / IMSI, e.g. 208930100007500")
	f.StringVar(&ki, "ki", "", "subscriber key Ki (32 hex digits)")
	f.StringVar(&opc, "opc", "", "operator key OPc (32 hex digits)")
	f.StringVar(&rid, "routing-indicator", "0", "SUCI routing indicator")
	f.StringVar(&name, "gnb-name", "orbit-gnb", "RAN node name")
	f.Uint32Var(&gnbID, "gnb-id", 1, "gNB identifier")
	f.Uint32Var(&gnbBits, "gnb-id-bits", 24, "gNB identifier bit length")
	f.StringVar(&mcc, "mcc", "", "mobile country code")
	f.StringVar(&mnc, "mnc", "", "mobile network code")
	f.Uint32Var(&tac, "tac", 1, "tracking area code")
	f.Uint32Var(&sst, "sst", 1, "slice/service type")
	f.StringVar(&sd, "sd", "", "slice differentiator (6 hex digits)")
	f.BoolVar(&withPDU, "pdu-session", false, "establish a PDU session after registration")
	f.Uint32Var(&pduID, "pdu-session-id", 1, "PDU session id")
	f.StringVar(&dnn, "dnn", "internet", "data network name")
	for _, r := range []string{"amf", "supi", "ki", "opc", "mcc", "mnc"} {
		_ = cmd.MarkFlagRequired(r)
	}
	return cmd
}

func newUEStatusCmd(serverURL *string) *cobra.Command {
	var supi string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show a registered UE's state",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := ueClient(serverURL).Status(cmd.Context(),
				connect.NewRequest(&orbitv1.StatusRequest{Supi: supi}))
			if err != nil {
				return err
			}
			s := res.Msg.GetStatus()
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  ip=%s  amf-ue-ngap-id=%d\n",
				s.GetSupi(), s.GetState(), s.GetPduAddress(), s.GetAmfUeNgapId())
			return nil
		},
	}
	cmd.Flags().StringVar(&supi, "supi", "", "SUPI / IMSI")
	_ = cmd.MarkFlagRequired("supi")
	return cmd
}

func newUEDeregisterCmd(serverURL *string) *cobra.Command {
	var supi string
	cmd := &cobra.Command{
		Use:   "deregister",
		Short: "Deregister a UE and release it",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := ueClient(serverURL).Deregister(cmd.Context(),
				connect.NewRequest(&orbitv1.DeregisterRequest{Supi: supi})); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "UE %s deregistered\n", supi)
			return nil
		},
	}
	cmd.Flags().StringVar(&supi, "supi", "", "SUPI / IMSI")
	_ = cmd.MarkFlagRequired("supi")
	return cmd
}

func newUEListCmd(serverURL *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered UEs",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := ueClient(serverURL).List(cmd.Context(), connect.NewRequest(&orbitv1.ListRequest{}))
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "SUPI\tSTATE\tIP\tAMF-UE-NGAP-ID")
			for _, u := range res.Msg.GetUes() {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", u.GetSupi(), u.GetState(), u.GetPduAddress(), u.GetAmfUeNgapId())
			}
			return w.Flush()
		},
	}
}

func newUEWatchCmd(serverURL *string) *cobra.Command {
	var supi string
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Stream UE lifecycle events (StateStream)",
		RunE: func(cmd *cobra.Command, args []string) error {
			stream, err := ueClient(serverURL).StateStream(cmd.Context(),
				connect.NewRequest(&orbitv1.StateStreamRequest{Supi: supi}))
			if err != nil {
				return err
			}
			for stream.Receive() {
				e := stream.Msg()
				ts := time.Unix(0, e.GetUnixNano()).Format("15:04:05.000")
				fmt.Fprintf(cmd.OutOrStdout(), "%s  %-20s %-20s %s\n", ts, e.GetSupi(), e.GetState(), e.GetDetail())
			}
			return stream.Err()
		},
	}
	cmd.Flags().StringVar(&supi, "supi", "", "only this SUPI (default: all)")
	return cmd
}
