package engine

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// newDrainSession builds the minimal appSession the drain contract needs: a
// correlator whose annotations are published back through publish, exactly as
// StartAppSession wires it.
func newDrainSession() *appSession {
	s := &appSession{
		id:   "app-drain",
		supi: appTestSUPI,
		cfg:  AppSessionConfig{App: "voip"},
		m:    NewManager(slog.New(slog.DiscardHandler)),
		subs: map[int]chan AppSample{},
	}
	s.corr = newCorrelator(nil, func(at time.Time, text string) {
		s.publish(AppSample{Time: at, TimeSource: "local", Event: AppEventAnnotation, Detail: text})
	})
	return s
}

// publish must hold the in-flight count until the sample has reached the
// correlator, not merely been recorded in s.events. The correlator's emit
// callback runs from inside observe, so reading the counter there observes
// the window finalize has to drain.
func TestPublishHoldsInflightThroughCorrelator(t *testing.T) {
	s := newDrainSession()

	var duringEmit int64
	s.corr = newCorrelator(nil, func(at time.Time, text string) {
		duringEmit = s.inflight.Load()
	})

	s.publish(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", corrBase))
	// Settling the window emits, which runs the callback above from within
	// observe -- i.e. from inside the publish that is still in flight.
	s.publish(mkEvent(StateHandoverStarted, "Xn handover gnb2 → gnb3", corrBase.Add(time.Second)))

	if duringEmit < 1 {
		t.Errorf("inflight during correlator emit = %d, want >= 1: publish released the "+
			"count before the sample reached the correlator", duringEmit)
	}
	if n := s.inflight.Load(); n != 0 {
		t.Errorf("inflight after publish returned = %d, want 0", n)
	}
}

// The regression: a sample already recorded as an event but not yet seen by
// the correlator must still make the annotation. drainPublishes is what holds
// flush back until it has. Deterministic -- the publisher keeps the count
// raised across a known delay, so an undrained flush would always lose the
// End Marker.
func TestDrainPublishesWaitsForCorrelatorBeforeFlush(t *testing.T) {
	s := newDrainSession()
	s.publish(mkEvent(StateHandoverStarted, "Xn handover gnb1 → gnb2", corrBase))

	var wg sync.WaitGroup
	wg.Add(1)
	// Stand in for a publish that has passed the s.mu section (the event is
	// recorded) and is descheduled before reaching s.corr.observe.
	s.inflight.Add(1)
	go func() {
		defer wg.Done()
		defer s.inflight.Add(-1)
		time.Sleep(5 * time.Millisecond)
		s.corr.observe(mkEvent(AppEventEndMarker,
			"GTP-U End Marker on gNB gnb1, TEID 0x202 (vacated pre-handover TEID: source path drained)",
			corrBase.Add(150*time.Microsecond)))
	}()

	s.drainPublishes()
	if n := s.inflight.Load(); n != 0 {
		t.Fatalf("drainPublishes returned with %d still in flight", n)
	}
	s.corr.flush(corrBase.Add(2 * time.Second))
	wg.Wait()

	joined := strings.Join(s.corr.list(), "\n")
	if !strings.Contains(joined, "XnHandover @") {
		t.Fatalf("no handover annotation: %q", s.corr.list())
	}
	if !strings.Contains(joined, "End Marker @") {
		t.Errorf("annotation lost the End Marker join: %q", s.corr.list())
	}
}

// drainPublishes must not hang teardown when a producer never finishes.
func TestDrainPublishesIsBounded(t *testing.T) {
	s := newDrainSession()
	s.inflight.Add(1) // never released
	defer s.inflight.Add(-1)

	start := time.Now()
	s.drainPublishes()
	elapsed := time.Since(start)

	if elapsed < publishDrainWait {
		t.Errorf("returned after %v, want at least the %v bound", elapsed, publishDrainWait)
	}
	if elapsed > 10*publishDrainWait {
		t.Errorf("returned after %v, far past the %v bound", elapsed, publishDrainWait)
	}
}
