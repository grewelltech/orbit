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
	cmd.AddCommand(newUEPingCmd(serverURL))
	cmd.AddCommand(newUEHandoverCmd(serverURL, false))
	cmd.AddCommand(newUEHandoverCmd(serverURL, true))
	cmd.AddCommand(newUETrafficCmd(serverURL))
	cmd.AddCommand(newUELatencyCmd(serverURL))
	cmd.AddCommand(newUEStatsCmd(serverURL))
	cmd.AddCommand(newUEAppCmd(serverURL))
	return cmd
}

func newUEStatsCmd(serverURL *string) *cobra.Command {
	var supi string
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show per-QFI uplink/downlink counters for a UE's N3 data path",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := ueClient(serverURL).DataStats(cmd.Context(),
				connect.NewRequest(&orbitv1.DataStatsRequest{Supi: supi}))
			if err != nil {
				return err
			}
			flows := res.Msg.GetFlows()
			if len(flows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no data-path activity yet (counters start with the first ping/traffic/latency run)")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "QFI\tUL-PKTS\tUL-BYTES\tDL-PKTS\tDL-BYTES")
			for _, f := range flows {
				fmt.Fprintf(w, "%d\t%d\t%d\t%d\t%d\n", f.GetQfi(),
					f.GetUplinkPackets(), f.GetUplinkBytes(),
					f.GetDownlinkPackets(), f.GetDownlinkBytes())
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&supi, "supi", "", "SUPI / IMSI")
	_ = cmd.MarkFlagRequired("supi")
	return cmd
}

func newUELatencyCmd(serverURL *string) *cobra.Command {
	var supi, target string
	var probes, spacingMs, timeoutMs uint32
	cmd := &cobra.Command{
		Use:   "latency",
		Short: "Probe RTT / jitter / loss from a UE over its N3 data path",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := ueClient(serverURL).Latency(cmd.Context(),
				connect.NewRequest(&orbitv1.LatencyRequest{
					Supi: supi, Target: target, Probes: probes,
					SpacingMs: spacingMs, TimeoutMs: timeoutMs,
				}))
			if err != nil {
				return err
			}
			m := res.Msg
			fmt.Fprintf(cmd.OutOrStdout(),
				"%d/%d replies (%.0f%% loss)  rtt min/mean/max %.2f/%.2f/%.2f ms  jitter %.2f ms\n",
				m.GetReceived(), m.GetSent(), m.GetLossPct(),
				m.GetMinMs(), m.GetMeanMs(), m.GetMaxMs(), m.GetJitterMs())
			return nil
		},
	}
	cmd.Flags().StringVar(&supi, "supi", "", "SUPI / IMSI")
	cmd.Flags().StringVar(&target, "target", "8.8.8.8", "destination IPv4 to probe")
	cmd.Flags().Uint32Var(&probes, "probes", 20, "number of echoes")
	cmd.Flags().Uint32Var(&spacingMs, "spacing-ms", 50, "spacing between echoes (ms)")
	cmd.Flags().Uint32Var(&timeoutMs, "timeout-ms", 1000, "per-echo timeout (ms)")
	_ = cmd.MarkFlagRequired("supi")
	return cmd
}

func newUETrafficCmd(serverURL *string) *cobra.Command {
	var supi, target, rate string
	var packetSize, durationMs uint32
	cmd := &cobra.Command{
		Use:   "traffic",
		Short: "Generate a loom UDP flow from a UE over its N3 data path",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := ueClient(serverURL).Traffic(cmd.Context(),
				connect.NewRequest(&orbitv1.TrafficRequest{
					Supi: supi, Target: target, Rate: rate,
					PacketSize: packetSize, DurationMs: durationMs,
				}))
			if err != nil {
				return err
			}
			m := res.Msg
			fmt.Fprintf(cmd.OutOrStdout(), "%d packets, %d bytes, %.1f Mbps over %dms\n",
				m.GetPackets(), m.GetBytes(), m.GetMbps(), m.GetDurationMs())
			return nil
		},
	}
	cmd.Flags().StringVar(&supi, "supi", "", "SUPI / IMSI")
	cmd.Flags().StringVar(&target, "target", "", "destination host:port")
	cmd.Flags().StringVar(&rate, "rate", "", "loom rate, e.g. 50Mbps (empty = unlimited)")
	cmd.Flags().Uint32Var(&packetSize, "packet-size", 1200, "inner UDP payload size")
	cmd.Flags().Uint32Var(&durationMs, "duration-ms", 5000, "flow duration (ms)")
	_ = cmd.MarkFlagRequired("supi")
	_ = cmd.MarkFlagRequired("target")
	return cmd
}

func newUEHandoverCmd(serverURL *string, xn bool) *cobra.Command {
	var (
		supi, amf, bind, gnbN3   string
		name, mcc, mnc, sd       string
		gnbID, gnbBits, tac, sst uint32
	)
	use, short := "handover", "Hand a registered UE over to a target gNB (N2 handover)"
	if xn {
		use, short = "xn-handover", "Hand a registered UE over to a target gNB (Xn handover / PathSwitch)"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			req := connect.NewRequest(&orbitv1.HandoverRequest{
				Supi:        supi,
				AmfAddress:  amf,
				BindAddress: bind,
				GnbN3Addr:   gnbN3,
				TargetGnb: &orbitv1.GnbConfig{
					Id: gnbID, IdBits: gnbBits, Name: name,
					Mcc: mcc, Mnc: mnc, Tac: tac,
					Slices: []*orbitv1.Snssai{{Sst: sst, Sd: sd}},
				},
			})
			client := ueClient(serverURL)
			call := client.Handover
			if xn {
				call = client.XnHandover
			}
			res, err := call(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (now on gNB %#x)\n",
				res.Msg.GetSupi(), res.Msg.GetState(), res.Msg.GetGnbId())
			return nil
		},
	}
	cmd.Flags().StringVar(&supi, "supi", "", "SUPI / IMSI of the registered UE")
	cmd.Flags().StringVar(&amf, "amf", "", "AMF N2 endpoint (host:port)")
	cmd.Flags().StringVar(&bind, "bind", "", "SCTP bind address for the target gNB (distinct routed source IP)")
	cmd.Flags().StringVar(&gnbN3, "gnb-n3", "", "target gNB N3 address for the downlink tunnel")
	cmd.Flags().Uint32Var(&gnbID, "gnb-id", 0x43, "target gNB ID")
	cmd.Flags().Uint32Var(&gnbBits, "gnb-bits", 24, "target gNB ID bit length")
	cmd.Flags().StringVar(&name, "gnb-name", "orbit-gnb-tgt", "target gNB name")
	cmd.Flags().StringVar(&mcc, "mcc", "208", "MCC")
	cmd.Flags().StringVar(&mnc, "mnc", "93", "MNC")
	cmd.Flags().Uint32Var(&tac, "tac", 1, "TAC")
	cmd.Flags().Uint32Var(&sst, "sst", 1, "slice SST")
	cmd.Flags().StringVar(&sd, "sd", "010203", "slice SD (6 hex)")
	_ = cmd.MarkFlagRequired("supi")
	_ = cmd.MarkFlagRequired("amf")
	return cmd
}

func newUEPingCmd(serverURL *string) *cobra.Command {
	var supi, dst string
	var count uint32
	cmd := &cobra.Command{
		Use:   "ping",
		Short: "Send ICMP echoes from a UE through its N3 data path",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := ueClient(serverURL).Ping(cmd.Context(),
				connect.NewRequest(&orbitv1.PingRequest{Supi: supi, Destination: dst, Count: count}))
			if err != nil {
				return err
			}
			m := res.Msg
			fmt.Fprintf(cmd.OutOrStdout(), "%d/%d replies from %s (last RTT %.1f ms)\n",
				m.GetReceived(), m.GetSent(), m.GetReplyFrom(), m.GetRttMs())
			return nil
		},
	}
	cmd.Flags().StringVar(&supi, "supi", "", "SUPI / IMSI")
	cmd.Flags().StringVar(&dst, "dst", "8.8.8.8", "echo target IPv4")
	cmd.Flags().Uint32Var(&count, "count", 3, "number of echoes")
	_ = cmd.MarkFlagRequired("supi")
	return cmd
}

func newUERegisterCmd(serverURL *string) *cobra.Command {
	var (
		amf, supi, ki, opc, rid         string
		name, mcc, mnc, sd, dnn, gnbN3  string
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
				req.GnbN3Addr = gnbN3
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
	f.StringVar(&gnbN3, "gnb-n3", "", "gNB N3 address for the data path (reachable from the UPF)")
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
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  ip=%s  amf-ue-ngap-id=%d  gnb=%s%s\n",
				s.GetSupi(), s.GetState(), s.GetPduAddress(), s.GetAmfUeNgapId(),
				gnbLabel(s), mobilitySuffix(s))
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
			fmt.Fprintln(w, "SUPI\tSTATE\tIP\tAMF-UE-NGAP-ID\tSERVING-GNB\tMOBILITY")
			for _, u := range res.Msg.GetUes() {
				mob := u.GetMobilityState()
				if mob == "" {
					mob = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", u.GetSupi(), u.GetState(),
					u.GetPduAddress(), u.GetAmfUeNgapId(), gnbLabel(u), mob)
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

// gnbLabel renders the serving cell as a name when the gNB advertises one,
// falling back to the hex id. A UE that has never been placed shows "-".
func gnbLabel(s *orbitv1.UEStatus) string {
	if n := s.GetServingGnbName(); n != "" {
		return n
	}
	if id := s.GetServingGnbId(); id != 0 {
		return fmt.Sprintf("%#x", id)
	}
	return "-"
}

// mobilitySuffix appends the mobility phase and its age, when the UE has moved.
// Registration and mobility are orthogonal axes, so this reads alongside the
// UE's state rather than replacing it — a handed-over UE is still
// SESSION_ACTIVE, and a UE whose handover failed says so here.
func mobilitySuffix(s *orbitv1.UEStatus) string {
	phase := s.GetMobilityState()
	if phase == "" {
		return ""
	}
	if ns := s.GetMobilityChangedUnixNano(); ns > 0 {
		return fmt.Sprintf("  mobility=%s (%s ago)", phase,
			time.Since(time.Unix(0, ns)).Truncate(time.Second))
	}
	return "  mobility=" + phase
}
