package scenario

import (
	"fmt"
	"math/rand"
	"sort"
)

// Distribution names supported by fleet.distribution.
const (
	DistributionEven   = "even"
	DistributionUneven = "uneven"
)

// defaultUnevenSpread is how far a gNB's share may stray from an equal one.
// 0.4 gives a visibly ragged population — roughly 60–140 UEs per gNB at 1000
// across 10 — without producing a cell that is empty or holds half the fleet.
const defaultUnevenSpread = 0.4

// GNBAssignment returns the serving gNB index for each of count UEs.
//
// "even" is round-robin, which is what a fleet run did unconditionally before
// this existed: exactly count/gnbs per cell, every time.
//
// "uneven" gives each gNB a random share around an equal one. A perfectly even
// population is a property of the generator, not of anything real — cells do
// not carry identical loads — and an even split hides whatever only shows up
// when one cell is busier than its neighbours. The totals still add to exactly
// count: this redistributes UEs, it does not invent or drop any.
//
// seed of 0 draws a fresh layout per run, which is what "non-deterministic"
// asks for; any other value reproduces a layout exactly, so a run can be
// repeated against the same distribution when comparing two builds.
func GNBAssignment(count, gnbs int, mode string, seed int64) ([]int, error) {
	if gnbs <= 0 {
		return nil, fmt.Errorf("distribution needs at least one gNB")
	}
	if count < 0 {
		return nil, fmt.Errorf("distribution needs a non-negative UE count, got %d", count)
	}
	switch mode {
	case "", DistributionEven:
		out := make([]int, count)
		for i := range out {
			out[i] = i % gnbs
		}
		return out, nil
	case DistributionUneven:
		return unevenAssignment(count, gnbs, seed), nil
	default:
		return nil, fmt.Errorf("fleet.distribution %q is not supported (%s, %s)",
			mode, DistributionEven, DistributionUneven)
	}
}

// unevenAssignment sizes each gNB by a random weight, then hands out UEs.
func unevenAssignment(count, gnbs int, seed int64) []int {
	if count == 0 {
		return []int{}
	}
	src := rand.New(rand.NewSource(seed)) //nolint:gosec // layout selection, not security
	if seed == 0 {
		// A fresh layout per run. rand.Int63 draws from the global source,
		// which is seeded randomly by the runtime.
		src = rand.New(rand.NewSource(rand.Int63())) //nolint:gosec // as above
	}

	weights := make([]float64, gnbs)
	var total float64
	for i := range weights {
		weights[i] = 1 - defaultUnevenSpread + src.Float64()*2*defaultUnevenSpread
		total += weights[i]
	}

	// Largest-remainder apportionment: floor every share, then hand the
	// leftover UEs to the largest fractional parts. Rounding each share
	// independently would not sum to count, and a fleet that quietly attaches
	// 998 of 1000 UEs is worse than one that is visibly uneven.
	type share struct {
		idx  int
		frac float64
	}
	counts := make([]int, gnbs)
	fracs := make([]share, gnbs)
	assigned := 0
	for i, w := range weights {
		exact := float64(count) * w / total
		counts[i] = int(exact)
		fracs[i] = share{idx: i, frac: exact - float64(counts[i])}
		assigned += counts[i]
	}
	sort.SliceStable(fracs, func(a, b int) bool { return fracs[a].frac > fracs[b].frac })
	for i := 0; assigned < count; i++ {
		counts[fracs[i%gnbs].idx]++
		assigned++
	}

	// Interleave rather than emitting each cell's UEs in one block. The attach
	// phase is rate-limited in index order, so contiguous blocks would attach
	// one cell at a time — every gNB should be taking traffic throughout.
	out := make([]int, 0, count)
	remaining := make([]int, gnbs)
	copy(remaining, counts)
	for len(out) < count {
		for g := 0; g < gnbs && len(out) < count; g++ {
			if remaining[g] > 0 {
				out = append(out, g)
				remaining[g]--
			}
		}
	}
	return out
}
