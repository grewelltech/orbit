package engine

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
)

func quietManager() *Manager {
	return NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A session that has never moved reports no mobility phase, so a client can
// tell "has not moved" from "moved and succeeded".
func TestMobilityUnsetBeforeFirstHandover(t *testing.T) {
	s := &Session{SUPI: "001010000000001"}
	phase, at := s.Mobility()
	if phase != "" {
		t.Errorf("mobility phase = %q, want empty before the first handover", phase)
	}
	if !at.IsZero() {
		t.Errorf("mobility timestamp = %v, want the zero time", at)
	}
}

// publishMobility must retain the phase on the session, not only stream it.
// StateStream drops on a full subscriber buffer and has no replay, so a
// polling client can observe mobility only if it is retained here.
func TestPublishMobilityRetainsPhaseOnSession(t *testing.T) {
	m := quietManager()
	const supi = "001010000000001"
	m.sessions[supi] = &Session{SUPI: supi, Result: &AttachResult{}}

	before := time.Now()
	m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverComplete, Detail: "on target"})

	phase, at := m.sessions[supi].Mobility()
	if phase != StateHandoverComplete {
		t.Errorf("mobility phase = %q, want %q", phase, StateHandoverComplete)
	}
	if at.Before(before) {
		t.Errorf("mobility timestamp %v predates the event", at)
	}
}

// The registration and mobility axes are independent. This is the property
// issue #32 depends on: after a FAILED handover a polling client must be able
// to tell the UE apart from a healthy one, even though both report
// SESSION_ACTIVE.
func TestFailedHandoverIsDistinguishableWhileStateUnchanged(t *testing.T) {
	m := quietManager()
	const healthy, broken = "001010000000001", "001010000000002"
	for _, supi := range []string{healthy, broken} {
		sess := &Session{SUPI: supi, Result: &AttachResult{}}
		sess.setState(StateSessionActive)
		m.sessions[supi] = sess
	}

	m.publishMobility(StateEvent{SUPI: broken, State: StateHandoverFailed, Detail: "no HandoverCommand"})

	if got := m.sessions[broken].State(); got != StateSessionActive {
		t.Errorf("registration state = %q after a failed handover, want it unchanged at %q", got, StateSessionActive)
	}
	hPhase, _ := m.sessions[healthy].Mobility()
	bPhase, _ := m.sessions[broken].Mobility()
	if hPhase == bPhase {
		t.Fatalf("healthy and failed UEs are indistinguishable: both report mobility %q", bPhase)
	}
	if bPhase != StateHandoverFailed {
		t.Errorf("failed UE mobility = %q, want %q", bPhase, StateHandoverFailed)
	}
}

// A handover moves the serving cell, and ServingGNB must follow it — the
// question "where is this UE now?" has to survive the move.
func TestServingGNBFollowsTheMove(t *testing.T) {
	src := gnb.Config{ID: 0x42, Name: "orbit-gnb-src"}
	tgt := gnb.Config{ID: 0x43, Name: "orbit-gnb-tgt"}

	s := &Session{SUPI: "001010000000001"}
	s.setServingGNB(src)
	if got := s.ServingGNB(); got.ID != src.ID || got.Name != src.Name {
		t.Fatalf("serving gNB = %+v, want %+v", got, src)
	}

	s.setServingGNB(tgt)
	got := s.ServingGNB()
	if got.ID != tgt.ID || got.Name != tgt.Name {
		t.Errorf("serving gNB = %+v after the move, want %+v", got, tgt)
	}
}

// The observable session fields are read by API handlers while a handover
// mutates them. Manager.mu guards the session map, not its values, so the
// session carries its own lock. Run under -race.
func TestObservableSessionStateIsRaceFree(t *testing.T) {
	m := quietManager()
	const supi = "001010000000001"
	sess := &Session{SUPI: supi, Result: &AttachResult{}}
	sess.setState(StateSessionActive)
	m.sessions[supi] = sess

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: handover phases and cell moves.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverStarted})
			sess.setServingGNB(gnb.Config{ID: uint32(i), Name: "orbit-gnb"})
			m.publishMobility(StateEvent{SUPI: supi, State: StateHandoverComplete})
		}
	}()

	// Readers: what List/Status do on every API call.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, s := range m.List() {
					_ = s.State()
					_ = s.ServingGNB().Name
					_, _ = s.Mobility()
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
