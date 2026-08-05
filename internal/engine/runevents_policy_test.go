package engine

import (
	"fmt"
	"strings"
	"testing"
)

// captured is one emitted event, flattened for assertions.
type captured struct{ severity, kind, supi, message string }

func capture() (*[]captured, RunEventFunc) {
	var got []captured
	return &got, func(severity, kind, supi, message string) {
		got = append(got, captured{severity, kind, supi, message})
	}
}

// At normal verbosity the routine per-UE lifecycle is suppressed: that volume
// is what would evict the failures the stream exists to show.
func TestRunEventsNormalSuppressesRoutineLifecycle(t *testing.T) {
	got, emit := capture()
	ev := NewRunEvents(emit, EventsNormal)

	ev.UEState("001010100007500", StateRegistering, "sent Registration Request")
	ev.UEState("001010100007500", StateRegistered, "UE is 5GMM-REGISTERED")
	if len(*got) != 0 {
		t.Fatalf("normal verbosity emitted %d lifecycle events, want 0: %+v", len(*got), *got)
	}

	// The things that are rare or diagnostic still come through.
	ev.Failure(EventKindAttach, "001010100007501", "auth failed")
	ev.Mobility("001010100007502", StateHandoverComplete, "gnb-1 → gnb-2")
	ev.DataPath("001010100007503", StateTCPConnsReset, "3 conns")
	ev.Traffic("cohort web-real started")
	ev.Milestone(EventKindAttach, "attach complete: 6/6")
	if len(*got) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(*got), *got)
	}
	for i, want := range []string{"error", "info", "warn", "info", "info"} {
		if (*got)[i].severity != want {
			t.Errorf("event %d severity = %q, want %q (%+v)", i, (*got)[i].severity, want, (*got)[i])
		}
	}
}

// Verbose adds the per-UE transitions without changing anything else.
func TestRunEventsVerboseAddsLifecycle(t *testing.T) {
	got, emit := capture()
	ev := NewRunEvents(emit, EventsVerbose)

	ev.UEState("001010100007500", StateRegistering, "sent Registration Request")
	if len(*got) != 1 {
		t.Fatalf("verbose emitted %d lifecycle events, want 1", len(*got))
	}
	e := (*got)[0]
	if e.kind != EventKindAttach || e.supi != "001010100007500" {
		t.Errorf("kind/supi = %q/%q, want %q/%q", e.kind, e.supi, EventKindAttach, "001010100007500")
	}
	if !strings.Contains(e.message, StateRegistering) || !strings.Contains(e.message, "Registration Request") {
		t.Errorf("message = %q, want the state and its detail", e.message)
	}
}

// The state sink routes each transition by what it means, so mobility and
// data-path disruptions are not suppressed along with routine lifecycle.
func TestRunEventsStateSinkRoutesByMeaning(t *testing.T) {
	got, emit := capture()
	sink := NewRunEvents(emit, EventsNormal).stateEventSink()

	sink(StateEvent{SUPI: "s1", State: StateRegistered})      // routine → suppressed
	sink(StateEvent{SUPI: "s2", State: StateHandoverStarted}) // mobility → kept
	sink(StateEvent{SUPI: "s3", State: StateHandoverFailed, Detail: "no target"})
	sink(StateEvent{SUPI: "s4", State: StateTCPConnsReset})      // data path → kept, warn
	sink(StateEvent{SUPI: "s5", State: StatePathSwitchComplete}) // mobility → kept

	if len(*got) != 4 {
		t.Fatalf("got %d events, want 4 (the routine one must be suppressed): %+v", len(*got), *got)
	}
	byKind := map[string]int{}
	for _, e := range *got {
		byKind[e.kind]++
	}
	if byKind[EventKindMobility] != 3 || byKind[EventKindDataPath] != 1 {
		t.Errorf("kinds = %v, want 3 mobility and 1 datapath", byKind)
	}
	// A failed handover is an error, not an info line to scroll past.
	for _, e := range *got {
		if strings.Contains(e.message, StateHandoverFailed) && e.severity != "error" {
			t.Errorf("failed handover severity = %q, want error", e.severity)
		}
	}
}

// The whole point of the policy: a population run's failures must still be in
// the ring after the routine traffic of a large attach phase.
func TestRunEventsFailuresSurviveAFloodInTheRing(t *testing.T) {
	ring := newEventRing(DefaultRunEventCap)
	ev := NewRunEvents(ring.emit, EventsNormal)

	// 1000 UEs attaching, six lifecycle transitions each, with one failure
	// early enough that a naive emitter would have evicted it long since.
	ev.Failure(EventKindAttach, "001010100007501", "auth failed: MAC mismatch")
	for i := 0; i < 1000; i++ {
		supi := fmt.Sprintf("00101010000%04d", i)
		for _, st := range []string{StateRegistering, StateAuthenticated, StateSecurityEstablished,
			StateRegistered, StateSessionActive, StateDeregistered} {
			ev.UEState(supi, st, "")
		}
	}
	ev.Milestone(EventKindAttach, "attach complete: 999/1000")

	events := ring.snapshotSince(0)
	if len(events) > DefaultRunEventCap {
		t.Fatalf("ring holds %d events, cap is %d", len(events), DefaultRunEventCap)
	}
	var sawFailure bool
	for _, e := range events {
		if e.Severity == "error" && strings.Contains(e.Message, "MAC mismatch") {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Errorf("the attach failure was evicted by routine traffic (%d events retained) — "+
			"the emission budget is not protecting what it exists to protect", len(events))
	}
}

// Verbose widens the ring, so the detail it emits is retained rather than
// immediately evicted by its own volume.
func TestEventVerbosityRingCap(t *testing.T) {
	if got := EventsNormal.RingCap(); got != DefaultRunEventCap {
		t.Errorf("normal ring cap = %d, want %d", got, DefaultRunEventCap)
	}
	if got := EventsVerbose.RingCap(); got != VerboseRunEventCap {
		t.Errorf("verbose ring cap = %d, want %d", got, VerboseRunEventCap)
	}
	if VerboseRunEventCap <= DefaultRunEventCap {
		t.Error("verbose must widen the ring, or it evicts its own detail")
	}
}

// An unknown verbosity is refused rather than silently downgraded: an operator
// who asked for detail and got the default would misread the quiet stream.
func TestParseEventVerbosity(t *testing.T) {
	for in, want := range map[string]EventVerbosity{"": EventsNormal, "normal": EventsNormal, "verbose": EventsVerbose} {
		got, err := ParseEventVerbosity(in)
		if err != nil || got != want {
			t.Errorf("ParseEventVerbosity(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
	if _, err := ParseEventVerbosity("loud"); err == nil {
		t.Error("ParseEventVerbosity(\"loud\") = nil error, want a refusal")
	}
}

// A nil policy is the CLI's in-process path: every method must be safe so the
// engine needs no guard at each call site.
func TestRunEventsNilIsSafe(t *testing.T) {
	var ev *RunEvents
	ev.Milestone(EventKindRun, "x")
	ev.Failure(EventKindAttach, "s", "x")
	ev.UEState("s", StateRegistered, "")
	ev.Mobility("s", StateHandoverComplete, "")
	ev.DataPath("s", StateTCPConnsReset, "")
	ev.Traffic("x")
	if sink := ev.stateEventSink(); sink != nil {
		t.Error("nil policy returned a non-nil state sink; Attach's no-emitter path is lost")
	}
	// A nil emitter yields a nil policy for the same reason.
	if got := NewRunEvents(nil, EventsVerbose); got != nil {
		t.Error("NewRunEvents(nil, …) should yield a nil policy")
	}
}

// The ring overwrites in place rather than shifting, so wraparound is where
// ordering, seq accounting and DroppedBefore would break if the index maths
// were wrong. Exercised well past one full lap.
func TestEventRingWrapsCorrectly(t *testing.T) {
	const cap = 8
	ring := newEventRing(cap)
	for i := 0; i < cap*3+3; i++ {
		ring.emit("info", EventKindAttach, "", fmt.Sprintf("event-%d", i))
	}
	total := uint64(cap*3 + 3)

	got := ring.snapshotSince(0)
	if len(got) != cap {
		t.Fatalf("retained %d events, want the cap %d", len(got), cap)
	}
	// Oldest-first, contiguous, and ending at the newest emitted.
	for i, ev := range got {
		wantSeq := total - uint64(cap) + uint64(i)
		if ev.Seq != wantSeq {
			t.Fatalf("event %d has seq %d, want %d (ordering broken across wraparound)", i, ev.Seq, wantSeq)
		}
		if want := fmt.Sprintf("event-%d", wantSeq); ev.Message != want {
			t.Errorf("event %d message = %q, want %q", i, ev.Message, want)
		}
	}

	// A subscriber resuming from an evicted point is told how many it missed.
	sub := ring.subscribeFrom(0)
	defer sub.Close()
	if want := total - uint64(cap); sub.DroppedBefore != want {
		t.Errorf("DroppedBefore = %d, want %d", sub.DroppedBefore, want)
	}
	if len(sub.Backlog) != cap {
		t.Errorf("backlog = %d events, want %d", len(sub.Backlog), cap)
	}

	// Resuming from a still-retained point loses nothing.
	from := total - 3
	sub2 := ring.subscribeFrom(from)
	defer sub2.Close()
	if sub2.DroppedBefore != 0 {
		t.Errorf("DroppedBefore = %d for a retained resume point, want 0", sub2.DroppedBefore)
	}
	if len(sub2.Backlog) != 3 {
		t.Errorf("backlog = %d, want 3", len(sub2.Backlog))
	}
}
