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
	"github.com/free5gc/ngap/ngapType"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/join"
	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/webui"
)

// Options carries the optional server-level knobs for New.
type Options struct {
	// CoreProfile selects the core-compatibility profile ("" = strict-3gpp).
	CoreProfile string
	// LoomAgent/LoomToken are the default N6 loomd control address and bearer
	// token for app sessions (`orbit serve --loom-agent/--loom-token`);
	// per-call values override them.
	LoomAgent, LoomToken string
	// RunHistory bounds how many terminal runs the registry retains (0 = the
	// engine default). It bounds the on-disk archive equally, so the two do
	// not disagree about how far back history goes.
	RunHistory int
	// StateDir is where terminal runs are archived so history survives a
	// restart. Empty disables persistence entirely — no filesystem access at
	// all, which is what tests and ephemeral CI servers want.
	StateDir string
	// ConformanceExpected is the expected-failures list conformance runs
	// apply (orbit serve --conformance-expected-failures); nil for none. The
	// caller loads it, so a bad file fails startup.
	ConformanceExpected *conformance.ExpectedFailures
	// Token is the bearer token every Connect procedure and /metrics
	// require ("" = none). /healthz and the dashboard's static files stay
	// open.
	Token string
	// TLS says orbit serve has TLS; without it nothing join is served
	// (ADR-0009 §1). TLSPin is the server's key, sha256/<base64>, which a
	// join one-liner pins, and Listen its listen address, host:port.
	TLS    bool
	TLSPin string
	Listen string
	// JoinBinaries are the orbit binaries a join serves, by arch (amd64,
	// arm64), path to file.
	JoinBinaries map[string]string
}

// New builds the ORBIT HTTP handler. The http.Server serving it enables
// unencrypted HTTP/2 when plaintext (cli's newHTTPServer), so gRPC clients
// connect without TLS; `orbit serve` allows plaintext only on loopback
// unless --insecure-listen (ADR-0008 §4).
// Every procedure refuses subscriber keys and tokens over plaintext from a
// non-loopback peer, and opts.Token, when set, guards the API.
func New(log *slog.Logger, version string, reg *prometheus.Registry, opts Options) http.Handler {
	return newHandler(log, version, reg, opts, newSecurity(log, opts.Token))
}

func newHandler(log *slog.Logger, version string, reg *prometheus.Registry, opts Options, sec *security) http.Handler {
	ic := connect.WithInterceptors(keysInterceptor{})
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
	runReg := engine.NewRunRegistry(log, opts.RunHistory)
	history := opts.RunHistory
	if history <= 0 {
		history = engine.DefaultRunHistory
	}
	archive, err := newArchiveStore(log, opts.StateDir, history)
	if err != nil {
		// A state directory that cannot be opened is a configuration error, but
		// it must not cost the operator a working server: run without
		// persistence and say so, rather than refusing to start.
		log.Error("run history will not persist", "dir", opts.StateDir, "err", err)
		archive, _ = newArchiveStore(log, "", history)
	}
	if archive.enabled() {
		log.Info("run history persists", "dir", opts.StateDir, "keep", history)
	} else {
		log.Warn("run history is in memory only; it will be lost on restart")
	}
	// gNB IDs a plan asks the server to assign rotate least recently used
	// first, persisted with the run history (ADR-0008 §5).
	rotation, err := engine.OpenGNBRotation(opts.StateDir, log)
	if err != nil {
		log.Error("gNB ID rotation will not persist", "dir", opts.StateDir, "err", err)
		rotation, _ = engine.OpenGNBRotation("", log)
	}
	// Responders registered by join (ADR-0009 §3), persisted with the run
	// history. A registry that cannot be read is not overwritten: the
	// server runs with an empty, unpersisted one and says so, and its
	// responders keep retrying until it is restored.
	responders, err := join.OpenRegistry(opts.StateDir)
	if err != nil {
		log.Error("registered responders could not be read; running without them, and not persisting", "dir", opts.StateDir, "err", err)
		responders, _ = join.OpenRegistry("")
	} else if !responders.Persistent() {
		log.Warn("registered responders are in memory only: after a restart every app host must join again (serve with --state-dir)")
	}
	runSvc := &runService{log: log, reg: runReg, mgr: mgr, archive: archive,
		specs: newSpecStore(), version: version, rotation: rotation, expected: opts.ConformanceExpected, responders: responders}
	runSvc.restoreFrames()

	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewSystemServiceHandler(&systemService{version: version}, ic))
	mux.Handle(orbitv1connect.NewCellServiceHandler(&cellService{log: log}, ic))
	mux.Handle(orbitv1connect.NewUEServiceHandler(&ueService{log: log, mgr: mgr, apps: mgr}, ic))
	mux.Handle(orbitv1connect.NewRunServiceHandler(runSvc, ic))
	mux.Handle(orbitv1connect.NewConformanceServiceHandler(&conformanceService{log: log, expected: opts.ConformanceExpected, runs: runSvc}, ic))
	joinSvc := newJoinService(log, sec, opts, responders)
	mux.Handle(orbitv1connect.NewJoinServiceHandler(joinSvc, ic))
	mux.Handle(orbitv1connect.NewResponderServiceHandler(&responderService{log: log, reg: responders}, ic))
	// A join token is its own credential: /join/ is outside the bearer
	// guard (protected), and served over TLS only.
	mux.Handle("/join/", joinSvc)
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	// The dashboard takes the catch-all route; the Connect handlers above
	// register explicit /<package>.<Service>/ prefixes, so they still win.
	mux.Handle("/", webui.Handler())
	if !webui.Available() {
		log.Warn("dashboard assets not embedded; run `make ui` to build them")
	}
	return sec.middleware(mux)
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

// maxNGSetupHold caps RunNGSetupRequest.hold_ms so a unary call cannot keep
// an association, and the RPC, open indefinitely.
const maxNGSetupHold = 10 * time.Minute

// defaultNGSetupTimeout bounds the dial plus the NG Setup exchange when the
// request carries no timeout_ms.
const defaultNGSetupTimeout = 10 * time.Second

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
	if _, err := sctp.NormalizeLocal(msg.GetLocalAddress()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	hold := time.Duration(msg.GetHoldMs()) * time.Millisecond
	if hold > maxNGSetupHold {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("hold_ms %d exceeds the cap of %d ms (%s)", msg.GetHoldMs(), maxNGSetupHold.Milliseconds(), maxNGSetupHold))
	}

	// timeout_ms bounds the dial and the NG Setup exchange; the hold, if
	// any, runs afterwards on its own clock and never extends that budget,
	// so the setup gets its own context here while the hold answers to the
	// RPC's (client cancel).
	timeout := time.Duration(msg.GetTimeoutMs()) * time.Millisecond
	if timeout == 0 {
		timeout = defaultNGSetupTimeout
	}
	setupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	s.log.InfoContext(ctx, "running NG Setup",
		"amf", msg.GetAmfAddress(), "gnb_id", cfg.ID, "plmn", cfg.MCC+"/"+cfg.MNC, "hold", hold)

	conn, err := sctp.Dial(setupCtx, msg.GetLocalAddress(), msg.GetAmfAddress())
	if err != nil {
		return nil, n2Error(err)
	}

	res, err := gnb.NGSetup(setupCtx, conn, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	s.log.InfoContext(ctx, "NG Setup finished",
		"accepted", res.Accepted, "amf_name", res.AMFName, "cause", res.Cause, "reply_ppid", res.ReplyPPID)

	resp := &orbitv1.RunNGSetupResponse{
		Accepted:  res.Accepted,
		AmfName:   res.AMFName,
		Cause:     res.Cause,
		ReplyPpid: res.ReplyPPID,
	}
	if la := conn.LocalAddr(); la != nil {
		resp.LocalAddress = la.String()
	}
	// A rejected gNB holding its association is not the behaviour a hold
	// measures, so only an accepted setup is held.
	if hold > 0 && res.Accepted {
		held := s.holdAssociation(ctx, conn, hold)
		resp.HeldMs = uint32(held / time.Millisecond)
	} else {
		_ = conn.Close()
	}
	return connect.NewResponse(resp), nil
}

// holdAssociation keeps conn open until hold elapses, ctx is done (client
// cancel or RPC deadline), or the peer closes it, whichever comes first, and
// closes it exactly once. SCTP heartbeat is kernel-level, so observing it
// needs only the open socket; the receive buffer must still be drained, so a
// reader logs each inbound NGAP PDU by procedure code and stream instead of
// letting it fill. conn.Close is what unblocks that reader on the timer and
// cancel paths. Returns how long the association was held, measured from the
// NG Setup Response to the close.
func (s *cellService) holdAssociation(ctx context.Context, conn *sctp.Conn, hold time.Duration) time.Duration {
	start := time.Now()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 65536)
		for {
			payload, stream, ppid, err := conn.ReadMsg(buf)
			if err != nil {
				return // peer closed or aborted, or we closed below
			}
			kind, proc, derr := describeNGAP(payload)
			if derr != nil {
				s.log.WarnContext(ctx, "undecodable NGAP during hold", "stream", stream, "ppid", ppid, "bytes", len(payload), "err", derr)
				continue
			}
			s.log.InfoContext(ctx, "NGAP during hold", "kind", kind, "proc", proc, "stream", stream, "ppid", ppid, "bytes", len(payload))
		}
	}()
	timer := time.NewTimer(hold)
	defer timer.Stop()
	why := "hold elapsed"
	select {
	case <-timer.C:
	case <-ctx.Done():
		why = "cancelled: " + ctx.Err().Error()
	case <-readDone:
		why = "peer closed"
	}
	_ = conn.Close()
	<-readDone
	held := time.Since(start)
	s.log.InfoContext(ctx, "NG Setup hold ended", "held", held.Round(time.Millisecond), "requested", hold, "why", why)
	return held
}

// describeNGAP names an NGAP PDU for the hold log: its kind (initiating,
// successful or unsuccessful outcome) and procedure code.
func describeNGAP(raw []byte) (kind string, proc int64, err error) {
	pdu, err := ngap.Decode(raw)
	if err != nil {
		return "", 0, err
	}
	switch pdu.Present {
	case ngapType.NGAPPDUPresentInitiatingMessage:
		return "initiating", pdu.InitiatingMessage.ProcedureCode.Value, nil
	case ngapType.NGAPPDUPresentSuccessfulOutcome:
		return "successful-outcome", pdu.SuccessfulOutcome.ProcedureCode.Value, nil
	case ngapType.NGAPPDUPresentUnsuccessfulOutcome:
		return "unsuccessful-outcome", pdu.UnsuccessfulOutcome.ProcedureCode.Value, nil
	default:
		return "", 0, fmt.Errorf("PDU present=%d is not a known choice", pdu.Present)
	}
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

// gnbConfigFromProto maps a request's gNB config and validates its network
// identity with the same rule the CLI applies (gnb.Config.ValidateIdentity),
// so the API rejects exactly what the CLI rejects.
func gnbConfigFromProto(p *orbitv1.GnbConfig) (gnb.Config, error) {
	cfg, err := gnbTargetFromProto(p)
	if err != nil {
		return cfg, err
	}
	if err := cfg.ValidateIdentity(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// gnbConfigToProto maps a gNB config to the API message, the inverse of
// gnbTargetFromProto.
func gnbConfigToProto(c gnb.Config) *orbitv1.GnbConfig {
	p := &orbitv1.GnbConfig{Id: c.ID, IdBits: uint32(c.IDBits), Name: c.Name, Mcc: c.MCC, Mnc: c.MNC, Tac: c.TAC}
	for _, s := range c.Slices {
		p.Slices = append(p.Slices, &orbitv1.Snssai{Sst: uint32(s.SST), Sd: s.SD})
	}
	return p
}

// gnbTargetFromProto maps a gNB config WITHOUT validating its network
// identity, for a handover target whose unset fields the engine fills from
// the UE's serving gNB before validating (engine.GNBEndpoint).
func gnbTargetFromProto(p *orbitv1.GnbConfig) (gnb.Config, error) {
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
		if err := gnb.ValidateSNSSAI(s.GetSst(), s.GetSd()); err != nil {
			return cfg, err
		}
		cfg.Slices = append(cfg.Slices, gnb.SNSSAI{SST: uint8(s.GetSst()), SD: s.GetSd()})
	}
	return cfg, nil
}
