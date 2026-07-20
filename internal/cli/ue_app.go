package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// newUEAppCmd groups the real-application-traffic commands: a registered UE
// runs a loom app engine over its N3 data path against a stock loomd agent
// on the N6 network (docs/design/real-app-traffic.md §7).
func newUEAppCmd(serverURL *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Run real application traffic from a UE to a loom agent",
	}
	cmd.AddCommand(newUEAppVoipCmd(serverURL))
	return cmd
}

func newUEAppVoipCmd(serverURL *string) *cobra.Command {
	var (
		supi, peer, token, peerDataIP string
		codec                         string
		ptime                         time.Duration
		jb                            int
		duration                      time.Duration
		jsonOut                       bool
	)
	cmd := &cobra.Command{
		Use:   "voip",
		Short: "Run a bidirectional RTP/RTCP call from a UE and stream MOS both ways",
		Long: "Place a duration-bounded VoIP call from a registered UE through its GTP-U\n" +
			"data path to the loomd agent at --peer (its control address on the management\n" +
			"network). Interval quality samples from BOTH ends — MOS/R, jitter, loss,\n" +
			"jitter-buffer discard, RTT, and one-way delay with its method and error bar —\n" +
			"stream live, with correlation events (handover phases, GTP-U End Markers)\n" +
			"inline; a both-end report follows when the call ends.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate before the uint32(ms) conversion: a negative duration
			// would wrap into ~49.7 days, silently defeating the far-end
			// duration bound (orphan protection) instead of being refused.
			if duration <= 0 {
				return fmt.Errorf("--duration must be positive, got %s", duration)
			}
			if ms := duration.Milliseconds(); ms > math.MaxUint32 {
				return fmt.Errorf("--duration %s exceeds the maximum of %s", duration,
					time.Duration(math.MaxUint32)*time.Millisecond)
			}
			params := map[string]string{"codec": codec}
			if ptime > 0 {
				params["ptime"] = ptime.String()
			}
			if jb > 0 {
				params["jb_ms"] = strconv.Itoa(jb)
			}
			client := ueClient(serverURL)
			start, err := client.StartApp(cmd.Context(), connect.NewRequest(&orbitv1.StartAppRequest{
				Supi:       supi,
				App:        "voip",
				Peer:       peer,
				Token:      token,
				PeerDataIp: peerDataIP,
				Params:     params,
				DurationMs: uint32(duration.Milliseconds()),
			}))
			if err != nil {
				return err
			}
			id := start.Msg.GetSessionId()
			out := cmd.OutOrStdout()
			if !jsonOut {
				fmt.Fprintf(out, "call %s: voip %s → %s (%s) for %s\n", id, supi, peer, codec, duration)
			}

			// Stream both-end interval samples and correlation events until
			// the session ends; a stream error still falls through to StopApp
			// so the report (and any terminal session error) is not lost.
			stream, err := client.AppStream(cmd.Context(), connect.NewRequest(&orbitv1.AppStreamRequest{SessionId: id}))
			if err == nil {
				for stream.Receive() {
					printAppSample(out, stream.Msg(), jsonOut)
				}
				err = stream.Err()
			}
			if err != nil && !jsonOut {
				fmt.Fprintf(out, "stream ended early: %v\n", err)
			}

			// Reap the report on a fresh context so it survives cancellation.
			sctx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), 30*time.Second)
			defer cancel()
			rep, err := client.StopApp(sctx, connect.NewRequest(&orbitv1.StopAppRequest{SessionId: id}))
			if err != nil {
				return err
			}
			printAppReport(out, rep.Msg, jsonOut)
			if e := rep.Msg.GetError(); e != "" {
				return fmt.Errorf("voip session %s failed: %s", id, e)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&supi, "supi", "", "SUPI / IMSI of the registered UE")
	f.StringVar(&peer, "peer", "", "N6 loomd control address (host:port) on the management network")
	f.StringVar(&token, "token", "", "loomd control-plane bearer token")
	f.StringVar(&peerDataIP, "peer-data-ip", "", "N6 media address, when it differs from the --peer host")
	f.StringVar(&codec, "codec", "g711", "codec: g711/pcmu, g711a/pcma, g729, opus")
	f.DurationVar(&ptime, "ptime", 20*time.Millisecond, "RTP packetization time")
	f.IntVar(&jb, "jb", 40, "jitter-buffer nominal delay (ms); late arrivals count as discards")
	f.DurationVar(&duration, "duration", 60*time.Second, "call duration (the far-end flow is duration-bounded)")
	f.BoolVar(&jsonOut, "json", false, "emit samples and the final report as JSON lines")
	_ = cmd.MarkFlagRequired("supi")
	_ = cmd.MarkFlagRequired("peer")
	return cmd
}

// printAppSample renders one stream item: correlation events inline,
// quality samples as one line per end.
func printAppSample(w io.Writer, s *orbitv1.AppSample, asJSON bool) {
	if asJSON {
		b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(s)
		if err == nil {
			fmt.Fprintln(w, string(b))
		}
		return
	}
	ts := time.Unix(0, s.GetUnixNano()).Format("15:04:05.000")
	for _, ev := range s.GetEvents() {
		fmt.Fprintf(w, "%s  ** %s", ts, ev.GetKind())
		if ev.GetDetail() != "" {
			fmt.Fprintf(w, " — %s", ev.GetDetail())
		}
		fmt.Fprintln(w)
	}
	if v := s.GetLocal(); v != nil {
		fmt.Fprintf(w, "%s  %s\n", ts, voipSampleLine("ue", v, s.GetFinal()))
	}
	if v := s.GetRemote(); v != nil {
		fmt.Fprintf(w, "%s  %s\n", ts, voipSampleLine("n6", v, s.GetFinal()))
	}
}

func voipSampleLine(end string, v *orbitv1.VoipMetrics, final bool) string {
	line := fmt.Sprintf("%-3s MOS %.2f  R %5.1f  jitter %.2fms  loss %.2f%%  discard %.2f%%  rtt %.2fms  owd %s",
		end, v.GetMosCq(), v.GetRFactor(), v.GetJitterMs(), v.GetLossPct(),
		v.GetDiscardPct(), v.GetRttMs(), owdString(v))
	if final {
		line += "  [final]"
	}
	return line
}

// owdString renders one-way delay with its error bar and method label — an
// RTT/2 guess is never presented as a measured number (design §5).
func owdString(v *orbitv1.VoipMetrics) string {
	m := v.GetOwdMethod()
	if m == "" || m == "none" {
		return "n/a"
	}
	return fmt.Sprintf("%.2f±%.2fms (%s)", v.GetOwdMs(), v.GetOwdErrMs(), m)
}

func printAppReport(w io.Writer, r *orbitv1.AppReport, asJSON bool) {
	if asJSON {
		b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(r)
		if err == nil {
			fmt.Fprintln(w, string(b))
		}
		return
	}
	dur := time.Duration(r.GetEndedUnixNano() - r.GetStartedUnixNano())
	fmt.Fprintf(w, "\ncall %s: %s %s → %s (media port %d), %s\n",
		r.GetSessionId(), r.GetApp(), r.GetSupi(), r.GetPeer(), r.GetDataPort(),
		dur.Round(time.Millisecond))
	if v := r.GetLocal(); v != nil {
		fmt.Fprintf(w, "  %s\n", voipReportLine("ue", v))
	}
	if v := r.GetRemote(); v != nil {
		fmt.Fprintf(w, "  %s\n", voipReportLine("n6", v))
	} else {
		fmt.Fprintln(w, "  n6  (no final remote sample arrived)")
	}
	if gaps := r.GetMediaGaps(); len(gaps) > 0 {
		fmt.Fprintln(w, "  media gaps:")
		for _, g := range gaps {
			d := time.Duration(g.GetEndUnixNano() - g.GetStartUnixNano())
			line := fmt.Sprintf("    %-3s %s  %s (%d pkts lost)",
				g.GetEnd(), time.Unix(0, g.GetStartUnixNano()).Format("15:04:05.000"),
				d.Round(time.Millisecond), g.GetPacketsLost())
			// Clock provenance: re-stamped remote gaps carry their error
			// bound; an un-re-stampable one is labeled, never presented as
			// aligned with the local timestamps above it (design §5/§7).
			switch g.GetClock() {
			case "timesync":
				if e := time.Duration(g.GetTimeErrNano()); e > 0 {
					line += fmt.Sprintf(" [±%s]", e.Round(10*time.Microsecond))
				}
			case "remote-clock":
				line += " [remote clock — not aligned]"
			}
			fmt.Fprintln(w, line)
		}
	}
	if ann := r.GetAnnotations(); len(ann) > 0 {
		fmt.Fprintln(w, "  events:")
		for _, a := range ann {
			fmt.Fprintf(w, "    %s\n", a)
		}
	}
	if r.GetError() != "" {
		fmt.Fprintf(w, "  error: %s\n", r.GetError())
	}
}

func voipReportLine(end string, v *orbitv1.VoipMetrics) string {
	return fmt.Sprintf("%-3s MOS %.2f (R %.1f)  tx %d  rx %d  lost %d (%.2f%%)  discard %.2f%%  jitter %.2fms  rtt %.2fms  owd %s",
		end, v.GetMosCq(), v.GetRFactor(), v.GetTxPackets(), v.GetRxPackets(),
		v.GetLost(), v.GetLossPct(), v.GetDiscardPct(), v.GetJitterMs(),
		v.GetRttMs(), owdString(v))
}
