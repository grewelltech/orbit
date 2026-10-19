package server

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
)

// The latency response carries the engine's counts, the probes that never
// left with the first send error, why the run stopped, and the elapsed time
// and rate, durations in milliseconds.
func TestLatencyResponse(t *testing.T) {
	got := latencyResponse(&engine.LatencyStats{
		Sent: 2913, Received: 2912, Lost: 1, NotSent: 87, LossPct: 0.034,
		Min: 2100 * time.Microsecond, Max: 9400 * time.Microsecond, Mean: 2800 * time.Microsecond,
		StdDev: time.Millisecond, Jitter: 400 * time.Microsecond,
		NotSentErr: "no open N3 data path", Stopped: "no open N3 data path",
		Elapsed: 37400 * time.Millisecond, Rate: 77.9,
	})
	want := &orbitv1.LatencyResponse{
		Sent: 2913, Received: 2912, Lost: 1, LossPct: 0.034,
		MinMs: 2.1, MeanMs: 2.8, MaxMs: 9.4, JitterMs: 0.4,
		NotSent: 87, NotSentError: "no open N3 data path", Stopped: "no open N3 data path",
		ElapsedMs: 37400, RatePerS: 77.9,
	}
	if !proto.Equal(got, want) {
		t.Errorf("latencyResponse =\n%v\nwant\n%v", got, want)
	}
}
