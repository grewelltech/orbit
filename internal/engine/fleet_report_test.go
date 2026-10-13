package engine

import (
	"context"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/load"
)

// The fleet report keeps the attach phase's totals per gNB: what the attach
// milestone says, split by gNB, still there after every UE deregistered (the
// live snapshot's per-gNB counts fall to zero then, #104).
func TestRunFleetReportPerGNB(t *testing.T) {
	amf, cfg, ki, opc := newBindTestAMF(t)
	addr := sctpFront(t, amf)
	cfgB := cfg
	cfgB.ID, cfgB.Name = 2, ""
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	live := NewFleetLiveStats()
	rep, err := RunFleet(ctx, testLogger(), FleetRunSpec{
		AMFAddr:  addr,
		GNBs:     []FleetGNB{{Config: cfg}, {Config: cfgB}},
		BaseIMSI: "208930100007500", Count: 4, MCC: "208", MNC: "93", Ki: ki, OPc: opc,
	}, FleetBehaviors{}, FleetDeps{Stats: live})
	if err != nil {
		t.Fatalf("RunFleet: %v", err)
	}
	// Deregistered is not asserted: the mock AMF assigns no 5G-GUTI, so the
	// engine has nothing to deregister with.
	if rep.Attached != 4 || rep.AttachFailed != 0 {
		t.Fatalf("report = %+v, want 4 attached, 0 failed", rep)
	}
	want := map[string]load.GNBProgress{
		"orbit-gnb": {Attempted: 2, Succeeded: 2},
		"gnb-2":     {Attempted: 2, Succeeded: 2},
	}
	if len(rep.PerGNB) != len(want) {
		t.Fatalf("PerGNB = %v, want %v", rep.PerGNB, want)
	}
	for k, v := range want {
		if rep.PerGNB[k] != v {
			t.Errorf("PerGNB[%s] = %+v, want %+v", k, rep.PerGNB[k], v)
		}
	}
	// The live snapshot still says what it counts: nobody is attached now.
	if s := live.Snapshot(); s.Attached != 0 {
		t.Errorf("live snapshot attached = %d after deregistration, want 0", s.Attached)
	}
}

// notePerGNB counts attempts, successes and failures per gNB.
func TestFleetReportNotePerGNB(t *testing.T) {
	var r FleetReport
	for _, tc := range []struct {
		gnb string
		ok  bool
	}{{"a", true}, {"a", false}, {"b", true}, {"a", true}} {
		r.notePerGNB(tc.gnb, tc.ok)
	}
	if got := r.PerGNB["a"]; got != (load.GNBProgress{Attempted: 3, Succeeded: 2, Failed: 1}) {
		t.Errorf("a = %+v", got)
	}
	if got := r.PerGNB["b"]; got != (load.GNBProgress{Attempted: 1, Succeeded: 1}) {
		t.Errorf("b = %+v", got)
	}
}
