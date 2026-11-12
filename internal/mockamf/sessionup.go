package mockamf

import "time"

// sessionUpWait bounds how long a path switch waits for the PDU Session
// Resource Setup Response of a session it moves.
const sessionUpWait = 2 * time.Second

// awaitSessionsUp waits, up to sessionUpWait, until each of the sessions ids
// of the UE amfID that the mock has set up carries the gNB's downlink tunnel.
// A UE registers on one association and its path switch arrives on
// another, and nothing orders the two: the setup response the gNB sent
// before the handover can still be in flight on the first association when
// the path switch is read from the second. Waiting for it lets the path
// switch see the downlink tunnel it moves away from, as an AMF that had
// read the first association to the end would.
func (a *AMF) awaitSessionsUp(amfID int64, ids []uint8) {
	deadline := time.Now().Add(sessionUpWait)
	for {
		a.mu.Lock()
		waiting := false
		if st := a.ues[amfID]; st != nil {
			for _, id := range ids {
				if s := st.sessions[id]; s != nil && s.dl == (Tunnel{}) {
					waiting = true
				}
			}
		}
		left := time.Until(deadline)
		if !waiting || left <= 0 {
			a.mu.Unlock()
			return
		}
		if a.sessionsUpCh == nil {
			a.sessionsUpCh = make(chan struct{})
		}
		ch := a.sessionsUpCh
		a.mu.Unlock()
		t := time.NewTimer(left)
		select {
		case <-ch:
		case <-t.C:
		}
		t.Stop()
	}
}

// wakeSessionsUpLocked wakes every awaitSessionsUp. The caller holds a.mu.
func (a *AMF) wakeSessionsUpLocked() {
	if a.sessionsUpCh != nil {
		close(a.sessionsUpCh)
		a.sessionsUpCh = nil
	}
}
