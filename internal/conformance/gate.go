package conformance

import (
	"fmt"
	"strings"
)

// GatePolicy is which verdicts fail a run's gate (ADR-0011 rule 8). The
// verdict word never changes with the policy; only the gate does.
type GatePolicy string

const (
	// GateDefault gates on FAIL, CRASH, BLOCKED, ERROR, DEVIATION and WARN.
	GateDefault GatePolicy = "default"
	// GateSpec gates on FAIL, CRASH, BLOCKED and ERROR: a violated "shall",
	// a core that stopped answering or did not complete a procedure, and a
	// check the tool could not run.
	GateSpec GatePolicy = "spec"
)

// ParseGatePolicy reads a policy name; "" is GateDefault.
func ParseGatePolicy(s string) (GatePolicy, error) {
	switch GatePolicy(strings.ToLower(strings.TrimSpace(s))) {
	case "", GateDefault:
		return GateDefault, nil
	case GateSpec:
		return GateSpec, nil
	}
	return "", fmt.Errorf("gate policy %q: want default or spec", s)
}

// Gates reports whether a result with verdict v fails a gate under p (an
// empty policy is GateDefault).
func (p GatePolicy) Gates(v Verdict) bool {
	if isSpecFailure(v) {
		return true
	}
	return p != GateSpec && isWarning(v)
}

// isSpecFailure reports whether v gates under every policy: FAIL, CRASH,
// BLOCKED or ERROR.
func isSpecFailure(v Verdict) bool {
	return v == Fail || v == Crash || v == Blocked || v == Error
}

// isWarning reports whether v gates under the default policy only:
// DEVIATION or WARN.
func isWarning(v Verdict) bool { return v == Deviation || v == Warn }

// Finding is one result that counts at the gate: a spec failure (FAIL,
// CRASH, BLOCKED, ERROR) or a warning (DEVIATION, WARN) that no
// expected-failures entry matched. Blocked lists the IDs of the BLOCKED
// results counted under it: the checks that needed a procedure this result
// reports as not completing (ADR-0011 rule 5).
type Finding struct {
	Result  Result
	Blocked []string
}

// SpecFailure reports whether the finding gates under every policy.
func (f Finding) SpecFailure() bool { return isSpecFailure(f.Result.Verdict) }

// Findings returns the results that count at a gate, in run order. A
// result an expected-failures entry matched does not count. A BLOCKED
// result counts under its procedure's owner, not on its own, when the
// owner ran in the same run and reported that procedure as not completing
// (Outcome.NotCompleted): one core fault counts once. It counts on its own
// when the owner was not selected or reported the procedure completing.
func Findings(results []Result) []Finding {
	ownerAt := map[string]int{}
	var out []Finding
	for _, r := range results {
		if !isSpecFailure(r.Verdict) && !isWarning(r.Verdict) || r.ExpectedFailure != nil {
			continue
		}
		if r.Verdict == Blocked && countedUnderOwner(r, results) {
			continue
		}
		ownerAt[r.ID] = len(out)
		out = append(out, Finding{Result: r})
	}
	for _, r := range results {
		if r.Verdict != Blocked || !countedUnderOwner(r, results) {
			continue
		}
		if i, ok := ownerAt[r.Owner]; ok {
			out[i].Blocked = append(out[i].Blocked, r.ID)
		}
	}
	return out
}

// countedUnderOwner reports whether BLOCKED result r is counted under its
// owner: the owner is among results and reported r's procedure as not
// completing. An owner matched by an expected-failures entry still takes
// its dependents: the fault is the one the entry lists.
func countedUnderOwner(r Result, results []Result) bool {
	if r.Owner == "" || r.BlockedOn == "" {
		return false
	}
	for _, o := range results {
		if o.ID == r.Owner {
			return o.NotCompleted == r.BlockedOn
		}
	}
	return false
}

// GateFindings is the findings that fail a gate under p.
func GateFindings(results []Result, p GatePolicy) []Finding {
	var out []Finding
	for _, f := range Findings(results) {
		if p.Gates(f.Result.Verdict) {
			out = append(out, f)
		}
	}
	return out
}

// GateLine says how a run stands at its gate, separating spec failures
// from warnings: "gate default: failed: 3 spec failures, 2 warnings". The
// counts are of Findings, whatever the policy; the policy decides which of
// them fail the gate.
func GateLine(results []Result, p GatePolicy) string {
	if p == "" {
		p = GateDefault
	}
	spec, warn := 0, 0
	for _, f := range Findings(results) {
		if f.SpecFailure() {
			spec++
		} else {
			warn++
		}
	}
	state := "passed"
	if len(GateFindings(results, p)) > 0 {
		state = "failed"
	}
	return fmt.Sprintf("gate %s: %s: %s, %s", p, state, plural(spec, "spec failure"), plural(warn, "warning"))
}

// plural renders "1 warning", "2 warnings".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
