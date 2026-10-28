package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/join"
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
	// responders are the responders registered by join, which a target
	// names without an address or token; nil in tests that build a runService
	// by hand.
	responders *join.Registry

	// frames holds each run's retained telemetry series, produced by one
	// sampler per run so every subscriber reads the same frames — and so a
	// finished run still has a series to replay.
	framesMu sync.Mutex
	frames   map[string]*frameLog
	// version is the running build, stamped into generated reports so an old
	// artefact is not read as though it came from today's binary.
	version string
	// specs remembers what each run was asked to do, so the archive — and the
	// report built from it — is self-describing.
	specs *specStore
	// beforeArchive, when set (tests only), runs in a run's sampler after
	// it has seen the run end and before it archives it.
	beforeArchive func(id string)
	// archive persists terminal runs so history survives a restart. Never nil;
	// a disabled store is a no-op rather than a special case at each call site.
	archive *archiveStore
	// mgr is the same Manager the UE service drives. A fleet run borrows its
	// N3 socket pool so an ad-hoc `orbit ue` session and a run cannot fight
	// over one gNB's UDP bind — see engine.FleetDeps.
	mgr *engine.Manager
	// rotation assigns gNB IDs to plans that ask for them (gnb_ids.assign),
	// least recently used first, persisted in the state directory. Nil: a
	// plan cannot ask.
	rotation *engine.GNBRotation
	// expected is the server's conformance expected-failures list (orbit
	// serve --conformance-expected-failures), which a conformance workload
	// without its own is judged against; nil for none.
	expected *conformance.ExpectedFailures
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
		arm, err := s.loadArm(spec.Load, verbosity) //nolint:staticcheck // the deprecated arm, kept for API clients
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		opts.Needs = arm.needs
		info, err := s.reg.StartLoad(opts, arm.load)
		if err != nil {
			return nil, runStartError(err)
		}
		s.specs.put(info.ID, m)
		s.startSampler(info.ID)
		return connect.NewResponse(&orbitv1.StartRunResponse{Run: runProto(info)}), nil
	case *orbitv1.StartRunRequest_Fleet:
		arm, err := s.fleetArm(spec.Fleet, verbosity) //nolint:staticcheck // the deprecated arm, kept for API clients
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		opts.Needs = arm.needs
		info, err := s.reg.StartFleet(opts, arm.fleet)
		if err != nil {
			return nil, runStartError(err)
		}
		s.specs.put(info.ID, m)
		s.startSampler(info.ID)
		return connect.NewResponse(&orbitv1.StartRunResponse{Run: runProto(info)}), nil
	case *orbitv1.StartRunRequest_Plan:
		info, _, err := s.startPlan(m, opts)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&orbitv1.StartRunResponse{Run: runProto(info)}), nil
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("a run spec is required (load or fleet)"))
	}
}

// startPlan builds a plan request's run and starts it under its leases,
// with its gates judged on completion; m is what the run records (and
// archives, masked). Errors are connect errors: INVALID_ARGUMENT for a
// plan that does not build, FAILED_PRECONDITION for an exhausted gNB
// range or a lease conflict, UNAVAILABLE when the rotation cannot save.
func (s *runService) startPlan(m *orbitv1.StartRunRequest, opts engine.RunOptions) (engine.RunInfo, *planRun, error) {
	plan := m.GetPlan()
	pr, err := s.buildPlan(plan, opts.Verbosity)
	if errors.Is(err, engine.ErrGNBRangeExhausted) {
		return engine.RunInfo{}, nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if errors.Is(err, engine.ErrRotationPersist) {
		return engine.RunInfo{}, nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if errors.Is(err, errResponderOffline) {
		return engine.RunInfo{}, nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		return engine.RunInfo{}, nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	opts.Needs = pr.needs
	opts.Judge = planJudge(plan)
	// Read before the run starts: a run that fails fast releases its
	// leases (and its IDs) while StartRun is still here.
	prov := pr.provenance()
	// The IDs go back once: from the run's release, or below if it never
	// starts (a launcher that panicked does both).
	var once sync.Once
	release := func() {}
	if ids := pr.assigned; len(ids) > 0 {
		keys, plmn := pr.amfKeys, pr.plmn
		release = func() { once.Do(func() { s.releaseIDs(keys, plmn, ids) }) }
		opts.OnRelease = release
	}
	var info engine.RunInfo
	switch {
	case pr.mixed != nil:
		opts.WorkloadKinds = pr.kinds
		info, err = s.reg.StartMixed(opts, pr.mixed)
	case pr.suite != nil:
		info, err = s.reg.StartConformance(opts, pr.suiteLive, pr.suite)
	case pr.fleet != nil:
		info, err = s.reg.StartFleet(opts, pr.fleet)
	default:
		info, err = s.reg.StartLoad(opts, pr.load)
	}
	if err != nil {
		release()
		return engine.RunInfo{}, nil, runStartError(err)
	}
	s.specs.put(info.ID, m, prov...)
	s.startSampler(info.ID)
	return info, pr, nil
}

func (s *runService) StopRun(
	ctx context.Context,
	req *connect.Request[orbitv1.StopRunRequest],
) (*connect.Response[orbitv1.StopRunResponse], error) {
	stop := s.reg.Stop
	if req.Msg.GetFinish() {
		stop = s.reg.Finish // judged on its results so far, not cancelled
	}
	info, err := stop(req.Msg.GetRunId())
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
	live := make(map[string]bool)
	for _, info := range s.reg.List() {
		live[info.ID] = true
		if filter != "" && info.Kind != filter && !slices.Contains(info.WorkloadKinds, filter) {
			continue
		}
		out = append(out, runProto(info))
	}
	// Then runs that exist only on disk. The registry's list is already
	// newest-first and every archived run predates every live one, so appending
	// keeps the whole list ordered. Runs held in both are skipped here — the
	// live record is the current one.
	for _, a := range s.archive.list() {
		r := a.GetRun()
		if live[r.GetRunId()] {
			continue
		}
		if k := req.Msg.GetKind(); filter != "" && r.GetKind() != k && !slices.Contains(r.GetWorkloadKinds(), k) {
			continue
		}
		out = append(out, r)
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
		if a, ok := s.archive.get(id); ok {
			resp := &orbitv1.GetRunResponse{
				Run:           a.GetRun(),
				LoadProgress:  a.GetLoadProgress(),
				FleetProgress: a.GetFleetProgress(),
			}
			// The archive holds a report only for a run that completed, the
			// same predicate as the live path below.
			if a.GetRun().GetState() == orbitv1.RunState_RUN_STATE_COMPLETE {
				resp.FleetReport = a.GetFleetReport()
			}
			return connect.NewResponse(resp), nil
		}
		return nil, runLookupError(err)
	}
	resp := &orbitv1.GetRunResponse{Run: runProto(info)}
	if !info.State.Terminal() {
		resp.Leases = leasesProto(s.reg.Leases(id))
	}
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
		// A finished run's live snapshot is torn down (every UE detached),
		// so its totals come from the report, on the same terms as
		// GetRunReport: only once the run completed.
		if info.State == engine.RunComplete {
			if rep, ok := s.reg.FleetResult(id); ok {
				resp.FleetReport = fleetReportProto(rep)
			}
		}
	case engine.RunKindConformance:
		if live := s.suiteLive(id); live != nil {
			resp.ConformanceProgress = live.progress()
		}
	case engine.RunKindMixed:
		resp.Workloads = workloadsProto(s.reg.MixedProgress(id), time.Now())
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
		if a, ok := s.archive.get(id); ok {
			return archivedReportResponse(id, a)
		}
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
	case info.Kind == engine.RunKindConformance:
		if live := s.suiteLive(id); live != nil {
			resp.Report = &orbitv1.GetRunReportResponse_Conformance{Conformance: live.summary()}
		}
	}
	// A plan's gates, judged as the run completed and kept with its report;
	// the archive as a second line.
	if rs := s.planVerdict(id); len(rs) > 0 {
		markStoppedEarly(rs, info.StoppedEarly)
		resp.Gates, resp.GatesPassed = gatesOf(rs)
		resp.Workloads = rs
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
		// A run that exists only on disk has a complete, closed log: replay it
		// and end the stream, rather than reporting the run as unknown.
		if a, ok := s.archive.get(id); ok {
			return sendArchivedEvents(stream, a, req.Msg.GetFromSeq())
		}
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
		Check:    checkEvent(ev.Kind, ev.Message),
	}
}

// checkText is a conformance check's verdict event as the suite emits it,
// "<check> <verdict>", which a mixed run's workload prefixes with
// "[<workload>] " (engine/mixed.go); a workload's name holds no brackets.
var checkText = regexp.MustCompile(`^(?:\[([^][]+)\] )?(\S+) (PASS|FAIL|ERROR|SKIP|DEVIATION)$`)

// checkEvent is a CONFORMANCE event's verdict as fields, beside its text
// for older clients; nil for any other event.
func checkEvent(kind, message string) *orbitv1.ConformanceCheckEvent {
	if kind != "CONFORMANCE" {
		return nil
	}
	m := checkText.FindStringSubmatch(message)
	if m == nil {
		return nil
	}
	return &orbitv1.ConformanceCheckEvent{Workload: m[1], CheckId: m[2], Verdict: m[3]}
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

// sampleInterval is the canonical cadence a run's series is produced at. One
// schedule per run rather than one per subscriber: retained history has to be a
// single series, and two viewers seeing different samples of the same run was
// never a feature.
const sampleInterval = time.Second

// frameLogFor returns a run's frame log, creating it — and starting its
// sampler — on first use.
//
// The sampler is tied to the run EXISTING, not to StartRun having been called:
// a run created any other way (the registry directly, as tests do) would
// otherwise have a log nothing ever writes to or closes, and a telemetry
// subscriber would wait on it forever.
func (s *runService) frameLogFor(id string) *frameLog {
	s.framesMu.Lock()
	if s.frames == nil {
		s.frames = make(map[string]*frameLog)
	}
	l, ok := s.frames[id]
	if !ok {
		l = newFrameLog(DefaultFrameCap)
		s.frames[id] = l
	}
	start := !l.sampling
	l.sampling = true
	s.framesMu.Unlock()

	if start {
		// Until this sampler has archived the run, its spec outlives the
		// run's eviction from the registry (sweep).
		s.specs.sampling(id, true)
		go s.sampleRun(id, l)
	}
	return l
}

// startSampler begins a run's series at the moment it starts, rather than when
// something first subscribes — history that only began when someone looked
// would not be history.
func (s *runService) startSampler(id string) { s.frameLogFor(id) }

// sampleRun produces a run's telemetry series until it is terminal, then emits
// one final frame and closes the log. Rates are derived here, once, so every
// subscriber reads the same numbers; each stream previously computed its own
// from its own cadence, which is why there was no single series to retain.
func (s *runService) sampleRun(id string, log *frameLog) {
	var rates fleetRates
	var seq uint64
	t := time.NewTicker(sampleInterval)
	defer t.Stop()

	// done carries the terminal info so the run can be archived with its
	// completed series; a run that vanished from the registry leaves it unset,
	// because there is nothing left to archive.
	var done *engine.RunInfo
	sample := func() bool {
		info, err := s.reg.Get(id)
		if err != nil {
			return true // evicted from history; nothing left to sample
		}
		f := s.telemetryFrame(id, info, uint32(sampleInterval.Milliseconds()), seq, &rates)
		seq++
		log.append(f)
		if info.State.Terminal() {
			done = &info
			return true
		}
		return false
	}

	// Archiving happens BEFORE the log closes, while the registry still holds
	// the run: the archive needs its report, final snapshot and events, all of
	// which go away when the record is evicted.
	// Each run's end is also when the registry evicts beyond its history:
	// the specs of the runs it let go are let go too (sweep).
	finish := func() {
		if done != nil {
			if s.beforeArchive != nil {
				s.beforeArchive(id)
			}
			s.archiveRun(id, *done, log.retained())
		}
		s.specs.sampling(id, false)
		s.specs.sweep(func(id string) bool {
			_, err := s.reg.Get(id)
			return err == nil
		})
		log.close()
	}

	// An immediate first frame, so a subscriber attaching straight after the
	// run starts has something rather than waiting a whole interval.
	if sample() {
		finish()
		return
	}
	for range t.C {
		if sample() {
			finish()
			return
		}
	}
}

func (s *runService) RunTelemetry(
	ctx context.Context,
	req *connect.Request[orbitv1.RunTelemetryRequest],
	stream *connect.ServerStream[orbitv1.TelemetryFrame],
) error {
	id := req.Msg.GetRunId()
	if _, err := s.reg.Get(id); err != nil {
		// A restored run is not in the registry, but restoreFrames seeded its
		// series — a pre-populated, closed log, which replays and ends. Only a
		// run in neither place is unknown.
		if _, ok := s.archive.get(id); !ok {
			return runLookupError(err)
		}
	}

	// Replay the retained series from the caller's resume point, then follow
	// live. This is what lets a reload, or a client attaching to a run already
	// under way, start with the run's history instead of an empty chart — and
	// it is why a finished run has anything to send at all.
	sub := s.frameLogFor(id).subscribeFrom(req.Msg.GetFromSeq())
	defer sub.Close()

	first := true
	send := func(f *orbitv1.TelemetryFrame) error {
		if first {
			first = false
			// Stated once, on the first frame: a gap the ring could not cover
			// is reported rather than drawn as continuous.
			if d := sub.DroppedBefore; d > 0 {
				cp, _ := proto.Clone(f).(*orbitv1.TelemetryFrame)
				cp.DroppedBefore = d
				f = cp
			}
		}
		return stream.Send(f)
	}

	for _, f := range sub.Backlog {
		if err := send(f); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case f, ok := <-sub.Ch:
			if !ok {
				// The run finished and the sampler closed the log; the caller
				// has the whole series.
				return nil
			}
			if err := send(f); err != nil {
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
	case engine.RunKindConformance:
		if live := s.suiteLive(id); live != nil {
			frame.Progress = &orbitv1.TelemetryFrame_Conformance{Conformance: live.progress()}
		}
	case engine.RunKindMixed:
		frame.Workloads = workloadsProto(s.reg.MixedProgress(id), now)
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
	for _, name := range sortedProcedures(s.Procedures) {
		st := s.Procedures[name]
		p.Latency = append(p.Latency, &orbitv1.ProcedureLatency{
			Procedure: name, Count: uint64(st.Count),
			P50Ms: msFloat(st.P50), P90Ms: msFloat(st.P90),
			P99Ms: msFloat(st.P99), P999Ms: msFloat(st.P999), MaxMs: msFloat(st.Max),
		})
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
		Name: c.Name, App: c.App, Ues: uint32(c.UEs), Failed: uint32(c.Failed),
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

// sortedProcedures keeps the latency list stable frame to frame; map order
// would otherwise reshuffle the panel's series on every tick.
func sortedProcedures(m map[string]load.Stats) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

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
// loadSpecFrom is a load arm as the storm engine's spec.
func loadSpecFrom(p *orbitv1.LoadRunSpec) (engine.LoadSpec, error) { //nolint:staticcheck // the deprecated arm, kept for API clients
	if p.GetAmfAddress() == "" {
		return engine.LoadSpec{}, fmt.Errorf("amf_address is required")
	}
	if p.GetBaseImsi() == "" {
		return engine.LoadSpec{}, fmt.Errorf("base_imsi is required")
	}
	if len(p.GetGnbs()) == 0 {
		return engine.LoadSpec{}, fmt.Errorf("at least one gnb is required")
	}
	ki, err := auth.ParseHexKey("Ki", p.GetCredentials().GetKi())
	if err != nil {
		return engine.LoadSpec{}, err
	}
	opc, err := auth.ParseHexKey("OPc", p.GetCredentials().GetOpc())
	if err != nil {
		return engine.LoadSpec{}, err
	}

	// N2 source addresses follow the same none / one / one-per-gNB rule as
	// `orbit load --bind`, enforced here so the API and the direct-drive path
	// cannot disagree about what a bind list means.
	binds, err := engine.ExpandBindAddrs(p.GetBindAddresses(), len(p.GetGnbs()))
	if err != nil {
		return engine.LoadSpec{}, err
	}

	// UE identity carries one PLMN, so every gNB in a run must agree on it.
	// A single core serves one home PLMN; silently keeping the last gNB's would
	// build UEs whose identity mismatches the gNBs serving them.
	gnbs := make([]engine.GNBSpec, 0, len(p.GetGnbs()))
	var mcc, mnc string
	for i, gp := range p.GetGnbs() {
		cfg, err := gnbConfigFromProto(gp)
		if err != nil {
			return engine.LoadSpec{}, err
		}
		if i == 0 {
			mcc, mnc = cfg.MCC, cfg.MNC
		} else if cfg.MCC != mcc || cfg.MNC != mnc {
			return engine.LoadSpec{}, fmt.Errorf("all gnbs must share one PLMN; gnb %d is %s/%s, not %s/%s",
				i, cfg.MCC, cfg.MNC, mcc, mnc)
		}
		gnbs = append(gnbs, engine.GNBSpec{Config: cfg, AMFAddr: p.GetAmfAddress(), BindAddr: binds[i]})
	}

	rate, err := loadRate(p)
	if err != nil {
		return engine.LoadSpec{}, err
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
	// The same rule the in-process load applies: a malformed base_imsi is one
	// error before any association is opened (InvalidArgument over the wire).
	if err := spec.Validate(); err != nil {
		return engine.LoadSpec{}, err
	}
	if pdu := p.GetPduSession(); pdu != nil {
		if pdu.GetSst() > 0xFF {
			return engine.LoadSpec{}, fmt.Errorf("sst %d exceeds one octet", pdu.GetSst())
		}
		spec.PDUSession = &ue.PDUSessionParams{
			PDUSessionID: uint8(pdu.GetPduSessionId()),
			SST:          uint8(pdu.GetSst()),
			SD:           pdu.GetSd(),
			DNN:          pdu.GetDnn(),
		}
		n3, err := gnb.CanonicalN3(p.GetGnbN3Addr())
		if err != nil {
			return engine.LoadSpec{}, fmt.Errorf("gnb_n3_addr: %w", err)
		}
		spec.GNBN3Addr = n3
	}

	return spec, nil
}

// loadRun is a load arm's launcher and leases.
func loadRun(log *slog.Logger, p *orbitv1.LoadRunSpec) (engine.LoadRunFunc, engine.Needs, error) { //nolint:staticcheck // the deprecated arm, kept for API clients
	spec, err := loadSpecFrom(p)
	if err != nil {
		return nil, engine.Needs{}, err
	}
	return loadRunFrom(log, spec)
}

// loadRunFrom is a built load spec's launcher and leases: the storm
// strategy of RunPopulation (engine.RunLoad).
func loadRunFrom(log *slog.Logger, spec engine.LoadSpec) (engine.LoadRunFunc, engine.Needs, error) {
	needs, err := engine.LoadNeeds(spec)
	if err != nil {
		return nil, engine.Needs{}, err
	}
	return func(ctx context.Context, stats *load.LiveStats, emit engine.RunEventFunc) (load.Report, error) {
		s := spec
		// Fan out each attempt to the live aggregates and to a run-event
		// reporter that turns failures into discrete events.
		s.Observer = load.Observers{stats, failureEvents(emit)}
		return engine.RunLoad(ctx, log, s)
	}, needs, nil
}

// loadRunFunc is loadRun without the leases.
func loadRunFunc(log *slog.Logger, p *orbitv1.LoadRunSpec) (engine.LoadRunFunc, error) { //nolint:staticcheck // the deprecated arm, kept for API clients
	fn, _, err := loadRun(log, p)
	return fn, err
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

// fleetSpecFrom is a parsed fleet arm's engine spec and behaviours, with
// the request's credentials: the server cannot expand the client's
// secrets from its own environment, so the YAML's ${ENV} are not used.
func fleetSpecFrom(f *scenario.FleetScenario, creds *orbitv1.Credentials) (engine.FleetRunSpec, engine.FleetBehaviors, error) {
	ki, err := auth.ParseHexKey("Ki", creds.GetKi())
	if err != nil {
		return engine.FleetRunSpec{}, engine.FleetBehaviors{}, err
	}
	opc, err := auth.ParseHexKey("OPc", creds.GetOpc())
	if err != nil {
		return engine.FleetRunSpec{}, engine.FleetBehaviors{}, err
	}
	return scenario.BuildFleetRun(f, ki, opc)
}

// fleetRunFrom is a built fleet spec's leases and launcher.
func fleetRunFrom(log *slog.Logger, mgr *engine.Manager, spec engine.FleetRunSpec, beh engine.FleetBehaviors, verbosity engine.EventVerbosity) (engine.FleetRunFunc, engine.Needs, error) {
	needs, err := engine.FleetNeeds(spec)
	if err != nil {
		return nil, engine.Needs{}, err
	}
	return func(ctx context.Context, stats *engine.FleetLiveStats, emit engine.RunEventFunc) (engine.FleetReport, error) {
		return engine.RunFleet(ctx, log, spec, beh, engine.FleetDeps{
			Stats:   stats,
			Events:  engine.NewRunEvents(emit, verbosity),
			Manager: mgr,
		})
	}, needs, nil
}

// loadRate turns the ramp/rate fields into a load.Rate (nil = concurrency-bound).
func loadRate(p *orbitv1.LoadRunSpec) (load.Rate, error) { //nolint:staticcheck // the deprecated arm, kept for API clients
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

// runStartError maps a StartLoad error to a Connect code. A run refused for
// a resource another run holds (ErrLeaseConflict, naming both) is a
// precondition failure, not an internal error.
func runStartError(err error) *connect.Error {
	var held *engine.ErrLeaseConflict
	if errors.As(err, &held) {
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
	for _, k := range info.WorkloadKinds {
		r.WorkloadKinds = append(r.WorkloadKinds, runKindProto(k))
	}
	r.StoppedEarly = info.StoppedEarly
	return r
}

func runKindProto(k engine.RunKind) orbitv1.RunKind {
	switch k {
	case engine.RunKindLoad:
		return orbitv1.RunKind_RUN_KIND_LOAD
	case engine.RunKindFleet:
		return orbitv1.RunKind_RUN_KIND_FLEET
	case engine.RunKindConformance:
		return orbitv1.RunKind_RUN_KIND_CONFORMANCE
	case engine.RunKindMixed:
		return orbitv1.RunKind_RUN_KIND_MIXED
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
	case orbitv1.RunKind_RUN_KIND_CONFORMANCE:
		return engine.RunKindConformance
	case orbitv1.RunKind_RUN_KIND_MIXED:
		return engine.RunKindMixed
	default:
		return "" // unspecified = no filter
	}
}

// runStateLabel names a proto run state for a message. Only the archive path
// needs it: everywhere else the engine's own RunState is still to hand.
func runStateLabel(s orbitv1.RunState) string {
	switch s {
	case orbitv1.RunState_RUN_STATE_PENDING:
		return string(engine.RunPending)
	case orbitv1.RunState_RUN_STATE_RUNNING:
		return string(engine.RunRunning)
	case orbitv1.RunState_RUN_STATE_DRAINING:
		return string(engine.RunDraining)
	case orbitv1.RunState_RUN_STATE_COMPLETE:
		return string(engine.RunComplete)
	case orbitv1.RunState_RUN_STATE_FAILED:
		return string(engine.RunFailed)
	case orbitv1.RunState_RUN_STATE_CANCELLED:
		return string(engine.RunCancelled)
	}
	return "UNSPECIFIED"
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
	return &orbitv1.LoadReport{
		Attempted:     uint32(r.Attempted),
		Succeeded:     uint32(r.Succeeded),
		Failed:        uint32(r.Failed),
		DurationMs:    r.Duration.Milliseconds(),
		AchievedRate:  r.AchievedRate,
		Latency:       procedureLatencies(r.Latencies),
		Resources:     resourceSamples(r.Resources),
		DistinctSupis: uint32(r.DistinctSUPIs),
		Abandoned:     uint32(r.Abandoned),
	}
}

func resourceSamples(rs []load.ResourceSample) []*orbitv1.ResourceSample {
	var out []*orbitv1.ResourceSample
	for _, s := range rs {
		out = append(out, &orbitv1.ResourceSample{
			AtMs:       s.At.Milliseconds(),
			Goroutines: uint32(s.Goroutines),
			RssBytes:   s.RSSBytes,
		})
	}
	return out
}

func fleetReportProto(r engine.FleetReport) *orbitv1.FleetReport {
	return &orbitv1.FleetReport{
		Attached:        uint32(r.Attached),
		AttachFailed:    uint32(r.AttachFailed),
		Abandoned:       uint32(r.Abandoned),
		AttachElapsedMs: r.AttachElapsed.Milliseconds(),
		Handovers:       uint32(r.Handovers),
		HandoverErrors:  uint32(r.HandoverErr),
		TrafficFlows:    uint32(r.TrafficFlows),
		TrafficBytes:    r.TrafficBytes,
		Deregistered:    uint32(r.Deregistered),
		PerGnb:          gnbProgressProtos(r.PerGNB),
		Latency:         procedureLatencies(r.Latencies),
		AchievedRate:    r.AchievedRate,
		Resources:       resourceSamples(r.Resources),
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

// leasesProto is a run's leases on the wire.
func leasesProto(ls []engine.LeaseInfo) []*orbitv1.Lease {
	out := make([]*orbitv1.Lease, 0, len(ls))
	for _, l := range ls {
		pl := &orbitv1.Lease{Plmn: l.PLMN, FirstSupi: l.FirstSUPI, LastSupi: l.LastSUPI, Amf: l.AMF, GnbId: l.GNBID, N2Ip: l.N2IP}
		switch l.Resource {
		case "supi_block":
			pl.Resource = orbitv1.LeaseResource_LEASE_RESOURCE_SUPI_BLOCK
		case "gnb_id":
			pl.Resource = orbitv1.LeaseResource_LEASE_RESOURCE_GNB_ID
		case "n2_ip":
			pl.Resource = orbitv1.LeaseResource_LEASE_RESOURCE_N2_IP
		}
		out = append(out, pl)
	}
	return out
}
