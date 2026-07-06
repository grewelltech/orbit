package conformance

import (
	"context"
	"testing"
	"time"
)

type fakeTest struct {
	id      string
	cat     Category
	verdict Verdict
}

func (f fakeTest) ID() string         { return f.id }
func (f fakeTest) Category() Category { return f.cat }
func (f fakeTest) SpecRef() string    { return "TS 00.000 §0" }
func (f fakeTest) Run(context.Context, Env) Result {
	return Result{Verdict: f.verdict, Observed: "fake"}
}

func TestRegistryRunStampsAndOrders(t *testing.T) {
	r := &Registry{}
	r.Register(fakeTest{"A", Procedural, Pass})
	r.Register(fakeTest{"B", NegativeIE, Fail})

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
	r.Register(fakeTest{"A", Procedural, Pass})
	r.Register(fakeTest{"B", NegativeIE, Pass})

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
