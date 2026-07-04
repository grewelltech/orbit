package meas

import (
	"testing"
	"time"
)

// A UE driving from beside cell 1 to beside cell 2 must hand over to cell 2
// once, roughly at the midpoint crossover.
func TestScenarioSingleHandover(t *testing.T) {
	s := Scenario{
		Cells: []Cell{
			{ID: 1, X: 0, Y: 0},
			{ID: 2, X: 1000, Y: 0},
		},
		Serving: 1,
		Track:   Track{StartX: 50, EndX: 950, Duration: 10 * time.Second, Step: 100 * time.Millisecond},
		Event:   EventA3{Offset: 2, Hysteresis: 1, TTT: 320 * time.Millisecond},
	}
	got := s.Run(base)
	if len(got) != 1 {
		t.Fatalf("expected exactly one handover, got %d: %+v", len(got), got)
	}
	if got[0].TargetCellID != 2 || got[0].Event != A3 {
		t.Fatalf("expected A3 handover to cell 2, got %+v", got[0])
	}
	// Crossover is at x=500 (t=5s from x=50..950 over 10s ≈ 5.0s + TTT).
	if d := got[0].At.Sub(base); d < 4500*time.Millisecond || d > 6000*time.Millisecond {
		t.Errorf("handover at %v, expected near the 5s midpoint", d)
	}
}

// A UE crossing three cells in a line hands over 1→2→3.
func TestScenarioChainedHandovers(t *testing.T) {
	s := Scenario{
		Cells: []Cell{
			{ID: 1, X: 0, Y: 0},
			{ID: 2, X: 1000, Y: 0},
			{ID: 3, X: 2000, Y: 0},
		},
		Serving: 1,
		Track:   Track{StartX: 0, EndX: 2000, Duration: 20 * time.Second, Step: 100 * time.Millisecond},
		Event:   EventA3{Offset: 2, Hysteresis: 1, TTT: 320 * time.Millisecond},
	}
	got := s.Run(base)
	if len(got) != 2 {
		t.Fatalf("expected two handovers (1→2→3), got %d: %+v", len(got), got)
	}
	if got[0].TargetCellID != 2 || got[1].TargetCellID != 3 {
		t.Fatalf("expected targets [2 3], got [%d %d]", got[0].TargetCellID, got[1].TargetCellID)
	}
}

// Standing still next to the serving cell must not trigger a handover.
func TestScenarioNoHandoverWhenStationary(t *testing.T) {
	s := Scenario{
		Cells:   []Cell{{ID: 1, X: 0, Y: 0}, {ID: 2, X: 1000, Y: 0}},
		Serving: 1,
		Track:   Track{StartX: 10, EndX: 10, Duration: 10 * time.Second, Step: 200 * time.Millisecond},
		Event:   EventA3{Offset: 2, Hysteresis: 1, TTT: 320 * time.Millisecond},
	}
	if got := s.Run(base); len(got) != 0 {
		t.Fatalf("stationary UE handed over: %+v", got)
	}
}
