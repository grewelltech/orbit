package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/scenario"
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
	verbosity := eventVerbosityFromProto(m.GetEventVerbosity())
	opts := engine.RunOptions{Name: m.GetName(), Verbosity: verbosity}
	switch spec := m.GetSpec().(type) {
	case *orbitv1.StartRunRequest_Load:
		fn, err := loadRunFunc(s.log, spec.Load)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		info, err := s.reg.StartLoad(opts, fn)
		if err != nil {
			return nil, runStartError(err)
		}
		return connect.NewResponse(&orbitv1.StartRunResponse{Run: runProto(info)}), nil
	case *orbitv1.StartRunRequest_Fleet:
		fn, err := fleetRunFunc(s.log, spec.Fleet, verbosity)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		info, err := s.reg.StartFleet(opts, fn)
		if err != nil {
			return nil, runStartError(err)
		}
		return connect.NewResponse(&orbitv1.StartRunResponse{Run: runProto(info)}), nil
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("a run spec is required (load or fleet)"))
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
	switch info.Kind {
	case engine.RunKindLoad:
		if snap, ok := s.reg.Snapshot(id); ok {
			resp.LoadProgress = loadProgressProto(snap)
		}
	case engine.RunKindFleet:
		if snap, ok := s.reg.FleetSnapshot(id); ok {
			// nil rate tracker: a single-shot Get has no previous sample to
			// derive rates from, and inventing one from the run's start would
			// report an average as though it were current.
			resp.FleetProgress = fleetProgressProto(snap, time.Now(), nil)
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
	switch {
	case info.State != engine.RunComplete:
		// No report yet: tell the caller why rather than returning an empty one.
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("run %s is %s; a report is available only once complete", id, info.State))
	case info.Kind == engine.RunKindLoad:
		if report, ok := s.reg.Report(id); ok {
			resp.Report = &orbitv1.GetRunReportResponse_Load{Load: loadReportProto(report)}
		}
	case info.Kind == engine.RunKindFleet:
		if report, ok := s.reg.FleetResult(id); ok {
			resp.Report = &orbitv1.GetRunReportResponse_Fleet{Fleet: fleetReportProto(report)}
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *runService) RunEvents(
	ctx context.Context,
	req *connect.Request[orbitv1.RunEventsRequest],
	stream *connect.ServerStream[orbitv1.RunEvent],
) error {
	id := req.Msg.GetRunId()
	sub, err := s.reg.SubscribeEvents(id, req.Msg.GetFromSeq())
	if err != nil {
		return runLookupError(err)
	}
	defer sub.Close()

	// nextSeq tracks the sequence number expected next, so a terminal-time
	// reconciliation can send exactly the events not yet delivered.
	nextSeq := req.Msg.GetFromSeq()
	first := true
	send := func(ev engine.RunEvent) error {
		pb := runEventProto(ev)
		if first {
			pb.DroppedBefore = sub.DroppedBefore
			first = false
		}
		if err := stream.Send(pb); err != nil {
			return err
		}
		nextSeq = ev.Seq + 1
		return nil
	}
	for _, ev := range sub.Backlog {
		if err := send(ev); err != nil {
			return err
		}
	}

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	// Stream live. Once the run is terminal, reconcile against the retained ring
	// rather than only the channel: a slow client's channel may have dropped
	// events (including the terminal one), but they are still in the ring. This
	// is what makes "the client always gets the final event" true even under
	// backpressure — the non-blocking fan-out alone cannot guarantee it.
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-sub.Ch:
			if !ok {
				return nil // subscription closed
			}
			if err := send(ev); err != nil {
				return err
			}
		case <-ticker.C:
			info, err := s.reg.Get(id)
			if err != nil || info.State.Terminal() {
				for _, ev := range s.reg.EventsSince(id, nextSeq) {
					if err := send(ev); err != nil {
						return err
					}
				}
				return nil
			}
		}
	}
}

// eventPollInterval bounds how long RunEvents waits between events before
// re-checking whether the run has ended.
const eventPollInterval = 200 * time.Millisecond

func runEventProto(ev engine.RunEvent) *orbitv1.RunEvent {
	return &orbitv1.RunEvent{
		Seq:      ev.Seq,
		UnixNano: ev.Time.UnixNano(),
		Severity: eventSeverityProto(ev.Severity),
		Kind:     ev.Kind,
		Supi:     ev.SUPI,
		Message:  ev.Message,
	}
}

func eventSeverityProto(s string) orbitv1.EventSeverity {
	switch s {
	case "info":
		return orbitv1.EventSeverity_EVENT_SEVERITY_INFO
	case "warn":
		return orbitv1.EventSeverity_EVENT_SEVERITY_WARN
	case "error":
		return orbitv1.EventSeverity_EVENT_SEVERITY_ERROR
	default:
		return orbitv1.EventSeverity_EVENT_SEVERITY_UNSPECIFIED
	}
}

// Telemetry cadence bounds. A too-fast interval floods the client for no gain
// (aggregates change on the order of the run's own sampling); a too-slow one
// makes "live" meaningless.
const (
	telemetryMinInterval     = 100 * time.Millisecond
	telemetryMaxInterval     = 10 * time.Second
	telemetryDefaultInterval = 1 * time.Second
)

func (s *runService) RunTelemetry(
	ctx context.Context,
	req *connect.Request[orbitv1.RunTelemetryRequest],
	stream *connect.ServerStream[orbitv1.TelemetryFrame],
) error {
	id := req.Msg.GetRunId()
	if _, err := s.reg.Get(id); err != nil {
		return runLookupError(err)
	}

	interval := telemetryDefaultInterval
	if req.Msg.GetIntervalMs() > 0 {
		interval = time.Duration(req.Msg.GetIntervalMs()) * time.Millisecond
	}
	if interval < telemetryMinInterval {
		interval = telemetryMinInterval
	}
	if interval > telemetryMaxInterval {
		interval = telemetryMaxInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var seq uint64
	// Rate state is per-stream, not per-run: a slow client's frames are spaced
	// by its own coalesced ticks, so deriving rates here gives each subscriber
	// figures over the interval it actually observed.
	var rates fleetRates
	// Each iteration sends one complete snapshot. A slow client backpressures
	// Send, which stalls the loop and coalesces ticks — frames are naturally
	// sampled, not queued, which is the whole point of self-contained frames.
	send := func() (terminal bool, err error) {
		info, err := s.reg.Get(id)
		if err != nil {
			// The run was evicted from history mid-stream; end cleanly.
			return true, nil
		}
		frame := s.telemetryFrame(id, info, uint32(interval.Milliseconds()), seq, &rates)
		seq++
		if err := stream.Send(frame); err != nil {
			return false, err
		}
		return info.State.Terminal(), nil
	}

	// Send an immediate first frame so a client sees state without waiting a
	// full interval, then one final frame once the run is terminal.
	if terminal, err := send(); err != nil || terminal {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if terminal, err := send(); err != nil || terminal {
				return err
			}
		}
	}
}

// telemetryFrame builds one snapshot frame for a run.
func (s *runService) telemetryFrame(id string, info engine.RunInfo, intervalMs uint32, seq uint64, rates *fleetRates) *orbitv1.TelemetryFrame {
	now := time.Now()
	elapsed := now.Sub(info.StartedAt)
	if !info.EndedAt.IsZero() {
		elapsed = info.EndedAt.Sub(info.StartedAt)
	}
	frame := &orbitv1.TelemetryFrame{
		RunId:      id,
		UnixNano:   now.UnixNano(),
		IntervalMs: intervalMs,
		FrameSeq:   seq,
		State:      runStateProto(info.State),
		ElapsedMs:  elapsed.Milliseconds(),
	}
	switch info.Kind {
	case engine.RunKindLoad:
		if snap, ok := s.reg.Snapshot(id); ok {
			frame.Progress = &orbitv1.TelemetryFrame_Load{Load: loadProgressProto(snap)}
		}
	case engine.RunKindFleet:
		if snap, ok := s.reg.FleetSnapshot(id); ok {
			frame.Progress = &orbitv1.TelemetryFrame_Fleet{Fleet: fleetProgressProto(snap, now, rates)}
		}
	}
	return frame
}

// fleetRates derives per-interval throughput from consecutive cumulative
// samples. It belongs to one telemetry stream: sharing it between subscribers
// would have each one consume the other's baseline and report rates over
// intervals neither observed.
type fleetRates struct {
	have                             bool
	at                               time.Time
	ulBytes, dlBytes, ulPkts, dlPkts uint64
	// Per-flow previous cumulative bytes, keyed by SUPI+app so a UE running
	// both a cohort client and a synthetic flow keeps them apart.
	//
	// flowsAt is tracked separately from `at`: observe() runs first and
	// advances `at` to now, so measuring the flow interval against it would
	// always yield zero and every per-flow rate would read 0 bps.
	flows     map[string]flowSample
	flowsAt   time.Time
	flowsHave bool
}

type flowSample struct{ ulBytes, dlBytes uint64 }

// MaxReportedFlows bounds the per-frame flow list. A population run has more
// flows than any table should render, and an unbounded list would grow the
// frame without bound; the frame also carries the untruncated total so a
// short list is never mistaken for a complete one.
const MaxReportedFlows = 100

// flowRates folds in this frame's flows and returns them as wire rows, ranked
// by current throughput and truncated. Rates come from the same per-stream
// discipline as the run totals: the first frame of a stream reports none.
func flowRows(now time.Time, flows []engine.FleetFlow, r *fleetRates) ([]*orbitv1.FlowRow, int) {
	var prev map[string]flowSample
	secs := 0.0
	if r != nil {
		prev = r.flows
		if r.flowsHave {
			secs = now.Sub(r.flowsAt).Seconds()
		}
		r.flows = make(map[string]flowSample, len(flows))
		r.flowsAt, r.flowsHave = now, true
	}

	rows := make([]*orbitv1.FlowRow, 0, len(flows))
	for _, f := range flows {
		key := f.SUPI + "|" + f.App
		if r != nil {
			r.flows[key] = flowSample{ulBytes: f.UplinkBytes, dlBytes: f.DownlinkBytes}
		}
		row := &orbitv1.FlowRow{
			Supi: f.SUPI, Cohort: f.Cohort, App: f.App, Peer: f.Peer, Gnb: f.GNB,
			UplinkBytes: f.UplinkBytes, DownlinkBytes: f.DownlinkBytes,
		}
		if !f.Started.IsZero() {
			row.ElapsedMs = now.Sub(f.Started).Milliseconds()
		}
		if p, ok := prev[key]; ok && secs > 0 {
			row.UplinkBps = deltaBytes(f.UplinkBytes, p.ulBytes) * 8 / secs
			row.DownlinkBps = deltaBytes(f.DownlinkBytes, p.dlBytes) * 8 / secs
		}
		rows = append(rows, row)
	}
	// Busiest first, so a truncated list keeps the flows worth looking at.
	// Ties break on SUPI so the table does not reshuffle between frames.
	sort.Slice(rows, func(i, j int) bool {
		ri, rj := rows[i].GetUplinkBps()+rows[i].GetDownlinkBps(), rows[j].GetUplinkBps()+rows[j].GetDownlinkBps()
		if ri != rj {
			return ri > rj
		}
		return rows[i].GetSupi() < rows[j].GetSupi()
	})
	total := len(rows)
	if len(rows) > MaxReportedFlows {
		rows = rows[:MaxReportedFlows]
	}
	return rows, total
}

// deltaBytes guards a counter that went backwards (a tunnel torn down between
// samples) rather than reporting a negative rate.
func deltaBytes(now, then uint64) float64 {
	if now < then {
		return 0
	}
	return float64(now - then)
}

// observe folds in one cumulative sample and returns the rates since the
// previous one. The first sample of a stream has no interval behind it and
// yields zeros rather than a rate computed against the run's start, which
// would understate a run that was already in flight when the client attached.
func (r *fleetRates) observe(now time.Time, s engine.FleetSnapshot) (ulBps, dlBps, ulPps, dlPps float64) {
	prev := *r
	r.have, r.at = true, now
	r.ulBytes, r.dlBytes = s.UplinkBytes, s.DownlinkBytes
	r.ulPkts, r.dlPkts = s.UplinkPackets, s.DownlinkPackets
	if !prev.have {
		return 0, 0, 0, 0
	}
	secs := now.Sub(prev.at).Seconds()
	if secs <= 0 {
		return 0, 0, 0, 0
	}
	// Counters are monotonic; delta guards against a UE's tunnel being torn
	// down between samples rather than against real regression.
	delta := func(now, then uint64) float64 {
		if now < then {
			return 0
		}
		return float64(now - then)
	}
	return delta(s.UplinkBytes, prev.ulBytes) * 8 / secs,
		delta(s.DownlinkBytes, prev.dlBytes) * 8 / secs,
		delta(s.UplinkPackets, prev.ulPkts) / secs,
		delta(s.DownlinkPackets, prev.dlPkts) / secs
}

// fleetProgressProto maps a fleet snapshot onto the wire, deriving rates
// through the stream's own tracker.
func fleetProgressProto(s engine.FleetSnapshot, now time.Time, rates *fleetRates) *orbitv1.FleetProgress {
	p := &orbitv1.FleetProgress{
		ElapsedMs:       s.Elapsed.Milliseconds(),
		Attached:        uint32(s.Attached),
		AttachFailed:    uint32(s.AttachFailed),
		Handovers:       uint32(s.Handovers),
		HandoverErrors:  uint32(s.HandoverErrors),
		TrafficFlows:    uint32(s.TrafficFlows),
		AppSessions:     uint32(s.AppSessions),
		UplinkBytes:     s.UplinkBytes,
		DownlinkBytes:   s.DownlinkBytes,
		UplinkPackets:   s.UplinkPackets,
		DownlinkPackets: s.DownlinkPackets,
	}
	if rates != nil {
		p.UplinkBps, p.DownlinkBps, p.UplinkPps, p.DownlinkPps = rates.observe(now, s)
	}
	// Flows are reported either way: the list and its cumulative counters do
	// not depend on having a previous sample, and only the *_bps fields do.
	// Gating the whole list on a rate tracker left a single-shot Get with no
	// flows at all.
	rows, total := flowRows(now, s.Flows, rates)
	p.Flows, p.FlowsTotal, p.FlowsReported = rows, uint32(total), uint32(len(rows))
	for _, c := range s.Cohorts {
		p.Cohorts = append(p.Cohorts, cohortProgressProto(c))
	}
	for _, g := range sortedGNBs(s.PerGNB) {
		p.PerGnb = append(p.PerGnb, &orbitv1.GnbProgress{Gnb: g, Succeeded: uint32(s.PerGNB[g])})
	}
	// Only when a probe actually ran: an unconfigured probe leaves the field
	// absent, so a consumer sees "not measured" rather than a 0 ms data path.
	if s.HasUPLatency {
		p.UpProbes, p.UpProbesLost = s.UPProbes, s.UPProbesLost
		p.UpLatency = &orbitv1.ProcedureLatency{
			Procedure: "user_plane",
			Count:     uint64(s.UPLatency.Count),
			P50Ms:     msFloat(s.UPLatency.P50),
			P90Ms:     msFloat(s.UPLatency.P90),
			P99Ms:     msFloat(s.UPLatency.P99),
			P999Ms:    msFloat(s.UPLatency.P999),
			MaxMs:     msFloat(s.UPLatency.Max),
		}
	}
	return p
}

// cohortProgressProto maps one cohort's live quality onto the wire. A family
// the cohort's app does not produce stays nil, so "absent" and "zero" remain
// distinguishable downstream.
func cohortProgressProto(c engine.FleetCohort) *orbitv1.CohortProgress {
	return &orbitv1.CohortProgress{
		Name: c.Name, App: c.App, Ues: uint32(c.UEs),
		ElapsedMs:     c.Elapsed.Milliseconds(),
		Mos:           quantilesProto(c.MOS),
		TtfbMs:        quantilesProto(c.TTFBMs),
		GoodputMbps:   quantilesProto(c.GoodputMbps),
		StallTimeMs:   quantilesProto(c.StallTimeMs),
		RebufferRatio: quantilesProto(c.RebufferRatio),
		BitrateKbps:   quantilesProto(c.BitrateKbps),
		StartupMs:     quantilesProto(c.StartupMs),
		FarEnd: &orbitv1.FarEndView{
			Available:  c.FarEnd.Available,
			Reason:     c.FarEnd.Reason,
			Bytes:      c.FarEnd.Bytes,
			Packets:    c.FarEnd.Packets,
			BitsPerSec: c.FarEnd.BitsPerSec,
			Requests:   c.FarEnd.Requests,
			Errors:     c.FarEnd.Errors,
		},
	}
}

func quantilesProto(q *engine.FleetQuantiles) *orbitv1.Quantiles {
	if q == nil {
		return nil
	}
	return &orbitv1.Quantiles{P5: q.P5, P50: q.P50, P95: q.P95}
}

// msFloat renders a duration as fractional milliseconds, the unit every
// latency field on the wire uses.
func msFloat(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// sortedGNBs orders gNB labels so the per-gNB list is stable frame to frame —
// map order would otherwise reshuffle the dashboard's bars every tick.
func sortedGNBs(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

	// UE identity carries one PLMN, so every gNB in a run must agree on it.
	// A single core serves one home PLMN; silently keeping the last gNB's would
	// build UEs whose identity mismatches the gNBs serving them.
	gnbs := make([]engine.GNBSpec, 0, len(p.GetGnbs()))
	var mcc, mnc string
	for i, gp := range p.GetGnbs() {
		cfg, err := gnbConfigFromProto(gp)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			mcc, mnc = cfg.MCC, cfg.MNC
		} else if cfg.MCC != mcc || cfg.MNC != mnc {
			return nil, fmt.Errorf("all gnbs must share one PLMN; gnb %d is %s/%s, not %s/%s",
				i, cfg.MCC, cfg.MNC, mcc, mnc)
		}
		gnbs = append(gnbs, engine.GNBSpec{Config: cfg, AMFAddr: p.GetAmfAddress()})
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

	return func(ctx context.Context, stats *load.LiveStats, emit engine.RunEventFunc) (load.Report, error) {
		s := spec
		// Fan out each attempt to the live aggregates and to a run-event
		// reporter that turns failures into discrete events.
		s.Observer = load.Observers{stats, failureEvents(emit)}
		return engine.RunLoad(ctx, log, s)
	}, nil
}

// failureEvents is a load.Observer that reports each failed attempt as a run
// event. Successes are not evented — at fleet scale they are the aggregates'
// job; the event stream is for what went wrong and for milestones.
type failureEvents engine.RunEventFunc

func (f failureEvents) Observe(s load.Sample) {
	if s.Err != nil {
		f("error", "ATTACH", s.SUPI, s.Err.Error())
	}
}

// fleetRunFunc parses the fleet scenario YAML, injects the request's
// credentials, and builds the launcher via the shared scenario→engine mapping —
// the same one `orbit run <fleet>` uses, so CLI and server runs are identical.
func fleetRunFunc(log *slog.Logger, p *orbitv1.FleetRunSpec, verbosity engine.EventVerbosity) (engine.FleetRunFunc, error) {
	if p.GetScenarioYaml() == "" {
		return nil, fmt.Errorf("scenario_yaml is required")
	}
	// Parse WITHOUT ${ENV} expansion: the YAML is client-supplied, and expanding
	// it against the server's environment would leak the server's variables back
	// to the client. Credentials come from the request, below.
	f, err := scenario.ParseFleetNoEnv([]byte(p.GetScenarioYaml()))
	if err != nil {
		return nil, err
	}
	// Credentials come from the request, not the YAML's ${ENV}: the server
	// cannot expand the client's secrets from its own environment.
	ki, err := auth.ParseHexKey("Ki", p.GetCredentials().GetKi())
	if err != nil {
		return nil, err
	}
	opc, err := auth.ParseHexKey("OPc", p.GetCredentials().GetOpc())
	if err != nil {
		return nil, err
	}
	spec, beh, err := scenario.BuildFleetRun(f, ki, opc)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, stats *engine.FleetLiveStats, emit engine.RunEventFunc) (engine.FleetReport, error) {
		return engine.RunFleet(ctx, log, spec, beh, stats, engine.NewRunEvents(emit, verbosity))
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

// eventVerbosityFromProto maps the wire enum onto the engine's verbosity,
// treating UNSPECIFIED as the default rather than rejecting it — an older
// client that never sets the field gets normal emission.
func eventVerbosityFromProto(v orbitv1.EventVerbosity) engine.EventVerbosity {
	if v == orbitv1.EventVerbosity_EVENT_VERBOSITY_VERBOSE {
		return engine.EventsVerbose
	}
	return engine.EventsNormal
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
		PerGnb:       gnbProgressProtos(s.PerGNB),
	}
}

// gnbProgressProtos maps the per-gNB spread to proto, sorted by gNB name so the
// frame is stable across samples (a map's iteration order is not).
func gnbProgressProtos(m map[string]load.GNBProgress) []*orbitv1.GnbProgress {
	if len(m) == 0 {
		return nil
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*orbitv1.GnbProgress, 0, len(names))
	for _, name := range names {
		g := m[name]
		out = append(out, &orbitv1.GnbProgress{
			Gnb:       name,
			Attempted: uint32(g.Attempted),
			Succeeded: uint32(g.Succeeded),
			Failed:    uint32(g.Failed),
		})
	}
	return out
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

func fleetReportProto(r engine.FleetReport) *orbitv1.FleetReport {
	return &orbitv1.FleetReport{
		Attached:        uint32(r.Attached),
		AttachFailed:    uint32(r.AttachFailed),
		AttachElapsedMs: r.AttachElapsed.Milliseconds(),
		Handovers:       uint32(r.Handovers),
		HandoverErrors:  uint32(r.HandoverErr),
		TrafficFlows:    uint32(r.TrafficFlows),
		TrafficBytes:    r.TrafficBytes,
		Deregistered:    uint32(r.Deregistered),
	}
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
