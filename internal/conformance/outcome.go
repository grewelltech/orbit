package conformance

import (
	"fmt"
	"strings"
	"time"
)

// Criterion is why an outcome with no normative requirement behind it is
// WARN and not INFO (ADR-0011 rule 4). The list is closed: an outcome that
// meets none of the three is INFO.
type Criterion string

const (
	// CriterionSecurity: the core acts on unprotected or unauthenticated input,
	// omits or weakens a protection, or accepts something it had the means
	// to reject as forged or replayed.
	CriterionSecurity Criterion = "security"
	// CriterionAvailability: the core drops or ignores a valid request, loses UE or
	// session state, or cannot complete a procedure.
	CriterionAvailability Criterion = "availability"
	// CriterionInteroperability: the behaviour breaks a peer that is itself
	// conformant.
	CriterionInteroperability Criterion = "interoperability"
)

// valid reports whether c is one of the three criteria.
func (c Criterion) valid() bool {
	return c == CriterionSecurity || c == CriterionAvailability || c == CriterionInteroperability
}

// SkipCondition is one of the three conditions under which a check may SKIP
// (ADR-0011 rule 3).
type SkipCondition string

const (
	// SkipRelease: the core's claimed release is older than the check
	// requires.
	SkipRelease SkipCondition = "release"
	// SkipMissingInput: an input the check needs is not configured.
	SkipMissingInput SkipCondition = "missing input"
	// SkipPermitted: the specification explicitly permits the alternative
	// the core chose; the outcome names the reference that permits it.
	SkipPermitted SkipCondition = "permitted alternative"
)

// Input is something a run may or may not be configured with, which a check
// that needs it SKIPs without.
type Input string

const (
	// InputUE is a subscriber to register (--supi, --ki/--opc, --gnb-n3).
	InputUE Input = "a UE"
	// InputUPFN3 is the UPF's N3 endpoint and a local N3 source address
	// (--upf-n3, --n3-bind).
	InputUPFN3 Input = "the UPF N3 endpoint"
	// InputSecondN2 is a second local N2 source address on another IP.
	InputSecondN2 Input = "a second N2 source address"
	// InputDownlink is an N6-side responder that can send downlink traffic
	// to a UE unasked (--responder).
	InputDownlink Input = "a downlink responder"
	// InputTraffic is an echo peer on the data network (--traffic).
	InputTraffic Input = "an echo peer"
	// InputIdleCore is a core no population is loading.
	InputIdleCore Input = "an idle core"
	// InputIntegrity is a NAS integrity algorithm other than the null one,
	// selected by the core for the UE.
	InputIntegrity Input = "NAS integrity protection"
)

// Bound is the wait an absence-based outcome is judged against (ADR-0011
// rule 7): "no answer" is inferred only within it.
type Bound struct {
	// Wait is how long the check waits.
	Wait time.Duration
	// Source says where the wait comes from: a timer of the specification
	// ("T3560, TS 24.501 Table 10.2.2") or "check bound" with its reason.
	Source string
}

// String renders the bound for checks.md: "6s (T3560, TS 24.501 Table 10.2.2)".
func (b Bound) String() string {
	if b.Wait == 0 {
		return b.Source
	}
	return fmt.Sprintf("%s (%s)", b.Wait, b.Source)
}

// Declarer is a check that declares the outcomes it can report.
type Declarer interface {
	// Outcomes returns every outcome the check's Run can report, beyond
	// the ones every check has.
	Outcomes() []Outcome
}

// declared returns t's declared outcomes, and whether it declares any.
func declared(t Test) ([]Outcome, bool) {
	d, ok := t.(Declarer)
	if !ok {
		return nil, false
	}
	return d.Outcomes(), true
}

// Outcome is one result a check can report, declared with the check
// (Declarer). A check's Run reports only its declared outcomes
// (Result.As) and the ones every check has (ERROR, CRASH, the SKIPs for a
// declared input and the BLOCKEDs for a declared need); runOne turns
// anything else into an ERROR. ValidateTest states the rules a declaration
// must meet.
type Outcome struct {
	// ID names the outcome within its check, in kebab case
	// ("silent-drop"). It is what an expected-failures entry and the JSON
	// output can be matched on.
	ID string
	// Verdict is the verdict a result with this outcome carries.
	Verdict Verdict
	// Summary says, in one line, what the core did; checks.md lists it.
	Summary string
	// Refs are the keys (SpecRef.Key) of the references the outcome rests
	// on: for FAIL the "shall" it violates, for DEVIATION the "should" it
	// does not follow, for PASS the requirement met, for a
	// SkipPermitted SKIP the text that permits the alternative. An INFO or
	// WARN outcome names no shall or should.
	Refs []string
	// Criterion is why a WARN outcome is one.
	Criterion Criterion
	// Skip is the condition of a SKIP outcome a check declares itself
	// (SkipPermitted; the release and missing-input SKIPs are derived).
	Skip SkipCondition
	// Bound is set on an outcome inferred from the absence of an answer.
	Bound *Bound
	// NotCompleted is set on a FAIL, DEVIATION or WARN outcome of an owner
	// check that reports a procedure it owns as not completing: the
	// BLOCKED results of its dependents are then counted under it.
	NotCompleted Procedure
	// blocked is the procedure of a derived BLOCKED outcome.
	blocked Procedure
}

// The outcomes every check has without declaring them.
const (
	// OutcomeError is the ERROR outcome: the tool could not run the check.
	OutcomeError = "error"
	// OutcomeCrash is the CRASH outcome: the core stopped answering.
	OutcomeCrash = "crash"
	// OutcomeSkipRelease is the SKIP for a target release older than the
	// check requires.
	OutcomeSkipRelease = "skip-release"
	// outcomeSkipInputPrefix and outcomeBlockedPrefix start the IDs of the
	// SKIP for a missing input and the BLOCKED for a needed procedure.
	outcomeSkipInputPrefix = "skip-no-"
	outcomeBlockedPrefix   = "blocked-"
)

// slug renders s as an outcome ID fragment: lower case, words joined by "-".
func slug(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), "-")
}

// skipInputID and blockedID are the IDs of the derived outcomes.
func skipInputID(in Input) string  { return outcomeSkipInputPrefix + slug(in.bare()) }
func blockedID(p Procedure) string { return outcomeBlockedPrefix + slug(string(p)) }

// bare is the input without its article: "UE", "second N2 source address".
func (in Input) bare() string {
	s := string(in)
	for _, article := range []string{"a ", "an ", "the "} {
		if strings.HasPrefix(s, article) {
			return strings.TrimPrefix(s, article)
		}
	}
	return s
}

// As returns r reporting outcome o with what was observed. It is how a
// check's Run sets a verdict: the verdict is the declared one.
func (r Result) As(o Outcome, observed, detail string) Result {
	r.Outcome, r.Verdict, r.Observed, r.Detail = o.ID, o.Verdict, observed, detail
	r.Criterion, r.NotCompleted = o.Criterion, o.NotCompleted
	r.BlockedOn = ""
	noteExercised(o)
	return r
}

// Errored returns r as the ERROR every check has: the tool, the setup or
// the testbed kept the check from running. It says nothing about the core.
func (r Result) Errored(observed, detail string) Result {
	return r.As(Outcome{ID: OutcomeError, Verdict: Error}, observed, detail)
}

// Crashed returns r as the CRASH every check has: the core stopped
// answering after the stimulus.
func (r Result) Crashed(observed, detail string) Result {
	return r.As(Outcome{ID: OutcomeCrash, Verdict: Crash}, observed, detail)
}

// Skipped returns r as the SKIP for an input the run does not configure.
// The check must declare the input (CheckNeeds.Inputs).
func (r Result) Skipped(in Input, observed, detail string) Result {
	return r.As(Outcome{ID: skipInputID(in), Verdict: Skip, Skip: SkipMissingInput}, observed, detail)
}

// BlockedBy returns r as BLOCKED: the core did not complete procedure p,
// which the check needs before its requirement can be tested. The check
// must declare the need (CheckNeeds.Procedures); runOne names the owner.
func (r Result) BlockedBy(p Procedure, observed, detail string) Result {
	r = r.As(Outcome{ID: blockedID(p), Verdict: Blocked}, observed, detail)
	r.BlockedOn = p
	return r
}

// derivedOutcomes are the outcomes t has without declaring them, from its
// needs: ERROR, CRASH, the release SKIP, a SKIP for each input and a
// BLOCKED for each procedure.
func derivedOutcomes(t Test) []Outcome {
	n := NeedsOf(t)
	out := []Outcome{
		{ID: OutcomeError, Verdict: Error, Summary: "not judged: the tool, the setup or the testbed kept the check from running"},
		{ID: OutcomeCrash, Verdict: Crash, Summary: "stopped answering after the stimulus"},
		{ID: OutcomeSkipRelease, Verdict: Skip, Skip: SkipRelease, Summary: "not judged: the target release is older than the check requires"},
	}
	for _, in := range n.inputs() {
		out = append(out, Outcome{ID: skipInputID(in), Verdict: Skip, Skip: SkipMissingInput, Summary: "not judged: the run has no " + in.bare()})
	}
	for _, p := range n.Procedures {
		out = append(out, Outcome{ID: blockedID(p), Verdict: Blocked, blocked: p, Summary: "did not complete " + p.article() + " the check needs first"})
	}
	return out
}

// AllOutcomes returns every outcome t can report: the ones it declares,
// then the derived ones.
func AllOutcomes(t Test) []Outcome {
	own, _ := declared(t)
	return append(append([]Outcome(nil), own...), derivedOutcomes(t)...)
}

// outcomeOf finds the outcome of t with the given ID.
func outcomeOf(t Test, id string) (Outcome, bool) {
	for _, o := range AllOutcomes(t) {
		if o.ID == id {
			return o, true
		}
	}
	return Outcome{}, false
}

// declaredWorst is the worst verdict among outs for a core, in the order
// FAIL, DEVIATION, WARN, INFO, PASS.
func declaredWorst(outs []Outcome) Verdict {
	rank := map[Verdict]int{Fail: 5, Deviation: 4, Warn: 3, Info: 2, Pass: 1}
	worst := Pass
	for _, o := range outs {
		if rank[o.Verdict] > rank[worst] {
			worst = o.Verdict
		}
	}
	return worst
}

// validateOutcomes checks t's declared outcomes against ADR-0011 rules 2 to
// 4 and 7: every outcome has an ID, a verdict and a summary; a FAIL names a
// "shall" of the check and a DEVIATION a "should"; a PASS exists only
// beside a requirement; a WARN names a criterion; an INFO or WARN names no
// shall or should; a declared SKIP is a permitted alternative and names the
// text that permits it; a bound says where it comes from; a procedure is
// reported as not completed only by an owner, on a FAIL, DEVIATION or WARN.
func validateOutcomes(t Test) error {
	refs := map[string]SpecRef{}
	for _, r := range t.SpecRefs() {
		refs[r.Key()] = r
	}
	outs, _ := declared(t)
	if len(outs) == 0 {
		return fmt.Errorf("%s: declares no outcomes", t.ID())
	}
	seen := map[string]bool{}
	for _, d := range derivedOutcomes(t) {
		seen[d.ID] = true
	}
	hasRequirement := cites(t, Shall) || cites(t, Should)
	for _, o := range outs {
		where := fmt.Sprintf("%s: outcome %q", t.ID(), o.ID)
		if o.ID == "" || o.ID != slug(o.ID) {
			return fmt.Errorf("%s: an outcome ID is lower-case words joined by '-'", where)
		}
		if seen[o.ID] {
			return fmt.Errorf("%s: declared twice, or the ID of a derived outcome", where)
		}
		seen[o.ID] = true
		if strings.TrimSpace(o.Summary) == "" {
			return fmt.Errorf("%s: no summary", where)
		}
		var cited []SpecRef
		for _, k := range o.Refs {
			r, ok := refs[k]
			if !ok {
				return fmt.Errorf("%s: cites %q, which is not the key of one of the check's references", where, k)
			}
			cited = append(cited, r)
		}
		citesStrength := func(s Strength) bool {
			for _, r := range cited {
				if r.Strength == s {
					return true
				}
			}
			return false
		}
		if o.Verdict != Warn && o.Criterion != "" {
			return fmt.Errorf("%s: a criterion belongs on a WARN outcome only", where)
		}
		if o.Verdict != Skip && o.Skip != "" {
			return fmt.Errorf("%s: a skip condition belongs on a SKIP outcome only", where)
		}
		switch o.Verdict {
		case Pass:
			if !hasRequirement {
				return fmt.Errorf("%s: PASS means a cited requirement was met, and the check cites no shall or should; report INFO", where)
			}
		case Fail:
			if !citesStrength(Shall) {
				return fmt.Errorf("%s: a FAIL outcome names the \"shall\" it violates (Refs)", where)
			}
		case Deviation:
			if !citesStrength(Should) {
				return fmt.Errorf("%s: a DEVIATION outcome names the \"should\" it does not follow (Refs)", where)
			}
		case Warn:
			if !o.Criterion.valid() {
				return fmt.Errorf("%s: a WARN outcome names its criterion: security, availability or interoperability", where)
			}
			if citesStrength(Shall) || citesStrength(Should) {
				return fmt.Errorf("%s: a WARN outcome rests on no shall or should; a violated requirement is FAIL or DEVIATION", where)
			}
		case Info:
			if citesStrength(Shall) || citesStrength(Should) {
				return fmt.Errorf("%s: an INFO outcome rests on no shall or should; a requirement is graded", where)
			}
		case Skip:
			if o.Skip != SkipPermitted {
				return fmt.Errorf("%s: a declared SKIP is a permitted alternative (SkipPermitted); the release and missing-input SKIPs come from the check's needs", where)
			}
			if len(cited) == 0 {
				return fmt.Errorf("%s: a permitted-alternative SKIP names the reference that permits it (Refs)", where)
			}
		default:
			return fmt.Errorf("%s: verdict %q cannot be declared; a check declares PASS, FAIL, DEVIATION, WARN, INFO and permitted-alternative SKIP outcomes", where, o.Verdict)
		}
		if o.Bound != nil && strings.TrimSpace(o.Bound.Source) == "" {
			return fmt.Errorf("%s: a bound says where it comes from (Source)", where)
		}
		if o.NotCompleted != "" && !owns(t, o.NotCompleted) {
			return fmt.Errorf("%s: reports %s as not completed, which the check does not own", where, o.NotCompleted)
		}
		if o.NotCompleted != "" && !isWarning(o.Verdict) && o.Verdict != Fail {
			return fmt.Errorf("%s: reports %s as not completed with verdict %s; only a FAIL, DEVIATION or WARN outcome can, as the finding its dependents count under", where, o.NotCompleted, o.Verdict)
		}
	}
	return nil
}
