package engine

import (
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/meas"
)

// TestFleetUETriggersCrossesToNeighbour checks the geometry-driven mobility
// logic: a UE crossing from its serving cell to the neighbour on a two-cell grid
// produces exactly one handover trigger, aimed at the neighbour, fired mid-run
// (when the neighbour becomes stronger by the a3-offset) — not on a timer.
func TestFleetUETriggersCrossesToNeighbour(t *testing.T) {
	cells := []meas.Cell{
		{ID: 0, X: 0, Y: 0},
		{ID: 1, X: 1000, Y: 0},
	}
	base := time.Unix(0, 0)
	const dur = 30 * time.Second

	triggers := fleetUETriggers(cells, 0, dur, base)
	if len(triggers) != 1 {
		t.Fatalf("got %d triggers, want exactly 1", len(triggers))
	}
	if triggers[0].TargetCellID != 1 {
		t.Errorf("target cell = %d, want 1 (the neighbour)", triggers[0].TargetCellID)
	}
	if at := triggers[0].At.Sub(base); at <= 0 || at >= dur {
		t.Errorf("trigger at %v, want within (0, %v)", at, dur)
	}

	// A stationary grid of one cell yields no handover (nowhere to go).
	if got := fleetUETriggers(cells[:1], 0, dur, base); len(got) != 0 {
		t.Errorf("single-cell grid produced %d triggers, want 0", len(got))
	}
}
