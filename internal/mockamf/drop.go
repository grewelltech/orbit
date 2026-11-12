package mockamf

// DropAssociations ends every association that has completed NG Setup, as
// a core that drops its gNBs and stays up does: each gNB sees its
// association close, and a new one is served as usual.
func (a *AMF) DropAssociations() {
	a.mu.Lock()
	pipes := make([]*pipe, 0, len(a.assocs))
	for p := range a.assocs {
		pipes = append(pipes, p)
	}
	a.mu.Unlock()
	for _, p := range pipes {
		_ = p.Close()
	}
}

// Crash stops the mock as a crashed core stops: every association that
// has completed NG Setup ends, and no later message on any association is
// answered, so a fresh NG Setup fails.
func (a *AMF) Crash() {
	a.crashed.Store(true)
	a.DropAssociations()
}

// WithCrashOnUEContextReleaseRequest makes the mock crash (Crash) on a UE
// CONTEXT RELEASE REQUEST instead of answering it.
func WithCrashOnUEContextReleaseRequest() Option {
	return func(a *AMF) { a.crashOnReleaseRequest = true }
}
