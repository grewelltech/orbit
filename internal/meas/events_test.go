package meas

import (
	"testing"
	"time"
)

var base = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

// a3 with offset 3 dB, hysteresis 1 dB: vs serving -90 dBm, a neighbour
// enters when RSRP > -86, leaves when RSRP < -88, and sits in the hysteresis
// band in between.
func newA3(ttt time.Duration) EventA3 {
	return EventA3{Offset: 3, Hysteresis: 1, TTT: ttt}
}

func TestA3FiresAfterTimeToTrigger(t *testing.T) {
	ev := NewEvaluator(newA3(640 * time.Millisecond))
	// Entering at t0 but TTT not yet elapsed: no trigger.
	if got := ev.Observe(base, -90, []Sample{{CellID: 2, RSRP: -85}}); len(got) != 0 {
		t.Fatalf("fired before TTT: %+v", got)
	}
	if got := ev.Observe(base.Add(300*time.Millisecond), -90, []Sample{{CellID: 2, RSRP: -85}}); len(got) != 0 {
		t.Fatalf("fired before TTT at 300ms: %+v", got)
	}
	// TTT elapsed with the entering criterion continuously held: fires.
	got := ev.Observe(base.Add(640*time.Millisecond), -90, []Sample{{CellID: 2, RSRP: -85}})
	if len(got) != 1 || got[0].TargetCellID != 2 || got[0].Event != A3 {
		t.Fatalf("expected A3 trigger for cell 2, got %+v", got)
	}
	// Does not re-fire while still entering.
	if got := ev.Observe(base.Add(1*time.Second), -90, []Sample{{CellID: 2, RSRP: -85}}); len(got) != 0 {
		t.Fatalf("re-fired without leaving: %+v", got)
	}
}

func TestA3HysteresisBandDoesNotFire(t *testing.T) {
	ev := NewEvaluator(newA3(0))
	// -87 is inside the band (-88..-86): neither entering nor leaving.
	if got := ev.Observe(base, -90, []Sample{{CellID: 2, RSRP: -87}}); len(got) != 0 {
		t.Fatalf("fired inside hysteresis band: %+v", got)
	}
}

func TestA3ResetsAndRefiresAfterLeaving(t *testing.T) {
	ev := NewEvaluator(newA3(0)) // TTT 0: fires as soon as entering holds
	if got := ev.Observe(base, -90, []Sample{{CellID: 2, RSRP: -85}}); len(got) != 1 {
		t.Fatalf("expected first fire, got %+v", got)
	}
	// Neighbour drops below the leaving threshold: criterion released.
	if got := ev.Observe(base.Add(time.Second), -90, []Sample{{CellID: 2, RSRP: -95}}); len(got) != 0 {
		t.Fatalf("unexpected fire on leaving: %+v", got)
	}
	// Rises again: must fire a second time.
	if got := ev.Observe(base.Add(2*time.Second), -90, []Sample{{CellID: 2, RSRP: -85}}); len(got) != 1 {
		t.Fatalf("expected re-fire after leaving, got %+v", got)
	}
}

func TestA3StrongestNeighbourFirst(t *testing.T) {
	ev := NewEvaluator(newA3(0))
	got := ev.Observe(base, -95, []Sample{{CellID: 2, RSRP: -80}, {CellID: 3, RSRP: -70}})
	if len(got) != 2 || got[0].TargetCellID != 3 || got[1].TargetCellID != 2 {
		t.Fatalf("expected cell 3 (stronger) first, got %+v", got)
	}
}

func TestA4Threshold(t *testing.T) {
	ev := NewEvaluator(EventA4{Threshold: -90, Hysteresis: 2, TTT: 0})
	// -89 enters (Mn-Hys = -91 > -90? no). Need Mn-2 > -90 => Mn > -88.
	if got := ev.Observe(base, -100, []Sample{{CellID: 2, RSRP: -89}}); len(got) != 0 {
		t.Fatalf("A4 fired below threshold+hys: %+v", got)
	}
	if got := ev.Observe(base, -100, []Sample{{CellID: 2, RSRP: -85}}); len(got) != 1 {
		t.Fatalf("A4 should fire above threshold+hys, got %+v", got)
	}
}

func TestA5DualCondition(t *testing.T) {
	// Serving worse than -100 AND neighbour better than -80.
	ev := NewEvaluator(EventA5{Threshold1: -100, Threshold2: -80, Hysteresis: 0, TTT: 0})
	// Serving fine (-90 not < -100): no fire even with a strong neighbour.
	if got := ev.Observe(base, -90, []Sample{{CellID: 2, RSRP: -70}}); len(got) != 0 {
		t.Fatalf("A5 fired while serving still good: %+v", got)
	}
	// Serving degraded and neighbour strong: fires.
	if got := ev.Observe(base, -105, []Sample{{CellID: 2, RSRP: -70}}); len(got) != 1 {
		t.Fatalf("A5 should fire, got %+v", got)
	}
	// Serving degraded but neighbour weak: no fire.
	ev2 := NewEvaluator(EventA5{Threshold1: -100, Threshold2: -80, Hysteresis: 0, TTT: 0})
	if got := ev2.Observe(base, -105, []Sample{{CellID: 2, RSRP: -90}}); len(got) != 0 {
		t.Fatalf("A5 fired with weak neighbour: %+v", got)
	}
}
