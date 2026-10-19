package engine

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockupf"
)

// A latency run racing a deregistration stops when the UE's data path goes:
// it returns promptly with what it measured, the rest counted as not sent and
// Stopped saying why, instead of running out the clock one probe per spacing.
// The stub UPF answers no echoes, so every sent probe is lost; what matters
// is that the accounting adds up and the run ends.
func TestLatencyStopsWhenDeregistered(t *testing.T) {
	p := newMockPair(t, 48, mockupf.Options{}, false)
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	const supi = "208930100000048"
	p.register(t, m, supi)

	const probes = 1000 // about a minute at 50 ms per probe if it ran them all
	type out struct {
		res *LatencyStats
		err error
		at  time.Time
	}
	done := make(chan out, 1)
	go func() {
		res, err := m.Latency(context.Background(), supi, "8.8.8.8", probes, 10*time.Millisecond, 40*time.Millisecond)
		done <- out{res, err, time.Now()}
	}()
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The mock AMF assigns no 5G-GUTI, so the switch-off NAS step fails;
	// the data path is torn down before it, which is what this test needs.
	if err := m.Deregister(ctx, supi); err != nil && !strings.Contains(err.Error(), "no 5G-GUTI") {
		t.Fatalf("Deregister: %v", err)
	}
	if _, err := m.Status(supi); err == nil {
		t.Fatal("the UE is still registered after Deregister")
	}
	deregAt := time.Now()

	var o out
	select {
	case o = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("latency kept probing after the UE was deregistered")
	}
	if o.err != nil {
		t.Fatalf("Latency: %v", o.err)
	}
	if took := o.at.Sub(deregAt); took > time.Second {
		t.Errorf("latency returned %v after the deregistration, want promptly", took)
	}
	r := o.res
	if r.Stopped == "" || r.NotSentErr == "" {
		t.Errorf("Stopped = %q, NotSentErr = %q; want both to say the data path went", r.Stopped, r.NotSentErr)
	}
	if r.Sent == 0 || r.NotSent == 0 || r.Sent+r.NotSent != probes {
		t.Errorf("sent %d + not sent %d, want both nonzero and %d in all", r.Sent, r.NotSent, probes)
	}
	if r.Lost != r.Sent-r.Received || r.LossPct != float64(r.Lost)/float64(r.Sent)*100 {
		t.Errorf("lost %d (%.1f%%) of %d sent, %d received: loss is not over the probes sent", r.Lost, r.LossPct, r.Sent, r.Received)
	}
}
