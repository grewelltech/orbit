package conformance

import "strings"

// Verdict is a conformance check's outcome: one of the nine words of
// ADR-0011. Each has the same meaning in every check, in the CLI, the JSON
// output, the API and the dashboard.
type Verdict string

const (
	// Pass: the cited requirement was met.
	Pass Verdict = "PASS"
	// Fail: a cited "shall" was observably violated.
	Fail Verdict = "FAIL"
	// Deviation: a cited "should" was not followed.
	Deviation Verdict = "DEVIATION"
	// Warn: no normative requirement, and the observed behaviour has
	// security or operational impact (see Criterion).
	Warn Verdict = "WARN"
	// Info: no normative requirement; the behaviour is recorded and has no
	// impact. Whether the core did what the descriptive text describes is
	// stated in the observed text.
	Info Verdict = "INFO"
	// Blocked: the core did not complete a procedure the check needs before
	// its requirement can be tested.
	Blocked Verdict = "BLOCKED"
	// Crash: the core stopped answering after the stimulus.
	Crash Verdict = "CRASH"
	// Skip: not applicable by a declared condition (see SkipCondition).
	Skip Verdict = "SKIP"
	// Error: the tool could not run the check. It says nothing about the
	// core; the setup needs fixing.
	Error Verdict = "ERROR"
)

// verdicts is every verdict, in the order reports tally them.
var verdicts = []Verdict{Pass, Fail, Deviation, Warn, Info, Blocked, Crash, Skip, Error}

// Verdicts returns every verdict, in the order reports tally them.
func Verdicts() []Verdict { return append([]Verdict(nil), verdicts...) }

// Valid reports whether v is one of the nine verdicts.
func (v Verdict) Valid() bool {
	for _, x := range verdicts {
		if v == x {
			return true
		}
	}
	return false
}

// Label is the verdict in lower case, as a tally names it ("3 fail").
func (v Verdict) Label() string { return strings.ToLower(string(v)) }

// VerdictPattern is a regular expression alternation matching any verdict
// word, for consumers that parse "<check> <verdict>" lines.
func VerdictPattern() string {
	words := make([]string, len(verdicts))
	for i, v := range verdicts {
		words[i] = string(v)
	}
	return strings.Join(words, "|")
}
