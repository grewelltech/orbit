package engine

import "fmt"

// EventVerbosity selects how much of a run's activity reaches the event stream.
//
// The stream is a bounded ring (ADR-0006), so emission is a budget, not a log
// level: a 1000-UE fleet emitting every lifecycle transition would push ~6000
// routine successes through a 500-event ring and evict the handful of failures
// that were the reason to watch it. The default therefore spends the budget on
// what is rare and diagnostic, and rolls the routine into milestones.
type EventVerbosity string

const (
	// EventsNormal emits failures, mobility outcomes and per-cohort traffic
	// milestones per occurrence, and aggregates routine per-UE progress into
	// phase milestones ("attach complete: 99/100 in 12.4s").
	EventsNormal EventVerbosity = "normal"
	// EventsVerbose additionally emits every UE lifecycle transition. Intended
	// for debugging a specific failure on a small run; it also widens the run's
	// ring (see RingCap) so the detail is retained rather than instantly
	// evicted by its own volume.
	EventsVerbose EventVerbosity = "verbose"
)

// VerboseRunEventCap is the ring size for a verbose run. Roughly six lifecycle
// transitions per UE means this holds a few hundred UEs' full detail — enough
// for the debugging runs verbose exists for, without letting a population soak
// pin an unbounded log in memory.
const VerboseRunEventCap = 10_000

// ParseEventVerbosity maps a caller-supplied value onto a verbosity, defaulting
// to EventsNormal for the empty string. An unknown value is an error rather
// than a silent downgrade: an operator who asked for detail and got the default
// would read the resulting quiet stream as "nothing happened".
func ParseEventVerbosity(s string) (EventVerbosity, error) {
	switch EventVerbosity(s) {
	case "":
		return EventsNormal, nil
	case EventsNormal:
		return EventsNormal, nil
	case EventsVerbose:
		return EventsVerbose, nil
	default:
		return "", fmt.Errorf("unknown event verbosity %q (want %q or %q)", s, EventsNormal, EventsVerbose)
	}
}

// RingCap is the event-ring capacity this verbosity needs.
func (v EventVerbosity) RingCap() int {
	if v == EventsVerbose {
		return VerboseRunEventCap
	}
	return DefaultRunEventCap
}

// Event kinds. They partition a run's activity by what an operator would be
// looking for, so the stream can be scanned or filtered by concern.
const (
	EventKindRun      = "RUN"      // lifecycle of the run itself
	EventKindAttach   = "ATTACH"   // registration / PDU session establishment
	EventKindMobility = "MOBILITY" // handover and path switch
	EventKindTraffic  = "TRAFFIC"  // app cohorts and synthetic flows
	EventKindDataPath = "DATAPATH" // user-plane disruptions
)

// RunEvents applies a run's emission policy. A nil *RunEvents is usable and
// discards everything, so execution paths need no guard at each call site (the
// CLI's in-process fleet run has no subscriber).
type RunEvents struct {
	emit    RunEventFunc
	verbose bool
}

// NewRunEvents binds an emitter and verbosity. A nil emit yields a nil policy,
// which discards — what the local `orbit run <fleet>` path wants.
func NewRunEvents(emit RunEventFunc, v EventVerbosity) *RunEvents {
	if emit == nil {
		return nil
	}
	return &RunEvents{emit: emit, verbose: v == EventsVerbose}
}

func (r *RunEvents) send(severity, kind, supi, message string) {
	if r == nil || r.emit == nil {
		return
	}
	r.emit(severity, kind, supi, message)
}

// Milestone reports a phase boundary — the aggregated stand-in for the routine
// per-UE successes that normal verbosity deliberately does not emit.
func (r *RunEvents) Milestone(kind, message string) {
	r.send("info", kind, "", message)
}

// Failure reports one UE's failure. Always emitted, at every verbosity: these
// are rare, they are the signal, and the budget exists to protect them.
func (r *RunEvents) Failure(kind, supi, message string) {
	r.send("error", kind, supi, message)
}

// UEState reports one routine lifecycle transition. Suppressed at normal
// verbosity — this is the volume the ring cannot afford at population scale.
func (r *RunEvents) UEState(supi, state, detail string) {
	if r == nil || !r.verbose {
		return
	}
	msg := state
	if detail != "" {
		msg = state + " — " + detail
	}
	r.send("info", EventKindAttach, supi, msg)
}

// Mobility reports a handover outcome. Always emitted: mobility is bounded by
// the mobile-UE count rather than the population, and a handover is exactly
// the kind of discrete occurrence the stream exists for.
func (r *RunEvents) Mobility(supi, state, detail string) {
	severity := "info"
	if state == StateHandoverFailed {
		severity = "error"
	}
	msg := state
	if detail != "" {
		msg = state + " — " + detail
	}
	r.send(severity, EventKindMobility, supi, msg)
}

// DataPath reports a user-plane disruption (a TCP reset across a handover, an
// End Marker). Degraded rather than failed, so it warns.
func (r *RunEvents) DataPath(supi, state, detail string) {
	msg := state
	if detail != "" {
		msg = state + " — " + detail
	}
	r.send("warn", EventKindDataPath, supi, msg)
}

// Traffic reports a cohort-level start or stop — one event per cohort, not per
// member, so a 500-UE cohort costs one line rather than 500.
func (r *RunEvents) Traffic(message string) {
	r.send("info", EventKindTraffic, "", message)
}

// stateEventSink returns the func the attach path publishes StateEvents to,
// routing each onto the policy by what it means. Returns nil when there is no
// policy, so Attach keeps its existing "no emitter" fast path.
func (r *RunEvents) stateEventSink() func(StateEvent) {
	if r == nil {
		return nil
	}
	return func(ev StateEvent) {
		switch ev.State {
		case StateHandoverStarted, StateHandoverComplete, StateHandoverFailed, StatePathSwitchComplete:
			r.Mobility(ev.SUPI, ev.State, ev.Detail)
		case StateTCPConnsReset:
			r.DataPath(ev.SUPI, ev.State, ev.Detail)
		default:
			r.UEState(ev.SUPI, ev.State, ev.Detail)
		}
	}
}
