package engine

import (
	"context"
	"strings"
	"sync"
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

// A fleet's attach phase runs on the load controller: a Poisson curve with
// a seed paces it, concurrency bounds it, and the report carries the same
// attach figures a load run does (latency distribution, achieved rate).
func TestRunFleetAttachOnLoadController(t *testing.T) {
	amf, cfg, ki, opc := newBindTestAMF(t)
	addr := sctpFront(t, amf)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seen := 0
	var mu sync.Mutex
	rep, err := RunFleet(ctx, testLogger(), FleetRunSpec{
		AMFAddr: addr, GNBs: []FleetGNB{{Config: cfg}},
		BaseIMSI: "208930100007500", Count: 6, MCC: "208", MNC: "93", Ki: ki, OPc: opc,
		Rate:        load.NewPoisson(load.Constant{RPS: 200}, 7),
		Concurrency: 2,
		Observer:    observerFunc(func(load.Sample) { mu.Lock(); seen++; mu.Unlock() }),
	}, FleetBehaviors{}, FleetDeps{Stats: NewFleetLiveStats()})
	if err != nil {
		t.Fatalf("RunFleet: %v", err)
	}
	if rep.Attached != 6 || rep.AttachFailed != 0 {
		t.Fatalf("report = %+v, want 6 attached", rep)
	}
	if rep.AchievedRate <= 0 || rep.Latencies["registration"].Count != 6 {
		t.Errorf("attach figures: rate %v, latencies %+v; want a rate and 6 registrations", rep.AchievedRate, rep.Latencies)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != 6 {
		t.Errorf("the observer saw %d attaches, want 6", seen)
	}
}

// A fleet whose SUPI pool leaves the PLMN fails before any gNB dials.
func TestRunFleetValidatesSUPIRange(t *testing.T) {
	_, err := RunFleet(context.Background(), testLogger(), FleetRunSpec{
		AMFAddr: "127.0.0.1:1", GNBs: []FleetGNB{{}}, BaseIMSI: "001010000000001", Count: 2, MCC: "208", MNC: "93",
	}, FleetBehaviors{}, FleetDeps{})
	if err == nil || !strings.Contains(err.Error(), "base IMSI") {
		t.Fatalf("err = %v, want the SUPI range refused up front", err)
	}
}

type observerFunc func(load.Sample)

func (f observerFunc) Observe(s load.Sample) { f(s) }
