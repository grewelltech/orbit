package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/load"
)

// clock records the order workloads start and end in.
type clock struct {
	mu  sync.Mutex
	log []string
}

func (c *clock) mark(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, s)
}

func (c *clock) index(s string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, x := range c.log {
		if x == s {
			return i
		}
	}
	return -1
}

// population is a load launcher that attaches count UEs, then holds until
// release closes (or the run stops), then returns err.
func population(c *clock, count int, release <-chan struct{}, err error) LoadRunFunc {
	return func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error) {
		c.mark("pop start")
		for i := 0; i < count; i++ {
			s.Observe(load.Sample{Metrics: map[string]time.Duration{"attach": time.Millisecond}})
		}
		c.mark("pop attached")
		if err != nil {
			c.mark("pop end")
			return load.Report{}, err
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		c.mark("pop end")
		return load.Report{Attempted: count, Succeeded: count}, nil
	}
}

func suite(c *clock) ConformanceRunFunc {
	return func(context.Context, RunEventFunc) (any, error) {
		c.mark("suite start")
		return "suite report", nil
	}
}

// A mixed run starts each workload at once or when the one it waits for
// reaches its phase; it completes with every workload's report.
func TestRunMixedStartConditions(t *testing.T) {
	tests := []struct {
		name  string
		phase Phase
		// before is a mark the suite must start before (or "" for none);
		// after one it must start after.
		before, after string
	}{
		// STARTED is latched as the launcher is called, so the suite may
		// start before the population's first line: only the end orders.
		{name: "after started", phase: PhaseStarted, before: "pop end"},
		// ATTACHED is latched once the population's attempts are counted,
		// which follows its start (the mark after them may come later).
		{name: "after attached", phase: PhaseAttached, after: "pop start", before: "pop end"},
		{name: "after complete", phase: PhaseComplete, after: "pop end"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := quietRegistry(0)
			c := &clock{}
			release := make(chan struct{})
			info, err := r.StartMixed(RunOptions{WorkloadKinds: []RunKind{RunKindLoad, RunKindConformance}}, []MixedWorkload{
				{Name: "pop", Load: population(c, 3, release, nil), Count: 3},
				{Name: "suite", Conformance: suite(c), After: "pop", Phase: tc.phase},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.phase != PhaseComplete {
				// The suite must not need the population to end.
				deadline := time.Now().Add(2 * time.Second)
				for c.index("suite start") < 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			close(release)
			waitState(t, r, info.ID, RunComplete)
			if s, a := c.index("suite start"), c.index(tc.after); s < 0 || tc.after != "" && s < a {
				t.Errorf("suite started at %d, %s at %d: %v", s, tc.after, a, c.log)
			}
			if tc.before != "" && c.index("suite start") > c.index(tc.before) {
				t.Errorf("suite started after %s: %v", tc.before, c.log)
			}
			rep, ok := r.MixedResult(info.ID)
			if !ok || len(rep.Workloads) != 2 || rep.Workloads[0].State != WorkloadDone || rep.Workloads[1].Report != "suite report" {
				t.Fatalf("report = %+v", rep)
			}
			if got, _ := r.Get(info.ID); got.Kind != RunKindMixed || len(got.WorkloadKinds) != 2 {
				t.Errorf("info = %+v", got)
			}
			ps := r.MixedProgress(info.ID)
			if len(ps) != 2 || ps[0].Load == nil || ps[1].Suite != nil || ps[1].State != WorkloadDone {
				t.Errorf("progress = %+v", ps)
			}
		})
	}
}

// A workload whose awaited one fails first is skipped, saying so, and the
// run fails with the failure.
func TestRunMixedDependencyFails(t *testing.T) {
	r := quietRegistry(0)
	c := &clock{}
	info, err := r.StartMixed(RunOptions{}, []MixedWorkload{
		{Name: "pop", Load: population(c, 0, nil, errors.New("AMF refused")), Count: 3},
		{Name: "suite", Conformance: suite(c), After: "pop", Phase: PhaseAttached},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, r, info.ID, RunFailed)
	if c.index("suite start") >= 0 {
		t.Errorf("the suite ran: %v", c.log)
	}
	got, _ := r.Get(info.ID)
	if !strings.Contains(got.Err, "workload pop: AMF refused") {
		t.Errorf("run error = %q", got.Err)
	}
	ps := r.MixedProgress(info.ID)
	if ps[1].State != WorkloadSkipped || ps[1].Detail != "pop ended without reaching attached" {
		t.Errorf("suite = %+v", ps[1])
	}
}

// Stopping a mixed run stops every workload; one still waiting is skipped.
func TestRunMixedStop(t *testing.T) {
	r := quietRegistry(0)
	c := &clock{}
	info, err := r.StartMixed(RunOptions{}, []MixedWorkload{
		{Name: "pop", Load: population(c, 1, nil, nil), Count: 1},
		{Name: "suite", Conformance: suite(c), After: "pop", Phase: PhaseComplete},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, r, info.ID, RunRunning)
	if _, err := r.Stop(info.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, info.ID, RunCancelled)
	if ps := r.MixedProgress(info.ID); ps[1].State != WorkloadSkipped {
		t.Errorf("suite = %+v", ps[1])
	}
}

// A mixed run's workloads must name each other consistently.
func TestValidateMixed(t *testing.T) {
	ok := func(name string) MixedWorkload {
		return MixedWorkload{Name: name, Conformance: func(context.Context, RunEventFunc) (any, error) { return nil, nil }}
	}
	tests := []struct {
		name    string
		ws      []MixedWorkload
		wantErr string
	}{
		{name: "one workload", ws: []MixedWorkload{ok("a")}, wantErr: "at least two"},
		{name: "duplicate names", ws: []MixedWorkload{ok("a"), ok("a")}, wantErr: "unique"},
		{name: "no launcher", ws: []MixedWorkload{ok("a"), {Name: "b"}}, wantErr: "exactly one launcher"},
		{name: "after an unknown workload", ws: []MixedWorkload{ok("a"), func() MixedWorkload { w := ok("b"); w.After, w.Phase = "x", PhaseStarted; return w }()}, wantErr: "does not have"},
		{name: "a cycle", ws: []MixedWorkload{
			func() MixedWorkload { w := ok("a"); w.After, w.Phase = "b", PhaseStarted; return w }(),
			func() MixedWorkload { w := ok("b"); w.After, w.Phase = "a", PhaseStarted; return w }()}, wantErr: "cycle"},
		{name: "after with no phase", ws: []MixedWorkload{ok("a"), func() MixedWorkload { w := ok("b"); w.After = "a"; return w }()}, wantErr: "needs a phase"},
		{name: "valid", ws: []MixedWorkload{ok("a"), func() MixedWorkload { w := ok("b"); w.After, w.Phase = "a", PhaseComplete; return w }()}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMixed(tc.ws)
			if (tc.wantErr == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
