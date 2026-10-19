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
	return Result{Verdict: f.verdict, Observed: "fake"}
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

func TestDeviationDoesNotFailGate(t *testing.T) {
	r := &Registry{}
	r.Register(fakeTest{"A", Procedural, Pass, ""})
	r.Register(fakeTest{"B", GTPU, Deviation, Should})
	sum := Summarize(r.Run(context.Background(), Env{}, time.Second))
	if sum.Deviation != 1 {
		t.Errorf("want 1 deviation, got %d", sum.Deviation)
	}
	if !sum.OK() {
		t.Error("a DEVIATION must not fail the gate")
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
	return Result{Verdict: Pass}
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
