package engine

import (
	"sync"
	"testing"
)

// fakeCounters is a fixed set of tunnel totals, standing in for a UE's data
// path so the aggregation is testable without a live core.
type fakeCounters struct{ ulp, ulb, dlp, dlb uint64 }

func (f fakeCounters) n3Totals() (uint64, uint64, uint64, uint64) {
	return f.ulp, f.ulb, f.dlp, f.dlb
}

// Throughput is summed from the registered UEs at snapshot time, so a counter
// that advances after attach is reflected without the run reporting anything.
func TestFleetLiveStatsSumsTunnelCounters(t *testing.T) {
	live := NewFleetLiveStats()
	live.AttachOK("gnb-1", fakeCounters{ulp: 10, ulb: 1000, dlp: 20, dlb: 2000})
	live.AttachOK("gnb-1", fakeCounters{ulp: 5, ulb: 500, dlp: 6, dlb: 600})
	live.AttachOK("gnb-2", fakeCounters{ulp: 1, ulb: 100, dlp: 2, dlb: 200})
	live.AttachFailed()

	got := live.Snapshot()
	if got.Attached != 3 || got.AttachFailed != 1 {
		t.Errorf("attached/failed = %d/%d, want 3/1", got.Attached, got.AttachFailed)
	}
	if got.UplinkBytes != 1600 || got.DownlinkBytes != 2800 {
		t.Errorf("bytes ul/dl = %d/%d, want 1600/2800", got.UplinkBytes, got.DownlinkBytes)
	}
	if got.UplinkPackets != 16 || got.DownlinkPackets != 28 {
		t.Errorf("packets ul/dl = %d/%d, want 16/28", got.UplinkPackets, got.DownlinkPackets)
	}
	if got.PerGNB["gnb-1"] != 2 || got.PerGNB["gnb-2"] != 1 {
		t.Errorf("per-gNB = %v, want gnb-1:2 gnb-2:1", got.PerGNB)
	}
}

// A UE with no data path yet (app cohorts open theirs lazily) contributes
// zeros rather than breaking the aggregate.
func TestFleetLiveStatsNilSourceIsHarmless(t *testing.T) {
	live := NewFleetLiveStats()
	live.AttachOK("gnb-1", nil)
	live.AttachOK("gnb-1", fakeCounters{ulb: 42})
	got := live.Snapshot()
	if got.Attached != 2 {
		t.Errorf("attached = %d, want 2", got.Attached)
	}
	if got.UplinkBytes != 42 {
		t.Errorf("uplink bytes = %d, want 42", got.UplinkBytes)
	}
}

// A handover moves the UE's attribution, so the per-gNB spread tracks the
// population instead of freezing at its attach-time shape. The total attached
// count must not change.
func TestFleetLiveStatsHandoverMovesAttribution(t *testing.T) {
	live := NewFleetLiveStats()
	live.AttachOK("gnb-1", fakeCounters{})
	live.AttachOK("gnb-1", fakeCounters{})
	live.MovedGNB("gnb-1", "gnb-2")
	live.Handover(nil)

	got := live.Snapshot()
	if got.Attached != 2 {
		t.Errorf("attached = %d, want 2 (a handover does not change the population)", got.Attached)
	}
	if got.PerGNB["gnb-1"] != 1 || got.PerGNB["gnb-2"] != 1 {
		t.Errorf("per-gNB = %v, want one on each", got.PerGNB)
	}
	if got.Handovers != 1 || got.HandoverErrors != 0 {
		t.Errorf("handovers = %d ok / %d err, want 1/0", got.Handovers, got.HandoverErrors)
	}
}

// Deregistration drops the UE from the population but NOT from the byte
// totals: those bytes crossed the wire, and a total that fell as the fleet
// drained would misreport the run.
func TestFleetLiveStatsDetachKeepsBytes(t *testing.T) {
	live := NewFleetLiveStats()
	live.AttachOK("gnb-1", fakeCounters{ulb: 900, dlb: 100})
	live.Detached("gnb-1")

	got := live.Snapshot()
	if got.Attached != 0 {
		t.Errorf("attached = %d, want 0", got.Attached)
	}
	if got.PerGNB["gnb-1"] != 0 {
		t.Errorf("per-gNB gnb-1 = %d, want 0", got.PerGNB["gnb-1"])
	}
	if got.UplinkBytes != 900 || got.DownlinkBytes != 100 {
		t.Errorf("bytes ul/dl = %d/%d, want 900/100 retained after detach",
			got.UplinkBytes, got.DownlinkBytes)
	}
}

// A nil *FleetLiveStats is the CLI's in-process path: every mutator and
// Snapshot must be safe, so RunFleet needs no nil checks at each call site.
func TestFleetLiveStatsNilReceiver(t *testing.T) {
	var live *FleetLiveStats
	live.AttachOK("gnb-1", fakeCounters{ulb: 1})
	live.AttachFailed()
	live.Handover(nil)
	live.MovedGNB("a", "b")
	live.TrafficFlowStarted()
	live.AppSessions(3)
	live.Detached("gnb-1")
	if got := live.Snapshot(); got.Attached != 0 || got.UplinkBytes != 0 {
		t.Errorf("nil snapshot = %+v, want zero", got)
	}
}

// The attach phase writes from one goroutine per UE while telemetry snapshots
// concurrently; -race would catch an unguarded field.
func TestFleetLiveStatsConcurrent(t *testing.T) {
	live := NewFleetLiveStats()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			live.AttachOK("gnb-1", fakeCounters{ulb: 10})
			live.TrafficFlowStarted()
		}()
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = live.Snapshot()
		}()
	}
	wg.Wait()
	got := live.Snapshot()
	if got.Attached != 50 || got.TrafficFlows != 50 {
		t.Errorf("attached/flows = %d/%d, want 50/50", got.Attached, got.TrafficFlows)
	}
	if got.UplinkBytes != 500 {
		t.Errorf("uplink bytes = %d, want 500", got.UplinkBytes)
	}
}
