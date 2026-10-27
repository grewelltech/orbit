package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/netip"
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
	RunKindLoad        RunKind = "load"
	RunKindFleet       RunKind = "fleet"
	RunKindConformance RunKind = "conformance"
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

// Terminal reports whether a state is final.
func (s RunState) Terminal() bool {
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

// RunOptions are the per-run settings the registry applies to every kind, so a
// new one does not mean another parameter on each Start* call.
type RunOptions struct {
	// Name is a caller-supplied label; the server names the run if empty.
	Name string
	// Verbosity selects the event-emission policy and the ring size.
	Verbosity EventVerbosity
	// Needs are the resources the run leases while active (LoadNeeds,
	// FleetNeeds). A run is refused with ErrLeaseConflict when another
	// active run holds any of them.
	Needs Needs
	// OnRelease, when set, runs once the run's leases are released (the
	// launcher returned, or the run never started after registering), off
	// the registry's lock: a caller returns what it allocated for the run,
	// such as gNB IDs from a GNBRotation.
	OnRelease func()
	// Judge, when set, judges a completed run's report (the *load.Report or
	// *FleetReport its launcher returned) before the run reads COMPLETE, and
	// the registry keeps what it returns beside the report for as long as
	// it keeps the run (Judgement): a plan's gate results, which then never
	// depend on anything the caller may have let go of since.
	Judge func(report any) any
}

// LoadRunFunc executes a load run to completion, reporting each attempt to
// stats as it goes. It is the seam between the registry (which owns lifecycle)
// and the engine's RunLoad (which owns execution), so the registry is testable
// without a live core.
type LoadRunFunc func(ctx context.Context, stats *load.LiveStats, emit RunEventFunc) (load.Report, error)

// run is the registry's mutable record. Every field is guarded by
// RunRegistry.mu; nothing here is touched without it. The event ring has its
// own lock and is safe to read after the record is published.
type run struct {
	info      RunInfo
	live      *load.LiveStats // live aggregates while running (load kind; nil otherwise)
	fleetLive *FleetLiveStats // live aggregates while running (fleet kind; nil otherwise)
	suiteLive any             // the caller's live progress (conformance kind; nil otherwise)
	events    *eventRing      // per-run discrete-event log
	report    any             // *load.Report or *FleetReport, set on COMPLETE
	cancel    context.CancelFunc
	leases    []LeaseInfo // what the run leased, kept after release for the archive
	onRelease func()
	judge     func(report any) any
	judgement any // Judge's result, set with report on COMPLETE
}

// FleetRunFunc executes a fleet run to completion, publishing its aggregates
// into stats as it goes so RunTelemetry can report progress before the
// end-of-run FleetReport exists.
type FleetRunFunc func(ctx context.Context, stats *FleetLiveStats, emit RunEventFunc) (FleetReport, error)

// DefaultRunEventCap bounds the per-run event ring. Sized so a soak keeps its
// whole interesting history rather than the last few seconds.
//
// The number is a question of how much history is useful, not of cost: a
// retained event measures ~120 B, so this is ~240 KB per run and ~12 MB across
// the 50 runs the registry keeps, and emit is O(1) in capacity (eventRing.buf
// overwrites in place rather than shifting — at cap 10000 that is the
// difference between 22 µs and 0.24 µs per event).
const DefaultRunEventCap = 2000

// runFunc is the kind-agnostic launcher the registry runs on a goroutine. It
// returns a pointer to the kind's report (or nil), which is retained only when
// the run completes cleanly.
type runFunc func(ctx context.Context) (any, error)

// RunRegistry owns run identity, lifecycle, and a bounded history. Runs execute
// in goroutines it launches; a run outlives the client that started it, and is
// stopped only by an explicit Stop (ADR-0005).
type RunRegistry struct {
	log        *slog.Logger
	maxHistory int

	mu     sync.Mutex
	runs   map[string]*run
	order  []string    // insertion order, for stable listing and history eviction
	leases *leaseTable // what each active run holds (ADR-0008 §5)
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
		leases:     newLeaseTable(),
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
// Policy (ADR-0008 §5): runs lease what they use (opts.Needs). A run whose
// leases overlap an active run's is rejected with ErrLeaseConflict, naming
// the resource and the holder, rather than queued; runs that share nothing
// run concurrently, whatever their kind.
func (r *RunRegistry) StartLoad(opts RunOptions, fn LoadRunFunc) (RunInfo, error) {
	live := load.NewLiveStats()
	return r.start(RunKindLoad, opts, live, nil, nil, func(emit RunEventFunc) runFunc {
		return func(ctx context.Context) (any, error) {
			rep, err := fn(ctx, live, emit)
			if err != nil {
				return nil, err
			}
			return &rep, nil
		}
	})
}

// StartFleet registers and launches a fleet run. Like StartLoad it takes the
// run's leases, and publishes live aggregates for RunTelemetry as the run
// proceeds.
func (r *RunRegistry) StartFleet(opts RunOptions, fn FleetRunFunc) (RunInfo, error) {
	live := NewFleetLiveStats()
	return r.start(RunKindFleet, opts, nil, live, nil, func(emit RunEventFunc) runFunc {
		return func(ctx context.Context) (any, error) {
			rep, err := fn(ctx, live, emit)
			if err != nil {
				return nil, err
			}
			return &rep, nil
		}
	})
}

// ConformanceRunFunc runs a conformance suite to completion. The suite's
// results are the caller's types (the engine does not import the suite):
// it publishes them as they come into the live holder it passed to
// StartConformance, and returns its report, kept on COMPLETE
// (ConformanceResult).
type ConformanceRunFunc func(ctx context.Context, emit RunEventFunc) (any, error)

// StartConformance registers and launches a conformance run, taking its
// leases as every kind does. live is the caller's progress holder, kept
// with the run for as long as the registry keeps it (ConformanceLive), so
// a reader can follow results without the launcher's help.
func (r *RunRegistry) StartConformance(opts RunOptions, live any, fn ConformanceRunFunc) (RunInfo, error) {
	return r.start(RunKindConformance, opts, nil, nil, live, func(emit RunEventFunc) runFunc {
		return func(ctx context.Context) (any, error) { return fn(ctx, emit) }
	})
}

// ConformanceLive is a conformance run's live progress holder, as passed
// to StartConformance; it outlives the run's end, for its partial results.
func (r *RunRegistry) ConformanceLive(id string) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.runs[id]; rec != nil && rec.suiteLive != nil {
		return rec.suiteLive, true
	}
	return nil, false
}

// ConformanceResult is a completed conformance run's report.
func (r *RunRegistry) ConformanceResult(id string) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.runs[id]; rec != nil && rec.info.Kind == RunKindConformance && rec.report != nil {
		return rec.report, true
	}
	return nil, false
}

// start registers a run of the given kind and launches exec on its own
// goroutine. Each kind supplies at most one live-aggregate holder (the other
// is nil). mkExec builds the launcher once the run's event emitter exists.
func (r *RunRegistry) start(kind RunKind, opts RunOptions, live *load.LiveStats, fleetLive *FleetLiveStats, suiteLive any, mkExec func(emit RunEventFunc) runFunc) (RunInfo, error) {
	// Canonicalising may resolve AMF names, so it happens before the lock:
	// a slow resolver must not stall every other call on the registry.
	want := r.leases.canonical(opts.Needs)
	r.mu.Lock()
	id := r.freshIDLocked()
	if err := r.leases.acquire(id, want); err != nil {
		r.mu.Unlock()
		return RunInfo{}, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Verbosity sizes the ring: a verbose run emits every UE transition, and
	// keeping the default capacity would let that volume evict its own detail.
	ring := newEventRing(opts.Verbosity.RingCap())
	rec := &run{
		info: RunInfo{
			ID: id, Kind: kind, Name: opts.Name,
			State: RunPending, StartedAt: time.Now(),
		},
		live:      live,
		fleetLive: fleetLive,
		suiteLive: suiteLive,
		events:    ring,
		cancel:    cancel,
		leases:    want.describe(),
		onRelease: opts.OnRelease,
		judge:     opts.Judge,
	}
	r.runs[id] = rec
	r.order = append(r.order, id)
	info := rec.info
	r.mu.Unlock()

	fn, err := r.launcher(id, cancel, ring, mkExec)
	if err != nil {
		return RunInfo{}, err
	}
	go r.exec(ctx, id, ring, fn)
	return info, nil
}

// launcher builds a registered run's launcher. If building it panics, the
// run never starts: its record and leases are dropped and StartRun fails,
// rather than leaving resources held by a run that will never release
// them.
func (r *RunRegistry) launcher(id string, cancel context.CancelFunc, ring *eventRing, mkExec func(emit RunEventFunc) runFunc) (fn runFunc, err error) {
	defer func() {
		if p := recover(); p != nil {
			cancel()
			r.mu.Lock()
			var hook func()
			if rec := r.runs[id]; rec != nil {
				hook = rec.onRelease
			}
			defer func() {
				if hook != nil {
					hook()
				}
			}()
			r.leases.release(id)
			delete(r.runs, id)
			kept := r.order[:0]
			for _, o := range r.order {
				if o != id {
					kept = append(kept, o)
				}
			}
			r.order = kept
			r.mu.Unlock()
			if r.log != nil {
				r.log.Error("run launcher setup panicked", "run", id, "panic", p, "stack", string(debug.Stack()))
			}
			fn, err = nil, fmt.Errorf("run setup panicked: %v", p)
		}
	}()
	return mkExec(ring.emit), nil
}

// exec runs the launcher and records the outcome. It runs on its own goroutine;
// the run survives a client disconnecting.
func (r *RunRegistry) exec(ctx context.Context, id string, ring *eventRing, fn runFunc) {
	if r.markRunning(id) {
		ring.emit("info", "RUN", "", "run started")
	}

	report, err := r.guard(ctx, id, fn)
	var judgement any
	if err == nil && ctx.Err() == nil {
		judgement = r.judge(id, report)
	}

	// Decide the terminal state before committing it.
	var state RunState
	var errMsg string
	switch {
	case ctx.Err() != nil:
		// A cancelled context is a requested stop, whatever the launcher returned.
		state = RunCancelled
	case err != nil:
		state, errMsg = RunFailed, err.Error()
	default:
		state = RunComplete
	}

	// Emit the terminal event BEFORE flipping the observable state, so a client
	// that sees the run terminal is guaranteed to have already received (or have
	// buffered) its final event — no missed last event on the RunEvents stream.
	msg := "run " + string(state)
	sev := "info"
	if state == RunFailed {
		sev, msg = "error", "run failed: "+errMsg
	}
	ring.emit(sev, "RUN", "", msg)

	var hook func()
	defer func() {
		if hook != nil {
			hook() // after the unlock below: hooks must not run under r.mu
		}
	}()
	r.mu.Lock()
	defer r.mu.Unlock()
	// The launcher has returned, its drain with it (UEs deregistered,
	// associations closed), so its leases go now: before the state reads
	// terminal, so a client that sees it terminal can start a run on the
	// same resources. A launcher that panicked reaches here too (guard).
	r.leases.release(id)
	rec := r.runs[id]
	if rec != nil {
		hook = rec.onRelease
	}
	if rec == nil {
		return // evicted — impossible while terminal-eviction skips this run, but defensive
	}
	rec.info.EndedAt = time.Now()
	rec.info.State = state
	rec.info.Err = errMsg
	if state == RunComplete {
		rec.report, rec.judgement = report, judgement
	}
	if r.log != nil {
		r.log.Info("run finished", "run", id, "kind", rec.info.Kind, "state", state)
	}
	r.evictLocked()
}

// guard runs the launcher, converting a panic into an error so a bug in the
// run cannot crash the daemon and take every other run down with it — and so
// the run still reaches a terminal state, and releases its leases, rather
// than sticking in RUNNING and holding them forever.
func (r *RunRegistry) guard(ctx context.Context, id string, fn runFunc) (report any, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("run panicked: %v", p)
			if r.log != nil {
				r.log.Error("run panicked", "run", id, "panic", p, "stack", string(debug.Stack()))
			}
		}
	}()
	return fn(ctx)
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
	if !rec.info.State.Terminal() {
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

// FleetSnapshot returns a fleet run's live aggregates. ok is false for a run
// that is not a fleet run, or that the registry has since evicted.
func (r *RunRegistry) FleetSnapshot(id string) (FleetSnapshot, bool) {
	r.mu.Lock()
	live := func() *FleetLiveStats {
		if rec := r.runs[id]; rec != nil {
			return rec.fleetLive
		}
		return nil
	}()
	r.mu.Unlock()
	if live == nil {
		return FleetSnapshot{}, false
	}
	return live.Snapshot(), true
}

// Report returns a completed run's final report. It is available only once the
// run has reached COMPLETE; a running or failed run returns false.
func (r *RunRegistry) Report(id string) (load.Report, bool) {
	if rep, ok := r.reportAny(id).(*load.Report); ok {
		return *rep, true
	}
	return load.Report{}, false
}

// FleetResult returns a completed fleet run's report.
func (r *RunRegistry) FleetResult(id string) (FleetReport, bool) {
	if rep, ok := r.reportAny(id).(*FleetReport); ok {
		return *rep, true
	}
	return FleetReport{}, false
}

// Judgement returns what the run's Judge made of its report: set once the
// run reads COMPLETE, and kept while the registry keeps the run.
func (r *RunRegistry) Judgement(id string) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.runs[id]; rec != nil && rec.judgement != nil {
		return rec.judgement, true
	}
	return nil, false
}

// judge runs the run's Judge on its report, off the registry's lock (a
// Judge is the caller's code). A panicking Judge judges nothing.
func (r *RunRegistry) judge(id string, report any) (out any) {
	r.mu.Lock()
	var fn func(any) any
	if rec := r.runs[id]; rec != nil {
		fn = rec.judge
	}
	r.mu.Unlock()
	if fn == nil || report == nil {
		return nil
	}
	defer func() {
		if p := recover(); p != nil {
			if r.log != nil {
				r.log.Error("run judge panicked", "run", id, "panic", p)
			}
			out = nil
		}
	}()
	return fn(report)
}

// reportAny returns the run's retained report pointer, or nil.
func (r *RunRegistry) reportAny(id string) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.runs[id]; rec != nil {
		return rec.report
	}
	return nil
}

// StopAll cancels every active run. Called on server shutdown so runs do not
// outlive the process silently.
func (r *RunRegistry) StopAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.runs {
		if !rec.info.State.Terminal() {
			rec.info.State = RunDraining
			rec.cancel()
		}
	}
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
func (r *RunRegistry) markRunning(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.runs[id]; rec != nil && rec.info.State == RunPending {
		rec.info.State = RunRunning
		return true
	}
	return false
}

// SubscribeEvents returns a run's event backlog from fromSeq plus a live feed,
// taken atomically. The returned cancel func must be called to release the
// subscription. A run without a ring (unknown id) returns nil.
func (r *RunRegistry) SubscribeEvents(id string, fromSeq uint64) (*EventSubscription, error) {
	r.mu.Lock()
	ring := func() *eventRing {
		if rec := r.runs[id]; rec != nil {
			return rec.events
		}
		return nil
	}()
	r.mu.Unlock()
	if ring == nil {
		return nil, &ErrRunNotFound{ID: id}
	}
	return ring.subscribeFrom(fromSeq), nil
}

// EventsSince returns a run's retained events with Seq >= fromSeq. It lets a
// consumer recover events its live subscription dropped (they remain in the
// ring), e.g. to reconcile at the end of a stream. Unknown id yields nil.
func (r *RunRegistry) EventsSince(id string, fromSeq uint64) []RunEvent {
	r.mu.Lock()
	ring := func() *eventRing {
		if rec := r.runs[id]; rec != nil {
			return rec.events
		}
		return nil
	}()
	r.mu.Unlock()
	if ring == nil {
		return nil
	}
	return ring.snapshotSince(fromSeq)
}

// evictLocked trims terminal runs beyond maxHistory, oldest first. Active runs
// are never evicted, so a burst of long runs cannot drop one that is still
// going. Callers hold r.mu.
func (r *RunRegistry) evictLocked() {
	var terminal []string
	for _, id := range r.order {
		if rec := r.runs[id]; rec != nil && rec.info.State.Terminal() {
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

// ErrRunNotFound is returned for an unknown run id.
type ErrRunNotFound struct{ ID string }

func (e *ErrRunNotFound) Error() string { return fmt.Sprintf("run %s not found", e.ID) }

// Leases is what run id leased: while it is active, and after (for its
// archive). Unknown ids have none.
func (r *RunRegistry) Leases(id string) []LeaseInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.runs[id]; rec != nil {
		return append([]LeaseInfo(nil), rec.leases...)
	}
	return nil
}

// AMFIdentity is an AMF's identity set, sorted: the address as written
// (lower-cased) and every IP:port it resolves to, as leases compare AMFs.
// It resolves names, so call it outside any lock; it takes none.
func (r *RunRegistry) AMFIdentity(amf string) []string {
	res := &resolver{t: r.leases, seen: map[string][]netip.Addr{}}
	id := res.amfIdentity(amf)
	out := make([]string, 0, len(id))
	for k := range id {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Lock order: GNBRotation.Assign holds its own lock while calling the
// returned check, which takes r.mu; nothing calls the rotation under r.mu
// (OnRelease runs after it is released).
//
// GNBInUse returns a check of whether a gNB ID at amf (any of its names or
// addresses, as leases compare them) in plmn is leased by an active run, for
// a GNBRotation's Assign. amf is resolved once, here, outside the lock.
func (r *RunRegistry) GNBInUse(amf, plmn string) func(id uint32) bool {
	res := &resolver{t: r.leases, seen: map[string][]netip.Addr{}}
	ident := res.amfIdentity(amf)
	return func(id uint32) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, l := range r.leases.held {
			for _, g := range l.gnbs {
				if g.plmn == plmn && g.id == id && g.amf.intersects(ident) {
					return true
				}
			}
		}
		return false
	}
}
