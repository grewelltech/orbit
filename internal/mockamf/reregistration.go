package mockamf

import "time"

// WithDropRegistrationAfterRelease makes the mock drop, with no answer, a
// Registration Request that arrives in an Initial UE Message within window
// of the UE CONTEXT RELEASE COMPLETE that ended the same SUPI's previous
// UE context. It stands in for a core that is still clearing a released UE
// when the UE registers again, so the FAIL path of the check for that can
// be exercised. A request that comes later is answered as usual.
func WithDropRegistrationAfterRelease(window time.Duration) Option {
	return func(a *AMF) { a.dropAfterRelease = window }
}

// WithCrashOnDroppedRegistration makes the mock crash at a Registration
// Request WithDropRegistrationAfterRelease drops: from then on any
// association closes on its next message, so a fresh NG Setup fails.
func WithCrashOnDroppedRegistration() Option { return func(a *AMF) { a.crashOnDrop = true } }

// DroppedRegistrations reports how many Registration Requests
// WithDropRegistrationAfterRelease dropped.
func (a *AMF) DroppedRegistrations() int64 { return a.droppedRegs.Load() }

// noteReleased records when the UE context named by a UE CONTEXT RELEASE
// COMPLETE's AMF UE NGAP ID was released, under its SUPI.
func (a *AMF) noteReleased(amfID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st := a.ues[amfID]; st != nil {
		a.releasedAt[st.supi] = time.Now()
	}
}

// dropsRegistration reports whether a Registration Request of supi that
// arrives now is one WithDropRegistrationAfterRelease drops, and counts it.
func (a *AMF) dropsRegistration(supi string) bool {
	if a.dropAfterRelease <= 0 {
		return false
	}
	a.mu.Lock()
	at, released := a.releasedAt[supi]
	a.mu.Unlock()
	if !released || time.Since(at) >= a.dropAfterRelease {
		return false
	}
	a.droppedRegs.Add(1)
	return true
}
