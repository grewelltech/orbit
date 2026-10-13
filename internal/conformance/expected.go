package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ExpectedFailures is a per-core list of conformance failures that are known
// and tolerated (docs/conformance/expected-failures.md). Each entry tolerates
// one FAIL mode of one check, matched exactly on (check, observed); anything
// else still fails the gate. A matched result stays FAIL: the report says
// what the spec says, and the entry only keeps it from gating.
//
// The methods are safe on a nil list, which matches nothing.
type ExpectedFailures struct {
	Core    string                 `yaml:"core"`
	Version string                 `yaml:"version"`
	Entries []ExpectedFailureEntry `yaml:"entries"`
}

// ExpectedFailureEntry is one tolerated failure: check Check producing FAIL
// with exactly Observed as its Result.Observed.
type ExpectedFailureEntry struct {
	Check    string `yaml:"check"`
	Observed string `yaml:"observed"`
	Finding  string `yaml:"finding"`
	Added    string `yaml:"added"` // YYYY-MM-DD
	Reason   string `yaml:"reason"`
}

// ExpectedFailure is what a matched result carries: the entry's finding,
// date and reason. It is unrelated to Result.Expected, which is the
// behaviour the spec expects of the core.
type ExpectedFailure struct {
	Finding string `json:"finding"`
	Added   string `json:"added"`
	Reason  string `json:"reason"`
}

// StaleExpectation is an entry whose check ran but did not produce the FAIL
// it tolerates, with what the check gave instead, so the list gets pruned.
type StaleExpectation struct {
	Check, Finding string
	// ExpectedObserved is the entry's observed text; Verdict and Observed
	// are what the check gave this run.
	ExpectedObserved string
	Verdict          Verdict
	Observed         string
}

// LoadExpectedFailures reads and validates a list. Every entry must name a
// registered check, an observed text, a finding, an added date (YYYY-MM-DD)
// and a reason; (check, observed) must be unique; unknown keys are refused.
// Errors name the file and, for an entry, its index.
func LoadExpectedFailures(path string) (*ExpectedFailures, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("expected failures: %w", err)
	}
	known := map[string]bool{}
	for _, t := range NewRegistry().Tests() {
		known[t.ID()] = true
	}
	return parseExpectedFailures(path, b, known)
}

func parseExpectedFailures(path string, b []byte, known map[string]bool) (*ExpectedFailures, error) {
	var x ExpectedFailures
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&x); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if strings.TrimSpace(x.Core) == "" {
		return nil, fmt.Errorf("%s: core is required", path)
	}
	if strings.TrimSpace(x.Version) == "" {
		return nil, fmt.Errorf("%s: version is required", path)
	}
	seen := map[[2]string]int{}
	for i, e := range x.Entries {
		where := fmt.Sprintf("%s: entry %d", path, i+1)
		for _, f := range []struct{ name, v string }{
			{"check", e.Check}, {"observed", e.Observed}, {"finding", e.Finding}, {"added", e.Added}, {"reason", e.Reason},
		} {
			if strings.TrimSpace(f.v) == "" {
				return nil, fmt.Errorf("%s: %s is required", where, f.name)
			}
		}
		if !known[e.Check] {
			return nil, fmt.Errorf("%s: unknown check %q", where, e.Check)
		}
		if _, err := time.Parse(time.DateOnly, e.Added); err != nil {
			return nil, fmt.Errorf("%s: added %q is not a YYYY-MM-DD date", where, e.Added)
		}
		key := [2]string{e.Check, e.Observed}
		if j, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s: duplicates entry %d (%s, %q)", where, j, e.Check, e.Observed)
		}
		seen[key] = i + 1
	}
	return &x, nil
}

// match returns the entry tolerating r, if any: r is a FAIL and an entry
// names its check and exactly its observed text.
func (x *ExpectedFailures) match(r Result) (ExpectedFailureEntry, bool) {
	if x == nil || r.Verdict != Fail {
		return ExpectedFailureEntry{}, false
	}
	for _, e := range x.Entries {
		if e.Check == r.ID && e.Observed == r.Observed {
			return e, true
		}
	}
	return ExpectedFailureEntry{}, false
}

// Apply marks each FAIL an entry tolerates with that entry, leaving the
// verdict FAIL. No other verdict is ever marked: an entry never hides a
// different failure mode, an ERROR or a crash.
func (x *ExpectedFailures) Apply(results []Result) {
	for i := range results {
		if e, ok := x.match(results[i]); ok {
			results[i].ExpectedFailure = &ExpectedFailure{Finding: e.Finding, Added: e.Added, Reason: e.Reason}
		}
	}
}

// Stale returns the entries whose check ran and gave something other than
// the FAIL they tolerate (a PASS, a DEVIATION, or a FAIL with other observed
// text). An entry whose check was not selected, SKIPped or ERRORed is neither
// matched nor stale: the run says nothing about whether the failure is gone.
func (x *ExpectedFailures) Stale(results []Result) []StaleExpectation {
	if x == nil {
		return nil
	}
	var out []StaleExpectation
	for _, e := range x.Entries {
		for _, r := range results {
			if r.ID != e.Check || r.Verdict == Skip || r.Verdict == Error {
				continue
			}
			if r.Verdict == Fail && r.Observed == e.Observed {
				continue
			}
			out = append(out, StaleExpectation{Check: e.Check, Finding: e.Finding, ExpectedObserved: e.Observed,
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
