package server

import orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"

// runSummary builds the headline digest stored on an archived run's Run
// message (Run.summary), so ListRuns and GetRun can label a run without
// fetching its full report.
//
// It sums each per-workload report in the archive — a.Reports is the
// authoritative source for plan and mixed runs, carrying the conformance
// tally, attach figures and gate verdicts for every workload — and falls
// back to the single top-level report a legacy, non-plan load or fleet run
// archives. A terminal run that produced no report (FAILED, CANCELLED)
// yields a nil summary, and the client renders such a row from Run.state.
func runSummary(a *orbitv1.RunArchive) *orbitv1.RunSummary {
	if a == nil || a.GetRun() == nil {
		return nil
	}

	reports := a.GetReports()
	if len(reports) == 0 {
		// Legacy single-workload run: wrap the top-level report so the same
		// accumulation handles it. judgedReports only fills a.Reports for a
		// run with a plan judgement.
		switch {
		case a.GetLoadReport() != nil:
			reports = []*orbitv1.RunReport{{
				Kind:   orbitv1.RunKind_RUN_KIND_LOAD,
				Report: &orbitv1.RunReport_Load{Load: a.GetLoadReport()},
			}}
		case a.GetFleetReport() != nil:
			reports = []*orbitv1.RunReport{{
				Kind:   orbitv1.RunKind_RUN_KIND_FLEET,
				Report: &orbitv1.RunReport_Fleet{Fleet: a.GetFleetReport()},
			}}
		default:
			return nil // no report to summarize
		}
	}

	s := &orbitv1.RunSummary{Kinds: summaryKinds(a.GetRun())}
	for _, rr := range reports {
		if c := rr.GetConformance(); c != nil {
			t := c.GetTally()
			s.ConformanceTotal += c.GetTotal()
			s.ConformancePassed += t.GetPassed()
			s.ConformanceFailed += t.GetFailed()
			s.ConformanceErrored += t.GetErrored()
			s.ConformanceSkipped += t.GetSkipped()
			s.ConformanceDeviations += t.GetDeviations()
			s.ConformanceInfo += t.GetInfo()
			s.ConformanceWarned += t.GetWarned()
			s.ConformanceBlocked += t.GetBlocked()
			s.ConformanceCrashed += t.GetCrashed()
			s.ConformanceExpectedFailures += t.GetExpectedFailures()
		}
		if l := rr.GetLoad(); l != nil {
			s.AttachAttempted += l.GetAttempted()
			s.AttachSucceeded += l.GetSucceeded()
			s.AttachFailed += l.GetFailed()
		}
		if f := rr.GetFleet(); f != nil {
			// A fleet workload's attach phase: attached + attach_failed are
			// the attempts (abandoned attaches are neither, as in the report).
			s.AttachAttempted += f.GetAttached() + f.GetAttachFailed()
			s.AttachSucceeded += f.GetAttached()
			s.AttachFailed += f.GetAttachFailed()
		}
		for _, g := range rr.GetGates() {
			s.GatesTotal++
			if g.GetPassed() {
				s.GatesPassed++
			}
		}
	}
	// "All gates passed" is only meaningful when the run had gates.
	s.GatesOk = s.GatesTotal > 0 && s.GatesPassed == s.GatesTotal
	return s
}

// summaryKinds returns every workload kind in the run, always non-empty for a
// run with a kind: Run.workload_kinds when set (a mixed or plan run), else the
// single Run.kind.
func summaryKinds(r *orbitv1.Run) []orbitv1.RunKind {
	if wk := r.GetWorkloadKinds(); len(wk) > 0 {
		out := make([]orbitv1.RunKind, len(wk))
		copy(out, wk)
		return out
	}
	if r.GetKind() != orbitv1.RunKind_RUN_KIND_UNSPECIFIED {
		return []orbitv1.RunKind{r.GetKind()}
	}
	return nil
}
