package cli

import (
	"errors"
	"fmt"
	"net"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgrewell/loom/control"

	"github.com/bgrewell/orbit/internal/join"
)

// newResponderCmd builds `orbit responder` — the far end of an application
// flow, hosted in the ORBIT binary (ADR-0007).
//
// The agent itself is loom's, constructed exactly as cmd/loomd does:
// control.NewServer → control.NewGRPCServer → Serve. ORBIT supplies the process
// and the flags; responder behaviour lives in loom.
func newResponderCmd(version string, serverURL *string) *cobra.Command {
	var (
		bind              string
		token             string
		tokenFile         string
		config            string
		telemetryInterval time.Duration
		maxFlows          int
	)
	cmd := &cobra.Command{
		Use:   "responder",
		Short: "Run the loom responder that application traffic runs against",
		Long: "Host loom's control-plane agent in the ORBIT binary, so a benchmark run\n" +
			"does not need a separate loomd install. This is the far end of every\n" +
			"application flow — place it on the N6 network (or on localhost for a\n" +
			"single-node run) and point app sessions at it with --peer, or set it as\n" +
			"the default with `orbit serve --loom-agent`.\n\n" +
			"--bind is required and has no default: an agent is a remotely-aimable\n" +
			"traffic generator, so its reachability is always an explicit choice.\n\n" +
			"A responder registered by `orbit join` runs with --config (its\n" +
			"/etc/orbit/responder.yaml): the bind and the token come from the file,\n" +
			"never the command line, and it keeps a heartbeat to its server.\n\n" +
			"Stock loomd remains fully supported and is interchangeable with this.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var cfg join.Config
			if config != "" {
				c, err := join.LoadConfig(config)
				if err != nil {
					return err
				}
				cfg = c
				if bind == "" {
					bind = cfg.Control
				}
				token = cfg.LoomToken
			}
			if tokenFile != "" {
				if token != "" {
					return errors.New("--token-file: give one of --token, --token-file and --config")
				}
				b, err := join.ReadSecretFile(tokenFile)
				if err != nil {
					return fmt.Errorf("--token-file: %w", err)
				}
				token = strings.TrimSpace(string(b))
			}
			if bind == "" {
				return errors.New("--bind is required (or --config)")
			}
			if _, _, err := net.SplitHostPort(bind); err != nil {
				return fmt.Errorf("invalid --bind %q (want host:port): %w", bind, err)
			}
			if telemetryInterval < 0 {
				return fmt.Errorf("--telemetry-interval must not be negative, got %s", telemetryInterval)
			}
			if maxFlows < 0 {
				return fmt.Errorf("--max-flows must not be negative, got %d", maxFlows)
			}

			lis, err := net.Listen("tcp", bind)
			if err != nil {
				return fmt.Errorf("listen %s: %w", bind, err)
			}

			opts := []control.Option{}
			if telemetryInterval > 0 {
				opts = append(opts, control.WithTelemetryInterval(telemetryInterval))
			}
			if maxFlows > 0 {
				opts = append(opts, control.WithMaxFlows(maxFlows))
			}
			if token != "" {
				opts = append(opts, control.WithAuthToken(token))
			}

			srv := control.NewServer(version, opts...)
			out := cmd.OutOrStdout()
			if !srv.AuthEnabled() && !isHostLocal(lis.Addr()) {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"WARNING: responder is listening on routable address %s with no --token — "+
						"the control plane is unauthenticated and anyone who can reach it can generate traffic\n",
					lis.Addr())
			}

			gs := control.NewGRPCServer(srv)

			// SIGINT/SIGTERM drain in-flight RPCs rather than cutting flows dead.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			go func() {
				<-ctx.Done()
				gs.GracefulStop()
			}()

			// A registered responder beats to its server; a removal stops it.
			var listening atomic.Bool
			var removedErr atomic.Pointer[error]
			if config != "" {
				client, err := join.Client(cfg.Server, cfg.Pin)
				if err != nil {
					return err
				}
				hb := &join.Heartbeater{Client: client, Credential: cfg.Credential, ID: cfg.ID, Version: version,
					Listening: listening.Load, Log: cmdLogger(cmd)}
				go func() {
					if err := hb.Run(ctx); errors.Is(err, join.ErrResponderRemoved) {
						e := error(&ExitError{Code: ExitResponderRemoved, Err: err})
						removedErr.Store(&e)
						stop()
					}
				}()
			}

			fmt.Fprintf(out, "orbit responder %s (loom agent) listening on %s\n", version, lis.Addr())
			listening.Store(true)
			if err := gs.Serve(lis); err != nil {
				return fmt.Errorf("responder serve: %w", err)
			}
			if e := removedErr.Load(); e != nil {
				return *e
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&bind, "bind", "", "control-plane listen address (host:port) — required unless --config gives it")
	f.StringVar(&token, "token", "", "control-plane bearer token; required in practice on a routable --bind (it shows in ps: prefer --token-file)")
	f.StringVar(&tokenFile, "token-file", "", "read the control-plane bearer token from this file (mode 0600)")
	f.StringVar(&config, "config", "", "a registered responder's config (orbit join: /etc/orbit/responder.yaml); the bind, the token and the heartbeat come from it")
	f.DurationVar(&telemetryInterval, "telemetry-interval", 0, "telemetry sampling interval (0 = loom default)")
	f.IntVar(&maxFlows, "max-flows", 0, "maximum concurrent flows (0 = loom default)")
	cmd.MarkFlagsMutuallyExclusive("token", "token-file")
	cmd.MarkFlagsMutuallyExclusive("token", "config")
	cmd.AddCommand(newResponderRegisterCmd(version), newResponderCheckCmd(),
		withServerURL(newResponderListCmd(serverURL), serverURL), withServerURL(newResponderRemoveCmd(serverURL), serverURL))
	return cmd
}

// isHostLocal reports whether addr is reachable only from this host, which is
// the case for loopback and for the unspecified address only when it is not.
// Rationale: 0.0.0.0 and :: accept from every interface, so they are treated as
// routable and warned about.
func isHostLocal(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
