package engine

import (
	"sync"
	"testing"
)

// A subscriber from seq 0 sees every retained event, in order, with no drops.
func TestEventRingReplayInOrder(t *testing.T) {
	r := newEventRing(10)
	r.emit("info", "RUN", "", "started")
	r.emit("error", "ATTACH", "imsi-1", "rejected")
	r.emit("info", "RUN", "", "complete")

	sub := r.subscribeFrom(0)
	defer sub.Close()
	if sub.DroppedBefore != 0 {
		t.Errorf("dropped-before = %d, want 0", sub.DroppedBefore)
	}
	if len(sub.Backlog) != 3 {
		t.Fatalf("backlog = %d events, want 3", len(sub.Backlog))
	}
	for i, ev := range sub.Backlog {
		if ev.Seq != uint64(i) {
			t.Errorf("backlog[%d] seq = %d, want %d", i, ev.Seq, i)
		}
	}
	if sub.Backlog[1].SUPI != "imsi-1" || sub.Backlog[1].Severity != "error" {
		t.Errorf("event 1 = %+v, want the imsi-1 error", sub.Backlog[1])
	}
}

// Beyond capacity, the oldest events are evicted and counted; a from-0
// subscriber is told how many it will never see.
func TestEventRingBoundedWithDropCount(t *testing.T) {
	r := newEventRing(3)
	for i := 0; i < 8; i++ {
		r.emit("info", "N", "", "e")
	}
	sub := r.subscribeFrom(0)
	defer sub.Close()

	if len(sub.Backlog) != 3 {
		t.Fatalf("backlog = %d, want the 3 cap", len(sub.Backlog))
	}
	// 8 emitted, 3 retained (seq 5,6,7) → 5 dropped before the first retained.
	if sub.DroppedBefore != 5 {
		t.Errorf("dropped-before = %d, want 5", sub.DroppedBefore)
	}
	if sub.Backlog[0].Seq != 5 {
		t.Errorf("earliest retained seq = %d, want 5", sub.Backlog[0].Seq)
	}
}

// A resume point that is still retained yields only newer events, no drops.
func TestEventRingResumeFromSeq(t *testing.T) {
	r := newEventRing(10)
	for i := 0; i < 5; i++ {
		r.emit("info", "N", "", "e")
	}
	sub := r.subscribeFrom(3)
	defer sub.Close()
	if sub.DroppedBefore != 0 {
		t.Errorf("dropped-before = %d, want 0", sub.DroppedBefore)
	}
	if len(sub.Backlog) != 2 || sub.Backlog[0].Seq != 3 {
		t.Errorf("backlog = %+v, want seq 3,4", sub.Backlog)
	}
}

// Live events emitted after subscribe arrive on the channel, and the
// subscribe/emit seam loses nothing.
func TestEventRingLiveDelivery(t *testing.T) {
	r := newEventRing(10)
	r.emit("info", "N", "", "before")
	sub := r.subscribeFrom(0)
	defer sub.Close()

	if len(sub.Backlog) != 1 {
		t.Fatalf("backlog = %d, want 1", len(sub.Backlog))
	}
	r.emit("info", "N", "", "after-1")
	r.emit("info", "N", "", "after-2")

	got := []uint64{}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-sub.Ch:
			got = append(got, ev.Seq)
		default:
			t.Fatalf("expected live event %d", i)
		}
	}
	if got[0] != 1 || got[1] != 2 {
		t.Errorf("live seqs = %v, want [1 2]", got)
	}
}

// A slow subscriber whose channel fills drops live events rather than blocking
// the emitter; the retained ring still holds the recent ones.
func TestEventRingSlowSubscriberDoesNotBlockEmit(t *testing.T) {
	r := newEventRing(4)
	sub := r.subscribeFrom(0)
	defer sub.Close()

	// Emit far more than the channel (cap 4) can hold without a reader.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			r.emit("info", "N", "", "e")
		}
		close(done)
	}()
	select {
	case <-done:
	default:
		// emit must not be blocked by the unread channel; give it a moment.
	}
	<-done // completes without deadlock — the assertion is that this returns
}

// Cancel unsubscribes and closing twice is safe.
func TestEventRingCancelIsIdempotent(t *testing.T) {
	r := newEventRing(4)
	sub := r.subscribeFrom(0)
	sub.Close()
	sub.Close() // must not panic on a double close
	r.emit("info", "N", "", "after-cancel")
	// The cancelled channel is closed; a receive returns the zero value, not a block.
	select {
	case _, ok := <-sub.Ch:
		if ok {
			t.Error("received a live event after cancel")
		}
	default:
		t.Error("cancelled channel should be closed, not blocking")
	}
}

// Concurrent emitters and a subscriber are race-free. Run under -race.
func TestEventRingConcurrent(t *testing.T) {
	r := newEventRing(64)
	sub := r.subscribeFrom(0)
	defer sub.Close()

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				r.emit("info", "N", "supi", "e")
			}
		}()
	}
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-sub.Ch:
			}
		}
	}()
	wg.Wait()
	close(stop)
}

// Concurrent emit and subscriber Close must not panic (send on closed channel).
// Run under -race. This is the crash the fan-out-under-lock fix prevents.
func TestEventRingEmitCancelRace(t *testing.T) {
	r := newEventRing(8)
	var wg sync.WaitGroup

	// Emitters.
	stop := make(chan struct{})
	for e := 0; e < 4; e++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					r.emit("info", "N", "", "e")
				}
			}
		}()
	}
	// Subscribers that repeatedly subscribe and cancel, racing the emits.
	for s := 0; s < 4; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				sub := r.subscribeFrom(0)
				sub.Close()
			}
		}()
	}
	// Let it run; the assertion is simply that no send-on-closed panic occurs.
	for i := 0; i < 50000; i++ {
		r.emit("info", "N", "", "e")
	}
	close(stop)
	wg.Wait()
}

// snapshotSince returns retained events past a sequence — the recovery path a
// slow RunEvents subscriber relies on to still receive the terminal event.
func TestEventRingSnapshotSince(t *testing.T) {
	r := newEventRing(10)
	for i := 0; i < 5; i++ {
		r.emit("info", "N", "", "e")
	}
	got := r.snapshotSince(2)
	if len(got) != 3 || got[0].Seq != 2 || got[2].Seq != 4 {
		t.Errorf("snapshotSince(2) = %+v, want seq 2,3,4", got)
	}
	if after := r.snapshotSince(99); after != nil {
		t.Errorf("snapshotSince beyond the end = %+v, want nil", after)
	}
}

// The recovery contract the RunEvents handler relies on: when a subscriber's
// channel drops events (full), the newest events — including the terminal one —
// remain in the ring and are recoverable via snapshotSince past the last one
// delivered. Deterministic: a single goroutine that never drains the channel.
func TestEventRingNewestRecoverableAfterDrop(t *testing.T) {
	r := newEventRing(4) // channel and ring both hold 4
	sub := r.subscribeFrom(0)
	defer sub.Close()

	const n = 10
	for i := 0; i < n; i++ {
		r.emit("info", "N", "", "e")
	}

	// Drain whatever the (unread, so full) channel retained.
	var lastDelivered uint64
	delivered := false
	for draining := true; draining; {
		select {
		case ev := <-sub.Ch:
			lastDelivered, delivered = ev.Seq, true
		default:
			draining = false
		}
	}

	// Everything past the last delivered that is still retained — the handler's
	// terminal reconciliation. The newest event (seq n-1) must be present even
	// though the channel dropped it.
	from := uint64(0)
	if delivered {
		from = lastDelivered + 1
	}
	tail := r.snapshotSince(from)
	var sawNewest bool
	for _, ev := range tail {
		if ev.Seq == n-1 {
			sawNewest = true
		}
	}
	if !sawNewest {
		t.Errorf("newest event (seq %d) not recoverable after a channel drop; tail=%+v", n-1, tail)
	}
}
