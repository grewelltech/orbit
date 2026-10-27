package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/load"
)

// RunKindMixed is a run of more than one workload (ADR-0008 §2): a
// population and a conformance suite together, "conformance under load".
const RunKindMixed RunKind = "mixed"

// Phase is how far a workload has got, for another's start condition.
type Phase int

const (
	// PhaseStarted is a workload whose launcher has begun.
	PhaseStarted Phase = iota + 1
	// PhaseAttached is a population whose attach phase is done: every UE
	// of a persistent population has attached or failed to, or a storm
	// has completed one pass of its count.
	PhaseAttached
	// PhaseComplete is a workload whose launcher has returned cleanly.
	PhaseComplete
)

func (p Phase) String() string {
	switch p {
	case PhaseStarted:
		return "started"
	case PhaseAttached:
		return "attached"
	case PhaseComplete:
		return "complete"
	}
	return fmt.Sprintf("phase(%d)", int(p))
}

// MixedWorkload is one workload of a mixed run: its launcher (exactly one
// of Load, Fleet and Conformance), and when it starts.
type MixedWorkload struct {
	Name        string
	Load        LoadRunFunc
	Fleet       FleetRunFunc
	Conformance ConformanceRunFunc
	// SuiteLive is a conformance workload's live holder (as for
	// StartConformance).
	SuiteLive any
	// Count is a population's attach count, which PhaseAttached waits on.
	Count int
	// After, when set, names the workload this one waits for, until it
	// reaches Phase. A workload whose condition can no longer be met (the
	// other ended without reaching it) is skipped.
	After string
	Phase Phase
}

// Kind is the workload's kind, by its launcher.
func (w MixedWorkload) Kind() RunKind {
	switch {
	case w.Load != nil:
		return RunKindLoad
	case w.Fleet != nil:
		return RunKindFleet
	default:
		return RunKindConformance
	}
}

// Workload states, as a mixed run's progress reports them.
const (
	WorkloadWaiting = "waiting"
	WorkloadRunning = "running"
	WorkloadDone    = "done"
	WorkloadFailed  = "failed"
	WorkloadSkipped = "skipped"
)

// MixedProgress is one workload of a mixed run, as it stands: its state
// and its live holder (one of Load, Fleet, Suite).
type MixedProgress struct {
	Name   string
	Kind   RunKind
	State  string
	Detail string // why it failed or was skipped
	Load   *load.LiveStats
	Fleet  *FleetLiveStats
	Suite  any
	// Report is the workload's report once it ended done, so a run that
	// ends otherwise still has each finished workload's.
	Report any
}

// MixedReport is a mixed run's outcome per workload, in plan order: its
// report (a *load.Report, *FleetReport, or the suite's report), or why it
// has none.
type MixedReport struct {
	Workloads []MixedResult
}

// MixedResult is one workload's outcome.
type MixedResult struct {
	Name   string
	Kind   RunKind
	State  string // WorkloadDone, WorkloadFailed or WorkloadSkipped
	Detail string
	Report any
}

// mixedState is one workload's progress within a run: its phases, latched
// as they are reached, and its end.
type mixedState struct {
	w     MixedWorkload
	live  *load.LiveStats
	fleet *FleetLiveStats

	mu      sync.Mutex
	state   string
	detail  string
	report  any                     // the workload's report once it ends done
	reached map[Phase]chan struct{} // closed when the phase is reached
	ended   chan struct{}           // closed when the workload is over, however
}

func newMixedState(w MixedWorkload) *mixedState {
	s := &mixedState{w: w, state: WorkloadWaiting, ended: make(chan struct{}),
		reached: map[Phase]chan struct{}{PhaseStarted: make(chan struct{}), PhaseAttached: make(chan struct{}), PhaseComplete: make(chan struct{})}}
	switch w.Kind() {
	case RunKindLoad:
		s.live = load.NewLiveStats()
	case RunKindFleet:
		s.fleet = NewFleetLiveStats()
	}
	return s
}

// reach latches phase p.
func (s *mixedState) reach(p Phase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.reached[p]:
	default:
		close(s.reached[p])
	}
}

// end records the workload's final state and report, once.
func (s *mixedState) end(state, detail string, report any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.ended:
		return
	default:
	}
	s.state, s.detail, s.report = state, detail, report
	close(s.ended)
}

func (s *mixedState) set(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

func (s *mixedState) progress() MixedProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return MixedProgress{Name: s.w.Name, Kind: s.w.Kind(), State: s.state, Detail: s.detail,
		Load: s.live, Fleet: s.fleet, Suite: s.w.SuiteLive, Report: s.report}
}

// attached reports whether a population has finished its attach phase.
func (s *mixedState) attached() bool {
	switch {
	case s.live != nil:
		return s.w.Count > 0 && s.live.Snapshot().Attempted >= s.w.Count
	case s.fleet != nil:
		snap := s.fleet.Snapshot()
		return s.w.Count > 0 && snap.Attached+snap.AttachFailed >= s.w.Count
	}
	return false
}

// partial is a stopped population's report from its live aggregates, the
// figures it had when it stopped; nil for a suite (its holder has them).
func (s *mixedState) partial() any {
	switch {
	case s.live != nil:
		snap := s.live.Snapshot()
		return &load.Report{Attempted: snap.Attempted, Succeeded: snap.Succeeded, Failed: snap.Failed,
			Duration: snap.Elapsed, AchievedRate: snap.AchievedRate, Latencies: snap.Latencies}
	case s.fleet != nil:
		snap := s.fleet.Snapshot()
		return &FleetReport{Attached: snap.Attached, AttachFailed: snap.AttachFailed,
			Handovers: snap.Handovers, HandoverErr: snap.HandoverErrors, TrafficFlows: snap.TrafficFlows,
			TrafficBytes: snap.UplinkBytes + snap.DownlinkBytes}
	}
	return nil
}

// attachedBy reports whether a population's final report shows its attach
// phase done.
func attachedBy(report any, count int) bool {
	switch r := report.(type) {
	case *load.Report:
		return count > 0 && r.Attempted >= count
	case *FleetReport:
		return count > 0 && r.Attached+r.AttachFailed >= count
	}
	return false
}

// attachPoll is how often a population is checked for PhaseAttached.
const attachPoll = 50 * time.Millisecond

// ValidateMixed checks a mixed run's workloads: unique names, one launcher
// each, and start conditions naming another workload, with no cycle.
func ValidateMixed(ws []MixedWorkload) error {
	if len(ws) < 2 {
		return errors.New("a mixed run needs at least two workloads")
	}
	by := map[string]MixedWorkload{}
	for _, w := range ws {
		n := 0
		for _, set := range []bool{w.Load != nil, w.Fleet != nil, w.Conformance != nil} {
			if set {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("workload %q: give exactly one launcher", w.Name)
		}
		if _, dup := by[w.Name]; dup || w.Name == "" {
			return fmt.Errorf("workload names must be unique and set: %q", w.Name)
		}
		by[w.Name] = w
	}
	for _, w := range ws {
		seen := map[string]bool{w.Name: true}
		for at := w; at.After != ""; {
			next, ok := by[at.After]
			if !ok {
				return fmt.Errorf("workload %q starts after %q, which the run does not have", at.Name, at.After)
			}
			if seen[next.Name] {
				return fmt.Errorf("workload %q: start conditions form a cycle", w.Name)
			}
			if at.Phase < PhaseStarted || at.Phase > PhaseComplete {
				return fmt.Errorf("workload %q: start after %q needs a phase", at.Name, at.After)
			}
			seen[next.Name] = true
			at = next
		}
	}
	return nil
}

// StartMixed registers and launches a run of several workloads under one
// set of leases (opts.Needs, the union of theirs). Each workload starts at
// once or when the one it waits for reaches its phase. The run completes
// when every workload has ended; it fails when one fails (a skipped one
// does not fail it alone, but the failure that skipped it does). Stopping
// it stops every workload.
func (r *RunRegistry) StartMixed(opts RunOptions, ws []MixedWorkload) (RunInfo, error) {
	if err := ValidateMixed(ws); err != nil {
		return RunInfo{}, err
	}
	states := make([]*mixedState, len(ws))
	byName := map[string]*mixedState{}
	for i, w := range ws {
		states[i] = newMixedState(w)
		byName[w.Name] = states[i]
	}
	return r.start(RunKindMixed, opts, nil, nil, nil, states, func(emit RunEventFunc) runFunc {
		return func(ctx context.Context) (any, error) { return runMixed(ctx, states, byName, emit, r.log) }
	})
}

// runMixed runs each workload on its own goroutine behind its start
// condition and gathers their outcomes.
func runMixed(ctx context.Context, states []*mixedState, byName map[string]*mixedState, emit RunEventFunc, log *slog.Logger) (any, error) {
	var wg sync.WaitGroup
	results := make([]MixedResult, len(states))
	for i, s := range states {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The registry's guard covers only the run's own goroutine: a
			// workload's panic is caught here, so it fails its workload
			// (and skips those waiting on it) rather than the server.
			defer func() {
				if p := recover(); p != nil {
					detail := fmt.Sprintf("workload panicked: %v", p)
					if log != nil {
						log.Error("run workload panicked", "workload", s.w.Name, "panic", p, "stack", string(debug.Stack()))
					}
					if emit != nil {
						emit("error", "WORKLOAD", "", "["+s.w.Name+"] "+detail)
					}
					results[i] = MixedResult{Name: s.w.Name, Kind: s.w.Kind(), State: WorkloadFailed, Detail: detail}
					s.end(WorkloadFailed, detail, nil)
				}
			}()
			results[i] = runWorkload(ctx, s, byName, emit)
		}()
	}
	wg.Wait()
	rep := &MixedReport{Workloads: results}
	var errs []error
	for _, res := range results {
		if res.State == WorkloadFailed {
			errs = append(errs, fmt.Errorf("workload %s: %s", res.Name, res.Detail))
		}
	}
	if len(errs) > 0 {
		return rep, errors.Join(errs...)
	}
	return rep, nil
}

// runWorkload waits for one workload's start condition, then runs it.
func runWorkload(ctx context.Context, s *mixedState, byName map[string]*mixedState, emit RunEventFunc) MixedResult {
	w := s.w
	tag := func(sev, kind, supi, msg string) {
		if emit != nil {
			emit(sev, kind, supi, "["+w.Name+"] "+msg)
		}
	}
	res := MixedResult{Name: w.Name, Kind: w.Kind()}
	if w.After != "" {
		dep := byName[w.After]
		select {
		case <-dep.reached[w.Phase]:
		case <-dep.ended:
			// It may have reached the phase as it ended.
			select {
			case <-dep.reached[w.Phase]:
			default:
				res.State, res.Detail = WorkloadSkipped, fmt.Sprintf("%s ended without reaching %s", w.After, w.Phase)
				s.end(res.State, res.Detail, nil)
				tag("warn", "WORKLOAD", "", "skipped: "+res.Detail)
				return res
			}
		case <-ctx.Done():
			res.State, res.Detail = WorkloadSkipped, "the run stopped before "+w.After+" reached "+w.Phase.String()
			s.end(res.State, res.Detail, nil)
			return res
		}
	}
	s.set(WorkloadRunning)
	s.reach(PhaseStarted)
	tag("info", "WORKLOAD", "", "started")

	// A population latches PhaseAttached as its attach phase completes.
	stop := make(chan struct{})
	defer close(stop) // also on a panic, which runMixed recovers
	if s.live != nil || s.fleet != nil {
		go func() {
			t := time.NewTicker(attachPoll)
			defer t.Stop()
			for {
				if s.attached() {
					s.reach(PhaseAttached)
					return
				}
				select {
				case <-stop:
					return
				case <-t.C:
				}
			}
		}()
	}
	var err error
	switch {
	case w.Load != nil:
		var rep load.Report
		rep, err = w.Load(ctx, s.live, tag)
		res.Report = &rep
	case w.Fleet != nil:
		var rep FleetReport
		rep, err = w.Fleet(ctx, s.fleet, tag)
		res.Report = &rep
	default:
		res.Report, err = w.Conformance(ctx, tag)
	}
	switch {
	case err != nil && ctx.Err() != nil:
		res.State, res.Detail = WorkloadSkipped, "stopped"
	case err != nil:
		res.State, res.Detail = WorkloadFailed, err.Error()
	default:
		res.State = WorkloadDone
		// The final report too: a fleet that attached and deregistered
		// inside one poll shows no attached UEs live any more.
		if s.attached() || attachedBy(res.Report, w.Count) {
			s.reach(PhaseAttached)
		}
		s.reach(PhaseComplete)
	}
	kept := res.Report
	if res.State != WorkloadDone {
		res.Report = nil
		// Stopped: what the population had so far, for the archive.
		kept = s.partial()
	}
	s.end(res.State, res.Detail, kept)
	tag("info", "WORKLOAD", "", res.State)
	return res
}

// MixedProgress is a mixed run's workloads as they stand, in plan order;
// nil for another kind of run.
func (r *RunRegistry) MixedProgress(id string) []MixedProgress {
	r.mu.Lock()
	var states []*mixedState
	if rec := r.runs[id]; rec != nil {
		states = rec.mixed
	}
	r.mu.Unlock()
	out := make([]MixedProgress, 0, len(states))
	for _, s := range states {
		out = append(out, s.progress())
	}
	return out
}

// MixedResult is a completed mixed run's report.
func (r *RunRegistry) MixedResult(id string) (*MixedReport, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.runs[id]; rec != nil {
		if rep, ok := rec.report.(*MixedReport); ok {
			return rep, true
		}
	}
	return nil, false
}
