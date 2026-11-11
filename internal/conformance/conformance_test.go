package conformance

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fakeTest struct {
	id       string
	cat      Category
	verdict  Verdict
	strength Strength // Shall when empty
}

func (f fakeTest) ID() string         { return f.id }
func (f fakeTest) Category() Category { return f.cat }
func (f fakeTest) SpecRefs() []SpecRef {
	s := f.strength
	if s == "" {
		s = Shall
	}
	return []SpecRef{{Spec: "TS 00.000", Clause: "0", Strength: s, Since: 15, Verified: ts38413Verified, WordingUnchanged: true}}
}
func (f fakeTest) Run(context.Context, Env) Result {
	return Result{Verdict: f.verdict, Outcome: verdictOutcomeID(f.verdict), Observed: "fake"}
}

// Outcomes declares the one outcome f reports.
func (f fakeTest) Outcomes() []Outcome {
	return []Outcome{{ID: verdictOutcomeID(f.verdict), Verdict: f.verdict, Summary: "fake"}}
}

func TestRegistryRunStampsAndOrders(t *testing.T) {
	r := &Registry{}
	r.Register(fakeTest{"A", Procedural, Pass, ""})
	r.Register(fakeTest{"B", NegativeIE, Fail, ""})

	got := r.Run(context.Background(), Env{}, time.Second)
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d", len(got))
	}
	if got[0].ID != "A" || got[0].Verdict != Pass || got[0].Category != Procedural || got[0].SpecRef == "" {
		t.Errorf("result 0 not stamped from the test: %+v", got[0])
	}
	if got[1].ID != "B" || got[1].Verdict != Fail {
		t.Errorf("result 1 wrong: %+v", got[1])
	}
}

func TestRegistryFiltersByCategory(t *testing.T) {
	r := &Registry{}
	r.Register(fakeTest{"A", Procedural, Pass, ""})
	r.Register(fakeTest{"B", NegativeIE, Pass, ""})

	only := r.Run(context.Background(), Env{}, time.Second, NegativeIE)
	if len(only) != 1 || only[0].ID != "B" {
		t.Fatalf("category filter failed: %+v", only)
	}
}

func TestBuiltinsRegistered(t *testing.T) {
	if len(NewRegistry().Tests()) == 0 {
		t.Fatal("no built-in conformance tests registered")
	}
}

// A DEVIATION fails the default gate and not the spec one; a FAIL fails
// both.
func TestDeviationGatesByPolicy(t *testing.T) {
	r := &Registry{}
	r.Register(fakeTest{"A", Procedural, Pass, ""})
	r.Register(fakeTest{"B", GTPU, Deviation, Should})
	sum := Summarize(r.Run(context.Background(), Env{}, time.Second))
	if sum.Deviation != 1 {
		t.Errorf("want 1 deviation, got %d", sum.Deviation)
	}
	if sum.OK() {
		t.Error("a DEVIATION must fail the default gate")
	}
	if sum.Gate = GateSpec; !sum.OK() {
		t.Error("a DEVIATION must not fail the spec gate")
	}

	r.Register(fakeTest{"C", NegativeIE, Fail, ""})
	if Summarize(r.Run(context.Background(), Env{}, time.Second)).OK() {
		t.Error("a FAIL must fail the gate")
	}
}

// deadlineTest records how long runOne gave it, and optionally asks for a
// budget.
type deadlineTest struct {
	provTest
	got *time.Duration
}

func (d deadlineTest) Run(ctx context.Context, _ Env) Result {
	if dl, ok := ctx.Deadline(); ok {
		*d.got = time.Until(dl)
	}
	return Result{Verdict: Pass, Outcome: verdictOutcomeID(Pass)}
}

type budgetedTest struct {
	deadlineTest
	b time.Duration
}

func (b budgetedTest) Budget() time.Duration { return b.b }

// A check that asks for a budget above the per-test timeout gets it, and the
// detail says so; one asking for less, or not asking, gets the per-test
// timeout and no note.
func TestRunOneBudget(t *testing.T) {
	const perTest = 200 * time.Millisecond
	base := provTest{id: "X", refs: []SpecRef{goodRef()}}
	tests := []struct {
		name       string
		budget     time.Duration // 0 = no Budgeter
		want       time.Duration
		wantRaised bool
	}{
		{name: "no budgeter", want: perTest},
		{name: "budget below the per-test timeout", budget: 50 * time.Millisecond, want: perTest},
		{name: "budget above the per-test timeout", budget: 2 * time.Second, want: 2 * time.Second, wantRaised: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got time.Duration
			var c Test = deadlineTest{provTest: base, got: &got}
			if tc.budget > 0 {
				c = budgetedTest{deadlineTest: deadlineTest{provTest: base, got: &got}, b: tc.budget}
			}
			res := runOne(context.Background(), c, Env{}, perTest)
			if got > tc.want || got < tc.want-100*time.Millisecond {
				t.Errorf("check was given %v, want about %v", got, tc.want)
			}
			raised := strings.Contains(res.Detail, "per-test timeout raised to "+tc.want.String()+" for this check")
			if raised != tc.wantRaised {
				t.Errorf("detail %q: raised note %v, want %v", res.Detail, raised, tc.wantRaised)
			}
		})
	}
}

// gnbIDTest reports the gNB ID its run was given.
type gnbIDTest struct {
	fakeTest
	got *uint32
}

func (g gnbIDTest) Run(_ context.Context, env Env) Result {
	*g.got = env.GNB.ID
	return Result{Verdict: Pass}
}

// Only narrows Select and a run to the named checks, whatever the case of
// the IDs, on top of the category filter; the checks keep the gNB IDs of a
// full run; an unknown ID is an error that changes nothing; no IDs lifts
// the restriction.
func TestRegistryOnly(t *testing.T) {
	var full, narrowed uint32
	r := &Registry{}
	r.Register(fakeTest{"A-ONE", Procedural, Pass, ""})
	r.Register(fakeTest{"B-TWO", NegativeIE, Pass, ""})
	r.Register(gnbIDTest{fakeTest{"C-THREE", Procedural, Pass, ""}, &full})
	ids := func(tests []Test) string {
		var out []string
		for _, t := range tests {
			out = append(out, t.ID())
		}
		return strings.Join(out, ",")
	}
	r.Run(context.Background(), Env{}, time.Second)

	if err := r.Only("c-three", " B-TWO "); err != nil {
		t.Fatal(err)
	}
	if got := ids(r.Select()); got != "B-TWO,C-THREE" {
		t.Errorf("Select = %s, want B-TWO,C-THREE in registry order", got)
	}
	if got := ids(r.Select(Procedural)); got != "C-THREE" {
		t.Errorf("Select(procedural) = %s, want C-THREE", got)
	}
	r.tests[2] = gnbIDTest{fakeTest{"C-THREE", Procedural, Pass, ""}, &narrowed}
	var total int
	res := r.Run(context.Background(), Env{}, time.Second)
	r.RunStream(context.Background(), Env{}, time.Second, func(_ Result, _, n int) { total = n })
	if len(res) != 2 || res[0].ID != "B-TWO" || res[1].ID != "C-THREE" || total != 2 {
		t.Errorf("ran %d results (total %d), want B-TWO and C-THREE", len(res), total)
	}
	if narrowed != full {
		t.Errorf("C-THREE ran with gNB ID %#x when selected alone, %#x in the full run", narrowed, full)
	}

	if err := r.Only("B-TWO", "NO-SUCH"); err == nil || err.Error() != `unknown check "NO-SUCH"` {
		t.Errorf("Only with an unknown ID: %v", err)
	}
	if got := ids(r.Select()); got != "B-TWO,C-THREE" {
		t.Errorf("after a refused Only, Select = %s", got)
	}
	if err := r.Only(); err != nil || ids(r.Select()) != "A-ONE,B-TWO,C-THREE" {
		t.Errorf("Only() = %v, Select = %s; want every check", err, ids(r.Select()))
	}
}
