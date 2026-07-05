package load

import (
	"testing"
	"time"
)

func sampleReport() Report {
	return Report{
		Attempted: 100, Succeeded: 99, Failed: 1,
		Latencies: map[string]Stats{
			"registration": {Count: 99, P50: 40 * time.Millisecond, P99: 120 * time.Millisecond, P999: 200 * time.Millisecond, Max: 250 * time.Millisecond},
		},
	}
}

func TestSLOPasses(t *testing.T) {
	slo := SLO{
		MinSuccessRate: 0.95,
		Latency:        map[string]LatencyBound{"registration": {P99: 150 * time.Millisecond}},
	}
	v := slo.Evaluate(sampleReport())
	if !v.Pass {
		t.Fatalf("expected pass, got %+v", v.Checks)
	}
}

func TestSLOFailsOnSuccessRate(t *testing.T) {
	slo := SLO{MinSuccessRate: 0.999}
	v := slo.Evaluate(sampleReport())
	if v.Pass {
		t.Fatal("expected fail on success rate")
	}
	if v.Checks[0].Name != "success_rate" || v.Checks[0].Pass {
		t.Fatalf("unexpected checks: %+v", v.Checks)
	}
}

func TestSLOFailsOnLatency(t *testing.T) {
	slo := SLO{Latency: map[string]LatencyBound{"registration": {P99: 100 * time.Millisecond}}}
	v := slo.Evaluate(sampleReport()) // P99 is 120ms > 100ms
	if v.Pass {
		t.Fatal("expected fail on registration.p99")
	}
}

func TestSLOFailsOnMissingMetric(t *testing.T) {
	slo := SLO{Latency: map[string]LatencyBound{"pdu_session": {P99: time.Second}}}
	v := slo.Evaluate(sampleReport())
	if v.Pass {
		t.Fatal("expected fail when the procedure has no samples")
	}
}

func TestSLOEmptyAssertsNothing(t *testing.T) {
	if !(SLO{}).Empty() {
		t.Fatal("zero SLO should be Empty")
	}
	if v := (SLO{}).Evaluate(sampleReport()); !v.Pass || len(v.Checks) != 0 {
		t.Fatalf("empty SLO should pass with no checks, got %+v", v)
	}
}
