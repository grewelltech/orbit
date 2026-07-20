package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
)

// deregisterableSession builds a synthetic registered UE complete enough for
// Manager.Deregister to run its teardown: a stub transport, a NAS security
// context, and a 5G-GUTI.
func deregisterableSession(supi string) *Session {
	sess := &Session{
		SUPI:   supi,
		Result: &AttachResult{},
		conn:   stubTransport{},
		guti:   []byte{0x02, 0xf8, 0x39, 0xca, 0xfe, 0x00, 0x00, 0x00, 0x01, 0x02},
		sec: &nas.SecurityContext{
			IntegrityAlg: nas.NIA2,
			CipheringAlg: nas.NEA0,
			KNASint:      [16]byte{},
			KNASenc:      [16]byte{},
		},
	}
	sess.setState(StateSessionActive)
	sess.setServingGNB(gnb.Config{
		ID: 1, IDBits: 24, Name: "orbit-gnb",
		MCC: "208", MNC: "93", TAC: 1,
		Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}},
	})
	return sess
}

// Two control-plane procedures on one UE must not overlap: they rewrite the
// association and the serving cell together, and interleaving leaves the
// session describing neither.
func TestProcedureLockExcludes(t *testing.T) {
	s := &Session{SUPI: "001010000000001"}

	release, err := s.beginProcedure(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// A second acquire must not succeed while the first is held.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.beginProcedure(ctx); err == nil {
		t.Fatal("second procedure acquired the lock while the first was held")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second acquire failed with %v, want a context deadline", err)
	}

	// Once released, the lock is available again.
	release()
	release2, err := s.beginProcedure(context.Background())
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

// A session constructed as a bare struct literal — as tests and any future
// construction site may do — must not deadlock on its first procedure. The
// lock channel is created on first use precisely so a nil channel can never
// block forever.
func TestProcedureLockOnZeroValueSession(t *testing.T) {
	s := &Session{SUPI: "001010000000001"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		release, err := s.beginProcedure(context.Background())
		if err != nil {
			t.Errorf("acquire on a zero-value session: %v", err)
			return
		}
		release()
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("beginProcedure blocked on a zero-value session — the lock channel was nil")
	}
}

// Deregistration is terminal. A procedure that was waiting when it happened
// must fail rather than run against a torn-down session.
func TestProcedureLockRejectsAfterRelease(t *testing.T) {
	s := &Session{SUPI: "001010000000001"}

	release, err := s.beginProcedure(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// A second procedure queues behind the first.
	type result struct {
		release func()
		err     error
	}
	queued := make(chan result, 1)
	go func() {
		r, err := s.beginProcedure(context.Background())
		queued <- result{r, err}
	}()

	// Give the waiter time to block on the channel, then deregister.
	time.Sleep(20 * time.Millisecond)
	s.markReleased()
	release()

	select {
	case got := <-queued:
		if got.err == nil {
			got.release()
			t.Fatal("a procedure queued behind a deregistration was allowed to run")
		}
	case <-time.After(time.Second):
		t.Fatal("queued procedure never resolved")
	}
}

// A cancelled caller must not be left holding, or waiting on, the lock.
func TestProcedureLockHonoursContext(t *testing.T) {
	s := &Session{SUPI: "001010000000001"}
	release, err := s.beginProcedure(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, err := s.beginProcedure(ctx); err == nil {
		t.Fatal("acquire succeeded despite a cancelled context")
	}

	// The failed acquire must not have consumed the token: releasing the
	// original holder leaves the lock free.
	release()
	r, err := s.beginProcedure(context.Background())
	if err != nil {
		t.Fatalf("lock was leaked by the cancelled acquire: %v", err)
	}
	r()
}

// Under contention exactly one holder is inside the critical section at a time.
func TestProcedureLockMutualExclusionUnderContention(t *testing.T) {
	s := &Session{SUPI: "001010000000001"}

	var mu sync.Mutex
	inside, maxInside := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.beginProcedure(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer release()

			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			inside--
			mu.Unlock()
		}()
	}
	wg.Wait()

	if maxInside != 1 {
		t.Errorf("%d procedures were inside the critical section at once, want 1", maxInside)
	}
}

// Manager.Deregister must wait for an in-flight procedure rather than tearing
// the association down underneath it. This is the concrete shape of the race
// the procedure lock exists to close: Deregister removes the session from the
// map, but a handover that already captured the pointer keeps running.
func TestDeregisterWaitsForInFlightProcedure(t *testing.T) {
	m := quietManager()
	const supi = "001010000000001"
	sess := deregisterableSession(supi)
	m.sessions[supi] = sess

	// Stand in for a handover already under way.
	release, err := sess.beginProcedure(context.Background())
	if err != nil {
		t.Fatalf("simulated handover could not acquire: %v", err)
	}

	deregDone := make(chan error, 1)
	go func() { deregDone <- m.Deregister(context.Background(), supi) }()

	select {
	case <-deregDone:
		t.Fatal("Deregister tore down the session while a procedure was in flight")
	case <-time.After(50 * time.Millisecond):
		// Correctly blocked.
	}

	release()
	select {
	case err := <-deregDone:
		if err != nil {
			t.Errorf("Deregister after the procedure finished: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deregister never completed after the procedure released")
	}
}

// A handover that arrives after the UE has been deregistered must be refused,
// not run against a closed association.
func TestHandoverAfterDeregisterIsRefused(t *testing.T) {
	m := quietManager()
	const supi = "001010000000001"
	sess := deregisterableSession(supi)
	m.sessions[supi] = sess

	if err := m.Deregister(context.Background(), supi); err != nil {
		t.Fatalf("deregister: %v", err)
	}

	// The session is gone from the map, so the Manager refuses on that basis;
	// a caller still holding the pointer is refused by the released flag.
	if _, err := sess.beginProcedure(context.Background()); err == nil {
		t.Error("a procedure was allowed to start on a deregistered session")
	}
}
