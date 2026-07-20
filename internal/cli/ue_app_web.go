// The TCP app-session commands (design Phases 6-7): `orbit ue app http`
// drives loom's real HTTP/1.1(+TLS/h2) client from a registered UE through
// the per-gNB gVisor netstack to a stock loomd HTTPOrigin on the N6 network;
// `orbit ue app video` drives loom's ABR player against the same origin's
// generated HLS ladder. Parameters travel verbatim to both ends (the loom
// controller discipline): each engine reads only the keys it documents, so
// --ladder/--seg-duration/--segments configure the origin while the player
// treats the ladder as its expectation.
package cli

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// webTransportFlags are the transport knobs the http client and the video
// player share (one grammar, both apps — loom httpx.NewTransport).
type webTransportFlags struct {
	tls, h2, tlsInsecure bool
	tlsCA, host          string
}

func (f *webTransportFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.BoolVar(&f.tls, "tls", false, "use https (the origin serves a self-signed certificate)")
	fl.BoolVar(&f.h2, "h2", false, "negotiate HTTP/2 via ALPN (requires --tls; h2c is not supported)")
	fl.StringVar(&f.host, "host", "", "Host header and TLS SNI/verification name, when it differs from the data address")
	fl.StringVar(&f.tlsCA, "tls-ca", "", "REFUSED against a stock loomd origin (its per-flow self-signed cert cannot be pre-pinned; see docs/USAGE.md) — reserved for when loom exposes the origin certificate")
	fl.BoolVar(&f.tlsInsecure, "tls-insecure", false, "disable certificate verification (explicit lab-only opt-in; the only working TLS path to a stock loomd origin — see docs/USAGE.md)")
}

// portRangeFlags pin the origin's data-port bind range (loom port_min /
// port_max) for firewall determinism — the design §8 matrix opens the HTTP
// data-port range from the UPF's N6 subnet the way it does RTP's.
type portRangeFlags struct {
	portMin, portMax int
}

func (f *portRangeFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.IntVar(&f.portMin, "port-min", 0, "origin data-port range start (firewall determinism; 0 = ephemeral)")
	fl.IntVar(&f.portMax, "port-max", 0, "origin data-port range end (defaults to --port-min)")
}

func (f *portRangeFlags) params(p map[string]string) map[string]string {
	if f.portMin > 0 {
		p["port_min"] = strconv.Itoa(f.portMin)
	}
	if f.portMax > 0 {
		p["port_max"] = strconv.Itoa(f.portMax)
	}
	return p
}

// params folds the transport flags into the app params map. Cross-flag
// mistakes are refused here, before any RPC — the same refusals loom would
// issue at engine build time, minus the round trip.
//
// --tls-ca is refused outright (errTLSCAUnusable): the stock loomd origin
// generates a fresh self-signed certificate for EVERY flow at Configure time
// and loom's control plane does not expose it (ConfigureResponse carries only
// flow_id and data_port), so no PEM an operator can obtain ever matches — the
// session would pass setup and then burn its whole duration on 100%-error
// samples. Fail fast instead; the engine (StartAppSession) enforces the same
// refusal for API callers.
func (f *webTransportFlags) params(p map[string]string) (map[string]string, error) {
	if f.h2 && !f.tls {
		return nil, fmt.Errorf("--h2 requires --tls (h2c is not supported)")
	}
	if (f.tlsCA != "" || f.tlsInsecure) && !f.tls {
		return nil, fmt.Errorf("--tls-ca/--tls-insecure are only meaningful with --tls")
	}
	if f.tlsCA != "" {
		return nil, fmt.Errorf("--tls-ca cannot verify a stock loomd origin: %s", errTLSCAUnusable)
	}
	if f.tls {
		p["tls"] = "true"
	}
	if f.h2 {
		p["h2"] = "true"
	}
	if f.host != "" {
		p["host"] = f.host
	}
	if f.tlsInsecure {
		p["tls_insecure"] = "true"
	}
	return p, nil
}

// errTLSCAUnusable states why pinning a stock loomd origin cannot work today
// (docs/USAGE.md "TLS, honestly"); shared wording with the engine refusal.
const errTLSCAUnusable = "the origin generates a fresh self-signed certificate per flow at Configure time and loom's control plane does not expose it, so no pre-obtained PEM can match and every request would fail verification for the whole session; use --tls-insecure (explicit lab-only opt-in) until loom can hand out the per-flow origin certificate"

func newUEAppHTTPCmd(serverURL *string) *cobra.Command {
	var (
		supi, peer, token, peerDataIP string
		urlPath, objectSize, think    string
		objects                       int
		tr                            webTransportFlags
		pr                            portRangeFlags
		duration                      time.Duration
		jsonOut                       bool
	)
	cmd := &cobra.Command{
		Use:   "http",
		Short: "Fetch objects over real HTTP(S) from a UE and stream TTFB/goodput",
		Long: "Run loom's real HTTP/1.1 (+ optional TLS 1.3 / HTTP/2) client from a registered\n" +
			"UE through the per-gNB userspace TCP stack and its GTP-U tunnel to the\n" +
			"HTTPOrigin a stock loomd agent (loom >= v0.11) serves at --peer. Objects are\n" +
			"fetched in a think-time loop; interval lines stream request/error counts,\n" +
			"TTFB p95, goodput, and connect/TLS handshake times, with correlation events\n" +
			"(handover phases, TCP_CONNS_RESET, GTP-U End Markers) inline; a both-end\n" +
			"report follows.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkAppDuration(duration); err != nil {
				return err
			}
			if objects < 0 {
				return fmt.Errorf("--objects must be >= 0, got %d", objects)
			}
			params := map[string]string{}
			if urlPath != "" {
				params["url_path"] = urlPath
			}
			if objects > 0 {
				params["objects"] = strconv.Itoa(objects)
			}
			if objectSize != "" {
				params["object_size"] = objectSize
			}
			if think != "" {
				params["think"] = think
			}
			params, err := tr.params(pr.params(params))
			if err != nil {
				return err
			}
			return runAppSession(cmd, serverURL, &orbitv1.StartAppRequest{
				Supi:       supi,
				App:        "http",
				Peer:       peer,
				Token:      token,
				PeerDataIp: peerDataIP,
				Params:     params,
				DurationMs: uint32(duration.Milliseconds()),
			}, jsonOut, fmt.Sprintf("http %s → %s for %s", supi, peer, duration))
		},
	}
	f := cmd.Flags()
	f.StringVar(&supi, "supi", "", "SUPI / IMSI of the registered UE")
	f.StringVar(&peer, "peer", "", "N6 loomd control address (host:port) on the management network")
	f.StringVar(&token, "token", "", "loomd control-plane bearer token")
	f.StringVar(&peerDataIP, "peer-data-ip", "", "N6 data address, when it differs from the --peer host")
	f.StringVar(&urlPath, "url-path", "", "fixed request path (e.g. /object/65536); overrides --object-size")
	f.IntVar(&objects, "objects", 0, "number of requests to issue (0 = bounded by --duration only)")
	f.StringVar(&objectSize, "object-size", "", `size distribution for generated /object/{bytes} paths, loom's size grammar ("100KB", "8KB..512KB"; default 100KB)`)
	f.StringVar(&think, "think", "", `inter-request think-time distribution, loom's duration grammar ("0", "200ms..2s"; default 0)`)
	tr.register(cmd)
	pr.register(cmd)
	f.DurationVar(&duration, "duration", 60*time.Second, "session duration (the far-end flow is duration-bounded)")
	f.BoolVar(&jsonOut, "json", false, "emit samples and the final report as JSON lines")
	_ = cmd.MarkFlagRequired("supi")
	_ = cmd.MarkFlagRequired("peer")
	return cmd
}

func newUEAppVideoCmd(serverURL *string) *cobra.Command {
	var (
		supi, peer, token, peerDataIP string
		urlName, ladder, abr          string
		segDuration                   time.Duration
		segments                      int
		startThreshold, bufferTarget  time.Duration
		rebufferTarget                time.Duration
		tr                            webTransportFlags
		pr                            portRangeFlags
		duration                      time.Duration
		jsonOut                       bool
	)
	cmd := &cobra.Command{
		Use:   "video",
		Short: "Play an ABR video stream from a UE and stream buffer/stall QoE",
		Long: "Run loom's ABR player from a registered UE through the per-gNB userspace TCP\n" +
			"stack against the HTTPOrigin a stock loomd agent (loom >= v0.11) serves at\n" +
			"--peer — the video far end IS the http origin (the player is client-only).\n" +
			"--ladder/--seg-duration/--segments configure the origin's generated HLS\n" +
			"ladder and double as the player's expectation. Interval lines stream buffer\n" +
			"level, bitrate, stalls, and startup delay; the report carries startup, stall\n" +
			"events, rebuffer ratio, and average bitrate, with handover correlation\n" +
			"(buffer drain → stall → ABR downshift) annotated.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkAppDuration(duration); err != nil {
				return err
			}
			if abr != "" && abr != "throughput" && abr != "buffer" {
				return fmt.Errorf(`--abr must be "throughput" or "buffer", got %q`, abr)
			}
			if segments < 0 {
				return fmt.Errorf("--segments must be >= 0, got %d", segments)
			}
			params := map[string]string{}
			if urlName != "" {
				params["url_name"] = urlName
			}
			if ladder != "" {
				params["ladder"] = ladder
			}
			if segDuration > 0 {
				params["seg_duration"] = segDuration.String()
			}
			if segments > 0 {
				params["segments"] = strconv.Itoa(segments)
			}
			if startThreshold > 0 {
				params["start_threshold"] = startThreshold.String()
			}
			if bufferTarget > 0 {
				params["buffer_target"] = bufferTarget.String()
			}
			if rebufferTarget > 0 {
				params["rebuffer_target"] = rebufferTarget.String()
			}
			if abr != "" {
				params["abr"] = abr
			}
			params, err := tr.params(pr.params(params))
			if err != nil {
				return err
			}
			return runAppSession(cmd, serverURL, &orbitv1.StartAppRequest{
				Supi:       supi,
				App:        "video",
				Peer:       peer,
				Token:      token,
				PeerDataIp: peerDataIP,
				Params:     params,
				DurationMs: uint32(duration.Milliseconds()),
			}, jsonOut, fmt.Sprintf("video %s → %s for %s", supi, peer, duration))
		},
	}
	f := cmd.Flags()
	f.StringVar(&supi, "supi", "", "SUPI / IMSI of the registered UE")
	f.StringVar(&peer, "peer", "", "N6 loomd control address (host:port) on the management network")
	f.StringVar(&token, "token", "", "loomd control-plane bearer token")
	f.StringVar(&peerDataIP, "peer-data-ip", "", "N6 data address, when it differs from the --peer host")
	f.StringVar(&urlName, "url-name", "", `media name on the origin (/media/{name}/manifest.m3u8; default "stream")`)
	f.StringVar(&ladder, "ladder", "", `bitrate ladder as label:rate pairs (default "240p:400k,480p:1200k,720p:2500k"); configures the origin and pins the player's expectation`)
	f.DurationVar(&segDuration, "seg-duration", 0, "origin segment duration (default 4s)")
	f.IntVar(&segments, "segments", 0, "segments per rendition playlist (default 150)")
	f.DurationVar(&startThreshold, "start-threshold", 0, "buffered media required before playback starts (default 2s)")
	f.DurationVar(&bufferTarget, "buffer-target", 0, "buffer level the player fetches ahead to (default 12s)")
	f.DurationVar(&rebufferTarget, "rebuffer-target", 0, "buffer level at which a stalled player resumes (default 4s)")
	f.StringVar(&abr, "abr", "", `ABR policy: "throughput" (default) or "buffer"`)
	tr.register(cmd)
	pr.register(cmd)
	f.DurationVar(&duration, "duration", 60*time.Second, "session duration bound (the far-end flow is duration-bounded)")
	f.BoolVar(&jsonOut, "json", false, "emit samples and the final report as JSON lines")
	_ = cmd.MarkFlagRequired("supi")
	_ = cmd.MarkFlagRequired("peer")
	return cmd
}
