package conformance

import (
	"slices"
	"strings"
	"testing"
)

// Each policy gates exactly its verdicts; the word never changes.
func TestGatePolicyGates(t *testing.T) {
	want := map[GatePolicy][]Verdict{
		GateDefault: {Fail, Deviation, Warn, Blocked, Crash, Error},
		GateSpec:    {Fail, Blocked, Crash, Error},
		"":          {Fail, Deviation, Warn, Blocked, Crash, Error},
	}
	for p, gated := range want {
		for _, v := range Verdicts() {
			if got := p.Gates(v); got != slices.Contains(gated, v) {
				t.Errorf("%q gates %s = %v", p, v, got)
			}
		}
	}
	for in, w := range map[string]GatePolicy{"": GateDefault, "default": GateDefault, "spec": GateSpec, " Spec ": GateSpec} {
		if p, err := ParseGatePolicy(in); err != nil || p != w {
			t.Errorf("ParseGatePolicy(%q) = %q, %v; want %q", in, p, err, w)
		}
	}
	if _, err := ParseGatePolicy("strict"); err == nil {
		t.Error("ParseGatePolicy accepted an unknown policy")
	}
}

// A run's findings: unlisted FAIL, CRASH, BLOCKED, ERROR, DEVIATION and
// WARN count; a listed one does not. A BLOCKED counts under its owner when
// the owner ran and reported the procedure as not completing, and on its
// own when the owner was not selected, completed it, or reported a verdict
// no policy gates. An owner with BLOCKED results under it gates under both
// policies, once: as its own verdict where the policy gates it, as BLOCKED
// otherwise, including when an expected-failures entry matched it (ADR-0012).
// The gate line separates spec failures, warnings and blocking owners.
func TestFindingsAndGate(t *testing.T) {
	listed := &ExpectedFailure{Finding: "EX-1"}
	ownerFailed := Result{ID: "OWNER", Verdict: Warn, NotCompleted: ProcN2HandoverPreparation}
	ownerDone := Result{ID: "OWNER", Verdict: Pass}
	dep := func(id string) Result {
		return Result{ID: id, Verdict: Blocked, BlockedOn: ProcN2HandoverPreparation, Owner: "OWNER"}
	}
	tests := []struct {
		name        string
		results     []Result
		want        []string // finding IDs, with "<-dep" for each BLOCKED counted under it
		defaultLine string
		specLine    string
	}{
		{name: "clean", results: []Result{{ID: "A", Verdict: Pass}, {ID: "B", Verdict: Info}, {ID: "C", Verdict: Skip}},
			defaultLine: "gate default: passed: 0 spec failures, 0 warnings", specLine: "gate spec: passed: 0 spec failures, 0 warnings"},
		{name: "every gating verdict", results: []Result{{ID: "F", Verdict: Fail}, {ID: "D", Verdict: Deviation}, {ID: "W", Verdict: Warn},
			{ID: "C", Verdict: Crash}, {ID: "E", Verdict: Error}, {ID: "B", Verdict: Blocked}},
			want:        []string{"F", "D", "W", "C", "E", "B"},
			defaultLine: "gate default: failed: 4 spec failures, 2 warnings", specLine: "gate spec: failed: 4 spec failures, 2 warnings"},
		{name: "warnings only", results: []Result{{ID: "D", Verdict: Deviation}, {ID: "W", Verdict: Warn}},
			want:        []string{"D", "W"},
			defaultLine: "gate default: failed: 0 spec failures, 2 warnings", specLine: "gate spec: passed: 0 spec failures, 2 warnings"},
		{name: "listed results", results: []Result{{ID: "F", Verdict: Fail, ExpectedFailure: listed}, {ID: "W", Verdict: Warn, ExpectedFailure: listed}},
			defaultLine: "gate default: passed: 0 spec failures, 0 warnings", specLine: "gate spec: passed: 0 spec failures, 0 warnings"},
		{name: "owner reported the procedure not completing", results: []Result{ownerFailed, dep("X"), dep("Y")},
			want:        []string{"OWNER<-X<-Y"},
			defaultLine: "gate default: failed: 0 spec failures, 1 warning", specLine: "gate spec: failed: 0 spec failures, 0 warnings, 1 blocked: OWNER blocked 2 checks"},
		{name: "a FAIL owner reported the procedure not completing", results: []Result{{ID: "OWNER", Verdict: Fail, NotCompleted: ProcN2HandoverPreparation}, dep("X")},
			want:        []string{"OWNER<-X"},
			defaultLine: "gate default: failed: 1 spec failure, 0 warnings", specLine: "gate spec: failed: 1 spec failure, 0 warnings"},
		{name: "an owner with a verdict no policy gates", results: []Result{{ID: "OWNER", Verdict: Info, NotCompleted: ProcN2HandoverPreparation}, dep("X")},
			want:        []string{"X"},
			defaultLine: "gate default: failed: 1 spec failure, 0 warnings", specLine: "gate spec: failed: 1 spec failure, 0 warnings"},
		{name: "owner reported the procedure not completing, nothing blocked", results: []Result{ownerFailed},
			want:        []string{"OWNER"},
			defaultLine: "gate default: failed: 0 spec failures, 1 warning", specLine: "gate spec: passed: 0 spec failures, 1 warning"},
		{name: "owner completed it", results: []Result{ownerDone, dep("X")},
			want:        []string{"X"},
			defaultLine: "gate default: failed: 1 spec failure, 0 warnings", specLine: "gate spec: failed: 1 spec failure, 0 warnings"},
		{name: "owner not selected", results: []Result{dep("X")},
			want:        []string{"X"},
			defaultLine: "gate default: failed: 1 spec failure, 0 warnings", specLine: "gate spec: failed: 1 spec failure, 0 warnings"},
		{name: "owner listed", results: []Result{{ID: "OWNER", Verdict: Warn, NotCompleted: ProcN2HandoverPreparation, ExpectedFailure: listed}, dep("X")},
			want:        []string{"OWNER<-X"},
			defaultLine: "gate default: failed: 0 spec failures, 0 warnings, 1 blocked: OWNER blocked 1 check", specLine: "gate spec: failed: 0 spec failures, 0 warnings, 1 blocked: OWNER blocked 1 check"},
		{name: "owner listed, nothing blocked", results: []Result{{ID: "OWNER", Verdict: Fail, NotCompleted: ProcN2HandoverPreparation, ExpectedFailure: listed}},
			defaultLine: "gate default: passed: 0 spec failures, 0 warnings", specLine: "gate spec: passed: 0 spec failures, 0 warnings"},
		{name: "a BLOCKED on another procedure", results: []Result{ownerFailed, {ID: "Z", Verdict: Blocked, BlockedOn: ProcRegistration, Owner: "OWNER"}},
			want:        []string{"OWNER", "Z"},
			defaultLine: "gate default: failed: 1 spec failure, 1 warning", specLine: "gate spec: failed: 1 spec failure, 1 warning"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, f := range Findings(tc.results) {
				s := f.Result.ID
				for _, b := range f.Blocked {
					s += "<-" + b
				}
				got = append(got, s)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("findings %q, want %q", got, tc.want)
			}
			if l := GateLine(tc.results, GateDefault); l != tc.defaultLine {
				t.Errorf("default: %q, want %q", l, tc.defaultLine)
			}
			if l := GateLine(tc.results, GateSpec); l != tc.specLine {
				t.Errorf("spec: %q, want %q", l, tc.specLine)
			}
			for _, p := range []GatePolicy{GateDefault, GateSpec} {
				s := Summarize(tc.results)
				s.Gate = p
				if ok := s.OK(); ok != (len(GateFindings(tc.results, p)) == 0) || ok == strings.Contains(s.GateLine(), ": failed: ") {
					t.Errorf("%s: OK = %v, line %q", p, ok, s.GateLine())
				}
			}
		})
	}
}

// The tally names the tolerated DEVIATIONs and WARNs as it does the FAILs.
func TestTallyLineExpected(t *testing.T) {
	listed := &ExpectedFailure{Finding: "EX-1"}
	s := Summarize([]Result{{ID: "A", Verdict: Fail, ExpectedFailure: listed}, {ID: "B", Verdict: Warn, ExpectedFailure: listed}, {ID: "C", Verdict: Deviation}})
	want := "3 checks: 0 pass, 1 fail (1 expected), 1 deviation, 1 warn (1 expected), 0 info, 0 blocked, 0 crash, 0 skip, 0 error"
	if got := s.TallyLine(); got != want || s.ExpectedWarn != 1 || s.ExpectedDeviation != 0 {
		t.Errorf("tally %q, want %q", got, want)
	}
}

// A gate's detail names each failing finding as the verdict it gates as,
// with the checks BLOCKED under it, and nothing when the gate passes.
func TestGateDetail(t *testing.T) {
	listed := &ExpectedFailure{Finding: "EX-1"}
	dep := func(id string) Result {
		return Result{ID: id, Verdict: Blocked, BlockedOn: ProcN2HandoverPreparation, Owner: "OWNER"}
	}
	warnOwner := Result{ID: "OWNER", Verdict: Warn, NotCompleted: ProcN2HandoverPreparation}
	tests := []struct {
		name    string
		results []Result
		p       GatePolicy
		want    string
	}{
		{name: "passed", results: []Result{{ID: "A", Verdict: Pass}}, p: GateSpec,
			want: "gate spec: passed: 0 spec failures, 0 warnings"},
		{name: "a FAIL", results: []Result{{ID: "F", Verdict: Fail}}, p: GateDefault,
			want: "gate default: failed: 1 spec failure, 0 warnings; 1 unexpected: F FAIL"},
		{name: "owner gates as its own verdict", results: []Result{warnOwner, dep("X"), dep("Y")}, p: GateDefault,
			want: "gate default: failed: 0 spec failures, 1 warning; 1 unexpected: OWNER WARN (and 2 BLOCKED under it: X, Y)"},
		{name: "owner gates as BLOCKED", results: []Result{warnOwner, dep("X")}, p: GateSpec,
			want: "gate spec: failed: 0 spec failures, 0 warnings, 1 blocked: OWNER blocked 1 check; 1 unexpected: OWNER BLOCKED (its WARN does not gate under spec; 1 BLOCKED under it: X)"},
		{name: "listed owner gates as BLOCKED", results: []Result{{ID: "OWNER", Verdict: Fail, NotCompleted: ProcN2HandoverPreparation, ExpectedFailure: listed}, dep("X")}, p: GateDefault,
			want: "gate default: failed: 0 spec failures, 0 warnings, 1 blocked: OWNER blocked 1 check; 1 unexpected: OWNER BLOCKED (its FAIL is expected: EX-1; 1 BLOCKED under it: X)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := GateDetail(tc.results, tc.p); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
