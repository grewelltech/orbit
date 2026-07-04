package meas

import (
	"sort"
	"time"
)

// Sample is a synthesized RSRP measurement of one cell at an instant.
type Sample struct {
	CellID int64
	RSRP   float64 // dBm
}

// Trigger is a fired measurement event: the neighbour whose criterion held for
// the time-to-trigger. For A3/A5 it is the handover target.
type Trigger struct {
	Event        Kind
	TargetCellID int64
	At           time.Time
	ServingRSRP  float64
	TargetRSRP   float64
}

// Evaluator applies one configured event to a stream of measurements for a
// single UE, honouring hysteresis (in the entering/leaving criteria) and the
// time-to-trigger: a neighbour must satisfy the entering criterion
// continuously for TTT before its event fires. It fires once per crossing;
// the neighbour must satisfy the leaving criterion before it can fire again.
type Evaluator struct {
	event Event
	// per-neighbour state
	enteringSince map[int64]time.Time // set while the entering criterion holds
	fired         map[int64]bool      // already reported this crossing
}

// NewEvaluator builds an Evaluator for one configured event.
func NewEvaluator(e Event) *Evaluator {
	return &Evaluator{
		event:         e,
		enteringSince: map[int64]time.Time{},
		fired:         map[int64]bool{},
	}
}

// Observe feeds the measurements at time t: the serving cell's RSRP and the
// neighbour cells' RSRP. It returns any neighbours whose event fired at t
// (entering criterion satisfied continuously for the time-to-trigger). When
// several fire at once they are ordered strongest-neighbour first.
func (ev *Evaluator) Observe(t time.Time, servingRSRP float64, neighbours []Sample) []Trigger {
	var out []Trigger
	seen := map[int64]bool{}
	ttt := ev.event.TimeToTrigger()

	for _, n := range neighbours {
		seen[n.CellID] = true
		switch {
		case ev.event.entering(servingRSRP, n.RSRP):
			start, tracking := ev.enteringSince[n.CellID]
			if !tracking {
				ev.enteringSince[n.CellID] = t
				start = t
			}
			if !ev.fired[n.CellID] && t.Sub(start) >= ttt {
				ev.fired[n.CellID] = true
				out = append(out, Trigger{
					Event: ev.event.Kind(), TargetCellID: n.CellID, At: t,
					ServingRSRP: servingRSRP, TargetRSRP: n.RSRP,
				})
			}
		case ev.event.leaving(servingRSRP, n.RSRP):
			// Criterion released: reset so a later re-entry can fire again.
			delete(ev.enteringSince, n.CellID)
			delete(ev.fired, n.CellID)
		default:
			// In the hysteresis band: neither entering nor leaving. Stop the
			// TTT timer (entering no longer continuously satisfied) but keep
			// the fired flag until the leaving criterion is met.
			delete(ev.enteringSince, n.CellID)
		}
	}

	// A neighbour that dropped out of the report entirely is no longer
	// entering; clear its timer but keep fired until it leaves explicitly.
	for id := range ev.enteringSince {
		if !seen[id] {
			delete(ev.enteringSince, id)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].TargetRSRP > out[j].TargetRSRP })
	return out
}
