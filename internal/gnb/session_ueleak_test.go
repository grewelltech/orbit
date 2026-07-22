package gnb

import "testing"

// NewUE registers a demux inbox and Close removes it, so a handover that
// allocates a target handle and closes the one it replaces keeps the session's
// lane count bounded. A steadily growing NumUEs would be the leak that
// fleetHandover's handle management prevents.
func TestSessionUELaneLifecycle(t *testing.T) {
	s := &Session{
		streams: 1,
		inboxes: make(map[int64]chan []byte),
		closed:  make(chan struct{}),
	}
	if s.NumUEs() != 0 {
		t.Fatalf("fresh session has %d lanes, want 0", s.NumUEs())
	}

	a, ranA := s.NewUE()
	b, ranB := s.NewUE()
	if ranA == ranB {
		t.Errorf("RAN-UE-NGAP-IDs collided: %d", ranA)
	}
	if s.NumUEs() != 2 {
		t.Fatalf("after 2 NewUE, lanes = %d, want 2", s.NumUEs())
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if s.NumUEs() != 1 {
		t.Errorf("after closing one, lanes = %d, want 1", s.NumUEs())
	}

	// Simulate the handover churn fleetHandover drives: repeatedly allocate a
	// new lane and close the previous one. The count must stay flat, not grow.
	prev := b
	for i := 0; i < 100; i++ {
		next, _ := s.NewUE()
		_ = prev.Close()
		prev = next
	}
	if s.NumUEs() != 1 {
		t.Errorf("after 100 allocate-and-release cycles, lanes = %d, want 1 (leak)", s.NumUEs())
	}

	_ = prev.Close()
	// Idempotent: closing twice does not underflow or panic.
	_ = prev.Close()
	if s.NumUEs() != 0 {
		t.Errorf("after releasing all, lanes = %d, want 0", s.NumUEs())
	}
}
