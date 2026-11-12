package mockamf

import (
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/nas"
)

// WithNoReleaseOnDeregistration makes the mock send no UE CONTEXT RELEASE
// COMMAND after a Deregistration Request. By default it sends one (cause
// NAS deregister), as an AMF does: a switch-off gets no Deregistration
// Accept (TS 24.501 5.5.2.2.2), so the release is its only answer, and a
// normal deregistration gets the release after its Accept.
func WithNoReleaseOnDeregistration() Option { return func(a *AMF) { a.noReleaseOnDereg = true } }

// WithNoDeregistrationAccept makes the mock answer a normal (not
// switch-off) Deregistration Request as it answers a switch-off: with no
// Deregistration Accept. By default it sends one, which TS 24.501 5.5.2.2.2
// requires.
func WithNoDeregistrationAccept() Option { return func(a *AMF) { a.noDeregAccept = true } }

// DeregistrationAccepts reports how many Deregistration Accepts the mock
// has sent.
func (a *AMF) DeregistrationAccepts() int64 { return a.deregAccepts.Load() }

// ReleaseIDs selects the UE NGAP IDs the release answering a
// deregistration names.
type ReleaseIDs int

const (
	// ReleaseIDPair names the UE by its AMF and RAN UE NGAP ID pair, as
	// TS 38.413 8.3.3.2 requires when both are available. The default.
	ReleaseIDPair ReleaseIDs = iota
	// ReleaseAMFIDOnly names the UE by its AMF UE NGAP ID alone.
	ReleaseAMFIDOnly
	// ReleaseOtherPair names a pair no UE of the association has.
	ReleaseOtherPair
)

// WithDeregistrationReleaseIDs makes the release answering a
// deregistration name the UE as ids selects.
func WithDeregistrationReleaseIDs(ids ReleaseIDs) Option {
	return func(a *AMF) { a.deregReleaseIDs = ids }
}

// otherPairOffset is added to both IDs of a ReleaseOtherPair release.
const otherPairOffset = 1000

// deregistrationRelease builds the release answering a deregistration of
// the UE (amfID, ranID), naming it as WithDeregistrationReleaseIDs selects.
func (a *AMF) deregistrationRelease(amfID, ranID int64) ngapType.NGAPPDU {
	switch a.deregReleaseIDs {
	case ReleaseOtherPair:
		return buildUEContextReleaseCommandCause(amfID+otherPairOffset, ranID+otherPairOffset, ngapType.CauseNasPresentDeregister)
	case ReleaseAMFIDOnly:
		pdu := buildUEContextReleaseCommandCause(amfID, ranID, ngapType.CauseNasPresentDeregister)
		list := pdu.InitiatingMessage.Value.UEContextReleaseCommand.ProtocolIEs.List
		for i := range list {
			if list[i].Id.Value == ngapType.ProtocolIEIDUENGAPIDs {
				list[i].Value.UENGAPIDs = &ngapType.UENGAPIDs{
					Present:     ngapType.UENGAPIDsPresentAMFUENGAPID,
					AMFUENGAPID: &ngapType.AMFUENGAPID{Value: amfID},
				}
			}
		}
		return pdu
	}
	return buildUEContextReleaseCommandCause(amfID, ranID, ngapType.CauseNasPresentDeregister)
}

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

// onDeregistration answers a Deregistration Request: a normal one with a
// Deregistration Accept under the UE's security context (unless
// WithNoDeregistrationAccept), and either kind with a release (unless
// WithNoReleaseOnDeregistration).
func (a *AMF) onDeregistration(p *pipe, st *ueState, msg *f5nas.Message) error {
	req := msg.DeregistrationRequestUEOriginatingDeregistration
	if req != nil && req.GetSwitchOff() == 0 && !a.noDeregAccept {
		acc, err := buildDeregistrationAccept()
		if err != nil {
			return err
		}
		wrapped, err := st.encodeSecure(acc, nas.SecHdrIntegrityCiphered)
		if err != nil {
			return err
		}
		if err := a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), wrapped)); err != nil {
			return err
		}
		a.deregAccepts.Add(1)
	}
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
		a.pendingReleases++
		a.mu.Unlock()
		return a.send(p, a.deregistrationRelease(amfID, ranID))
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
// unless WithIgnoreUEContextReleaseRequest (WithCrashOnUEContextReleaseRequest
// crashes the mock instead). A request for a UE the
// association never registered is ignored.
func (a *AMF) onUEContextReleaseRequest(p *pipe, st *ueState) error {
	a.releaseRequests.Add(1)
	if a.crashOnReleaseRequest {
		a.Crash()
		return errCrashed
	}
	if a.ignoreReleaseRequest || st.amfID == 0 {
		return nil
	}
	amfID, ranID := st.amfID, a.ranIDOf(st)
	a.mu.Lock()
	st.releasing = true
	a.requestReleases = append(a.requestReleases, [2]int64{amfID, ranID})
	a.mu.Unlock()
	return a.send(p, buildUEContextReleaseCommandCause(amfID, ranID, ngapType.CauseNasPresentNormalRelease))
}

// ReleaseRequests reports how many UE CONTEXT RELEASE REQUESTs the mock has
// received.
func (a *AMF) ReleaseRequests() int64 { return a.releaseRequests.Load() }

// IdleDeregistrations reports how many of the Deregistration Requests the
// mock received came as an initial NAS message, in an Initial UE Message
// (Deregistrations counts them too).
func (a *AMF) IdleDeregistrations() int64 { return a.idleDeregs.Load() }
