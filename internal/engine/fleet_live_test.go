package engine

import (
	"sync"
	"testing"
	"time"

	loomv1 "github.com/bgrewell/loom/api/loomv1"
)

// fakeCounters is a fixed set of tunnel totals, standing in for a UE's data
// path so the aggregation is testable without a live core.
type fakeCounters struct{ ulp, ulb, dlp, dlb uint64 }

func (f fakeCounters) Totals() (uint64, uint64, uint64, uint64) {
	return f.ulp, f.ulb, f.dlp, f.dlb
}

// Throughput is summed from the registered UEs at snapshot time, so a counter
// that advances after attach is reflected without the run reporting anything.
func TestFleetLiveStatsSumsTunnelCounters(t *testing.T) {
	live := NewFleetLiveStats()
	live.AttachOK("gnb-1", "supi-1", fakeCounters{ulp: 10, ulb: 1000, dlp: 20, dlb: 2000})
	live.AttachOK("gnb-1", "supi-2", fakeCounters{ulp: 5, ulb: 500, dlp: 6, dlb: 600})
	live.AttachOK("gnb-2", "supi-3", fakeCounters{ulp: 1, ulb: 100, dlp: 2, dlb: 200})
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
	live.AttachOK("gnb-1", "supi-4", nil)
	live.AttachOK("gnb-1", "supi-5", fakeCounters{ulb: 42})
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
	live.AttachOK("gnb-1", "supi-6", fakeCounters{})
	live.AttachOK("gnb-1", "supi-7", fakeCounters{})
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

// closableCounters models a real Session: once its data path closes it reports
// zeros. A fixed fake cannot catch the bug this guards, which is why the first
// version of this test passed while a live run's final report read 0 B.
type closableCounters struct {
	ulp, ulb, dlp, dlb uint64
	closed             bool
}

func (c *closableCounters) Totals() (uint64, uint64, uint64, uint64) {
	if c.closed {
		return 0, 0, 0, 0
	}
	return c.ulp, c.ulb, c.dlp, c.dlb
}

// A UE's traffic must survive its data path closing. RunFleet deregisters every
// UE at the end, so a total summed only from live sources empties at exactly
// the moment the run's final report is taken.
func TestFleetLiveStatsTotalsSurviveTeardown(t *testing.T) {
	live := NewFleetLiveStats()
	a := &closableCounters{ulp: 10, ulb: 1000, dlp: 20, dlb: 2000}
	b := &closableCounters{ulp: 5, ulb: 500, dlp: 6, dlb: 600}
	live.AttachOK("gnb-1", "supi-8", a)
	live.AttachOK("gnb-1", "supi-9", b)

	if got := live.Snapshot(); got.UplinkBytes != 1500 || got.DownlinkBytes != 2600 {
		t.Fatalf("live totals = %d/%d, want 1500/2600", got.UplinkBytes, got.DownlinkBytes)
	}

	// Teardown order as RunFleet does it: retire, then close.
	live.Detached("gnb-1", a)
	a.closed = true
	live.Detached("gnb-1", b)
	b.closed = true

	got := live.Snapshot()
	if got.UplinkBytes != 1500 || got.DownlinkBytes != 2600 {
		t.Errorf("totals after teardown = %d/%d, want 1500/2600 retained",
			got.UplinkBytes, got.DownlinkBytes)
	}
	if got.UplinkPackets != 15 || got.DownlinkPackets != 26 {
		t.Errorf("packets after teardown = %d/%d, want 15/26",
			got.UplinkPackets, got.DownlinkPackets)
	}
	if got.Attached != 0 {
		t.Errorf("attached = %d, want 0", got.Attached)
	}
}

// A retired source must not also be summed as live, or its traffic is counted
// twice.
func TestFleetLiveStatsDetachDoesNotDoubleCount(t *testing.T) {
	live := NewFleetLiveStats()
	c := &closableCounters{ulb: 700}
	live.AttachOK("gnb-1", "supi-10", c)
	live.Detached("gnb-1", c) // still open: naive code would count it twice

	if got := live.Snapshot(); got.UplinkBytes != 700 {
		t.Errorf("uplink bytes = %d, want 700 (retired source counted twice)", got.UplinkBytes)
	}
}

// Deregistration drops the UE from the population but NOT from the byte
// totals: those bytes crossed the wire, and a total that fell as the fleet
// drained would misreport the run.
func TestFleetLiveStatsDetachKeepsBytes(t *testing.T) {
	live := NewFleetLiveStats()
	src := &closableCounters{ulb: 900, dlb: 100}
	live.AttachOK("gnb-1", "supi-11", src)
	live.Detached("gnb-1", src)

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
	live.AttachOK("gnb-1", "supi-12", fakeCounters{ulb: 1})
	live.AttachFailed()
	live.Handover(nil)
	live.MovedGNB("a", "b")
	live.TrafficFlowStarted()
	live.AppSessions(3)
	live.Detached("gnb-1", nil)
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
			live.AttachOK("gnb-1", "supi-13", fakeCounters{ulb: 10})
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

// User-plane latency is absent until a probe runs, so a consumer can tell
// "not measured" from "measured as zero" — a 0 ms RTT would read as an
// instant data path.
func TestFleetLiveStatsUPLatencyAbsentUntilProbed(t *testing.T) {
	live := NewFleetLiveStats()
	if got := live.Snapshot(); got.HasUPLatency {
		t.Fatal("HasUPLatency true before any probe")
	}
	live.RecordUPLatency(4*time.Millisecond, false)
	got := live.Snapshot()
	if !got.HasUPLatency {
		t.Fatal("HasUPLatency false after a probe")
	}
	if got.UPProbes != 1 || got.UPProbesLost != 0 {
		t.Errorf("probes = %d sent / %d lost, want 1/0", got.UPProbes, got.UPProbesLost)
	}
	if got.UPLatency.P50 < 3*time.Millisecond || got.UPLatency.P50 > 5*time.Millisecond {
		t.Errorf("p50 = %v, want ~4ms", got.UPLatency.P50)
	}
}

// A lost probe counts as loss without entering the percentiles: a timeout is
// evidence about the path, not a latency, and folding it in would drag the
// tail toward the timeout value.
func TestFleetLiveStatsLostProbeStaysOutOfPercentiles(t *testing.T) {
	live := NewFleetLiveStats()
	for i := 0; i < 9; i++ {
		live.RecordUPLatency(2*time.Millisecond, false)
	}
	live.RecordUPLatency(0, true)

	got := live.Snapshot()
	if got.UPProbes != 10 || got.UPProbesLost != 1 {
		t.Errorf("probes = %d sent / %d lost, want 10/1", got.UPProbes, got.UPProbesLost)
	}
	if got.UPLatency.Count != 9 {
		t.Errorf("histogram count = %d, want 9 (the lost probe must not be a sample)", got.UPLatency.Count)
	}
	if got.UPLatency.Max > 3*time.Millisecond {
		t.Errorf("max = %v, want ~2ms (a timeout leaked into the percentiles)", got.UPLatency.Max)
	}
}

// An unwatched far end must carry its reason, never look like a far end that
// received nothing. The distinction is the whole point of a second observer:
// "not measured" and "measured zero" mean opposite things.
func TestFarEndAbsenceCarriesItsReason(t *testing.T) {
	var unwatched *farEndWatch
	if got := unwatched.snapshot(); got.Available || got.Reason == "" {
		t.Errorf("nil watch = %+v, want unavailable with a reason", got)
	}

	w := newFarEndWatch()
	if got := w.snapshot(); got.Available {
		t.Error("a fresh watch reports available before any sample")
	}
	w.unavailable("per-UE far ends are not watched")
	got := w.snapshot()
	if got.Available || got.Reason != "per-UE far ends are not watched" {
		t.Errorf("got %+v, want the recorded reason", got)
	}
	if got.Bytes != 0 {
		t.Errorf("bytes = %d, want 0 for an unavailable view", got.Bytes)
	}
}

// The rate comes from the sample's own interval accounting — the far end
// measured it on its own clock, which is what makes it an independent witness
// rather than a restatement of our own timing.
func TestFarEndUsesTheAgentsOwnInterval(t *testing.T) {
	w := newFarEndWatch()
	w.observe(&loomv1.TelemetrySample{
		Bytes: 5_000_000, Packets: 4000,
		IntervalBytes: 1_000_000, IntervalNanos: int64(time.Second),
	})
	got := w.snapshot()
	if !got.Available {
		t.Fatal("a sample did not mark the view available")
	}
	if got.Bytes != 5_000_000 || got.Packets != 4000 {
		t.Errorf("cumulative = %d B / %d pkts, want 5000000/4000", got.Bytes, got.Packets)
	}
	// 1 MB in 1s = 8 Mbps.
	if got.BitsPerSec != 8_000_000 {
		t.Errorf("rate = %v bps, want 8000000 (from the sample's own interval)", got.BitsPerSec)
	}

	// A sample with no interval leaves the previous rate rather than dividing
	// by zero.
	w.observe(&loomv1.TelemetrySample{Bytes: 6_000_000})
	if got := w.snapshot(); got.Bytes != 6_000_000 {
		t.Errorf("bytes = %d, want the updated 6000000", got.Bytes)
	}
}
