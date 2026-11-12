package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ExpectedFailures is a per-core list of conformance failures that are known
// and tolerated (docs/conformance/expected-failures.md). Each entry tolerates
// one FAIL, DEVIATION or WARN mode of one check, matched exactly on (check,
// verdict, observed); anything else still fails the gate. A matched result
// keeps its verdict: the report says what the spec says, and the entry only
// keeps it from gating (ADR-0011 rule 8).
//
// The methods are safe on a nil list, which matches nothing.
type ExpectedFailures struct {
	Core    string                 `yaml:"core"`
	Version string                 `yaml:"version"`
	Entries []ExpectedFailureEntry `yaml:"entries"`
}

// ExpectedFailureEntry is one tolerated failure: check Check producing
// Verdict with exactly Observed as its Result.Observed.
type ExpectedFailureEntry struct {
	Check string `yaml:"check"`
	// Verdict is the verdict the entry expects: FAIL, DEVIATION or WARN.
	// An entry that gives none expects FAIL (Expects).
	Verdict  Verdict `yaml:"verdict,omitempty"`
	Observed string  `yaml:"observed"`
	Finding  string  `yaml:"finding"`
	Added    string  `yaml:"added"` // YYYY-MM-DD
	Reason   string  `yaml:"reason"`
}

// ExpectedFailure is what a matched result carries: the entry's finding,
// date and reason. It is unrelated to Result.Expected, which is the
// behaviour the spec expects of the core.
type ExpectedFailure struct {
	Finding string `json:"finding"`
	Added   string `json:"added"`
	Reason  string `json:"reason"`
}

// StaleExpectation is an entry whose check ran but did not produce the
// result it tolerates, with what the check gave instead, so the list gets
// pruned.
type StaleExpectation struct {
	Check, Finding string
	// ExpectedVerdict and ExpectedObserved are the entry's; Verdict and
	// Observed are what the check gave this run.
	ExpectedVerdict  Verdict `json:",omitempty"`
	ExpectedObserved string
	Verdict          Verdict
	Observed         string
}

// LoadExpectedFailures reads and validates a list. Every entry must name a
// registered check that can report the entry's verdict (FAIL, DEVIATION or
// WARN; FAIL when none is given), an observed text, a finding, an added
// date (YYYY-MM-DD) and a reason; (check, verdict, observed) must be
// unique; unknown keys are refused.
// An observed text from a liveness probe (a core that stopped answering, or
// one that could not be checked) is refused: those always gate.
// Errors name the file and, for an entry, its index.
func LoadExpectedFailures(path string) (*ExpectedFailures, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("expected failures: %w", err)
	}
	known := listableChecks(NewRegistry().Tests())
	return parseExpectedFailures(path, b, known)
}

// listableChecks maps every check in tests to the verdicts an
// expected-failures entry may name for it: the ones of FAIL, DEVIATION and
// WARN the check can report.
func listableChecks(tests []Test) map[string][]Verdict {
	known := map[string][]Verdict{}
	for _, t := range tests {
		known[t.ID()] = []Verdict{}
		for _, v := range listableVerdicts {
			if canReport(t, v) {
				known[t.ID()] = append(known[t.ID()], v)
			}
		}
	}
	return known
}

// listableVerdicts are the verdicts an expected-failures entry may name.
// CRASH, BLOCKED and ERROR always gate.
var listableVerdicts = []Verdict{Fail, Deviation, Warn}

// canReport reports whether t can report verdict v: one of its declared
// outcomes has it.
func canReport(t Test, v Verdict) bool {
	outs, _ := declared(t)
	for _, o := range outs {
		if o.Verdict == v {
			return true
		}
	}
	return false
}

// Expects is the verdict e expects: its Verdict, or FAIL when it gives
// none.
func (e ExpectedFailureEntry) Expects() Verdict {
	if e.Verdict == "" {
		return Fail
	}
	return e.Verdict
}

func parseExpectedFailures(path string, b []byte, known map[string][]Verdict) (*ExpectedFailures, error) {
	var x ExpectedFailures
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&x); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := x.validate(path, known); err != nil {
		return nil, err
	}
	return &x, nil
}

// validate checks a list as parseExpectedFailures does; path names it in
// errors, and known is listableChecks of the registry.
func (x *ExpectedFailures) validate(path string, known map[string][]Verdict) error {
	if strings.TrimSpace(x.Core) == "" {
		return fmt.Errorf("%s: core is required", path)
	}
	if strings.TrimSpace(x.Version) == "" {
		return fmt.Errorf("%s: version is required", path)
	}
	seen := map[[3]string]int{}
	for i, e := range x.Entries {
		where := fmt.Sprintf("%s: entry %d", path, i+1)
		for _, f := range []struct{ name, v string }{
			{"check", e.Check}, {"observed", e.Observed}, {"finding", e.Finding}, {"added", e.Added}, {"reason", e.Reason},
		} {
			if strings.TrimSpace(f.v) == "" {
				return fmt.Errorf("%s: %s is required", where, f.name)
			}
		}
		if hasPrefixAny(e.Observed, livenessTexts) {
			return fmt.Errorf("%s: observed %q reports a core that stopped answering or could not be checked; it cannot be listed", where, e.Observed)
		}
		v := e.Expects()
		if !slices.Contains(listableVerdicts, v) {
			return fmt.Errorf("%s: verdict %q cannot be listed: an entry names FAIL, DEVIATION or WARN; CRASH, BLOCKED and ERROR always gate", where, e.Verdict)
		}
		switch listable, ok := known[e.Check]; {
		case !ok:
			return fmt.Errorf("%s: unknown check %q", where, e.Check)
		case !slices.Contains(listable, v):
			return fmt.Errorf("%s: check %q never reports %s; it cannot be listed with that verdict", where, e.Check, v)
		}
		if _, err := time.Parse(time.DateOnly, e.Added); err != nil {
			return fmt.Errorf("%s: added %q is not a YYYY-MM-DD date", where, e.Added)
		}
		key := [3]string{e.Check, string(v), e.Observed}
		if j, dup := seen[key]; dup {
			return fmt.Errorf("%s: duplicates entry %d (%s, %s, %q)", where, j, e.Check, v, e.Observed)
		}
		seen[key] = i + 1
	}
	return nil
}

// NewExpectedFailures is a list given inline (a plan's conformance
// workload) rather than read from a file, validated as a file is; where
// names it in errors.
func NewExpectedFailures(where string, x ExpectedFailures) (*ExpectedFailures, error) {
	known := listableChecks(NewRegistry().Tests())
	if err := x.validate(where, known); err != nil {
		return nil, err
	}
	return &x, nil
}

// match returns the entry tolerating r, if any: an entry names r's check,
// its verdict and exactly its observed text.
func (x *ExpectedFailures) match(r Result) (ExpectedFailureEntry, bool) {
	if x == nil || !slices.Contains(listableVerdicts, r.Verdict) {
		return ExpectedFailureEntry{}, false
	}
	for _, e := range x.Entries {
		if e.Check == r.ID && e.Expects() == r.Verdict && e.Observed == r.Observed {
			return e, true
		}
	}
	return ExpectedFailureEntry{}, false
}

// Apply marks each result an entry tolerates with that entry, leaving its
// verdict as it is. Only a FAIL, DEVIATION or WARN is ever marked: an entry
// never hides a different failure mode, a BLOCKED, an ERROR or a CRASH.
func (x *ExpectedFailures) Apply(results []Result) {
	for i := range results {
		if e, ok := x.match(results[i]); ok {
			results[i].ExpectedFailure = &ExpectedFailure{Finding: e.Finding, Added: e.Added, Reason: e.Reason}
		}
	}
}

// Stale returns the entries whose check ran and gave something other than
// the result they tolerate (another verdict, or the same verdict with other
// observed text), per verdict. An entry whose check was not selected, or
// gave SKIP, ERROR, BLOCKED
// or CRASH, is neither matched nor stale: the run did not judge the listed
// behaviour, so it says nothing about whether the failure is gone.
func (x *ExpectedFailures) Stale(results []Result) []StaleExpectation {
	if x == nil {
		return nil
	}
	var out []StaleExpectation
	for _, e := range x.Entries {
		for _, r := range results {
			if r.ID != e.Check {
				continue
			}
			switch r.Verdict {
			case Skip, Error, Blocked, Crash:
				continue
			}
			if r.Verdict == e.Expects() && r.Observed == e.Observed {
				continue
			}
			out = append(out, StaleExpectation{Check: e.Check, Finding: e.Finding, ExpectedVerdict: e.Expects(), ExpectedObserved: e.Observed,
				Verdict: r.Verdict, Observed: r.Observed})
		}
	}
	return out
}

// Label is the list's core line for reports, "" for a nil list.
func (x *ExpectedFailures) Label() string {
	if x == nil {
		return ""
	}
	return x.Core
}
