package scenario

import "testing"

func counts(assign []int, gnbs int) []int {
	out := make([]int, gnbs)
	for _, g := range assign {
		out[g]++
	}
	return out
}

func TestEvenDistributionIsRoundRobin(t *testing.T) {
	a, err := GNBAssignment(10, 3, DistributionEven, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 1, 2, 0, 1, 2, 0, 1, 2, 0}
	for i := range want {
		if a[i] != want[i] {
			t.Fatalf("assignment = %v, want %v", a, want)
		}
	}
}

func TestUnevenTotalsExactly(t *testing.T) {
	// The whole point: redistribute, never invent or drop. A fleet that
	// quietly attaches 998 of 1000 is worse than a visibly uneven one.
	for _, seed := range []int64{1, 2, 3, 99, 12345} {
		a, err := GNBAssignment(1000, 10, DistributionUneven, seed)
		if err != nil {
			t.Fatal(err)
		}
		if len(a) != 1000 {
			t.Fatalf("seed %d: got %d assignments, want 1000", seed, len(a))
		}
		sum := 0
		for _, c := range counts(a, 10) {
			sum += c
		}
		if sum != 1000 {
			t.Errorf("seed %d: counts sum to %d, want 1000", seed, sum)
		}
	}
}

func TestUnevenIsActuallyUneven(t *testing.T) {
	a, _ := GNBAssignment(1000, 10, DistributionUneven, 7)
	c := counts(a, 10)
	same := true
	for _, v := range c {
		if v != c[0] {
			same = false
		}
	}
	if same {
		t.Errorf("every gNB got %d UEs — that is the even distribution", c[0])
	}
	// …but not absurdly so: no empty cell, none holding a third of the fleet.
	for i, v := range c {
		if v == 0 {
			t.Errorf("gNB %d got no UEs", i)
		}
		if v > 333 {
			t.Errorf("gNB %d got %d of 1000 UEs, which is not a spread", i, v)
		}
	}
}

func TestUnevenSeedReproduces(t *testing.T) {
	// Needed to compare two builds against the same layout.
	a, _ := GNBAssignment(500, 8, DistributionUneven, 42)
	b, _ := GNBAssignment(500, 8, DistributionUneven, 42)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed produced different layouts at %d", i)
		}
	}
}

func TestUnevenInterleavesCells(t *testing.T) {
	// The attach phase is rate-limited in index order, so contiguous blocks
	// would attach one cell at a time instead of loading all of them.
	a, _ := GNBAssignment(200, 10, DistributionUneven, 5)
	seen := map[int]bool{}
	for _, g := range a[:10] {
		seen[g] = true
	}
	if len(seen) < 5 {
		t.Errorf("first 10 UEs touch only %d gNBs (%v) — cells are being filled in blocks", len(seen), a[:10])
	}
}

func TestDistributionEdgeCases(t *testing.T) {
	if _, err := GNBAssignment(10, 0, DistributionEven, 0); err == nil {
		t.Error("zero gNBs should be an error")
	}
	if _, err := GNBAssignment(10, 2, "sideways", 0); err == nil {
		t.Error("an unknown distribution should be rejected, not silently even")
	}
	a, err := GNBAssignment(0, 4, DistributionUneven, 1)
	if err != nil || len(a) != 0 {
		t.Errorf("zero UEs: got %v, %v", a, err)
	}
	// Fewer UEs than gNBs must still total exactly.
	a, _ = GNBAssignment(3, 10, DistributionUneven, 2)
	if len(a) != 3 {
		t.Errorf("got %d assignments for 3 UEs", len(a))
	}
}
