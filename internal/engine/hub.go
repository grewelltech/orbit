package engine

import (
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/ue"

	f5nas "github.com/free5gc/nas"
)

// hub is a simple fan-out event bus for StateStream. Each subscriber gets a
// buffered channel; a slow subscriber that fills its buffer drops events
// rather than blocking the FSM producer (observability must never
// backpressure the control path — see internal/observability).
type hub struct {
	mu   sync.Mutex
	subs map[int]chan StateEvent
	next int
}

func newHub() *hub {
	return &hub{subs: make(map[int]chan StateEvent)}
}

// publish stamps the event with the current time and fans it out. Full
// subscriber buffers are skipped, not awaited.
func (h *hub) publish(ev StateEvent) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default: // drop rather than block the producer
		}
	}
}

// subscribe registers a new subscriber, returning its channel and an
// unsubscribe func.
func (h *hub) subscribe() (<-chan StateEvent, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan StateEvent, 64)
	h.subs[id] = ch
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
	}
}

// ueBuildDeregistration is a thin indirection so manager.go does not import
// the ue package directly for this one call.
func ueBuildDeregistration(guti []byte) (*f5nas.Message, error) {
	return ue.BuildDeregistrationRequest(guti, true)
}
