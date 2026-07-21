package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// runService adapts the engine.RunRegistry to the RunService RPCs. The server
// owns run execution (ADR-0005): StartRun launches the run here and returns its
// identity, and the run outlives the client that started it.
type runService struct {
	log *slog.Logger
	reg *engine.RunRegistry
}

func (s *runService) StartRun(
	ctx context.Context,
	req *connect.Request[orbitv1.StartRunRequest],
) (*connect.Response[orbitv1.StartRunResponse], error) {
	m := req.Msg
	switch spec := m.GetSpec().(type) {
	case *orbitv1.StartRunRequest_Load:
		fn, err := loadRunFunc(s.log, spec.Load)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		info, err := s.reg.StartLoad(m.GetName(), fn)
		if err != nil {
			return nil, runStartError(err)
		}
		return connect.NewResponse(&orbitv1.StartRunResponse{Run: runProto(info)}), nil
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("a run spec is required (only load is supported)"))
	}
}

func (s *runService) StopRun(
	ctx context.Context,
	req *connect.Request[orbitv1.StopRunRequest],
) (*connect.Response[orbitv1.StopRunResponse], error) {
	info, err := s.reg.Stop(req.Msg.GetRunId())
	if err != nil {
		return nil, runLookupError(err)
	}
	return connect.NewResponse(&orbitv1.StopRunResponse{Run: runProto(info)}), nil
}

func (s *runService) ListRuns(
	ctx context.Context,
	req *connect.Request[orbitv1.ListRunsRequest],
) (*connect.Response[orbitv1.ListRunsResponse], error) {
	filter := runKindFromProto(req.Msg.GetKind())
	var out []*orbitv1.Run
	for _, info := range s.reg.List() {
		if filter != "" && info.Kind != filter {
			continue
		}
		out = append(out, runProto(info))
	}
	return connect.NewResponse(&orbitv1.ListRunsResponse{Runs: out}), nil
}

func (s *runService) GetRun(
	ctx context.Context,
	req *connect.Request[orbitv1.GetRunRequest],
) (*connect.Response[orbitv1.GetRunResponse], error) {
	id := req.Msg.GetRunId()
	info, err := s.reg.Get(id)
	if err != nil {
		return nil, runLookupError(err)
	}
	resp := &orbitv1.GetRunResponse{Run: runProto(info)}
	if info.Kind == engine.RunKindLoad {
		if snap, ok := s.reg.Snapshot(id); ok {
			resp.LoadProgress = loadProgressProto(snap)
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *runService) GetRunReport(
	ctx context.Context,
	req *connect.Request[orbitv1.GetRunReportRequest],
) (*connect.Response[orbitv1.GetRunReportResponse], error) {
	id := req.Msg.GetRunId()
	info, err := s.reg.Get(id)
	if err != nil {
		return nil, runLookupError(err)
	}
	resp := &orbitv1.GetRunReportResponse{Run: runProto(info)}
	if report, ok := s.reg.Report(id); ok {
		resp.Report = &orbitv1.GetRunReportResponse_Load{Load: loadReportProto(report)}
	} else if info.State != engine.RunComplete {
		// No report yet: tell the caller why rather than returning an empty one.
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("run %s is %s; a report is available only once complete", id, info.State))
	}
	return connect.NewResponse(resp), nil
}

// loadRunFunc builds the launcher the registry runs. It validates the spec up
// front (so a bad request fails synchronously at StartRun) and closes over
// engine.RunLoad, wiring the run's LiveStats in as the load Observer.
func loadRunFunc(log *slog.Logger, p *orbitv1.LoadRunSpec) (engine.LoadRunFunc, error) {
	if p.GetAmfAddress() == "" {
		return nil, fmt.Errorf("amf_address is required")
	}
	if p.GetBaseImsi() == "" {
		return nil, fmt.Errorf("base_imsi is required")
	}
	if len(p.GetGnbs()) == 0 {
		return nil, fmt.Errorf("at least one gnb is required")
	}
	ki, err := auth.ParseHexKey("Ki", p.GetCredentials().GetKi())
	if err != nil {
		return nil, err
	}
	opc, err := auth.ParseHexKey("OPc", p.GetCredentials().GetOpc())
	if err != nil {
		return nil, err
	}

	gnbs := make([]engine.GNBSpec, 0, len(p.GetGnbs()))
	var mcc, mnc string
	for _, gp := range p.GetGnbs() {
		cfg, err := gnbConfigFromProto(gp)
		if err != nil {
			return nil, err
		}
		gnbs = append(gnbs, engine.GNBSpec{Config: cfg, AMFAddr: p.GetAmfAddress()})
		mcc, mnc = cfg.MCC, cfg.MNC
	}

	rate, err := loadRate(p)
	if err != nil {
		return nil, err
	}

	spec := engine.LoadSpec{
		GNBs:        gnbs,
		BaseIMSI:    p.GetBaseImsi(),
		Count:       int(p.GetCount()),
		MCC:         mcc,
		MNC:         mnc,
		Ki:          ki,
		OPc:         opc,
		Concurrency: int(p.GetConcurrency()),
		Rate:        rate,
		Duration:    time.Duration(p.GetDurationMs()) * time.Millisecond,
		SampleEvery: time.Duration(p.GetSampleIntervalMs()) * time.Millisecond,
	}
	if pdu := p.GetPduSession(); pdu != nil {
		if pdu.GetSst() > 0xFF {
			return nil, fmt.Errorf("sst %d exceeds one octet", pdu.GetSst())
		}
		spec.PDUSession = &ue.PDUSessionParams{
			PDUSessionID: uint8(pdu.GetPduSessionId()),
			SST:          uint8(pdu.GetSst()),
			SD:           pdu.GetSd(),
			DNN:          pdu.GetDnn(),
		}
		spec.GNBN3Addr = p.GetGnbN3Addr()
	}

	return func(ctx context.Context, stats *load.LiveStats) (load.Report, error) {
		s := spec
		s.Observer = stats
		return engine.RunLoad(ctx, log, s)
	}, nil
}

// loadRate turns the ramp/rate fields into a load.Rate (nil = concurrency-bound).
func loadRate(p *orbitv1.LoadRunSpec) (load.Rate, error) {
	if p.GetRampSeconds() > 0 || p.GetRampStart() > 0 || p.GetRampEnd() > 0 {
		if p.GetRampSeconds() == 0 {
			return nil, fmt.Errorf("ramp_seconds must be > 0 when a ramp is set")
		}
		return load.LinearRamp{
			Start: p.GetRampStart(),
			End:   p.GetRampEnd(),
			Over:  time.Duration(p.GetRampSeconds()) * time.Second,
		}, nil
	}
	if p.GetRate() > 0 {
		return load.Constant{RPS: p.GetRate()}, nil
	}
	return nil, nil
}

// runStartError maps a StartLoad error to a Connect code. A rejected concurrent
// run is a precondition failure, not an internal error.
func runStartError(err error) *connect.Error {
	var active *engine.ErrRunActive
	if errors.As(err, &active) {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// runLookupError maps a registry lookup error to a Connect code.
func runLookupError(err error) *connect.Error {
	var notFound *engine.ErrRunNotFound
	if errors.As(err, &notFound) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// runProto maps a RunInfo onto the wire type.
func runProto(info engine.RunInfo) *orbitv1.Run {
	r := &orbitv1.Run{
		RunId:           info.ID,
		Kind:            runKindProto(info.Kind),
		Name:            info.Name,
		State:           runStateProto(info.State),
		StartedUnixNano: info.StartedAt.UnixNano(),
		Error:           info.Err,
	}
	if !info.EndedAt.IsZero() {
		r.EndedUnixNano = info.EndedAt.UnixNano()
	}
	return r
}

func runKindProto(k engine.RunKind) orbitv1.RunKind {
	switch k {
	case engine.RunKindLoad:
		return orbitv1.RunKind_RUN_KIND_LOAD
	case engine.RunKindFleet:
		return orbitv1.RunKind_RUN_KIND_FLEET
	default:
		return orbitv1.RunKind_RUN_KIND_UNSPECIFIED
	}
}

func runKindFromProto(k orbitv1.RunKind) engine.RunKind {
	switch k {
	case orbitv1.RunKind_RUN_KIND_LOAD:
		return engine.RunKindLoad
	case orbitv1.RunKind_RUN_KIND_FLEET:
		return engine.RunKindFleet
	default:
		return "" // unspecified = no filter
	}
}

func runStateProto(s engine.RunState) orbitv1.RunState {
	switch s {
	case engine.RunPending:
		return orbitv1.RunState_RUN_STATE_PENDING
	case engine.RunRunning:
		return orbitv1.RunState_RUN_STATE_RUNNING
	case engine.RunDraining:
		return orbitv1.RunState_RUN_STATE_DRAINING
	case engine.RunComplete:
		return orbitv1.RunState_RUN_STATE_COMPLETE
	case engine.RunFailed:
		return orbitv1.RunState_RUN_STATE_FAILED
	case engine.RunCancelled:
		return orbitv1.RunState_RUN_STATE_CANCELLED
	default:
		return orbitv1.RunState_RUN_STATE_UNSPECIFIED
	}
}

func loadProgressProto(s load.Snapshot) *orbitv1.LoadProgress {
	return &orbitv1.LoadProgress{
		ElapsedMs:    s.Elapsed.Milliseconds(),
		Attempted:    uint32(s.Attempted),
		Succeeded:    uint32(s.Succeeded),
		Failed:       uint32(s.Failed),
		AchievedRate: s.AchievedRate,
		Latency:      procedureLatencies(s.Latencies),
	}
}

func loadReportProto(r load.Report) *orbitv1.LoadReport {
	out := &orbitv1.LoadReport{
		Attempted:    uint32(r.Attempted),
		Succeeded:    uint32(r.Succeeded),
		Failed:       uint32(r.Failed),
		DurationMs:   r.Duration.Milliseconds(),
		AchievedRate: r.AchievedRate,
		Latency:      procedureLatencies(r.Latencies),
	}
	for _, rs := range r.Resources {
		out.Resources = append(out.Resources, &orbitv1.ResourceSample{
			AtMs:       rs.At.Milliseconds(),
			Goroutines: uint32(rs.Goroutines),
			RssBytes:   rs.RSSBytes,
		})
	}
	return out
}

// procedureLatencies maps the per-procedure stats map to the wire slice, in a
// stable order so successive snapshots do not reshuffle.
func procedureLatencies(stats map[string]load.Stats) []*orbitv1.ProcedureLatency {
	order := []string{"registration", "pdu_session", "attach"}
	seen := make(map[string]bool, len(stats))
	var out []*orbitv1.ProcedureLatency
	add := func(name string, st load.Stats) {
		ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
		out = append(out, &orbitv1.ProcedureLatency{
			Procedure: name,
			Count:     uint64(st.Count),
			P50Ms:     ms(st.P50),
			P90Ms:     ms(st.P90),
			P99Ms:     ms(st.P99),
			P999Ms:    ms(st.P999),
			MaxMs:     ms(st.Max),
		})
	}
	for _, name := range order {
		if st, ok := stats[name]; ok {
			add(name, st)
			seen[name] = true
		}
	}
	// Any procedure not in the canonical order still gets reported.
	for name, st := range stats {
		if !seen[name] {
			add(name, st)
		}
	}
	return out
}
