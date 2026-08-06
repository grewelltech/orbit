package engine

import (
	"testing"
	"time"
)

func TestAttachProcedureDurationsSplitsRatherThanDuplicates(t *testing.T) {
	// The regression: pdu_session was recorded as the whole attach, so the two
	// reported an identical distribution and the split was unobservable.
	m := attachProcedureDurations(1000*time.Millisecond, 650*time.Millisecond, true)

	if got := m[ProcedureAttach]; got != 1000*time.Millisecond {
		t.Errorf("attach = %v, want the full 1s", got)
	}
	if got := m[ProcedureRegistration]; got != 650*time.Millisecond {
		t.Errorf("registration = %v, want 650ms", got)
	}
	if got := m[ProcedurePDUSession]; got != 350*time.Millisecond {
		t.Errorf("pdu_session = %v, want the 350ms remainder", got)
	}
	if m[ProcedurePDUSession] == m[ProcedureAttach] {
		t.Error("pdu_session equals attach: one measurement written down twice, not two measurements")
	}
	if sum := m[ProcedureRegistration] + m[ProcedurePDUSession]; sum != m[ProcedureAttach] {
		t.Errorf("halves sum to %v but attach is %v: the split must account for the whole",
			sum, m[ProcedureAttach])
	}
}

func TestAttachProcedureDurationsWithoutSession(t *testing.T) {
	m := attachProcedureDurations(700*time.Millisecond, 700*time.Millisecond, false)
	if _, ok := m[ProcedurePDUSession]; ok {
		t.Error("a registration-only attach must not report a pdu_session")
	}
	if m[ProcedureRegistration] != 700*time.Millisecond {
		t.Errorf("registration = %v, want 700ms", m[ProcedureRegistration])
	}
}

func TestAttachProcedureDurationsWithoutRegisteredEvent(t *testing.T) {
	// REGISTERED never observed: there is no split to report, and deriving one
	// from the total would recreate the same conflation.
	m := attachProcedureDurations(900*time.Millisecond, 0, true)
	if len(m) != 1 || m[ProcedureAttach] != 900*time.Millisecond {
		t.Errorf("want only the attach total, got %v", m)
	}
}

func TestAttachProcedureDurationsIgnoresNonPositiveRemainder(t *testing.T) {
	// Clock granularity can make regDur == total. A zero or negative remainder
	// is not a measurement, and RecordProcedure would drop it anyway.
	m := attachProcedureDurations(500*time.Millisecond, 500*time.Millisecond, true)
	if _, ok := m[ProcedurePDUSession]; ok {
		t.Errorf("want no pdu_session for a zero remainder, got %v", m[ProcedurePDUSession])
	}
}
