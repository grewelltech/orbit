package mockamf

import "github.com/free5gc/ngap/ngapType"

// WithNoReleaseOnDeregistration makes the mock leave a UE's switch-off
// Deregistration Request unanswered. By default it answers with a UE
// CONTEXT RELEASE COMMAND (cause NAS deregister), as an AMF does: a
// switch-off gets no Deregistration Accept (TS 24.501 5.5.2.2.1), and the
// release is the AMF's answer.
func WithNoReleaseOnDeregistration() Option { return func(a *AMF) { a.noReleaseOnDereg = true } }

// WithIgnoreUEContextReleaseRequest makes the mock leave a UE CONTEXT
// RELEASE REQUEST unanswered. By default it answers with a UE CONTEXT
// RELEASE COMMAND.
func WithIgnoreUEContextReleaseRequest() Option {
	return func(a *AMF) { a.ignoreReleaseRequest = true }
}

// onDeregistration answers a Deregistration Request with a release,
// unless WithNoReleaseOnDeregistration.
func (a *AMF) onDeregistration(p *pipe, st *ueState) error {
	if a.noReleaseOnDereg {
		return nil
	}
	return a.send(p, buildUEContextReleaseCommandCause(st.amfID, a.ranIDOf(st), ngapType.CauseNasPresentDeregister))
}

// onUEContextReleaseRequest answers a UE CONTEXT RELEASE REQUEST with a UE
// CONTEXT RELEASE COMMAND, cause NAS normal-release (TS 38.413 8.3.2),
// unless WithIgnoreUEContextReleaseRequest. A request for a UE the
// association never registered is ignored.
func (a *AMF) onUEContextReleaseRequest(p *pipe, st *ueState) error {
	if a.ignoreReleaseRequest || st.amfID == 0 {
		return nil
	}
	return a.send(p, buildUEContextReleaseCommandCause(st.amfID, a.ranIDOf(st), ngapType.CauseNasPresentNormalRelease))
}
