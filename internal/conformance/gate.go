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
// expected-failures entry matched, or an owner whose BLOCKED dependents are
// counted under it. Blocked lists the IDs of those BLOCKED results: the
// checks that needed a procedure this result reports as not completing
// (ADR-0011 rule 5).
type Finding struct {
	Result  Result
	Blocked []string
}

// GatesAs is the verdict the finding gates as under p (ADR-0012): the
// result's own verdict where p gates it and no expected-failures entry
// matched it; otherwise BLOCKED when checks were BLOCKED under it, since
// conformance cannot be claimed for checks the core kept from running;
// otherwise its own verdict, which then does not gate under p.
func (f Finding) GatesAs(p GatePolicy) Verdict {
	if f.Result.ExpectedFailure == nil && p.Gates(f.Result.Verdict) {
		return f.Result.Verdict
	}
	if len(f.Blocked) > 0 {
		return Blocked
	}
	return f.Result.Verdict
}

// gates reports whether the finding fails a gate under p.
func (f Finding) gates(p GatePolicy) bool { return p.Gates(f.GatesAs(p)) }

// blocking reports whether the finding gates under p only for the checks
// BLOCKED under it: an owner whose own verdict p does not gate, or that an
// expected-failures entry matched.
func (f Finding) blocking(p GatePolicy) bool {
	return f.Result.Verdict != Blocked && f.GatesAs(p) == Blocked
}

// Describe renders the finding for a gate's detail under p: "X FAIL",
// "OWNER WARN (and 2 BLOCKED under it: A, B)", or for an owner that gates
// for its dependents alone "OWNER BLOCKED (its WARN does not gate under
// spec; 2 BLOCKED under it: A, B)".
func (f Finding) Describe(p GatePolicy) string {
	if p == "" {
		p = GateDefault
	}
	s := f.Result.ID + " " + string(f.GatesAs(p))
	under := ""
	if len(f.Blocked) > 0 {
		under = fmt.Sprintf("%d BLOCKED under it: %s", len(f.Blocked), strings.Join(f.Blocked, ", "))
	}
	switch {
	case f.blocking(p) && f.Result.ExpectedFailure != nil:
		return fmt.Sprintf("%s (its %s is expected: %s; %s)", s, f.Result.Verdict, f.Result.ExpectedFailure.Finding, under)
	case f.blocking(p):
		return fmt.Sprintf("%s (its %s does not gate under %s; %s)", s, f.Result.Verdict, p, under)
	case under != "":
		return fmt.Sprintf("%s (and %s)", s, under)
	}
	return s
}

// gating reports whether v is a verdict some policy gates.
func gating(v Verdict) bool { return isSpecFailure(v) || isWarning(v) }

// Findings returns the results that count at a gate, in run order. A
// BLOCKED result counts under its procedure's owner, not on its own, when
// the owner ran in the same run, reported that procedure as not completing
// (Outcome.NotCompleted) with a verdict that gates under some policy: one
// core fault counts once. It counts on its own when the owner was not
// selected, reported the procedure completing, or reported a verdict no
// policy gates. A result an expected-failures entry matched counts only
// when BLOCKED results are counted under it, and then gates as BLOCKED
// (GatesAs): the entry covers the owner's own verdict, not the checks the
// core kept from running.
func Findings(results []Result) []Finding {
	ownerAt := map[string]int{}
	var out []Finding
	for _, r := range results {
		if !gating(r.Verdict) {
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
	kept := out[:0]
	for _, f := range out {
		if f.Result.ExpectedFailure == nil || len(f.Blocked) > 0 {
			kept = append(kept, f)
		}
	}
	return kept
}

// countedUnderOwner reports whether BLOCKED result r is counted under its
// owner: the owner is among results, reported r's procedure as not
// completing, and has a verdict some policy gates. An owner matched by an
// expected-failures entry still takes its dependents (Findings).
func countedUnderOwner(r Result, results []Result) bool {
	if r.Owner == "" || r.BlockedOn == "" {
		return false
	}
	for _, o := range results {
		if o.ID == r.Owner {
			return o.NotCompleted == r.BlockedOn && o.Verdict != Blocked && gating(o.Verdict)
		}
	}
	return false
}

// GateFindings is the findings that fail a gate under p (Finding.GatesAs).
func GateFindings(results []Result, p GatePolicy) []Finding {
	var out []Finding
	for _, f := range Findings(results) {
		if f.gates(p) {
			out = append(out, f)
		}
	}
	return out
}

// GateLine says how a run stands at its gate under p, separating spec
// failures from warnings and from owners that gate for the checks BLOCKED
// under them alone (ADR-0012): "gate default: failed: 3 spec failures, 2
// warnings" or "gate spec: failed: 0 spec failures, 1 warning, 1 blocked:
// OWNER blocked 2 checks". Each finding counts once, as the verdict it
// gates as under p.
func GateLine(results []Result, p GatePolicy) string {
	if p == "" {
		p = GateDefault
	}
	spec, warn := 0, 0
	var blocking []string
	for _, f := range Findings(results) {
		switch {
		case f.blocking(p):
			blocking = append(blocking, fmt.Sprintf("%s blocked %s", f.Result.ID, plural(len(f.Blocked), "check")))
		case isSpecFailure(f.GatesAs(p)):
			spec++
		default:
			warn++
		}
	}
	state := "passed"
	if len(GateFindings(results, p)) > 0 {
		state = "failed"
	}
	line := fmt.Sprintf("gate %s: %s: %s, %s", p, state, plural(spec, "spec failure"), plural(warn, "warning"))
	if len(blocking) > 0 {
		line += fmt.Sprintf(", %d blocked: %s", len(blocking), strings.Join(blocking, "; "))
	}
	return line
}

// GateDetail is GateLine followed, when the gate fails, by the findings
// that fail it (Finding.Describe): "gate spec: failed: 1 spec failure, 0
// warnings; 1 unexpected: X FAIL". The CLI and the server's conformance
// gate both report it.
func GateDetail(results []Result, p GatePolicy) string {
	line := GateLine(results, p)
	bad := GateFindings(results, p)
	if len(bad) == 0 {
		return line
	}
	items := make([]string, len(bad))
	for i, f := range bad {
		items[i] = f.Describe(p)
	}
	return fmt.Sprintf("%s; %d unexpected: %s", line, len(bad), strings.Join(items, ", "))
}

// plural renders "1 warning", "2 warnings".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
