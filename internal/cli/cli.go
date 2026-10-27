// Package cli implements the orbit command tree. Every command except
// `serve` is a Connect API client — the CLI never touches the engine
// directly, so nothing is CLI-only and the API can never drift behind
// (DESIGN §3).
package cli

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/server"
	"github.com/bgrewell/orbit/internal/tlspin"
)

// bindFlagHelp is the one help text for --bind on every command that opens
// an N2 association, so the flag reads the same everywhere and the N2/N3
// pairing is explicit (--bind is N2; --gnb-n3 / --n3-bind are N3).
const bindFlagHelp = "local N2 source address for the gNB's SCTP association, ip[:port]; " +
	"empty binds the address the host routes to the AMF from (N3 is --gnb-n3 / --n3-bind)"

// supiFlagHelp is the one help text for every --supi flag, so each names the
// two accepted spellings; the server normalises both to digits.
const supiFlagHelp = "SUPI, digits or imsi-<digits> (e.g. 001010100007500)"

// baseIMSIFlagHelp is the one help text for --base-imsi on load commands.
const baseIMSIFlagHelp = "first " + supiFlagHelp + "; each UE increments it"

// DefaultListen is the default API listen address. Loopback by default:
// the API carries subscriber credentials from Phase 1a on and is not meant
// to be exposed beyond the lab host without TLS.
const DefaultListen = "127.0.0.1:8412"

// New builds the root command. version is stamped at build time.
func New(version string, opts ...Option) *cobra.Command {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	var serverURL string

	root := &cobra.Command{
		Use:           "orbit",
		Short:         "ORBIT — Open Radio Benchmark and Integration Testbed",
		Long:          "A PHY-less software gNB and UE for 5G SA, and a test harness built on them.\nSee docs/DESIGN.md for the architecture and phased plan.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&serverURL, "server", "", serverFlagHelp)

	root.AddCommand(newVersionCmd(version))
	root.AddCommand(newServeCmd(version))
	// The API clients resolve --server / $ORBIT_SERVER once, in a group-level
	// hook, before any of their subcommands builds a client (serverurl.go).
	// `run` resolves inside RunE instead: a `kind: fleet` scenario never talks
	// to the server, so only its step path reads the variable.
	root.AddCommand(withServerURL(newSystemCmd(&serverURL), &serverURL))
	root.AddCommand(withServerURL(newCellCmd(&serverURL), &serverURL))
	root.AddCommand(withServerURL(newUECmd(&serverURL), &serverURL))
	root.AddCommand(newRunCmd(&serverURL, o))
	root.AddCommand(newTestCmd(&serverURL, o))
	root.AddCommand(withServerURL(newRunsCmd(&serverURL, o), &serverURL))
	root.AddCommand(newLoadCmd())
	root.AddCommand(newConformanceCmd())
	root.AddCommand(newResponderCmd(version))
	root.AddCommand(newServerCmd())
	root.AddCommand(newTargetCmd(&serverURL, o)) // resolves --server in RunE (init, probe)
	installTargets(root, o)
	return root
}

func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of this binary",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "orbit %s\n", version)
		},
	}
}

func newServeCmd(version string) *cobra.Command {
	var listen string
	var logLevel string
	var coreProfile string
	var loomAgent, loomToken string
	var stateDir string
	var runHistory int
	var expectedPath string
	var tlsMode, tlsCert, tlsKey, tokenFile string
	var noAuth, insecureListen bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the ORBIT API server",
		Long: "Run the ORBIT API server and dashboard. On loopback (the default) it\n" +
			"serves plaintext and needs no token. Any other --listen needs TLS\n" +
			"(--tls auto, or --tls-cert/--tls-key) unless --insecure-listen, and a\n" +
			"bearer token (--token-file) unless --no-auth. Subscriber keys and tokens\n" +
			"are refused over plaintext from off the host either way.",
		RunE: func(cmd *cobra.Command, args []string) error {
			level := slog.LevelInfo
			if err := level.UnmarshalText([]byte(logLevel)); err != nil {
				return fmt.Errorf("invalid --log-level %q: %w", logLevel, err)
			}
			log := observability.NewLogger(os.Stderr, level)
			dir, err := resolveStateDir(stateDir)
			if err != nil {
				return err
			}
			// Settled before anything binds: a listener that would expose the
			// API without TLS or a token fails here, not after it is reachable.
			sec, err := resolveServeSecurity(listen, tlsMode, tlsCert, tlsKey, tokenFile, dir, noAuth, insecureListen)
			if err != nil {
				return err
			}
			if sec.leaf != nil {
				if sec.repaired != "" {
					log.Warn("TLS certificate regenerated", "reason", sec.repaired)
				}
				log.Info("TLS", "spki", tlspin.Fingerprint(sec.leaf), "subject", sec.leaf.Subject.String(),
					"pin_with", "orbit server trust https://"+listen)
			}
			if sec.token == "" {
				if loopback, _ := listenLoopback(listen); !loopback {
					log.Warn("NO AUTHENTICATION: --no-auth on a non-loopback listener; anyone who can reach " + listen + " can drive the API")
				}
			}

			// Loaded before anything starts: a bad list fails startup
			// rather than serving runs that gate on the wrong FAILs.
			var expected *conformance.ExpectedFailures
			if expectedPath != "" {
				x, err := conformance.LoadExpectedFailures(expectedPath)
				if err != nil {
					return fmt.Errorf("--conformance-expected-failures: %w", err)
				}
				expected = x
				log.Info("conformance expected-failures list loaded", "core", x.Core, "entries", len(x.Entries), "path", expectedPath)
			}

			shutdown, err := observability.SetupTracing(cmd.Context(),
				os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), "orbit", version)
			if err != nil {
				return fmt.Errorf("tracing setup: %w", err)
			}
			defer func() {
				if err := shutdown(cmd.Context()); err != nil {
					log.Error("tracing shutdown", "err", err)
				}
			}()

			reg := observability.NewRegistry()
			handler := server.New(log, version, reg, server.Options{
				Token:       sec.token,
				CoreProfile: coreProfile,
				LoomAgent:   loomAgent,
				LoomToken:   loomToken,
				StateDir:    dir,
				RunHistory:  runHistory,

				ConformanceExpected: expected,
			})

			srv := newHTTPServer(listen, handler, sec.tls)
			log.Info("orbit API listening", "addr", listen, "version", version, "tls", sec.tls != nil, "auth", sec.token != "")
			serve := srv.ListenAndServe
			if sec.tls != nil {
				serve = func() error { return srv.ListenAndServeTLS("", "") }
			}
			if err := serve(); err != nil && err != http.ErrServerClosed {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "",
		"where finished runs are archived so history survives a restart "+
			"(default $XDG_STATE_HOME/orbit/runs; \"none\" disables persistence)")
	cmd.Flags().IntVar(&runHistory, "run-history", 0,
		"how many finished runs to keep, in memory and on disk (0 = default 50)")
	cmd.Flags().StringVar(&expectedPath, "conformance-expected-failures", "",
		"YAML list of the core's known, tolerated conformance FAILs that dashboard and API runs apply (docs/conformance/expected-failures.md)")
	cmd.Flags().StringVar(&listen, "listen", DefaultListen, "listen address (host:port); anything but loopback needs TLS and a token")
	cmd.Flags().StringVar(&tlsMode, "tls", "", "auto: serve TLS with a self-signed certificate kept in --state-dir/tls (pin it with orbit server trust)")
	cmd.Flags().StringVar(&tlsCert, "tls-cert", "", "TLS certificate (PEM), with --tls-key")
	cmd.Flags().StringVar(&tlsKey, "tls-key", "", "TLS private key (PEM), with --tls-cert")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "file whose first line is the bearer token every API call needs (at least 32 characters, mode 600)")
	cmd.Flags().BoolVar(&noAuth, "no-auth", false, "serve a non-loopback listener with no token (anyone who reaches it drives the API)")
	cmd.Flags().BoolVar(&insecureListen, "insecure-listen", false, "serve a non-loopback listener in plaintext")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	cmd.Flags().StringVar(&coreProfile, "core-profile", "", "core compatibility profile (strict-3gpp, sdcore); default strict-3gpp")
	cmd.Flags().StringVar(&loomAgent, "loom-agent", "", "default N6 loomd control address (host:port) for app sessions; per-call --peer overrides")
	cmd.Flags().StringVar(&loomToken, "loom-token", "", "default loomd control-plane bearer token for app sessions")
	return cmd
}

func systemClient(url *string) orbitv1connect.SystemServiceClient {
	h, guard := apiClient(*url)
	return orbitv1connect.NewSystemServiceClient(h, *url, guard)
}

func cellClient(url *string) orbitv1connect.CellServiceClient {
	h, guard := apiClient(*url)
	return orbitv1connect.NewCellServiceClient(h, *url, guard)
}

func ueClient(url *string) orbitv1connect.UEServiceClient {
	h, guard := apiClient(*url)
	return orbitv1connect.NewUEServiceClient(h, *url, guard)
}

// newHTTPServer is serve's http.Server: TLS when tlsConf is set (HTTP/2 by
// ALPN), else plaintext HTTP/1.1 and unencrypted HTTP/2 (prior knowledge),
// so gRPC clients connect to a loopback server without TLS.
func newHTTPServer(listen string, h http.Handler, tlsConf *tls.Config) *http.Server {
	srv := &http.Server{
		Addr:              listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsConf,
	}
	if tlsConf == nil {
		var p http.Protocols
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
		srv.Protocols = &p
	}
	return srv
}

// resolveStateDir turns the --state-dir flag into a path.
//
// Order: an explicit flag, then the XDG state directory, then the system
// default. XDG is right for an interactive user, but a daemon usually has
// neither XDG_STATE_HOME nor HOME — systemd sets no environment by default —
// so /var/lib is the fallback rather than giving up.
//
// It never silently returns "": persistence turning itself off because an
// environment variable was missing is exactly the failure an operator would
// not notice until the history they expected was gone. Only "none" disables
// it, and that is deliberate and visible.
func resolveStateDir(flag string) (string, error) {
	if flag == "none" {
		return "", nil
	}
	if flag != "" {
		return flag, nil
	}
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "orbit", "runs"), nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "orbit", "runs"), nil
	}
	return filepath.Join(SystemStateDir, "runs"), nil
}

// SystemStateDir is where a service with no user environment keeps state, per
// the FHS: /var/lib is for data that persists between restarts of the program.
const SystemStateDir = "/var/lib/orbit"
