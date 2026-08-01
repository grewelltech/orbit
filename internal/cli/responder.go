package cli

import (
	"fmt"
	"net"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgrewell/loom/control"
)

// newResponderCmd builds `orbit responder` — the far end of an application
// flow, hosted in the ORBIT binary (ADR-0007).
//
// The agent itself is loom's, constructed exactly as cmd/loomd does:
// control.NewServer → control.NewGRPCServer → Serve. ORBIT supplies the process
// and the flags; responder behaviour lives in loom.
func newResponderCmd(version string) *cobra.Command {
	var (
		bind              string
		token             string
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
			"Stock loomd remains fully supported and is interchangeable with this.",
		RunE: func(cmd *cobra.Command, args []string) error {
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

			fmt.Fprintf(out, "orbit responder %s (loom agent) listening on %s\n", version, lis.Addr())
			if err := gs.Serve(lis); err != nil {
				return fmt.Errorf("responder serve: %w", err)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&bind, "bind", "", "control-plane listen address (host:port) — required, no default")
	f.StringVar(&token, "token", "", "control-plane bearer token; required in practice on a routable --bind")
	f.DurationVar(&telemetryInterval, "telemetry-interval", 0, "telemetry sampling interval (0 = loom default)")
	f.IntVar(&maxFlows, "max-flows", 0, "maximum concurrent flows (0 = loom default)")
	_ = cmd.MarkFlagRequired("bind")
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
