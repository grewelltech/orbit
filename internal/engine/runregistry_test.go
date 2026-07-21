package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

	info, err := r.StartLoad("soak", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
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
	info, _ := r.StartLoad("bad", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
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
	info, _ := r.StartLoad("long", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
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
	info, _ := r.StartLoad("racy", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
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
	first, _ := r.StartLoad("first", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
		<-block
		return load.Report{}, nil
	})

	_, err := r.StartLoad("second", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
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
	if _, err := r.StartLoad("third", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
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
	info, _ := r.StartLoad("live", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
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
		info, err := r.StartLoad("n", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
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
		info, _ := r.StartLoad("n", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
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
	info, _ := r.StartLoad("a", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
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
		info, err := r.StartLoad("n", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
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
