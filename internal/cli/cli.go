// Package cli implements the orbit command tree. Every command except
// `serve` is a Connect API client — the CLI never touches the engine
// directly, so nothing is CLI-only and the API can never drift behind
// (DESIGN §3).
package cli

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/server"
)

// DefaultListen is the default API listen address. Loopback by default:
// the API carries subscriber credentials from Phase 1a on and is not meant
// to be exposed beyond the lab host without TLS.
const DefaultListen = "127.0.0.1:8412"

// New builds the root command. version is stamped at build time.
func New(version string) *cobra.Command {
	var serverURL string

	root := &cobra.Command{
		Use:           "orbit",
		Short:         "ORBIT — Open Radio Benchmark and Integration Testbed",
		Long:          "PHY-less 5G SA gNB + UE simulator and test harness.\nSee docs/DESIGN.md for the architecture and phased plan.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&serverURL, "server", "http://"+DefaultListen,
		"base URL of the ORBIT API server")

	root.AddCommand(newVersionCmd(version))
	root.AddCommand(newServeCmd(version))
	root.AddCommand(newSystemCmd(&serverURL))
	root.AddCommand(newCellCmd(&serverURL))
	root.AddCommand(newUECmd(&serverURL))
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
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the ORBIT API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			level := slog.LevelInfo
			if err := level.UnmarshalText([]byte(logLevel)); err != nil {
				return fmt.Errorf("invalid --log-level %q: %w", logLevel, err)
			}
			log := observability.NewLogger(os.Stderr, level)

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
			handler := server.New(log, version, reg, coreProfile)

			srv := &http.Server{
				Addr:              listen,
				Handler:           handler,
				ReadHeaderTimeout: 10 * time.Second,
			}
			log.Info("orbit API listening", "addr", listen, "version", version)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&listen, "listen", DefaultListen, "listen address (host:port)")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	cmd.Flags().StringVar(&coreProfile, "core-profile", "", "core compatibility profile (strict-3gpp, sdcore); default strict-3gpp")
	return cmd
}

func systemClient(url *string) orbitv1connect.SystemServiceClient {
	return orbitv1connect.NewSystemServiceClient(http.DefaultClient, *url)
}

func cellClient(url *string) orbitv1connect.CellServiceClient {
	return orbitv1connect.NewCellServiceClient(http.DefaultClient, *url)
}

func ueClient(url *string) orbitv1connect.UEServiceClient {
	return orbitv1connect.NewUEServiceClient(http.DefaultClient, *url)
}
