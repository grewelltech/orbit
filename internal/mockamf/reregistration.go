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

// WithDropRegistrationAfterAnyRelease makes the mock drop, with no answer,
// a Registration Request that arrives in an Initial UE Message while a
// deregistration's UE context release is in progress (its release command
// sent, its RELEASE COMPLETE not yet received) or within window of any UE
// CONTEXT RELEASE COMPLETE, whichever SUPI's context it ended. It stands in
// for a core whose release of one UE context swallows an Initial UE Message
// that arrives while it runs, for another UE too. The in-progress case
// matters because a request on another association can reach the mock
// before the RELEASE COMPLETE does.
func WithDropRegistrationAfterAnyRelease(window time.Duration) Option {
	return func(a *AMF) { a.dropAfterAnyRelease = window }
}

// WithCrashOnDroppedRegistration makes the mock crash at a Registration
// Request WithDropRegistrationAfterRelease drops: from then on any
// association closes on its next message, so a fresh NG Setup fails.
func WithCrashOnDroppedRegistration() Option { return func(a *AMF) { a.crashOnDrop = true } }

// DroppedRegistrations reports how many Registration Requests
// WithDropRegistrationAfterRelease and WithDropRegistrationAfterAnyRelease
// dropped.
func (a *AMF) DroppedRegistrations() int64 { return a.droppedRegs.Load() }

// noteReleased records when the UE context named by a UE CONTEXT RELEASE
// COMPLETE's AMF UE NGAP ID was released, under its SUPI.
func (a *AMF) noteReleased(amfID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.lastRelease = now
	if a.pendingReleases > 0 {
		a.pendingReleases--
	}
	if st := a.ues[amfID]; st != nil {
		a.releasedAt[st.supi] = now
	}
}

// dropsRegistration reports whether a Registration Request of supi that
// arrives now is one WithDropRegistrationAfterRelease or
// WithDropRegistrationAfterAnyRelease drops, and counts it.
func (a *AMF) dropsRegistration(supi string) bool {
	if a.dropAfterRelease <= 0 && a.dropAfterAnyRelease <= 0 {
		return false
	}
	a.mu.Lock()
	at, released := a.releasedAt[supi]
	last := a.lastRelease
	pending := a.pendingReleases > 0
	a.mu.Unlock()
	same := a.dropAfterRelease > 0 && released && time.Since(at) < a.dropAfterRelease
	any := a.dropAfterAnyRelease > 0 && (pending || !last.IsZero() && time.Since(last) < a.dropAfterAnyRelease)
	if !same && !any {
		return false
	}
	a.droppedRegs.Add(1)
	return true
}
