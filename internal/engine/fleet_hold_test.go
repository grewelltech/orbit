package engine

import (
	"context"
	"testing"
	"time"
)

// A held fleet (a plan's until.duration) keeps its UEs for all of the
// duration even with no behaviours; an unheld one (a scenario's
// run.duration, which only bounds behaviours) deregisters once attached.
// A cancel ends a hold early.
func TestFleetHold(t *testing.T) {
	amf, cfg, ki, opc := newBindTestAMF(t)
	addr := sctpFront(t, amf)
	tests := []struct {
		name     string
		beh      FleetBehaviors
		cancelAt time.Duration // 0: never
		min, max time.Duration
	}{
		{name: "held, no behaviours", beh: FleetBehaviors{Duration: 2 * time.Second, Hold: true}, min: 2 * time.Second, max: 10 * time.Second},
		{name: "unheld, no behaviours", beh: FleetBehaviors{Duration: 2 * time.Second}, min: 0, max: 1500 * time.Millisecond},
		{name: "held, cancelled early", beh: FleetBehaviors{Duration: time.Minute, Hold: true}, cancelAt: time.Second,
			min: time.Second, max: 10 * time.Second},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg
			c.ID, c.Name = uint32(30+i), ""
			spec := FleetRunSpec{AMFAddr: addr, GNBs: []FleetGNB{{Config: c}},
				BaseIMSI: "208930100007500", Count: 2, MCC: "208", MNC: "93", Ki: ki, OPc: opc}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelAt > 0 {
				time.AfterFunc(tc.cancelAt, cancel)
			}
			start := time.Now()
			rep, err := RunFleet(ctx, testLogger(), spec, tc.beh, FleetDeps{Stats: NewFleetLiveStats()})
			took := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Attached != 2 {
				t.Errorf("attached %d, want 2", rep.Attached)
			}
			if took < tc.min || took > tc.max {
				t.Errorf("ran %v, want between %v and %v", took, tc.min, tc.max)
			}
		})
	}
}
