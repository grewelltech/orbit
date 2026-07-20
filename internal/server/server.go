// Package server exposes the ORBIT engine through the Connect API
// (DESIGN §3): one net/http mux serving gRPC, gRPC-Web, and Connect/JSON
// simultaneously, plus /metrics and /healthz. The server is a thin façade —
// procedure logic lives in the engine packages it calls.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/sctp"
)

// Options carries the optional server-level knobs for New.
type Options struct {
	// CoreProfile selects the core-compatibility profile ("" = strict-3gpp).
	CoreProfile string
	// LoomAgent/LoomToken are the default N6 loomd control address and bearer
	// token for app sessions (`orbit serve --loom-agent/--loom-token`);
	// per-call values override them.
	LoomAgent, LoomToken string
}

// New builds the ORBIT HTTP handler. The h2c wrapper lets gRPC clients
// connect without TLS on lab-internal listeners; the API is not meant to be
// externally exposed (DESIGN §8, credential handling).
func New(log *slog.Logger, version string, reg *prometheus.Registry, opts Options) http.Handler {
	mgr := engine.NewManager(log)
	mgr.EnableAppMetrics(reg)
	if opts.CoreProfile != "" {
		if err := mgr.UseProfile(opts.CoreProfile); err != nil {
			log.Warn("core profile not applied; using strict-3gpp", "err", err)
		} else {
			log.Info("core compatibility profile active", "profile", opts.CoreProfile)
		}
	}
	if opts.LoomAgent != "" || opts.LoomToken != "" {
		mgr.SetLoomDefaults(opts.LoomAgent, opts.LoomToken)
		log.Info("default loom agent configured", "agent", opts.LoomAgent)
	}
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewSystemServiceHandler(&systemService{version: version}))
	mux.Handle(orbitv1connect.NewCellServiceHandler(&cellService{log: log}))
	mux.Handle(orbitv1connect.NewUEServiceHandler(&ueService{log: log, mgr: mgr, apps: mgr}))
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return h2c.NewHandler(mux, &http2.Server{})
}

type systemService struct {
	version string
}

func (s *systemService) GetInfo(
	ctx context.Context,
	req *connect.Request[orbitv1.GetInfoRequest],
) (*connect.Response[orbitv1.GetInfoResponse], error) {
	return connect.NewResponse(&orbitv1.GetInfoResponse{
		Version:   s.version,
		GoVersion: runtime.Version(),
	}), nil
}

type cellService struct {
	log *slog.Logger
}

func (s *cellService) RunNGSetup(
	ctx context.Context,
	req *connect.Request[orbitv1.RunNGSetupRequest],
) (*connect.Response[orbitv1.RunNGSetupResponse], error) {
	msg := req.Msg
	if msg.GetAmfAddress() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("amf_address is required"))
	}
	cfg, err := gnbConfigFromProto(msg.GetGnb())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	timeout := time.Duration(msg.GetTimeoutMs()) * time.Millisecond
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	s.log.InfoContext(ctx, "running NG Setup",
		"amf", msg.GetAmfAddress(), "gnb_id", cfg.ID, "plmn", cfg.MCC+"/"+cfg.MNC)

	conn, err := sctp.Dial(msg.GetLocalAddress(), msg.GetAmfAddress())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	defer conn.Close()

	res, err := gnb.NGSetup(ctx, conn, cfg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	s.log.InfoContext(ctx, "NG Setup finished",
		"accepted", res.Accepted, "amf_name", res.AMFName, "cause", res.Cause, "reply_ppid", res.ReplyPPID)

	return connect.NewResponse(&orbitv1.RunNGSetupResponse{
		Accepted:  res.Accepted,
		AmfName:   res.AMFName,
		Cause:     res.Cause,
		ReplyPpid: res.ReplyPPID,
	}), nil
}

// withTimeout derives a context with a millisecond timeout, falling back to
// defaultMs when ms is zero.
func withTimeout(ctx context.Context, ms uint32, defaultMs uint32) (context.Context, context.CancelFunc) {
	d := time.Duration(ms) * time.Millisecond
	if d == 0 {
		d = time.Duration(defaultMs) * time.Millisecond
	}
	return context.WithTimeout(ctx, d)
}

func gnbConfigFromProto(p *orbitv1.GnbConfig) (gnb.Config, error) {
	var cfg gnb.Config
	if p == nil {
		return cfg, fmt.Errorf("gnb config is required")
	}
	cfg = gnb.Config{
		ID:     p.GetId(),
		IDBits: int(p.GetIdBits()),
		Name:   p.GetName(),
		MCC:    p.GetMcc(),
		MNC:    p.GetMnc(),
		TAC:    p.GetTac(),
	}
	for _, s := range p.GetSlices() {
		if s.GetSst() > 0xFF {
			return cfg, fmt.Errorf("sst %d exceeds one octet", s.GetSst())
		}
		cfg.Slices = append(cfg.Slices, gnb.SNSSAI{SST: uint8(s.GetSst()), SD: s.GetSd()})
	}
	return cfg, nil
}
