package mockamf

import "github.com/free5gc/ngap/ngapType"

// WithReleaseOnDeregistration makes the mock answer a UE's Deregistration
// Request with a UE CONTEXT RELEASE COMMAND, as an AMF does after a
// switch-off deregistration (cause NAS deregister). By default the mock
// sends nothing.
func WithReleaseOnDeregistration() Option { return func(a *AMF) { a.releaseOnDereg = true } }

// WithIgnoreUEContextReleaseRequest makes the mock leave a UE CONTEXT
// RELEASE REQUEST unanswered. By default it answers with a UE CONTEXT
// RELEASE COMMAND.
func WithIgnoreUEContextReleaseRequest() Option {
	return func(a *AMF) { a.ignoreReleaseRequest = true }
}

// onDeregistration answers a Deregistration Request
// (WithReleaseOnDeregistration).
func (a *AMF) onDeregistration(p *pipe, st *ueState) error {
	if !a.releaseOnDereg {
		return nil
	}
	return a.send(p, buildUEContextReleaseCommandCause(st.amfID, st.ranID, ngapType.CauseNasPresentDeregister))
}

// onUEContextReleaseRequest answers a UE CONTEXT RELEASE REQUEST with a UE
// CONTEXT RELEASE COMMAND, cause NAS normal-release (TS 38.413 8.3.2),
// unless WithIgnoreUEContextReleaseRequest. A request for a UE the
// association never registered is ignored.
func (a *AMF) onUEContextReleaseRequest(p *pipe, st *ueState) error {
	if a.ignoreReleaseRequest || st.amfID == 0 {
		return nil
	}
	return a.send(p, buildUEContextReleaseCommandCause(st.amfID, st.ranID, ngapType.CauseNasPresentNormalRelease))
}
