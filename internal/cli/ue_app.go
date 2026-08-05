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
	cmd.AddCommand(newUEAppHTTPCmd(serverURL))
	cmd.AddCommand(newUEAppVideoCmd(serverURL))
	return cmd
}

// checkAppDuration validates the shared --duration contract before the
// uint32(ms) conversion: a negative duration would wrap into ~49.7 days,
// silently defeating the far-end duration bound (orphan protection) instead
// of being refused.
func checkAppDuration(duration time.Duration) error {
	if duration <= 0 {
		return fmt.Errorf("--duration must be positive, got %s", duration)
	}
	if ms := duration.Milliseconds(); ms > math.MaxUint32 {
		return fmt.Errorf("--duration %s exceeds the maximum of %s", duration,
			time.Duration(math.MaxUint32)*time.Millisecond)
	}
	return nil
}

// runAppSession drives the shared lifecycle of every `orbit ue app`
// subcommand: StartApp, stream both-end interval samples and correlation
// events until the session ends, then StopApp for the final report — with a
// nonzero exit when the session itself failed. headline is the human line
// printed once the session id is known (suppressed under --json).
func runAppSession(cmd *cobra.Command, serverURL *string, req *orbitv1.StartAppRequest, jsonOut bool, headline string) error {
	client := ueClient(serverURL)
	start, err := client.StartApp(cmd.Context(), connect.NewRequest(req))
	if err != nil {
		return err
	}
	id := start.Msg.GetSessionId()
	out := cmd.OutOrStdout()
	if !jsonOut {
		fmt.Fprintf(out, "session %s: %s\n", id, headline)
	}

	// Stream both-end interval samples and correlation events until the
	// session ends; a stream error still falls through to StopApp so the
	// report (and any terminal session error) is not lost.
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
		return fmt.Errorf("%s session %s failed: %s", req.GetApp(), id, e)
	}
	return nil
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
			if err := checkAppDuration(duration); err != nil {
				return err
			}
			params := map[string]string{"codec": codec}
			if ptime > 0 {
				params["ptime"] = ptime.String()
			}
			if jb > 0 {
				params["jb_ms"] = strconv.Itoa(jb)
			}
			return runAppSession(cmd, serverURL, &orbitv1.StartAppRequest{
				Supi:       supi,
				App:        "voip",
				Peer:       peer,
				Token:      token,
				PeerDataIp: peerDataIP,
				Params:     params,
				DurationMs: uint32(duration.Milliseconds()),
			}, jsonOut, fmt.Sprintf("voip %s → %s (%s) for %s", supi, peer, codec, duration))
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
	if v := s.GetLocalHttp(); v != nil {
		fmt.Fprintf(w, "%s  %s\n", ts, httpSampleLine("ue", v, s.GetFinal()))
	}
	if v := s.GetRemoteHttp(); v != nil {
		fmt.Fprintf(w, "%s  %s\n", ts, httpSampleLine("n6", v, s.GetFinal()))
	}
	if v := s.GetLocalVideo(); v != nil {
		fmt.Fprintf(w, "%s  %s\n", ts, videoSampleLine("ue", v, s.GetFinal()))
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

// httpSampleLine renders one HTTP interval: request/error counts, TTFB p95,
// and goodput, with connect/TLS-handshake means when the interval opened new
// connections (reused keep-alive connections contribute none). The n6 (origin)
// end measures no client-side latencies, so its timing fields print only when
// present.
func httpSampleLine(end string, h *orbitv1.HttpMetrics, final bool) string {
	line := fmt.Sprintf("%-3s reqs %d  err %d", end, h.GetRequests(), h.GetErrors())
	// Time-to-first-byte is a requester-side measurement. The origin records
	// only bytes, status and abort per response, so its TTFB percentile is
	// unset — and printing "ttfb-p95 0.00ms" for it reads as an instant
	// response rather than as no measurement.
	if t := h.GetTtfbMsP95(); t > 0 {
		line += fmt.Sprintf("  ttfb-p95 %.2fms", t)
	}
	line += fmt.Sprintf("  goodput %.2f Mbps", h.GetGoodputMbps())
	if c := h.GetConnectMs(); c > 0 {
		line += fmt.Sprintf("  connect %.2fms", c)
	}
	if tl := h.GetTlsHandshakeMs(); tl > 0 {
		line += fmt.Sprintf("  tls %.2fms", tl)
	}
	if final {
		line += "  [final]"
	}
	return line
}

// videoSampleLine renders one video QoE interval: current buffer level,
// interval average bitrate, stall count/time, and the startup delay once
// playback has begun ("startup n/a" while still buffering — an honest 0 would
// read as instant startup).
func videoSampleLine(end string, v *orbitv1.VideoMetrics, final bool) string {
	startup := "n/a"
	if v.GetStartupMs() > 0 {
		startup = fmt.Sprintf("%.0fms", v.GetStartupMs())
	}
	line := fmt.Sprintf("%-3s buffer %.1fs  bitrate %.0f kbps  stalls %d  stalled %.1fs  startup %s",
		end, v.GetBufferMs()/1000, v.GetAvgBitrateKbps(), v.GetStalls(),
		v.GetStallTimeMs()/1000, startup)
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
	if v := r.GetLocalHttp(); v != nil {
		fmt.Fprintf(w, "  %s\n", httpSampleLine("ue", v, false))
	}
	if v := r.GetLocalVideo(); v != nil {
		fmt.Fprintf(w, "  %s\n", videoReportLine("ue", v))
	}
	switch {
	case r.GetRemote() != nil:
		fmt.Fprintf(w, "  %s\n", voipReportLine("n6", r.GetRemote()))
	case r.GetRemoteHttp() != nil:
		fmt.Fprintf(w, "  %s\n", httpSampleLine("n6", r.GetRemoteHttp(), false))
	default:
		fmt.Fprintln(w, "  n6  (no final remote sample arrived)")
	}
	if stalls := r.GetLocalVideo().GetStallEvents(); len(stalls) > 0 {
		fmt.Fprintln(w, "  stalls:")
		for _, g := range stalls {
			d := time.Duration(g.GetEndUnixNano() - g.GetStartUnixNano())
			fmt.Fprintf(w, "    %s  %s\n",
				time.Unix(0, g.GetStartUnixNano()).Format("15:04:05.000"),
				d.Round(time.Millisecond))
		}
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

// videoReportLine renders the whole-run player summary: startup, stall
// count/total time, rebuffer ratio, and duration-weighted average bitrate.
func videoReportLine(end string, v *orbitv1.VideoMetrics) string {
	startup := "n/a (never played)"
	if v.GetStartupMs() > 0 {
		startup = fmt.Sprintf("%.0fms", v.GetStartupMs())
	}
	return fmt.Sprintf("%-3s startup %s  stalls %d (%.1fs stalled, rebuffer %.1f%%)  avg bitrate %.0f kbps",
		end, startup, v.GetStalls(), v.GetStallTimeMs()/1000,
		v.GetRebufferRatio()*100, v.GetAvgBitrateKbps())
}
