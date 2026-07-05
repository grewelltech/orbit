package meas

import (
	"math"
	"time"
)

// Cell is a synthesized transmitter at a planar position. RSRP is derived from
// a log-distance path-loss model — there is no radio, so these values model
// relative signal strength for the event logic, not a calibrated link budget.
type Cell struct {
	ID      int64
	X, Y    float64 // metres
	TxPower float64 // reference EIRP (dBm); default 46 if zero
}

// Path-loss model: PL(d) = refLoss + 10·η·log10(d/d0), RSRP = TxPower − PL
// (log-distance, TS 38.901-style). η ≈ 3.5 approximates an urban-macro slope.
const (
	refDist     = 1.0  // d0 (m)
	refLoss     = 32.0 // PL at d0 (dB)
	pathLossExp = 3.5  // η
	defaultEIRP = 46.0 // dBm
)

// RSRP returns the synthesized RSRP (dBm) this cell presents at (x, y).
func (c Cell) RSRP(x, y float64) float64 {
	tx := c.TxPower
	if tx == 0 {
		tx = defaultEIRP
	}
	d := math.Hypot(x-c.X, y-c.Y)
	if d < refDist {
		d = refDist
	}
	return tx - (refLoss + 10*pathLossExp*math.Log10(d/refDist))
}

// Track is a straight-line UE trajectory sampled at Step intervals.
type Track struct {
	StartX, StartY float64
	EndX, EndY     float64
	Duration       time.Duration
	Step           time.Duration
}

// at returns the UE position at the given elapsed time (clamped to the path).
func (t Track) at(elapsed time.Duration) (x, y float64) {
	f := float64(elapsed) / float64(t.Duration)
	if f < 0 {
		f = 0
	} else if f > 1 {
		f = 1
	}
	return t.StartX + (t.EndX-t.StartX)*f, t.StartY + (t.EndY-t.StartY)*f
}

// Scenario drives a single UE along a Track past a set of cells, evaluating a
// measurement event at each step. When the event fires, the UE hands over to
// the target cell (serving switches, the evaluator is reset against the new
// reference) — so a UE crossing several cells yields a sequence of handovers.
type Scenario struct {
	Cells   []Cell
	Serving int64 // initial serving cell ID
	Track   Track
	Event   Event
}

// Run executes the scenario from base time and returns the ordered handover
// triggers. It reflects the sim side end to end: synthesized mobility →
// RSRP → measurement events → handover targets. Executing each handover on a
// real core is the engine's job; this decides when and where.
func (s Scenario) Run(base time.Time) []Trigger {
	byID := make(map[int64]Cell, len(s.Cells))
	for _, c := range s.Cells {
		byID[c.ID] = c
	}
	serving := s.Serving
	ev := NewEvaluator(s.Event)

	var triggers []Trigger
	step := s.Track.Step
	if step <= 0 {
		step = 100 * time.Millisecond
	}
	for elapsed := time.Duration(0); elapsed <= s.Track.Duration; elapsed += step {
		x, y := s.Track.at(elapsed)
		sp := byID[serving].RSRP(x, y)
		neighbours := make([]Sample, 0, len(s.Cells)-1)
		for _, c := range s.Cells {
			if c.ID == serving {
				continue
			}
			neighbours = append(neighbours, Sample{CellID: c.ID, RSRP: c.RSRP(x, y)})
		}
		fired := ev.Observe(base.Add(elapsed), sp, neighbours)
		if len(fired) == 0 {
			continue
		}
		best := fired[0] // strongest target
		triggers = append(triggers, best)
		serving = best.TargetCellID // hand over
		ev = NewEvaluator(s.Event)  // re-anchor on the new serving cell
	}
	return triggers
}
