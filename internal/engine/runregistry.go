package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/load"
)

// RunKind identifies what a run does. The registry is kind-agnostic; each kind
// supplies its own launcher (ADR-0005 build order, step 3).
type RunKind string

const (
	RunKindLoad  RunKind = "load"
	RunKindFleet RunKind = "fleet"
)

// RunState is a run's lifecycle position.
//
//	PENDING → RUNNING → (DRAINING) → COMPLETE | FAILED | CANCELLED
//
// DRAINING is entered when a stop is requested; the terminal state is decided
// once the launcher goroutine returns.
type RunState string

const (
	RunPending   RunState = "PENDING"
	RunRunning   RunState = "RUNNING"
	RunDraining  RunState = "DRAINING"
	RunComplete  RunState = "COMPLETE"
	RunFailed    RunState = "FAILED"
	RunCancelled RunState = "CANCELLED"
)

// terminal reports whether a state is final.
func (s RunState) terminal() bool {
	return s == RunComplete || s == RunFailed || s == RunCancelled
}

// RunInfo is a snapshot of a run's identity and lifecycle, safe to hand to a
// caller — it copies out from under the registry lock.
type RunInfo struct {
	ID        string
	Kind      RunKind
	Name      string
	State     RunState
	StartedAt time.Time
	EndedAt   time.Time // zero while not terminal
	Err       string    // set when State == RunFailed
}

// LoadRunFunc executes a load run to completion, reporting each attempt to
// stats as it goes. It is the seam between the registry (which owns lifecycle)
// and the engine's RunLoad (which owns execution), so the registry is testable
// without a live core.
type LoadRunFunc func(ctx context.Context, stats *load.LiveStats) (load.Report, error)

// run is the registry's mutable record. Every field is guarded by
// RunRegistry.mu; nothing here is touched without it.
type run struct {
	info   RunInfo
	live   *load.LiveStats // live aggregates while running (load kind)
	report *load.Report    // final report, set on COMPLETE
	cancel context.CancelFunc
}

// RunRegistry owns run identity, lifecycle, and a bounded history. Runs execute
// in goroutines it launches; a run outlives the client that started it, and is
// stopped only by an explicit Stop (ADR-0005).
type RunRegistry struct {
	log        *slog.Logger
	maxHistory int

	mu    sync.Mutex
	runs  map[string]*run
	order []string // insertion order, for stable listing and history eviction
}

// DefaultRunHistory is how many terminal runs the registry keeps. Active runs
// are never evicted.
const DefaultRunHistory = 50

// NewRunRegistry returns an empty registry. maxHistory <= 0 uses the default.
func NewRunRegistry(log *slog.Logger, maxHistory int) *RunRegistry {
	if maxHistory <= 0 {
		maxHistory = DefaultRunHistory
	}
	return &RunRegistry{
		log:        log,
		maxHistory: maxHistory,
		runs:       make(map[string]*run),
	}
}

// newRunID returns a short, unique, human-typeable run id (e.g. "run-7f3a91c2").
func newRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "run-" + hex.EncodeToString(b[:])
}

// StartLoad registers and launches a load run. name is a caller-supplied label.
//
// Policy (ADR-0005, initial): one active run per kind. A second is rejected
// with ErrRunActive rather than queued — whether concurrent runs against a
// single core are meaningful is a question about the core, revisited later.
func (r *RunRegistry) StartLoad(name string, fn LoadRunFunc) (RunInfo, error) {
	r.mu.Lock()
	if active := r.activeOfKindLocked(RunKindLoad); active != "" {
		r.mu.Unlock()
		return RunInfo{}, &ErrRunActive{Kind: RunKindLoad, ActiveID: active}
	}

	id := r.freshIDLocked()
	ctx, cancel := context.WithCancel(context.Background())
	live := load.NewLiveStats()
	rec := &run{
		info: RunInfo{
			ID: id, Kind: RunKindLoad, Name: name,
			State: RunPending, StartedAt: time.Now(),
		},
		live:   live,
		cancel: cancel,
	}
	r.runs[id] = rec
	r.order = append(r.order, id)
	info := rec.info
	r.mu.Unlock()

	go r.execLoad(ctx, id, live, fn)
	return info, nil
}

// execLoad runs the launcher and records the outcome. It runs on its own
// goroutine; the run survives a client disconnecting.
func (r *RunRegistry) execLoad(ctx context.Context, id string, live *load.LiveStats, fn LoadRunFunc) {
	r.markRunning(id)

	report, err := r.guard(id, ctx, live, fn)

	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.runs[id]
	if rec == nil {
		return // evicted — impossible while terminal-eviction skips this run, but defensive
	}
	rec.info.EndedAt = time.Now()
	switch {
	case ctx.Err() != nil:
		// A cancelled context is a requested stop, whatever error fn returned.
		rec.info.State = RunCancelled
	case err != nil:
		rec.info.State = RunFailed
		rec.info.Err = err.Error()
	default:
		rec.info.State = RunComplete
		rec.report = &report
	}
	if r.log != nil {
		r.log.Info("run finished", "run", id, "kind", rec.info.Kind, "state", rec.info.State)
	}
	r.evictLocked()
}

// guard runs the launcher, converting a panic into an error so a bug in the
// run cannot crash the daemon and take every other run down with it — and so
// the run still reaches a terminal state rather than sticking in RUNNING and
// blocking all future runs of its kind.
func (r *RunRegistry) guard(id string, ctx context.Context, live *load.LiveStats, fn LoadRunFunc) (report load.Report, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("run panicked: %v", p)
			if r.log != nil {
				r.log.Error("run panicked", "run", id, "panic", p, "stack", string(debug.Stack()))
			}
		}
	}()
	return fn(ctx, live)
}

// Stop requests cancellation of a run. It is idempotent and returns the run's
// info as of the request. Stopping an already-terminal run is a no-op.
func (r *RunRegistry) Stop(id string) (RunInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.runs[id]
	if rec == nil {
		return RunInfo{}, &ErrRunNotFound{ID: id}
	}
	if !rec.info.State.terminal() {
		rec.info.State = RunDraining
		rec.cancel()
	}
	return rec.info, nil
}

// Get returns a run's info snapshot.
func (r *RunRegistry) Get(id string) (RunInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.runs[id]
	if rec == nil {
		return RunInfo{}, &ErrRunNotFound{ID: id}
	}
	return rec.info, nil
}

// List returns every retained run, newest first.
func (r *RunRegistry) List() []RunInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RunInfo, 0, len(r.order))
	for _, id := range r.order {
		if rec := r.runs[id]; rec != nil {
			out = append(out, rec.info)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// Snapshot returns the live aggregate view of a run. It is meaningful while the
// run is active; a terminal run returns its last live view. Runs without live
// stats (a kind that does not report to LiveStats) return false.
func (r *RunRegistry) Snapshot(id string) (load.Snapshot, bool) {
	r.mu.Lock()
	live := func() *load.LiveStats {
		if rec := r.runs[id]; rec != nil {
			return rec.live
		}
		return nil
	}()
	r.mu.Unlock()
	if live == nil {
		return load.Snapshot{}, false
	}
	return live.Snapshot(), true
}

// Report returns a completed run's final report. It is available only once the
// run has reached COMPLETE; a running or failed run returns false.
func (r *RunRegistry) Report(id string) (load.Report, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.runs[id]
	if rec == nil || rec.report == nil {
		return load.Report{}, false
	}
	return *rec.report, true
}

// StopAll cancels every active run. Called on server shutdown so runs do not
// outlive the process silently.
func (r *RunRegistry) StopAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.runs {
		if !rec.info.State.terminal() {
			rec.info.State = RunDraining
			rec.cancel()
		}
	}
}

// activeOfKindLocked returns the id of an active run of the given kind, or "".
func (r *RunRegistry) activeOfKindLocked(kind RunKind) string {
	for _, id := range r.order {
		rec := r.runs[id]
		if rec != nil && rec.info.Kind == kind && !rec.info.State.terminal() {
			return id
		}
	}
	return ""
}

// freshIDLocked returns an id not currently in use.
func (r *RunRegistry) freshIDLocked() string {
	for {
		id := newRunID()
		if _, exists := r.runs[id]; !exists {
			return id
		}
	}
}

// markRunning promotes a PENDING run to RUNNING. It promotes only from PENDING,
// so a run already DRAINING — a Stop that landed before the launcher goroutine
// was scheduled — is not flipped back to RUNNING, which would transiently hide
// a stop a client had already been told succeeded.
func (r *RunRegistry) markRunning(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.runs[id]; rec != nil && rec.info.State == RunPending {
		rec.info.State = RunRunning
	}
}

// evictLocked trims terminal runs beyond maxHistory, oldest first. Active runs
// are never evicted, so a burst of long runs cannot drop one that is still
// going. Callers hold r.mu.
func (r *RunRegistry) evictLocked() {
	var terminal []string
	for _, id := range r.order {
		if rec := r.runs[id]; rec != nil && rec.info.State.terminal() {
			terminal = append(terminal, id)
		}
	}
	drop := len(terminal) - r.maxHistory
	if drop <= 0 {
		return
	}
	evicted := make(map[string]bool, drop)
	for _, id := range terminal[:drop] {
		delete(r.runs, id)
		evicted[id] = true
	}
	kept := r.order[:0]
	for _, id := range r.order {
		if !evicted[id] {
			kept = append(kept, id)
		}
	}
	r.order = kept
}

// ErrRunActive is returned when a run of the same kind is already active.
type ErrRunActive struct {
	Kind     RunKind
	ActiveID string
}

func (e *ErrRunActive) Error() string {
	return fmt.Sprintf("a %s run is already active (%s)", e.Kind, e.ActiveID)
}

// ErrRunNotFound is returned for an unknown run id.
type ErrRunNotFound struct{ ID string }

func (e *ErrRunNotFound) Error() string { return fmt.Sprintf("run %s not found", e.ID) }
