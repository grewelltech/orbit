package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// waitPoll is how often --wait checks a run's state; tests shorten it.
var waitPoll = 500 * time.Millisecond

// waitHelp is the --wait flag's help.
const waitHelp = "follow the run to its end and exit with its result: 0 passed, 1 a gate failed, " +
	"2 did not complete, 3 could not start. Without gates, exit 0 means the run completed, not that " +
	"the core passed; add gates to judge it. Ctrl-C finishes the run (gates judged on the results " +
	"so far), a second Ctrl-C cancels it; SIGTERM cancels"

// waitSignalsFn installs --wait's signal handler; tests replace it.
var waitSignalsFn = waitSignals

// waitSignals is SIGINT and SIGTERM for --wait, until stop.
func waitSignals() (sigs <-chan os.Signal, stop func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	return ch, func() { signal.Stop(ch) }
}

// waitRun follows run id to its end and returns the exit its result calls
// for (nil for a pass, after saying so on errOut). The first SIGINT
// finishes the run, judging its gates on the results so far; a second, or
// a SIGTERM (what CI sends), cancels it.
func waitRun(ctx context.Context, out, errOut io.Writer, c orbitv1connect.RunServiceClient, id string, sigs <-chan os.Signal) error {
	return waitRunWith(ctx, out, errOut, c, id, sigs, waitOpts{})
}

// waitOpts are what a command adds to waitRun's following: progress, given
// the run as it stands every progressEvery while it goes, and render,
// given the finished run's report before its gates are printed.
type waitOpts struct {
	progress      func(*orbitv1.GetRunResponse)
	progressEvery time.Duration
	render        func(io.Writer, *orbitv1.GetRunReportResponse)
	// started is called once the run has started (thinRun), before it is
	// followed.
	started func()
}

// waitRunWith is waitRun with opts.
func waitRunWith(ctx context.Context, out, errOut io.Writer, c orbitv1connect.RunServiceClient, id string, sigs <-chan os.Signal, opts waitOpts) error {
	tick := time.NewTicker(waitPoll)
	defer tick.Stop()
	interrupted := false
	var run *orbitv1.Run
	lastProgress := time.Now()
	for {
		g, err := c.GetRun(ctx, connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
		if err != nil {
			return exitWith(ExitDidNotComplete, fmt.Errorf("following run %s: %w", id, err))
		}
		run = g.Msg.GetRun()
		if runTerminal(run.GetState()) {
			break
		}
		if opts.progress != nil && opts.progressEvery > 0 && time.Since(lastProgress) >= opts.progressEvery {
			opts.progress(g.Msg)
			lastProgress = time.Now()
		}
		select {
		case <-ctx.Done():
			return exitWith(ExitDidNotComplete, ctx.Err())
		case sig := <-sigs:
			finish := sig == os.Interrupt && !interrupted
			interrupted = true
			if finish {
				_, _ = fmt.Fprintln(errOut, "finishing: judging gates on results so far; press Ctrl-C again to cancel")
			} else {
				_, _ = fmt.Fprintln(errOut, "cancelling")
			}
			if _, err := c.StopRun(ctx, connect.NewRequest(&orbitv1.StopRunRequest{RunId: id, Finish: finish})); err != nil {
				return exitWith(ExitDidNotComplete, fmt.Errorf("stopping run %s: %w", id, err))
			}
		case <-tick.C:
		}
	}
	if run.GetState() != orbitv1.RunState_RUN_STATE_COMPLETE {
		detail := ""
		if run.GetError() != "" {
			detail = ": " + run.GetError()
		}
		return exitWith(ExitDidNotComplete, fmt.Errorf("run %s ended %s%s", id, runStateLabel(run.GetState()), detail))
	}
	r, err := c.GetRunReport(ctx, connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id}))
	if err != nil {
		return exitWith(ExitDidNotComplete, fmt.Errorf("run %s has no report: %w", id, err))
	}
	if opts.render != nil {
		opts.render(out, r.Msg)
	}
	printWaitReport(out, run, r.Msg)
	if w := incompleteWorkload(r.Msg); w != "" {
		return exitWith(ExitDidNotComplete, fmt.Errorf("run %s: workload %s did not complete", id, w))
	}
	failed := 0
	for _, g := range r.Msg.GetGates() {
		if !g.GetPassed() {
			failed++
		}
	}
	if failed > 0 {
		return gateFailed("run %s: %d of %d gates failed", id, failed, len(r.Msg.GetGates()))
	}
	_, _ = fmt.Fprintf(errOut, "exit %d: %s\n", ExitPass, ExitMeaning(ExitPass))
	return nil
}

// runTerminal reports whether a run state is final.
func runTerminal(s orbitv1.RunState) bool {
	switch s {
	case orbitv1.RunState_RUN_STATE_COMPLETE, orbitv1.RunState_RUN_STATE_FAILED, orbitv1.RunState_RUN_STATE_CANCELLED:
		return true
	}
	return false
}

// printWaitReport is a finished run's result: whether it was stopped
// early, a soak's spread over its SUPIs, and each gate.
func printWaitReport(out io.Writer, run *orbitv1.Run, r *orbitv1.GetRunReportResponse) {
	_, _ = fmt.Fprintf(out, "run %s: %s", run.GetRunId(), runStateLabel(run.GetState()))
	if run.GetStoppedEarly() {
		_, _ = fmt.Fprint(out, " (finished early: judged on the results so far)")
	}
	_, _ = fmt.Fprintln(out)
	// Each workload's report, or the run's own for a run with none (an
	// old arm's).
	loads := []*orbitv1.LoadReport{r.GetLoad()}
	if ws := r.GetWorkloads(); len(ws) > 0 {
		loads = loads[:0]
		for _, w := range ws {
			loads = append(loads, w.GetLoad())
		}
	}
	for _, l := range loads {
		if line := soakLine(l.GetAttempted(), l.GetDistinctSupis()); line != "" {
			_, _ = fmt.Fprintln(out, line)
		}
	}
	for _, g := range r.GetGates() {
		verdict := "PASS"
		if !g.GetPassed() {
			verdict = "FAIL"
		}
		name := g.GetGate().GetName()
		if name == "" {
			name = "gate"
		}
		_, _ = fmt.Fprintf(out, "gate %s: %s  %s\n", name, verdict, g.GetDetail())
	}
}

// soakLine says a timed storm's attempts ran across fewer SUPIs, as a soak
// cycles its count; "" when every attempt had its own.
func soakLine(attempted, distinct uint32) string {
	if distinct == 0 || attempted <= distinct {
		return ""
	}
	return fmt.Sprintf("soak: %d attempts across %d SUPIs", attempted, distinct)
}

// incompleteWorkload names a workload of the report that did not
// complete: one with no report (it failed, was stopped or never ran) or
// a conformance suite cut short; "" when every workload completed.
func incompleteWorkload(r *orbitv1.GetRunReportResponse) string {
	for _, w := range r.GetWorkloads() {
		switch {
		case w.GetLoad() == nil && w.GetFleet() == nil && w.GetConformance() == nil,
			w.GetConformance().GetIncomplete():
			name := w.GetWorkload()
			if name == "" {
				name = "(unnamed)"
			}
			return name
		}
	}
	return ""
}
