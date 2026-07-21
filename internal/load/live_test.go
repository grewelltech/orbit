package load

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLiveStatsCountsOutcomes(t *testing.T) {
	l := NewLiveStats()
	l.Observe(Sample{Metrics: map[string]time.Duration{"attach": 10 * time.Millisecond}})
	l.Observe(Sample{Metrics: map[string]time.Duration{"attach": 20 * time.Millisecond}})
	l.Observe(Sample{Err: errors.New("registration rejected")})

	s := l.Snapshot()
	if s.Attempted != 3 || s.Succeeded != 2 || s.Failed != 1 {
		t.Errorf("attempted/succeeded/failed = %d/%d/%d, want 3/2/1", s.Attempted, s.Succeeded, s.Failed)
	}
	if got := s.Latencies["attach"].Count; got != 2 {
		t.Errorf("attach histogram count = %d, want 2 (the failure contributes no latency)", got)
	}
}

// A failed attempt must not contribute latency, even when it carries partial
// timings: counting those would bias percentiles toward whatever failed early.
func TestLiveStatsExcludesFailedAttemptLatency(t *testing.T) {
	l := NewLiveStats()
	l.Observe(Sample{
		Metrics: map[string]time.Duration{"attach": time.Microsecond},
		Err:     errors.New("failed after a fast partial attach"),
	})
	l.Observe(Sample{Metrics: map[string]time.Duration{"attach": 100 * time.Millisecond}})

	s := l.Snapshot()
	if got := s.Latencies["attach"].Count; got != 1 {
		t.Fatalf("attach histogram count = %d, want 1", got)
	}
	// With the failure excluded, P50 is the one good sample, not the 1µs outlier.
	if p50 := s.Latencies["attach"].P50; p50 < 90*time.Millisecond {
		t.Errorf("P50 = %s, want ~100ms — the failed attempt's timing leaked in", p50)
	}
}

// Percentiles must reflect only what has completed, and must be readable while
// the run is still going.
func TestLiveStatsSnapshotIsIncremental(t *testing.T) {
	l := NewLiveStats()
	for i := 0; i < 10; i++ {
		l.Observe(Sample{Metrics: map[string]time.Duration{"attach": 5 * time.Millisecond}})
	}
	first := l.Snapshot()
	for i := 0; i < 10; i++ {
		l.Observe(Sample{Metrics: map[string]time.Duration{"attach": 5 * time.Millisecond}})
	}
	second := l.Snapshot()

	if first.Succeeded != 10 || second.Succeeded != 20 {
		t.Errorf("succeeded = %d then %d, want 10 then 20", first.Succeeded, second.Succeeded)
	}
	if first.Latencies["attach"].Count != 10 || second.Latencies["attach"].Count != 20 {
		t.Errorf("histogram counts = %d then %d, want 10 then 20",
			first.Latencies["attach"].Count, second.Latencies["attach"].Count)
	}
}

// Snapshot must not mutate state: two consecutive calls report the same counts.
func TestLiveStatsSnapshotIsIdempotent(t *testing.T) {
	l := NewLiveStats()
	l.Observe(Sample{Metrics: map[string]time.Duration{"attach": time.Millisecond}})

	a, b := l.Snapshot(), l.Snapshot()
	if a.Attempted != b.Attempted || a.Succeeded != b.Succeeded || a.Failed != b.Failed {
		t.Errorf("snapshot is not idempotent: %+v then %+v", a, b)
	}
}

// Observe runs on the load pool's goroutines while Snapshot runs on whatever
// displays progress. hdrhistogram is not safe for concurrent read/write, so
// this is the invariant the lock exists for. Run under -race.
func TestLiveStatsConcurrentObserveAndSnapshot(t *testing.T) {
	l := NewLiveStats()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				l.Observe(Sample{Metrics: map[string]time.Duration{"attach": time.Millisecond}})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = l.Snapshot()
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	if s := l.Snapshot(); s.Succeeded == 0 {
		t.Error("no attempts recorded; the test did not exercise the lock")
	}
}

// Observers fans out to every member and tolerates nil entries, so callers can
// build the slice with optional members.
func TestObserversFanOut(t *testing.T) {
	a, b := NewLiveStats(), NewLiveStats()
	var obs Observer = Observers{a, nil, b}

	obs.Observe(Sample{Metrics: map[string]time.Duration{"attach": time.Millisecond}})

	if got := a.Snapshot().Succeeded; got != 1 {
		t.Errorf("first observer saw %d attempts, want 1", got)
	}
	if got := b.Snapshot().Succeeded; got != 1 {
		t.Errorf("second observer saw %d attempts, want 1", got)
	}
}

// AchievedRate is computed from the run clock, which starts with the first
// observed attempt rather than at construction — so a caller that builds
// LiveStats before bringing up associations does not have setup time folded
// into its rate.
func TestLiveStatsRateExcludesPreRunTime(t *testing.T) {
	l := NewLiveStats()

	// Stand in for association bring-up between construction and the storm.
	time.Sleep(40 * time.Millisecond)

	before := l.Snapshot()
	if before.Elapsed != 0 || before.AchievedRate != 0 {
		t.Errorf("before the first attempt: elapsed %s rate %.2f, want 0 and 0",
			before.Elapsed, before.AchievedRate)
	}

	l.Observe(Sample{Metrics: map[string]time.Duration{"attach": time.Millisecond}})
	after := l.Snapshot()

	if after.Elapsed >= 40*time.Millisecond {
		t.Errorf("elapsed %s includes the pre-run sleep; the clock should start at the first attempt", after.Elapsed)
	}
	if after.AchievedRate <= 0 {
		t.Errorf("achieved rate = %.2f, want > 0 once an attempt has completed", after.AchievedRate)
	}
}

// AchievedRate is successes per second over the run, and failures do not count
// toward it.
func TestLiveStatsAchievedRateCountsSuccessesOnly(t *testing.T) {
	l := NewLiveStats()
	l.Observe(Sample{Metrics: map[string]time.Duration{"attach": time.Millisecond}})
	l.Observe(Sample{Err: errors.New("rejected")})
	time.Sleep(20 * time.Millisecond)

	s := l.Snapshot()
	want := float64(s.Succeeded) / s.Elapsed.Seconds()
	if diff := s.AchievedRate - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("achieved rate = %.3f, want %.3f (succeeded/elapsed)", s.AchievedRate, want)
	}
	if s.Succeeded != 1 {
		t.Errorf("succeeded = %d, want 1 — the failure must not count toward the rate", s.Succeeded)
	}
}
