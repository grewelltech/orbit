package conformance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
)

func init() {
	builtins = append(builtins, nasDeregAcceptNotSwitchOff{}, ngapCtxReleaseAfterSwitchOff{})
}

const (
	// deregRegisterWait bounds the registration that precedes a
	// deregistration check's stimulus.
	deregRegisterWait = 10 * time.Second
	// deregAcceptWait bounds the wait for the Deregistration Accept: 15 s,
	// the default of T3521 (TS 24.501 Table 10.2.1), on whose expiry a UE
	// retransmits the request.
	deregAcceptWait = engine.DefaultDeregistrationAcceptWait
	// deregReleaseWait bounds the wait for the UE Context Release Command
	// that ends a deregistration.
	deregReleaseWait = 5 * time.Second
)

// nasDeregAcceptNotSwitchOff registers the UE, without a PDU session, and
// sends a UE-originating Deregistration Request whose De-registration type
// does not indicate switch-off. TS 24.501 5.5.2.2.2 then requires a
// DEREGISTRATION ACCEPT, which is read through the UE's NAS security
// context: PASS. None within deregAcceptWait, or a UE Context Release
// Command in its place, is FAIL, and reports the deregistration as not
// completed: the check owns the deregistration. The release that follows
// the Accept is recorded, not judged. A core that answered with neither is
// sent a switch-off Deregistration Request before the association closes,
// so it is not left holding the UE.
type nasDeregAcceptNotSwitchOff struct {
	// registerWait, acceptWait and releaseWait override deregRegisterWait,
	// deregAcceptWait and deregReleaseWait (tests only).
	registerWait, acceptWait, releaseWait time.Duration
}

// deregAcceptRef is the key of the shall every outcome of
// NAS-DEREG-ACCEPT-NOT-SWITCH-OFF rests on.
const deregAcceptRef = "TS 24.501 5.5.2.2.2"

// The outcomes of NAS-DEREG-ACCEPT-NOT-SWITCH-OFF.
var (
	deregAccepted = Outcome{ID: "accepted", Verdict: Pass, Refs: []string{deregAcceptRef},
		Summary: "answered the Deregistration Request that is not switch-off with a Deregistration Accept"}
	deregReleasedInstead = Outcome{ID: "release-without-accept", Verdict: Fail, Refs: []string{deregAcceptRef}, NotCompleted: ProcDeregistration,
		Summary: "released the UE context without a Deregistration Accept"}
	deregNoAccept = Outcome{ID: "no-accept", Verdict: Fail, Refs: []string{deregAcceptRef}, NotCompleted: ProcDeregistration,
		Bound:   &Bound{Wait: deregAcceptWait, Source: "T3521, TS 24.501 Table 10.2.1: the UE's retransmission timer at its normal value; 5.5.2.2.2 sets the AMF no time limit"},
		Summary: "sent no Deregistration Accept for the Deregistration Request that is not switch-off"}
)

// Outcomes declares what the check can report.
func (nasDeregAcceptNotSwitchOff) Outcomes() []Outcome {
	return []Outcome{deregAccepted, deregReleasedInstead, deregNoAccept}
}

func (nasDeregAcceptNotSwitchOff) ID() string { return "NAS-DEREG-ACCEPT-NOT-SWITCH-OFF" }
func (nasDeregAcceptNotSwitchOff) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.5.2.2.2", Strength: Shall, Since: 15, Verified: ts24501Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 titles the clause \"UE-initiated de-registration procedure completion - common procedure\", \"UE-initiated de-registration procedure completion\" by V15.7.0; the AMF sentence is unchanged from V15.0.0 through V19.8.0"},
			}},
		{Spec: "TS 24.501", Clause: "Table 10.2.1 T3521", Strength: Descriptive, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 16, Note: "adds a WB-N1/CE mode value of 45s; the normal value stays 15s"},
				{Release: 17, Note: "adds 27s for access via a satellite NG-RAN cell; the normal value stays 15s"},
			}},
	}
}

func (c nasDeregAcceptNotSwitchOff) registerTimeout() time.Duration {
	return orDefault(c.registerWait, deregRegisterWait)
}

func (c nasDeregAcceptNotSwitchOff) acceptTimeout() time.Duration {
	return orDefault(c.acceptWait, deregAcceptWait)
}

func (c nasDeregAcceptNotSwitchOff) releaseTimeout() time.Duration {
	return orDefault(c.releaseWait, deregReleaseWait)
}

// orDefault is d, or def when d is not positive.
func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Budget covers the registration, the Accept wait, the release wait twice
// (after the Accept, or after the switch-off that follows no answer) and
// the liveness probe.
func (c nasDeregAcceptNotSwitchOff) Budget() time.Duration {
	return c.registerTimeout() + c.acceptTimeout() + 2*c.releaseTimeout() + 5*time.Second
}

func (c nasDeregAcceptNotSwitchOff) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "a Deregistration Accept for a UE-originating Deregistration Request that does not indicate switch-off (TS 24.501 5.5.2.2.2)"}
	h, done := deregRegister(ctx, env, c.registerTimeout(), &r)
	if done {
		return r
	}
	defer h.Close()

	res, err := h.DeregisterWith(ctx, engine.WithNormalDeregistration(c.acceptTimeout()), engine.WithReleaseWait(c.releaseTimeout()))
	r = judgeDeregAccept(r, res, err, ctx.Err() != nil, c)
	if r.Verdict == Error {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the deregistration")
}

// deregRegister registers env's UE, without a PDU session, for a
// deregistration check. done is true when r already holds the result: SKIP
// without a UE, ERROR when the harness failed or the check's time ran out,
// and otherwise, for a registration the core did not complete, BLOCKED on
// it (Result.SetupFailed).
func deregRegister(ctx context.Context, env Env, registerWait time.Duration, r *Result) (h *UEHarness, done bool) {
	if !env.UE.Configured() {
		*r = r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run checks that register a UE")
		return nil, true
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		*r = r.Errored("harness setup failed", err.Error())
		return nil, true
	}
	rctx, cancel := context.WithTimeout(ctx, registerWait)
	_, err = h.RegisterWithProbe(rctx, nil)
	cancel()
	switch {
	case err != nil && ctx.Err() != nil:
		h.Close()
		*r = r.Errored("timed out during the registration", fmt.Sprintf("%v; the per-test timeout expired before the registration ended; raise --per-test-timeout", err))
		return nil, true
	case err != nil:
		h.Close()
		*r = r.SetupFailed("registration failed", err)
		return nil, true
	}
	return h, false
}

// deregHarness registers env's UE, without a PDU session, for a
// deregistration check that has not declared its outcomes. done is true
// when r already holds the result: SKIP without a UE, ERROR when the
// harness or the registration failed.
func deregHarness(ctx context.Context, env Env, registerWait time.Duration, r *Result) (h *UEHarness, done bool) {
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run checks that register a UE"
		return nil, true
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return nil, true
	}
	rctx, cancel := context.WithTimeout(ctx, registerWait)
	_, err = h.RegisterWithProbe(rctx, nil)
	cancel()
	if err != nil {
		h.Close()
		r.Verdict, r.Observed, r.Detail = Error, "registration failed", err.Error()
		return nil, true
	}
	return h, false
}

// deregAcceptFail is the FAIL message of NAS-DEREG-ACCEPT-NOT-SWITCH-OFF.
const deregAcceptFail = "no Deregistration Accept for a non-switch-off deregistration. TS 24.501 5.5.2.2.2. Likely cause: AMF treats every deregistration as switch-off."

// judgeDeregAccept returns r with the result of a normal deregistration's
// outcome. cut reports that the check's own time ran out.
func judgeDeregAccept(r Result, res *engine.DeregisterResult, err error, cut bool, c nasDeregAcceptNotSwitchOff) Result {
	switch {
	case err != nil || res == nil:
		return r.Errored("Deregistration Request not sent", fmt.Sprintf("%v; the association was dropped with the UE still registered", err))
	case res.Accepted:
		return r.As(deregAccepted, "Deregistration Accept received",
			fmt.Sprintf("%s after the request; the release is recorded, not judged: %s", res.AcceptAt.Sub(res.SentAt).Round(time.Millisecond), releaseNote(res)))
	case cut:
		return r.Errored("timed out before the Deregistration Accept wait ended",
			fmt.Sprintf("raise --per-test-timeout to at least %s", c.Budget()))
	case res.Release != nil && !res.SwitchOffFallback:
		return r.As(deregReleasedInstead, "UE Context Release Command without a Deregistration Accept",
			joinDetail(deregAcceptFail, fmt.Sprintf("the release came %s after the request", res.Release.At.Sub(res.SentAt).Round(time.Millisecond))))
	}
	return r.As(deregNoAccept, fmt.Sprintf("no Deregistration Accept within %s", c.acceptTimeout()),
		joinDetail(deregAcceptFail, "nothing answered the request, so a switch-off Deregistration Request followed: "+releaseNote(res)))
}

// releaseNote says what a deregistration's result records about the UE
// Context Release.
func releaseNote(res *engine.DeregisterResult) string {
	switch {
	case res.Release != nil && res.ReleaseErr != nil:
		return fmt.Sprintf("the core released the UE context; %v", res.ReleaseErr)
	case res.Release != nil:
		return "the core released the UE context (UE Context Release Complete sent)"
	}
	return fmt.Sprintf("%v; the association was closed without it", res.ReleaseErr)
}

// ngapCtxReleaseAfterSwitchOff registers the UE, without a PDU session, and
// sends a switch-off Deregistration Request, which gets no Deregistration
// Accept (TS 24.501 5.5.2.2.2); TS 23.502 4.2.2.3.2 step 8 has the AMF send
// a UE CONTEXT RELEASE COMMAND for the UE-associated connection. TS 38.413
// 8.3.3.2 requires both UE NGAP IDs in the command when both are available,
// as they are after a registration. A command naming the connection's ID
// pair is PASS; one naming the UE by its AMF UE NGAP ID alone, or by no ID,
// is FAIL. No command for the connection within deregReleaseWait (none at
// all, or one for another ID pair) is FAIL on step 8: the step is the
// AMF's release of the N2 signalling connection for a deregistration over
// 3GPP access, conditional only on such a connection existing, which it
// does. The check owns the release after switch-off.
type ngapCtxReleaseAfterSwitchOff struct {
	// registerWait and releaseWait override deregRegisterWait and
	// deregReleaseWait (tests only).
	registerWait, releaseWait time.Duration
}

// The keys of the requirements the outcomes of
// NGAP-CTX-RELEASE-AFTER-SWITCH-OFF rest on: the UE NGAP IDs of the
// command, and the release itself.
const (
	switchOffReleaseRef     = "TS 38.413 8.3.3.2"
	switchOffReleaseStepRef = "TS 23.502 4.2.2.3.2 step 8"
)

// The outcomes of NGAP-CTX-RELEASE-AFTER-SWITCH-OFF. A release that does
// not come for the deregistered UE's connection is FAIL on TS 23.502
// 4.2.2.3.2 step 8, read as the requirement it states: with an N2
// signalling connection to NG-RAN, "the AMF sends N2 UE Release command to
// NG-RAN with Cause set to Deregistration to release N2 signalling
// connection", the procedure's only network action after a switch-off,
// which gets no DEREGISTRATION ACCEPT (step 7).
var (
	switchOffReleaseBothIDs = Outcome{ID: "both-ids", Verdict: Pass, Refs: []string{switchOffReleaseRef},
		Summary: "released the UE context after the switch-off, naming both UE NGAP IDs"}
	switchOffReleaseNoIDs = Outcome{ID: "no-ue-ngap-id", Verdict: Fail, Refs: []string{switchOffReleaseRef},
		Summary: "sent a UE CONTEXT RELEASE COMMAND carrying no UE NGAP ID"}
	switchOffReleaseAMFIDOnly = Outcome{ID: "amf-id-only", Verdict: Fail, Refs: []string{switchOffReleaseRef},
		Summary: "named the UE by its AMF UE NGAP ID alone, with both IDs available"}
	switchOffReleaseNone = Outcome{ID: "no-release", Verdict: Fail, Refs: []string{switchOffReleaseStepRef}, NotCompleted: ProcSwitchOffRelease,
		Bound:   &Bound{Wait: deregReleaseWait, Source: "check bound; neither TS 23.502 nor TS 38.413 sets a timer for the release"},
		Summary: "sent no UE CONTEXT RELEASE COMMAND after the switch-off deregistration"}
	switchOffReleaseOtherIDs = Outcome{ID: "release-for-other-ids", Verdict: Fail, Refs: []string{switchOffReleaseStepRef}, NotCompleted: ProcSwitchOffRelease,
		Summary: "released only another UE NGAP ID pair, not the deregistered UE's connection"}
)

// Outcomes declares what the check can report.
func (ngapCtxReleaseAfterSwitchOff) Outcomes() []Outcome {
	return []Outcome{switchOffReleaseBothIDs, switchOffReleaseNoIDs, switchOffReleaseAMFIDOnly, switchOffReleaseNone, switchOffReleaseOtherIDs}
}

func (ngapCtxReleaseAfterSwitchOff) ID() string { return "NGAP-CTX-RELEASE-AFTER-SWITCH-OFF" }
func (ngapCtxReleaseAfterSwitchOff) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.3.3.2", Strength: Shall, Since: 15, Verified: ts38413Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 15, Note: "IE-handling sentences are added after V15.0.0, and 8.3.3.1 is extended to mention releasing the old connection when the UE establishes a new one; the UE NGAP ID sentences are unchanged from V15.0.0 through V19.4.0"},
				{Release: 16, Note: "further IE-handling sentences are added; the UE NGAP ID sentences are unchanged"},
			}},
		{Spec: "TS 23.502", Clause: "4.2.2.3.2 step 8", Strength: Shall, Since: 15, Verified: ts23502Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 reads \"AMF sends N2 UE Release command to RAN\", \"to NG-RAN\" by V15.17.0; graded as a requirement: the step states the AMF's release of the N2 signalling connection in the indicative, with no alternative"},
				{Release: 16, Note: "the non-3GPP sentence names N3IWF/TNGF/W-AGF; the NG-RAN sentence is unchanged from V15.17.0 through V19.9.0"},
			}},
		{Spec: "TS 24.501", Clause: "5.5.2.2.2", Strength: Descriptive, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 titles the clause \"UE-initiated de-registration procedure completion - common procedure\", \"UE-initiated de-registration procedure completion\" by V15.7.0; the AMF sentences are unchanged from V15.0.0 through V19.8.0"},
			}},
	}
}

func (c ngapCtxReleaseAfterSwitchOff) registerTimeout() time.Duration {
	return orDefault(c.registerWait, deregRegisterWait)
}

func (c ngapCtxReleaseAfterSwitchOff) releaseTimeout() time.Duration {
	return orDefault(c.releaseWait, deregReleaseWait)
}

// Budget covers the registration, the release wait and the liveness probe.
func (c ngapCtxReleaseAfterSwitchOff) Budget() time.Duration {
	return c.registerTimeout() + c.releaseTimeout() + 5*time.Second
}

func (c ngapCtxReleaseAfterSwitchOff) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "after a switch-off Deregistration Request, a UE CONTEXT RELEASE COMMAND releasing the N2 signalling connection (TS 23.502 4.2.2.3.2 step 8), naming the connection by both UE NGAP IDs (TS 38.413 8.3.3.2)"}
	h, done := deregRegister(ctx, env, c.registerTimeout(), &r)
	if done {
		return r
	}
	defer h.Close()

	res, err := h.DeregisterWith(ctx, engine.WithReleaseWait(c.releaseTimeout()))
	r = judgeSwitchOffRelease(r, res, err, ctx.Err() != nil, c)
	if r.Verdict == Error {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the switch-off deregistration")
}

const (
	// switchOffNoRelease is what the check says of a core that sent no UE
	// CONTEXT RELEASE COMMAND for the connection.
	switchOffNoRelease = "no UE CONTEXT RELEASE COMMAND after switch-off deregistration. TS 23.502 4.2.2.3.2 step 8: with an N2 signalling connection to NG-RAN, the AMF sends N2 UE Release command with Cause set to Deregistration to release it. Likely cause: AMF leaves the N2 UE context."
	// switchOffReleaseFail is the FAIL message; the first verb says which
	// ID the command lacked.
	switchOffReleaseFail = "UE CONTEXT RELEASE COMMAND after switch-off deregistration %s, with both UE NGAP IDs available. TS 38.413 8.3.3.2. Likely cause: AMF builds the release without the UE NGAP ID pair."
)

// judgeSwitchOffRelease returns r with the result of a switch-off
// deregistration's outcome. cut reports that the check's own time ran out.
func judgeSwitchOffRelease(r Result, res *engine.DeregisterResult, err error, cut bool, c ngapCtxReleaseAfterSwitchOff) Result {
	if err != nil || res == nil {
		return r.Errored("Deregistration Request not sent", fmt.Sprintf("%v; the association was dropped with the UE still registered", err))
	}
	rel := res.Release
	conn := fmt.Sprintf("the connection is AMF UE NGAP ID %d, RAN UE NGAP ID %d", res.AMFUENGAPID, res.RANUENGAPID)
	switch {
	case errors.Is(res.ReleaseErr, engine.ErrReleaseCompleteNotSent):
		return r.Errored("UE CONTEXT RELEASE COMPLETE not sent",
			fmt.Sprintf("the core sent a UE CONTEXT RELEASE COMMAND, which was not judged: %v", res.ReleaseErr))
	case rel == nil && cut:
		return r.Errored("timed out before the release wait ended", fmt.Sprintf("raise --per-test-timeout to at least %s", c.Budget()))
	case rel == nil:
		return r.As(switchOffReleaseNone, fmt.Sprintf("no UE CONTEXT RELEASE COMMAND within %s", c.releaseTimeout()),
			joinDetail(switchOffNoRelease, res.ReleaseErr.Error()))
	}
	after := rel.At.Sub(res.SentAt).Round(time.Millisecond)
	switch {
	case !rel.HasAMFUENGAPID:
		return r.As(switchOffReleaseNoIDs, "UE CONTEXT RELEASE COMMAND without UE NGAP IDs",
			joinDetail(fmt.Sprintf(switchOffReleaseFail, "carries no UE NGAP ID"), conn))
	case !rel.HasRANUENGAPID && rel.AMFUENGAPID == res.AMFUENGAPID:
		return r.As(switchOffReleaseAMFIDOnly, "UE CONTEXT RELEASE COMMAND without the RAN UE NGAP ID",
			joinDetail(fmt.Sprintf(switchOffReleaseFail, fmt.Sprintf("names the UE by AMF UE NGAP ID %d alone", rel.AMFUENGAPID)), conn))
	case rel.AMFUENGAPID != res.AMFUENGAPID || (rel.HasRANUENGAPID && rel.RANUENGAPID != res.RANUENGAPID):
		named := fmt.Sprintf("AMF UE NGAP ID %d", rel.AMFUENGAPID)
		if rel.HasRANUENGAPID {
			named += fmt.Sprintf(", RAN UE NGAP ID %d", rel.RANUENGAPID)
		}
		return r.As(switchOffReleaseOtherIDs, "a UE CONTEXT RELEASE COMMAND for another UE NGAP ID",
			joinDetail(switchOffNoRelease, fmt.Sprintf("the command, %s after the request, names %s; %s, so it does not answer this deregistration, and none that does came", after, named, conn)))
	}
	return r.As(switchOffReleaseBothIDs, "UE CONTEXT RELEASE COMMAND naming both UE NGAP IDs",
		fmt.Sprintf("the command, %s after the request, names AMF UE NGAP ID %d and RAN UE NGAP ID %d, the connection's pair; UE Context Release Complete sent", after, rel.AMFUENGAPID, rel.RANUENGAPID))
}
