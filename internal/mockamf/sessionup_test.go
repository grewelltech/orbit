package mockamf

import (
	"testing"
	"time"
)

// awaitSessionsUp returns at once when there is nothing to wait for, wakes
// when the setup response is recorded, and gives up after sessionUpWait.
func TestAwaitSessionsUp(t *testing.T) {
	up := Tunnel{Addr: "127.0.0.1", TEID: 7}
	tests := []struct {
		name    string
		dl      Tunnel // the session's downlink tunnel at the start
		amfID   int64  // the UE asked about
		upAfter time.Duration
		minWait time.Duration
		maxWait time.Duration
		wantDL  Tunnel
	}{
		{name: "already up", dl: up, amfID: 1, maxWait: 100 * time.Millisecond, wantDL: up},
		{name: "unknown UE", amfID: 9, maxWait: 100 * time.Millisecond},
		{name: "set up while waiting", amfID: 1, upAfter: 50 * time.Millisecond, minWait: 50 * time.Millisecond, maxWait: time.Second, wantDL: up},
		{name: "never set up", amfID: 1, minWait: sessionUpWait, maxWait: sessionUpWait + time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := New(Config{Ki: make([]byte, 16), OPc: make([]byte, 16), SQN: make([]byte, 6), AMF: []byte{0x80, 0}, MCC: "208", MNC: "93", SST: 1})
			if err != nil {
				t.Fatal(err)
			}
			s := &pduSession{id: 1, dl: tc.dl}
			a.mu.Lock()
			a.ues[1] = &ueState{amfID: 1, supi: "208930100000001", sessions: map[uint8]*pduSession{1: s}}
			a.mu.Unlock()
			start := time.Now()
			if tc.upAfter > 0 {
				timer := time.AfterFunc(tc.upAfter, func() { a.sessionsUp(1, map[uint8]Tunnel{1: up}) })
				defer timer.Stop()
			}
			a.awaitSessionsUp(tc.amfID, []uint8{1})
			if took := time.Since(start); took < tc.minWait || took > tc.maxWait {
				t.Errorf("waited %s, want %s to %s", took, tc.minWait, tc.maxWait)
			}
			a.mu.Lock()
			got := s.dl
			a.mu.Unlock()
			if tc.amfID == 1 && got != tc.wantDL {
				t.Errorf("downlink %+v, want %+v", got, tc.wantDL)
			}
		})
	}
}
