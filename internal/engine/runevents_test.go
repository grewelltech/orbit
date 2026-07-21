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
