package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/load"
)

func quietRegistry(maxHistory int) *RunRegistry {
	return NewRunRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), maxHistory)
}

// waitState polls until a run reaches want, or fails the test.
func waitState(t *testing.T, r *RunRegistry, id string, want RunState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		info, err := r.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if info.State == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	info, _ := r.Get(id)
	t.Fatalf("run %s never reached %s (stuck at %s)", id, want, info.State)
}

// A run that returns cleanly ends COMPLETE with its report retained.
func TestRunRegistryCompletes(t *testing.T) {
	r := quietRegistry(0)
	want := load.Report{Attempted: 10, Succeeded: 9, Failed: 1}

	info, err := r.StartLoad("soak", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		s.Observe(load.Sample{Metrics: map[string]time.Duration{"attach": time.Millisecond}})
		return want, nil
	})
	if err != nil {
		t.Fatalf("StartLoad: %v", err)
	}
	if info.State != RunPending && info.State != RunRunning {
		t.Errorf("initial state %s, want PENDING or RUNNING", info.State)
	}

	waitState(t, r, info.ID, RunComplete)

	got, ok := r.Report(info.ID)
	if !ok {
		t.Fatal("no report on a completed run")
	}
	if got.Attempted != want.Attempted || got.Succeeded != want.Succeeded {
		t.Errorf("report = %+v, want %+v", got, want)
	}
	if fin, _ := r.Get(info.ID); fin.EndedAt.IsZero() {
		t.Error("completed run has no end time")
	}
}

// A run that returns an error ends FAILED with the message, and no report.
func TestRunRegistryFails(t *testing.T) {
	r := quietRegistry(0)
	info, _ := r.StartLoad("bad", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		return load.Report{}, errors.New("AMF unreachable")
	})
	waitState(t, r, info.ID, RunFailed)

	got, _ := r.Get(info.ID)
	if got.Err != "AMF unreachable" {
		t.Errorf("error = %q, want %q", got.Err, "AMF unreachable")
	}
	if _, ok := r.Report(info.ID); ok {
		t.Error("a failed run must not expose a report")
	}
}

// Stop cancels the launcher's context and the run ends CANCELLED, regardless of
// what the launcher returns on its way out.
func TestRunRegistryStopCancels(t *testing.T) {
	r := quietRegistry(0)
	started := make(chan struct{})
	info, _ := r.StartLoad("long", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		close(started)
		<-ctx.Done()
		return load.Report{}, ctx.Err()
	})
	<-started

	if _, err := r.Stop(info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitState(t, r, info.ID, RunCancelled)
}

// A cancelled run is CANCELLED even if the launcher returns nil — a stop that
// races a clean finish must not read as COMPLETE.
func TestRunRegistryStopBeatsCleanReturn(t *testing.T) {
	r := quietRegistry(0)
	release := make(chan struct{})
	info, _ := r.StartLoad("racy", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		<-release
		return load.Report{Attempted: 1}, nil // clean return, but after a stop
	})

	if _, err := r.Stop(info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(release)
	waitState(t, r, info.ID, RunCancelled)
	if _, ok := r.Report(info.ID); ok {
		t.Error("a cancelled run must not expose a report even if the launcher returned one")
	}
}

// Only one run of a kind may be active; a second is rejected while the first runs.
func TestRunRegistryOneActivePerKind(t *testing.T) {
	r := quietRegistry(0)
	block := make(chan struct{})
	first, _ := r.StartLoad("first", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		<-block
		return load.Report{}, nil
	})

	_, err := r.StartLoad("second", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		return load.Report{}, nil
	})
	var active *ErrRunActive
	if !errors.As(err, &active) {
		t.Fatalf("second StartLoad error = %v, want ErrRunActive", err)
	}
	if active.ActiveID != first.ID {
		t.Errorf("ErrRunActive names %s, want the running %s", active.ActiveID, first.ID)
	}

	// Once the first finishes, a new run is allowed.
	close(block)
	waitState(t, r, first.ID, RunComplete)
	if _, err := r.StartLoad("third", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		return load.Report{}, nil
	}); err != nil {
		t.Errorf("StartLoad after the first completed: %v", err)
	}
}

// Snapshot exposes live aggregates while a run is in flight.
func TestRunRegistrySnapshotLive(t *testing.T) {
	r := quietRegistry(0)
	proceed := make(chan struct{})
	seen := make(chan struct{})
	info, _ := r.StartLoad("live", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		s.Observe(load.Sample{Metrics: map[string]time.Duration{"attach": time.Millisecond}})
		close(seen)
		<-proceed
		return load.Report{}, nil
	})

	<-seen
	snap, ok := r.Snapshot(info.ID)
	if !ok {
		t.Fatal("no snapshot for a running load run")
	}
	if snap.Succeeded != 1 {
		t.Errorf("snapshot succeeded = %d, want 1", snap.Succeeded)
	}
	close(proceed)
	waitState(t, r, info.ID, RunComplete)
}

// History is bounded: terminal runs beyond the cap are evicted oldest-first,
// and the newest survivors are kept.
func TestRunRegistryHistoryBounded(t *testing.T) {
	r := quietRegistry(3)
	var ids []string
	for i := 0; i < 6; i++ {
		info, err := r.StartLoad("n", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
			return load.Report{}, nil
		})
		if err != nil {
			t.Fatalf("StartLoad %d: %v", i, err)
		}
		waitState(t, r, info.ID, RunComplete)
		ids = append(ids, info.ID)
	}

	list := r.List()
	if len(list) != 3 {
		t.Fatalf("history holds %d runs, want the 3 cap", len(list))
	}
	// The three oldest must be gone, the three newest kept.
	for _, old := range ids[:3] {
		if _, err := r.Get(old); err == nil {
			t.Errorf("run %s should have been evicted", old)
		}
	}
	for _, recent := range ids[3:] {
		if _, err := r.Get(recent); err != nil {
			t.Errorf("run %s should have been kept: %v", recent, err)
		}
	}
}

// List is newest-first.
func TestRunRegistryListNewestFirst(t *testing.T) {
	r := quietRegistry(0)
	var ids []string
	for i := 0; i < 3; i++ {
		info, _ := r.StartLoad("n", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
			return load.Report{}, nil
		})
		waitState(t, r, info.ID, RunComplete)
		ids = append(ids, info.ID)
		time.Sleep(2 * time.Millisecond) // distinct start times
	}
	list := r.List()
	if len(list) != 3 {
		t.Fatalf("got %d runs", len(list))
	}
	if list[0].ID != ids[2] || list[2].ID != ids[0] {
		t.Errorf("order = %s..%s, want newest %s first", list[0].ID, list[2].ID, ids[2])
	}
}

// Get/Stop/Report on an unknown id return ErrRunNotFound (Report just returns false).
func TestRunRegistryUnknownID(t *testing.T) {
	r := quietRegistry(0)
	if _, err := r.Get("run-nope"); err == nil {
		t.Error("Get of unknown id returned no error")
	}
	if _, err := r.Stop("run-nope"); err == nil {
		t.Error("Stop of unknown id returned no error")
	}
	if _, ok := r.Report("run-nope"); ok {
		t.Error("Report of unknown id returned ok")
	}
}

// StopAll cancels every active run.
func TestRunRegistryStopAll(t *testing.T) {
	r := quietRegistry(0)
	started := make(chan struct{})
	info, _ := r.StartLoad("a", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		close(started)
		<-ctx.Done()
		return load.Report{}, ctx.Err()
	})
	<-started
	r.StopAll()
	waitState(t, r, info.ID, RunCancelled)
}

// The registry is safe under concurrent starts, stops, and reads. Run under -race.
func TestRunRegistryConcurrent(t *testing.T) {
	r := quietRegistry(5)
	var wg sync.WaitGroup

	// Readers.
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = r.List()
			}
		}()
	}

	// Serial starts (one active per kind), each stopped promptly.
	for i := 0; i < 20; i++ {
		info, err := r.StartLoad("n", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
			<-ctx.Done()
			return load.Report{}, ctx.Err()
		})
		if err != nil {
			// A previous run may not have finished cancelling yet; retry.
			time.Sleep(time.Millisecond)
			continue
		}
		_, _ = r.Stop(info.ID)
		waitState(t, r, info.ID, RunCancelled)
	}

	close(stop)
	wg.Wait()
}

// A launcher that panics must fail the run — not crash the process — and must
// leave the run terminal so it stops blocking future runs of its kind. That
// this test's process survives at all is half the assertion.
func TestRunRegistryPanicBecomesFailed(t *testing.T) {
	r := quietRegistry(0)
	info, err := r.StartLoad("boom", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		panic("nil map write deep in RunLoad")
	})
	if err != nil {
		t.Fatalf("StartLoad: %v", err)
	}
	waitState(t, r, info.ID, RunFailed)

	got, _ := r.Get(info.ID)
	if !strings.Contains(got.Err, "panicked") {
		t.Errorf("failed run error = %q, want it to mention the panic", got.Err)
	}

	// The kind is no longer active, so a new load run is accepted.
	if _, err := r.StartLoad("after", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		return load.Report{}, nil
	}); err != nil {
		t.Errorf("panicked run left the kind stuck active: %v", err)
	}
}

// markRunning promotes only from PENDING, so a Stop that set DRAINING before
// the launcher was scheduled is never flipped back to RUNNING.
func TestMarkRunningRespectsDraining(t *testing.T) {
	r := quietRegistry(0)

	// Seed a record directly in DRAINING, as a Stop-before-schedule leaves it.
	r.mu.Lock()
	r.runs["run-x"] = &run{info: RunInfo{ID: "run-x", Kind: RunKindLoad, State: RunDraining}, cancel: func() {}}
	r.order = append(r.order, "run-x")
	r.mu.Unlock()

	r.markRunning("run-x")
	if got, _ := r.Get("run-x"); got.State != RunDraining {
		t.Errorf("markRunning overwrote DRAINING with %s", got.State)
	}

	// And it does promote a genuinely pending run.
	r.mu.Lock()
	r.runs["run-y"] = &run{info: RunInfo{ID: "run-y", Kind: RunKindLoad, State: RunPending}, cancel: func() {}}
	r.order = append(r.order, "run-y")
	r.mu.Unlock()

	r.markRunning("run-y")
	if got, _ := r.Get("run-y"); got.State != RunRunning {
		t.Errorf("markRunning left a pending run at %s, want RUNNING", got.State)
	}
}

// A fleet run flows through the same lifecycle as a load run and exposes its
// FleetReport on completion.
func TestRunRegistryFleetCompletes(t *testing.T) {
	r := quietRegistry(0)
	want := FleetReport{Attached: 20, AttachFailed: 1, Handovers: 4, Deregistered: 20}
	info, err := r.StartFleet("fleet-1", func(ctx context.Context, _ *FleetLiveStats, _ RunEventFunc) (FleetReport, error) {
		return want, nil
	})
	if err != nil {
		t.Fatalf("StartFleet: %v", err)
	}
	if info.Kind != RunKindFleet {
		t.Errorf("kind = %s, want fleet", info.Kind)
	}
	waitState(t, r, info.ID, RunComplete)

	got, ok := r.FleetResult(info.ID)
	if !ok {
		t.Fatal("no fleet report on a completed fleet run")
	}
	if got.Attached != 20 || got.Handovers != 4 {
		t.Errorf("fleet report = %+v, want attached 20 / handovers 4", got)
	}
	// A fleet report must not masquerade as a load report.
	if _, ok := r.Report(info.ID); ok {
		t.Error("FleetReport leaked through the load Report accessor")
	}
}

// Load and fleet are independent kinds: one of each may be active at once.
func TestRunRegistryLoadAndFleetAreIndependentKinds(t *testing.T) {
	r := quietRegistry(0)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	if _, err := r.StartLoad("l", func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		<-block
		return load.Report{}, nil
	}); err != nil {
		t.Fatalf("StartLoad: %v", err)
	}
	// A fleet run is allowed while a load run is active (different kind).
	if _, err := r.StartFleet("f", func(ctx context.Context, _ *FleetLiveStats, _ RunEventFunc) (FleetReport, error) {
		<-block
		return FleetReport{}, nil
	}); err != nil {
		t.Errorf("StartFleet rejected while only a load run was active: %v", err)
	}
	// But a second fleet run is not.
	if _, err := r.StartFleet("f2", func(ctx context.Context, _ *FleetLiveStats, _ RunEventFunc) (FleetReport, error) {
		return FleetReport{}, nil
	}); err == nil {
		t.Error("a second concurrent fleet run was allowed")
	}
}

// A run emits lifecycle events (started, terminal) and the launcher can emit
// its own — and the terminal event is emitted before the run reads terminal,
// so a late subscriber still gets it.
func TestRunRegistryEmitsLifecycleEvents(t *testing.T) {
	r := quietRegistry(0)
	info, err := r.StartLoad("evt", func(ctx context.Context, s *load.LiveStats, emit RunEventFunc) (load.Report, error) {
		emit("error", "ATTACH", "imsi-9", "rejected · cause 22")
		return load.Report{}, nil
	})
	if err != nil {
		t.Fatalf("StartLoad: %v", err)
	}
	waitState(t, r, info.ID, RunComplete)

	// Subscribe after completion: the whole backlog must be retained, including
	// the terminal event (emitted before the state flipped).
	sub, err := r.SubscribeEvents(info.ID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()

	kinds := map[string]int{}
	var sawTerminal, sawFailure bool
	for _, ev := range sub.Backlog {
		kinds[ev.Kind]++
		if ev.Kind == "RUN" && ev.Message == "run COMPLETE" {
			sawTerminal = true
		}
		if ev.Kind == "ATTACH" && ev.SUPI == "imsi-9" {
			sawFailure = true
		}
	}
	if !sawTerminal {
		t.Errorf("no terminal RUN event in backlog: %+v", sub.Backlog)
	}
	if !sawFailure {
		t.Error("launcher-emitted failure event missing from backlog")
	}
	if kinds["RUN"] < 2 {
		t.Errorf("expected at least started+terminal RUN events, got %d", kinds["RUN"])
	}
}

// SubscribeEvents on an unknown run is a not-found error.
func TestRunRegistrySubscribeUnknown(t *testing.T) {
	r := quietRegistry(0)
	if _, err := r.SubscribeEvents("run-nope", 0); err == nil {
		t.Error("SubscribeEvents on unknown id returned no error")
	}
}
