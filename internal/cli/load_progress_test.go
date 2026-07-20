package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/load"
)

// syncBuf is an io.Writer safe for the printer goroutine to write while the
// test reads it.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestLoadProgressPrintsWhileRunning(t *testing.T) {
	live := load.NewLiveStats()
	var out syncBuf

	stop := startLoadProgress(&out, live, 10*time.Millisecond)
	live.Observe(load.Sample{Metrics: map[string]time.Duration{"attach": 5 * time.Millisecond}})
	time.Sleep(60 * time.Millisecond)
	stop()

	got := out.String()
	if !strings.Contains(got, "1 ok / 1 attempted") {
		t.Errorf("progress did not report the completed attempt:\n%s", got)
	}
	if !strings.Contains(got, "attach P50") {
		t.Errorf("progress omitted live attach percentiles:\n%s", got)
	}
}

// The stop func must block until the printer has exited, so a progress line
// can never interleave with the final report written afterwards.
func TestLoadProgressStopIsSynchronous(t *testing.T) {
	live := load.NewLiveStats()
	var out syncBuf

	stop := startLoadProgress(&out, live, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	stop()

	settled := out.String()
	time.Sleep(20 * time.Millisecond)
	if after := out.String(); after != settled {
		t.Errorf("printer wrote after stop returned:\n before: %q\n after:  %q", settled, after)
	}
}

// A zero cadence disables progress entirely — the machine-readable path.
func TestLoadProgressDisabled(t *testing.T) {
	live := load.NewLiveStats()
	var out syncBuf

	stop := startLoadProgress(&out, live, 0)
	live.Observe(load.Sample{Metrics: map[string]time.Duration{"attach": time.Millisecond}})
	time.Sleep(20 * time.Millisecond)
	stop()

	if got := out.String(); got != "" {
		t.Errorf("progress printed while disabled: %q", got)
	}
}

// The interval rate comes from the delta between snapshots; the cumulative
// average comes from the run start. They are distinct numbers and both appear.
func TestFormatLoadProgressUsesIntervalDelta(t *testing.T) {
	prev := load.Snapshot{Succeeded: 10, Elapsed: 1 * time.Second}
	cur := load.Snapshot{Succeeded: 40, Elapsed: 2 * time.Second, Attempted: 40, AchievedRate: 20}

	line := formatLoadProgress(cur, prev)
	// 30 successes over the 1s gap.
	if !strings.Contains(line, "30.0 attach/s") {
		t.Errorf("interval rate not computed from the snapshot delta: %q", line)
	}
	if !strings.Contains(line, "avg 20.0") {
		t.Errorf("cumulative average missing: %q", line)
	}
}

// The first tick has no previous snapshot; the interval rate must not divide by
// zero or report a nonsense value.
func TestFormatLoadProgressFirstTick(t *testing.T) {
	cur := load.Snapshot{Succeeded: 5, Attempted: 5, Elapsed: 2 * time.Second, AchievedRate: 2.5}

	line := formatLoadProgress(cur, load.Snapshot{})
	if strings.Contains(line, "NaN") || strings.Contains(line, "Inf") {
		t.Errorf("first tick produced a degenerate rate: %q", line)
	}
	if !strings.Contains(line, "5 ok / 5 attempted") {
		t.Errorf("first tick lost its counts: %q", line)
	}
}
