package mockamf

import (
	"time"

	"github.com/free5gc/ngap/ngapType"
)

// WithNoReleaseOnDeregistration makes the mock leave a UE's switch-off
// Deregistration Request unanswered. By default it answers with a UE
// CONTEXT RELEASE COMMAND (cause NAS deregister), as an AMF does: a
// switch-off gets no Deregistration Accept (TS 24.501 5.5.2.2.1), and the
// release is the AMF's answer.
func WithNoReleaseOnDeregistration() Option { return func(a *AMF) { a.noReleaseOnDereg = true } }

// WithNoiseBeforeRelease makes the mock send two PDUs that are not a UE
// CONTEXT RELEASE COMMAND ahead of the one answering a switch-off: a
// Downlink NAS Transport (a plain 5GMM Status) and a successful outcome of
// the UE Context Release procedure. A gNB waiting for the release must
// skip both.
func WithNoiseBeforeRelease() Option { return func(a *AMF) { a.noiseBeforeRelease = true } }

// WithReleaseDelay makes the mock send the release answering a switch-off
// d after the Deregistration Request, rather than at once. A UE Context
// Release Complete that arrives before it is a stray (StrayReleaseCompletes).
func WithReleaseDelay(d time.Duration) Option { return func(a *AMF) { a.releaseDelay = d } }

// WithStaleRelease makes the mock release each UE (cause NAS normal
// release) d after its Registration Complete, as a core's AN or inactivity
// release would: a release that answers no deregistration.
func WithStaleRelease(d time.Duration) Option { return func(a *AMF) { a.staleRelease = d } }

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
	amfID, ranID := st.amfID, a.ranIDOf(st)
	if a.noiseBeforeRelease {
		// 5GMM Status, cause 111 (protocol error, unspecified), in the clear.
		if err := a.send(p, buildDownlinkNASTransport(amfID, ranID, []byte{0x7e, 0x00, 0x64, 0x6f})); err != nil {
			return err
		}
		if err := a.send(p, buildReleaseCompleteShaped(amfID, ranID)); err != nil {
			return err
		}
	}
	release := func() error {
		a.mu.Lock()
		a.deregReleases = append(a.deregReleases, [2]int64{amfID, ranID})
		a.mu.Unlock()
		return a.send(p, buildUEContextReleaseCommandCause(amfID, ranID, ngapType.CauseNasPresentDeregister))
	}
	if a.releaseDelay > 0 {
		go func() {
			time.Sleep(a.releaseDelay)
			_ = release()
		}()
		return nil
	}
	return release()
}

// sendStaleRelease releases a registered UE d after its registration
// (WithStaleRelease).
func (a *AMF) sendStaleRelease(p *pipe, st *ueState) {
	amfID, ranID := st.amfID, a.ranIDOf(st)
	go func() {
		time.Sleep(a.staleRelease)
		_ = a.send(p, buildUEContextReleaseCommandCause(amfID, ranID, ngapType.CauseNasPresentNormalRelease))
	}()
}

// buildReleaseCompleteShaped is a successful outcome of the UE Context
// Release procedure: the procedure code of a release, but not a command.
func buildReleaseCompleteShaped(amfID, ranID int64) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeUEContextRelease
	pdu.SuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentUEContextReleaseComplete
	c := new(ngapType.UEContextReleaseComplete)
	pdu.SuccessfulOutcome.Value.UEContextReleaseComplete = c
	c.ProtocolIEs.List = append(c.ProtocolIEs.List, []ngapType.UEContextReleaseCompleteIEs{
		{Id: ngapType.ProtocolIEID{Value: ngapType.ProtocolIEIDAMFUENGAPID}, Criticality: ngapType.Criticality{Value: ngapType.CriticalityPresentIgnore},
			Value: ngapType.UEContextReleaseCompleteIEsValue{Present: ngapType.UEContextReleaseCompleteIEsPresentAMFUENGAPID, AMFUENGAPID: &ngapType.AMFUENGAPID{Value: amfID}}},
		{Id: ngapType.ProtocolIEID{Value: ngapType.ProtocolIEIDRANUENGAPID}, Criticality: ngapType.Criticality{Value: ngapType.CriticalityPresentIgnore},
			Value: ngapType.UEContextReleaseCompleteIEsValue{Present: ngapType.UEContextReleaseCompleteIEsPresentRANUENGAPID, RANUENGAPID: &ngapType.RANUENGAPID{Value: ranID}}},
	}...)
	return pdu
}

// releaseCompleteIDs is the NGAP ID pair a UE CONTEXT RELEASE COMPLETE
// names.
func releaseCompleteIDs(c *ngapType.UEContextReleaseComplete) [2]int64 {
	var ids [2]int64
	for _, ie := range c.ProtocolIEs.List {
		switch {
		case ie.Id.Value == ngapType.ProtocolIEIDAMFUENGAPID && ie.Value.AMFUENGAPID != nil:
			ids[0] = ie.Value.AMFUENGAPID.Value
		case ie.Id.Value == ngapType.ProtocolIEIDRANUENGAPID && ie.Value.RANUENGAPID != nil:
			ids[1] = ie.Value.RANUENGAPID.Value
		}
	}
	return ids
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
