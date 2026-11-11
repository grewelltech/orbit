package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
)

// declTest is a configurable check that declares its outcomes, needs and
// owned procedures.
type declTest struct {
	provTest
	outs  []Outcome
	needs CheckNeeds
	owned []Procedure
}

func (d declTest) Outcomes() []Outcome { return d.outs }
func (d declTest) Needs() CheckNeeds   { return d.needs }
func (d declTest) Owns() []Procedure   { return d.owned }

// The references the declaration tests cite, by strength; the shall is the
// primary one.
var (
	declShall  = SpecRef{Spec: "TS 38.413", Clause: "10.6", Strength: Shall, Since: 15, Verified: ts38413Verified, WordingUnchanged: true, Primary: true}
	declShould = SpecRef{Spec: "TS 38.413", Clause: "8.7.1.3", Strength: Should, Since: 15, Verified: ts38413Verified, WordingUnchanged: true}
	declDesc   = SpecRef{Spec: "TS 38.413", Clause: "8.7.1.2", Strength: Descriptive, Since: 15, Verified: ts38413Verified, WordingUnchanged: true}
)

func primary(r SpecRef) SpecRef { r.Primary = true; return r }

func plain(r SpecRef) SpecRef { r.Primary = false; return r }

// ValidateTest refuses a declaration that breaks ADR-0011 rules 2, 3, 4, 5
// or 7, and accepts one that keeps them.
func TestValidateDeclaration(t *testing.T) {
	pass := Outcome{ID: "met", Verdict: Pass, Summary: "did it", Refs: []string{declShall.Key()}}
	tests := []struct {
		name    string
		refs    []SpecRef
		outs    []Outcome
		needs   CheckNeeds
		owned   []Procedure
		wantErr string
	}{
		{name: "a shall check with every kind of outcome", refs: []SpecRef{declShall, declShould, declDesc}, outs: []Outcome{
			pass,
			{ID: "violated", Verdict: Fail, Summary: "did not", Refs: []string{declShall.Key()}, Bound: &Bound{Wait: time.Second, Source: "check bound"}},
			{ID: "not-followed", Verdict: Deviation, Summary: "did otherwise", Refs: []string{declShould.Key()}},
			{ID: "unprotected", Verdict: Warn, Summary: "acted on it", Criterion: CriterionSecurity, Refs: []string{declDesc.Key()}},
			{ID: "recorded", Verdict: Info, Summary: "did something harmless"},
			{ID: "allowed", Verdict: Skip, Skip: SkipPermitted, Summary: "chose the alternative", Refs: []string{declDesc.Key()}},
		}},
		{name: "a characterisation check", refs: []SpecRef{primary(declDesc)}, outs: []Outcome{
			{ID: "described", Verdict: Info, Summary: "did what the text describes", Refs: []string{declDesc.Key()}},
			{ID: "refused", Verdict: Warn, Summary: "refused", Criterion: CriterionAvailability},
		}},
		{name: "an owner reporting its procedure", refs: []SpecRef{primary(declDesc)}, owned: []Procedure{ProcNGSetup}, outs: []Outcome{
			{ID: "refused", Verdict: Warn, Summary: "refused", Criterion: CriterionAvailability, NotCompleted: ProcNGSetup},
		}},
		{name: "no outcomes", refs: []SpecRef{declShall}, wantErr: "declares no outcomes"},
		{name: "no reference", outs: []Outcome{pass}, wantErr: "no spec references"},
		{name: "no primary reference", refs: []SpecRef{plain(declShall)}, outs: []Outcome{pass}, wantErr: "0 references are marked primary; exactly one is"},
		{name: "two primary references", refs: []SpecRef{declShall, primary(declShould)}, outs: []Outcome{pass}, wantErr: "2 references are marked primary; exactly one is"},
		{name: "two references with one key", refs: []SpecRef{declShall, plain(declShall)}, outs: []Outcome{pass}, wantErr: `two references share the key "TS 38.413 10.6"`},
		{name: "FAIL citing nothing", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Fail, Summary: "s"}},
			wantErr: `outcome "x": a FAIL outcome names the "shall" it violates`},
		{name: "FAIL citing a should", refs: []SpecRef{declShall, declShould}, outs: []Outcome{{ID: "x", Verdict: Fail, Summary: "s", Refs: []string{declShould.Key()}}},
			wantErr: `a FAIL outcome names the "shall" it violates`},
		{name: "FAIL citing descriptive text", refs: []SpecRef{primary(declDesc)}, outs: []Outcome{{ID: "x", Verdict: Fail, Summary: "s", Refs: []string{declDesc.Key()}}},
			wantErr: `a FAIL outcome names the "shall" it violates`},
		{name: "FAIL citing a reference of another check", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Fail, Summary: "s", Refs: []string{"TS 24.501 5.5.1.2.4"}}},
			wantErr: `cites "TS 24.501 5.5.1.2.4", which is not the key of one of the check's references`},
		{name: "DEVIATION citing nothing", refs: []SpecRef{declShall, declShould}, outs: []Outcome{{ID: "x", Verdict: Deviation, Summary: "s"}},
			wantErr: `a DEVIATION outcome names the "should" it does not follow`},
		{name: "DEVIATION citing a shall", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Deviation, Summary: "s", Refs: []string{declShall.Key()}}},
			wantErr: `a DEVIATION outcome names the "should" it does not follow`},
		{name: "WARN without a criterion", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Warn, Summary: "s"}},
			wantErr: "a WARN outcome names its criterion: security, availability or interoperability"},
		{name: "WARN with a criterion off the list", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Warn, Summary: "s", Criterion: "performance"}},
			wantErr: "a WARN outcome names its criterion"},
		{name: "WARN citing a shall", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Warn, Summary: "s", Criterion: CriterionSecurity, Refs: []string{declShall.Key()}}},
			wantErr: "a WARN outcome rests on no shall or should"},
		{name: "WARN citing a should", refs: []SpecRef{declShall, declShould}, outs: []Outcome{{ID: "x", Verdict: Warn, Summary: "s", Criterion: CriterionSecurity, Refs: []string{declShould.Key()}}},
			wantErr: "a WARN outcome rests on no shall or should"},
		{name: "INFO citing a shall", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Info, Summary: "s", Refs: []string{declShall.Key()}}},
			wantErr: "an INFO outcome rests on no shall or should"},
		{name: "INFO citing a should", refs: []SpecRef{declShall, declShould}, outs: []Outcome{{ID: "x", Verdict: Info, Summary: "s", Refs: []string{declShould.Key()}}},
			wantErr: "an INFO outcome rests on no shall or should"},
		{name: "a criterion on an INFO", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Info, Summary: "s", Criterion: CriterionSecurity}},
			wantErr: "a criterion belongs on a WARN outcome only"},
		{name: "PASS in a check with no requirement", refs: []SpecRef{primary(declDesc)}, outs: []Outcome{{ID: "x", Verdict: Pass, Summary: "s"}},
			wantErr: "PASS means a cited requirement was met, and the check cites no shall or should"},
		{name: "a declared SKIP for a missing input", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Skip, Skip: SkipMissingInput, Summary: "s"}},
			wantErr: "a declared SKIP is a permitted alternative"},
		{name: "a permitted-alternative SKIP citing nothing", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Skip, Skip: SkipPermitted, Summary: "s"}},
			wantErr: "a permitted-alternative SKIP names the reference that permits it"},
		{name: "a declared BLOCKED", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Blocked, Summary: "s"}},
			wantErr: `verdict "BLOCKED" cannot be declared`},
		{name: "a declared ERROR", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Error, Summary: "s"}},
			wantErr: `verdict "ERROR" cannot be declared`},
		{name: "a declared CRASH", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Crash, Summary: "s"}},
			wantErr: `verdict "CRASH" cannot be declared`},
		{name: "an outcome declared twice", refs: []SpecRef{declShall}, outs: []Outcome{pass, pass}, wantErr: `outcome "met": declared twice`},
		{name: "the ID of a derived outcome", refs: []SpecRef{declShall}, outs: []Outcome{{ID: OutcomeError, Verdict: Info, Summary: "s"}},
			wantErr: `outcome "error": declared twice, or the ID of a derived outcome`},
		{name: "an ID that is not kebab case", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "No Answer", Verdict: Info, Summary: "s"}},
			wantErr: "an outcome ID is lower-case words joined by '-'"},
		{name: "no summary", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Info}}, wantErr: "no summary"},
		{name: "a bound with no source", refs: []SpecRef{declShall}, outs: []Outcome{{ID: "x", Verdict: Fail, Summary: "s", Refs: []string{declShall.Key()}, Bound: &Bound{Wait: time.Second}}},
			wantErr: "a bound says where it comes from"},
		{name: "not completed, of a procedure it does not own", refs: []SpecRef{primary(declDesc)}, outs: []Outcome{
			{ID: "x", Verdict: Warn, Summary: "s", Criterion: CriterionAvailability, NotCompleted: ProcRegistration}},
			wantErr: "reports registration as not completed, which the check does not own"},
		{name: "a need off the fixed list", refs: []SpecRef{declShall}, outs: []Outcome{pass}, needs: CheckNeeds{Procedures: []Procedure{"paging"}},
			wantErr: `"paging" is not a procedure of the fixed list`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTest(declTest{provTest: provTest{id: "X", refs: tc.refs}, outs: tc.outs, needs: tc.needs, owned: tc.owned})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// runOne holds a check to its declaration: an outcome it declares, or has
// from its needs, is reported with what the declaration says of it; a
// result with no outcome, an undeclared one or another verdict is an ERROR.
func TestRunOneEnforcesDeclaration(t *testing.T) {
	fail := Outcome{ID: "violated", Verdict: Fail, Summary: "did not", Refs: []string{declShall.Key()}, Bound: &Bound{Wait: 3 * time.Second, Source: "check bound"}}
	dev := Outcome{ID: "not-followed", Verdict: Deviation, Summary: "did otherwise", Refs: []string{declShould.Key()}}
	warn := Outcome{ID: "refused", Verdict: Warn, Summary: "refused", Criterion: CriterionAvailability, NotCompleted: ProcANRelease}
	info := Outcome{ID: "recorded", Verdict: Info, Summary: "recorded"}
	needs := CheckNeeds{UEs: 1, Procedures: []Procedure{ProcNGSetup, ProcRegistration}}
	base := Result{Expected: "the answer"}
	tests := []struct {
		name        string
		result      Result
		want        Verdict
		wantOutcome string
		wantDetail  []string
		check       func(t *testing.T, r Result)
	}{
		{name: "a declared FAIL names its shall and bound", result: base.As(fail, "no answer", "nothing came"), want: Fail, wantOutcome: "violated",
			wantDetail: []string{"nothing came; violates TS 38.413 10.6 (shall)", "absence inferred within 3s (check bound)"},
			check: func(t *testing.T, r Result) {
				if len(r.Refs) != 1 || r.Refs[0] != "TS 38.413 10.6 (shall)" {
					t.Errorf("Refs = %v", r.Refs)
				}
			}},
		{name: "a bound the detail already states is not repeated", result: base.As(fail, "no answer", "none within 1s (absence inferred within this bound)"), want: Fail, wantOutcome: "violated",
			check: func(t *testing.T, r Result) {
				if strings.Count(r.Detail, "absence inferred") != 1 {
					t.Errorf("detail %q states the bound twice", r.Detail)
				}
			}},
		{name: "a declared DEVIATION names its should", result: base.As(dev, "other", ""), want: Deviation, wantOutcome: "not-followed",
			wantDetail: []string{"does not follow TS 38.413 8.7.1.3 (should)"}},
		{name: "a declared WARN names its criterion", result: base.As(warn, "refused", "cause 7"), want: Warn, wantOutcome: "refused",
			wantDetail: []string{"cause 7; no requirement violated; WARN for availability impact"},
			check: func(t *testing.T, r Result) {
				if r.Criterion != CriterionAvailability || r.NotCompleted != ProcANRelease {
					t.Errorf("criterion %q, not completed %q", r.Criterion, r.NotCompleted)
				}
			}},
		{name: "a declared INFO", result: base.As(info, "seen", "d"), want: Info, wantOutcome: "recorded"},
		{name: "ERROR", result: base.Errored("bad bind", "d"), want: Error, wantOutcome: OutcomeError},
		{name: "CRASH", result: base.Crashed(ObservedAMFUnresponsive+" (x)", "d"), want: Crash, wantOutcome: OutcomeCrash},
		{name: "SKIP for a declared input", result: base.Skipped(InputUE, "no UE configured", "d"), want: Skip, wantOutcome: "skip-no-ue"},
		{name: "BLOCKED on a declared need names the owner", result: base.BlockedBy(ProcRegistration, "UE registration failed", "rejected"), want: Blocked, wantOutcome: "blocked-registration",
			wantDetail: []string{"rejected; blocked: the core did not complete a registration, which this check needs first; OWNER-REG owns whether it completes"},
			check: func(t *testing.T, r Result) {
				if r.BlockedOn != ProcRegistration || r.Owner != "OWNER-REG" {
					t.Errorf("blocked on %q, owner %q", r.BlockedOn, r.Owner)
				}
			}},
		{name: "BLOCKED on a need no check owns says so", result: base.BlockedBy(ProcNGSetup, "setup failed", "d"), want: Blocked, wantOutcome: "blocked-ng-setup",
			wantDetail: []string{"no check of this run's registry owns whether it completes"}},
		{name: "BLOCKED on a procedure it does not need", result: base.BlockedBy(ProcPDUSession, "no session", "d"), want: Error, wantOutcome: OutcomeError,
			wantDetail: []string{`Run returned outcome "blocked-pdu-session-establishment"`, "neither declares nor has from its needs"}},
		{name: "SKIP for an input it does not declare", result: base.Skipped(InputDownlink, "no responder", "d"), want: Error, wantOutcome: OutcomeError,
			wantDetail: []string{`Run returned outcome "skip-no-downlink-responder"`}},
		{name: "an outcome of another check", result: base.As(Outcome{ID: "elsewhere", Verdict: Fail}, "x", "d"), want: Error, wantOutcome: OutcomeError,
			wantDetail: []string{`Run returned outcome "elsewhere" (FAIL "x")`}},
		{name: "no outcome", result: Result{Verdict: Fail, Observed: "x"}, want: Error, wantOutcome: OutcomeError,
			wantDetail: []string{`Run returned FAIL "x" without one of the check's declared outcomes`}},
		{name: "a declared outcome with another verdict", result: func() Result { r := base.As(info, "seen", ""); r.Verdict = Fail; return r }(), want: Error, wantOutcome: OutcomeError,
			wantDetail: []string{`Run returned outcome "recorded" as FAIL; it is declared INFO`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := declTest{provTest: provTest{id: "X", refs: []SpecRef{declShall, declShould, declDesc}, result: tc.result},
				outs: []Outcome{fail, dev, warn, info}, needs: needs, owned: []Procedure{ProcANRelease}}
			if err := ValidateTest(c); err != nil {
				t.Fatal(err)
			}
			res := runOne(context.Background(), c, Env{owners: map[Procedure]string{ProcRegistration: "OWNER-REG"}}, time.Second)
			if res.Verdict != tc.want || res.Outcome != tc.wantOutcome {
				t.Fatalf("= %s %q (%s), want %s %q", res.Verdict, res.Outcome, res.Detail, tc.want, tc.wantOutcome)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(res.Detail, w) {
					t.Errorf("detail %q lacks %q", res.Detail, w)
				}
			}
			if !strings.HasSuffix(res.Detail, "requires Rel-15 or later; "+RenderRefs(c.SpecRefs())) {
				t.Errorf("detail %q does not end with the requirement line", res.Detail)
			}
			if res.Expected != "the answer" && tc.name != "no outcome" {
				t.Errorf("Expected = %q, want it kept", res.Expected)
			}
			if tc.check != nil {
				tc.check(t, res)
			}
		})
	}
}

// The SKIPs runOne gives itself are the derived outcomes: the release SKIP
// for every check, and the idle-core SKIP under load.
func TestRunOneDerivedSkips(t *testing.T) {
	ref17 := declShall
	ref17.Since, ref17.Verified = 17, nil
	old := declTest{provTest: provTest{id: "X", refs: []SpecRef{ref17}}, outs: []Outcome{{ID: "met", Verdict: Pass, Summary: "s"}}}
	if res := runOne(context.Background(), old, Env{TargetRelease: 16}, time.Second); res.Verdict != Skip || res.Outcome != OutcomeSkipRelease {
		t.Errorf("older target: %s %q, want SKIP %q", res.Verdict, res.Outcome, OutcomeSkipRelease)
	}
	idle := declTest{provTest: provTest{id: "Y", refs: []SpecRef{declShall}}, outs: []Outcome{{ID: "met", Verdict: Pass, Summary: "s"}},
		needs: CheckNeeds{RequiresIdleCore: true, IdleCoreReason: "measures timers"}}
	res := runOne(context.Background(), idle, Env{Load: func() string { return "fleet" }}, time.Second)
	if res.Verdict != Skip || res.Outcome != "skip-no-idle-core" || !strings.Contains(res.Detail, "measures timers; population fleet is active") {
		t.Errorf("under load: %s %q (%s), want the idle-core SKIP", res.Verdict, res.Outcome, res.Detail)
	}
	if _, ok := outcomeOf(idle, "skip-no-idle-core"); !ok {
		t.Error("the idle-core SKIP is not among the check's derived outcomes")
	}
}

// A check's derived outcomes follow its needs, and its class is the worst
// verdict it declares.
func TestDerivedOutcomesAndClass(t *testing.T) {
	c := declTest{provTest: provTest{id: "X", refs: []SpecRef{declShall}},
		outs:  []Outcome{{ID: "met", Verdict: Pass, Summary: "s"}, {ID: "odd", Verdict: Warn, Criterion: CriterionSecurity, Summary: "s"}},
		needs: CheckNeeds{UEs: 1, SecondN2Source: true, Inputs: []Input{InputDownlink, InputUE}, Procedures: []Procedure{ProcNGSetup, ProcPDUSession}}}
	var ids []string
	for _, o := range AllOutcomes(c) {
		ids = append(ids, o.ID+"="+string(o.Verdict))
	}
	want := "met=PASS odd=WARN error=ERROR crash=CRASH skip-release=SKIP skip-no-ue=SKIP skip-no-second-n2-source-address=SKIP " +
		"skip-no-downlink-responder=SKIP blocked-ng-setup=BLOCKED blocked-pdu-session-establishment=BLOCKED"
	if got := strings.Join(ids, " "); got != want {
		t.Errorf("outcomes:\n got %s\nwant %s", got, want)
	}
	classes := []struct {
		outs []Verdict
		want Verdict
	}{
		{[]Verdict{Pass, Info}, Info}, {[]Verdict{Info, Warn}, Warn}, {[]Verdict{Warn, Deviation, Pass}, Deviation},
		{[]Verdict{Deviation, Fail, Warn}, Fail}, {[]Verdict{Pass}, Pass},
	}
	for _, tc := range classes {
		var outs []Outcome
		for i, v := range tc.outs {
			outs = append(outs, Outcome{ID: fmt.Sprint("o", i), Verdict: v})
		}
		d := declTest{outs: outs}
		if got := WorstVerdict(d); got != tc.want || VerdictClass(d) != tc.want {
			t.Errorf("%v: worst %s, class %s, want %s", tc.outs, got, VerdictClass(d), tc.want)
		}
	}
}

// The registry refuses two owners of one procedure, a need no registered
// check owns, a check that needs what it owns and a procedure off the
// fixed list.
func TestRegistryValidate(t *testing.T) {
	check := func(id string, owned []Procedure, needs ...Procedure) Test {
		return declTest{provTest: provTest{id: id}, owned: owned, needs: CheckNeeds{Procedures: needs}}
	}
	tests := []struct {
		name    string
		tests   []Test
		wantErr string
	}{
		{name: "an owner for every need", tests: []Test{check("SETUP", []Procedure{ProcNGSetup}), check("REG", []Procedure{ProcRegistration}, ProcNGSetup),
			check("USER", nil, ProcNGSetup, ProcRegistration), provTest{id: "LEGACY"}}},
		{name: "an owned procedure nobody needs", tests: []Test{check("SETUP", []Procedure{ProcNGSetup})}},
		{name: "two owners", tests: []Test{check("A", []Procedure{ProcNGSetup}), check("B", []Procedure{ProcNGSetup})},
			wantErr: `procedure "NG Setup" has two owners: A and B`},
		{name: "a need with no owner", tests: []Test{check("SETUP", []Procedure{ProcNGSetup}), check("USER", nil, ProcNGSetup, ProcRegistration), check("OTHER", nil, ProcANRelease)},
			wantErr: `no registered check owns a needed procedure: OTHER needs "AN release"; USER needs "registration"`},
		{name: "a check that needs what it owns", tests: []Test{check("SETUP", []Procedure{ProcNGSetup}, ProcNGSetup)},
			wantErr: `SETUP: needs "NG Setup", which it owns`},
		{name: "an owned procedure off the list", tests: []Test{check("A", []Procedure{"paging"})}, wantErr: `A: owns "paging", which is not a procedure of the fixed list`},
		{name: "a needed procedure off the list", tests: []Test{check("A", nil, "paging")}, wantErr: `A: needs "paging", which is not a procedure of the fixed list`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Registry{}
			for _, c := range tc.tests {
				r.Register(c)
			}
			err := r.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if tc.wantErr == "" && r.OwnerOf(ProcNGSetup) != "SETUP" {
				t.Errorf("OwnerOf(NG Setup) = %q", r.OwnerOf(ProcNGSetup))
			}
		})
	}
}

// A setup step that did not complete is attributed in one place: ERROR for
// a local fault, otherwise BLOCKED on the procedure the error names.
func TestSetupFailed(t *testing.T) {
	bind := &syscallError{syscall.EADDRNOTAVAIL}
	tests := []struct {
		name       string
		err        error
		want       Verdict
		wantProc   Procedure
		wantDetail string
	}{
		{name: "a registration the core rejects", err: fmt.Errorf("registration on gNB 0x100: %w", engine.ErrRegistrationRejected), want: Blocked, wantProc: ProcRegistration},
		{name: "a registration the core leaves unanswered", err: fmt.Errorf("registration: %w", context.DeadlineExceeded), want: Blocked, wantProc: ProcRegistration,
			wantDetail: "the core did not complete a registration before the check's time ran out"},
		{name: "an NG Setup the engine could not complete", err: fmt.Errorf("registration on gNB 0x100: %w", engine.ErrNGSetup), want: Blocked, wantProc: ProcNGSetup},
		{name: "an NG Setup the check's own association did not complete", err: during(ProcNGSetup, errors.New("NG Setup rejected: misc/5")), want: Blocked, wantProc: ProcNGSetup},
		{name: "a refused association", err: during(ProcNGSetup, &syscallError{syscall.ECONNREFUSED}), want: Blocked, wantProc: ProcNGSetup},
		{name: "a PDU session the core did not establish", err: during(ProcPDUSession, errors.New("PDU session establishment: no active session")), want: Blocked, wantProc: ProcPDUSession},
		{name: "a source address this host cannot bind", err: during(ProcNGSetup, fmt.Errorf("SCTP dial: %w", bind)), want: Error},
		{name: "a data path that could not be opened", err: &localError{errors.New("data path: no bound N3 socket")}, want: Error},
		{name: "a cancelled run", err: fmt.Errorf("registration: %w", context.Canceled), want: Error},
		{name: "no route to the core", err: during(ProcNGSetup, &syscallError{syscall.ENETUNREACH}), want: Error},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Result{Expected: "x"}.SetupFailed("setup failed", tc.err)
			if r.Verdict != tc.want || r.BlockedOn != tc.wantProc || r.Observed != "setup failed" || !strings.Contains(r.Detail, tc.err.Error()) {
				t.Errorf("= %s on %q, %q (%s); want %s on %q", r.Verdict, r.BlockedOn, r.Observed, r.Detail, tc.want, tc.wantProc)
			}
			if !strings.Contains(r.Detail, tc.wantDetail) {
				t.Errorf("detail %q lacks %q", r.Detail, tc.wantDetail)
			}
			if tc.want == Blocked && r.Outcome != blockedID(tc.wantProc) {
				t.Errorf("outcome %q, want %q", r.Outcome, blockedID(tc.wantProc))
			}
		})
	}
}

// syscallError wraps an errno the way a socket call reports it.
type syscallError struct{ errno syscall.Errno }

func (e *syscallError) Error() string { return "socket: " + e.errno.Error() }
func (e *syscallError) Unwrap() error { return e.errno }

// checks.md lists each declaring check's outcomes with what the verdict
// rests on: the review surface of ADR-0011.
func TestChecksMarkdownOutcomes(t *testing.T) {
	owner := declTest{provTest: provTest{id: "OWNER", refs: []SpecRef{primary(declDesc), declShould}}, owned: []Procedure{ProcNGSetup}, outs: []Outcome{
		{ID: "accepted", Verdict: Info, Summary: "accepted it", Refs: []string{declDesc.Key()}},
		{ID: "refused", Verdict: Warn, Summary: "refused it", Criterion: CriterionAvailability, NotCompleted: ProcNGSetup},
		{ID: "silent", Verdict: Deviation, Summary: "said nothing", Refs: []string{declShould.Key()}, Bound: &Bound{Wait: 4 * time.Second, Source: "check bound"}},
	}}
	user := declTest{provTest: provTest{id: "USER", refs: []SpecRef{declShall}}, needs: CheckNeeds{UEs: 1, Procedures: []Procedure{ProcNGSetup, ProcRegistration}}, outs: []Outcome{
		{ID: "met", Verdict: Pass, Summary: "did it", Refs: []string{declShall.Key()}},
		{ID: "violated", Verdict: Fail, Summary: "did not", Refs: []string{declShall.Key()}},
		{ID: "allowed", Verdict: Skip, Skip: SkipPermitted, Summary: "chose the alternative", Refs: []string{declShall.Key()}},
	}}
	doc := ChecksMarkdown([]Test{owner, user, provTest{id: "LEGACY", refs: []SpecRef{declShall}}})
	for _, want := range []string{
		"## Declared outcomes",
		"### OWNER outcomes\n\nOwns: NG Setup. Needs no procedure first.",
		"| `accepted` | INFO | TS 38.413 8.7.1.2 (descriptive) | accepted it |",
		"| `refused` | WARN | availability; reports NG Setup as not completed | refused it |",
		"| `silent` | DEVIATION | TS 38.413 8.7.1.3 (should); absence within 4s (check bound) | said nothing |",
		"### USER outcomes\n\nNeeds: NG Setup, registration.",
		"| `met` | PASS | TS 38.413 10.6 (shall) | did it |",
		"| `violated` | FAIL | TS 38.413 10.6 (shall) | did not |",
		"| `allowed` | SKIP | permitted alternative; TS 38.413 10.6 (shall) | chose the alternative |",
		"| `skip-no-ue` | SKIP | missing input | not judged: the run has no UE |",
		"| `blocked-ng-setup` | BLOCKED | NG Setup (owner OWNER) | did not complete an NG Setup the check needs first |",
		"| `blocked-registration` | BLOCKED | registration (owner no registered check) | did not complete a registration the check needs first |",
		"- TS 38.413 8.7.1.2 (descriptive, since Rel-15):",
		"; primary\n",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("checks.md lacks %q", want)
		}
	}
	if strings.Contains(doc, "### LEGACY outcomes") {
		t.Error("a check that declares no outcomes has an outcomes table")
	}
	if none := ChecksMarkdown([]Test{provTest{id: "LEGACY", refs: []SpecRef{declShall}}}); strings.Contains(none, "## Declared outcomes") {
		t.Error("an outcomes section with no declaring check")
	}
}
