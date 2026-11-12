package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
)

func init() {
	builtins = append(builtins, ngapUECtxReleaseOnRequest{})
}

// ueCtxReleaseWait is the bound within which the UE CONTEXT RELEASE COMMAND
// is expected after the request.
const ueCtxReleaseWait = 5 * time.Second

// ueCtxReleaseFailure is the check's message for a missing command.
const ueCtxReleaseFailure = "no UE CONTEXT RELEASE COMMAND after UE CONTEXT RELEASE REQUEST. TS 38.413 8.3.2.2 (should; Rel-18 V18.1.0: split by cause). Likely cause: AMF ignores RAN-initiated release."

// ngapUECtxReleaseOnRequest registers a UE without a PDU session, sends a
// UE CONTEXT RELEASE REQUEST from its gNB with cause radio network "release
// due to NG-RAN generated reason", and expects the AMF to start the UE
// Context Release procedure: a UE CONTEXT RELEASE COMMAND, which the gNB
// answers with UE CONTEXT RELEASE COMPLETE.
//
// TS 38.413 8.3.2.2 says "should", so a missing command is a DEVIATION,
// never a FAIL. The cause is never "User inactivity": from V18.1.0 that
// cause makes the release conditional on there being no downlink
// signalling. A released UE is still registered; the check then
// deregisters it with an initial NAS message, and says in the detail how
// that went.
//
// The check owns the AN release: the checks that need a UE moved to
// CM-IDLE report BLOCKED, naming it, when the core does not release.
type ngapUECtxReleaseOnRequest struct {
	// wait overrides ueCtxReleaseWait (tests only).
	wait time.Duration
}

// ueCtxReleaseRef is the key of the should the check's outcomes rest on.
const ueCtxReleaseRef = "TS 38.413 8.3.2.2"

// The outcomes of NGAP-UE-CTX-RELEASE-ON-REQUEST.
var (
	ueCtxReleaseCommanded = Outcome{ID: "release-command", Verdict: Pass, Refs: []string{ueCtxReleaseRef},
		Summary: "answered the UE CONTEXT RELEASE REQUEST with a UE CONTEXT RELEASE COMMAND"}
	ueCtxReleaseNoCommand = Outcome{ID: "no-release-command", Verdict: Deviation, Refs: []string{ueCtxReleaseRef},
		Bound:        &Bound{Wait: ueCtxReleaseWait, Source: "check bound; TS 38.413 sets no timer for the command"},
		NotCompleted: ProcANRelease,
		Summary:      "sent no UE CONTEXT RELEASE COMMAND for the UE CONTEXT RELEASE REQUEST"}
	ueCtxReleaseAssociationLost = Outcome{ID: "association-lost", Verdict: Deviation, Refs: []string{ueCtxReleaseRef},
		NotCompleted: ProcANRelease,
		Summary:      "dropped the association instead of sending a UE CONTEXT RELEASE COMMAND"}
)

// Outcomes declares what the check can report.
func (ngapUECtxReleaseOnRequest) Outcomes() []Outcome {
	return []Outcome{ueCtxReleaseCommanded, ueCtxReleaseNoCommand, ueCtxReleaseAssociationLost}
}

func (ngapUECtxReleaseOnRequest) ID() string { return "NGAP-UE-CTX-RELEASE-ON-REQUEST" }
func (ngapUECtxReleaseOnRequest) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.3.2.2", Strength: Should, Since: 15, Verified: ts38413ReleaseRequestVerified, Primary: true,
			Changed: []SpecChange{
				{Release: 18, Note: "V18.1.0 splits the should by cause: with \"User inactivity\" the release should be initiated when there is no downlink signalling; Rel-15 to Rel-17 have one unconditional sentence"},
				{Release: 19, Note: "the cited text is unchanged; V19.1.0 adds a GW Context Release Indication sentence"},
			}},
	}
}

func (c ngapUECtxReleaseOnRequest) waitCommand() time.Duration {
	if c.wait > 0 {
		return c.wait
	}
	return ueCtxReleaseWait
}

func (c ngapUECtxReleaseOnRequest) Run(ctx context.Context, env Env) Result {
	wait := c.waitCommand()
	r := Result{Expected: fmt.Sprintf("UE CONTEXT RELEASE COMMAND within %s of a UE CONTEXT RELEASE REQUEST whose cause is not \"User inactivity\" (TS 38.413 8.3.2.2)", wait)}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run this check")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	res, err := h.RegisterWithProbe(ctx, nil)
	if err != nil {
		return r.SetupFailed("UE registration failed", fmt.Errorf("registration on gNB %#x: %w", env.GNB.ID, err))
	}
	if !res.Registered {
		return r.SetupFailed("UE registration failed", fmt.Errorf("registration on gNB %#x: not registered", env.GNB.ID))
	}
	rel, err := h.RequestContextRelease(ctx, wait)
	if err != nil {
		return releaseRequestFailed(r, err, ctx.Err())
	}
	// Deregister here rather than in Close, so the detail says how the
	// cleanup went; the check's own context may be nearly spent.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	derr := h.Deregister(dctx)
	cancel()
	return r.As(classifyUECtxRelease(rel, wait, derr))
}

// classifyUECtxRelease judges the answer to a UE CONTEXT RELEASE REQUEST:
// PASS with the command, DEVIATION without it. The detail records the
// command's cause, what else was read while waiting, and how the UE's
// deregistration afterwards went (derr).
func classifyUECtxRelease(rel engine.ContextReleaseResult, wait time.Duration, derr error) (Outcome, string, string) {
	var facts []string
	if rel.Released {
		facts = append(facts, "the command's cause is "+rel.Cause+" (recorded only)", "UE Context Release Complete sent")
	}
	if len(rel.Skipped) > 0 {
		facts = append(facts, "other PDUs while waiting: "+strings.Join(rel.Skipped, ", "))
	}
	how := "over its NG connection"
	if rel.Released {
		how = "with an initial NAS message, the UE having no NG connection"
	}
	if derr != nil {
		facts = append(facts, fmt.Sprintf("cleanup: the UE was not deregistered %s: %v", how, derr))
	} else {
		facts = append(facts, "cleanup: the UE was deregistered "+how)
	}
	detail := strings.Join(facts, "; ")
	if rel.Released {
		return ueCtxReleaseCommanded, fmt.Sprintf("UE CONTEXT RELEASE COMMAND after %s", rel.After.Round(time.Millisecond)), detail
	}
	return ueCtxReleaseNoCommand, fmt.Sprintf("no UE CONTEXT RELEASE COMMAND within %s (absence inferred within this bound)", wait), ueCtxReleaseFailure + " Recorded: " + detail
}

// releaseRequestFailed is the result when the UE CONTEXT RELEASE REQUEST
// could not be sent or its wait ended in an error: the per-test timeout
// (ctxErr set) is ERROR; an association read failure while waiting for the
// command (engine.ErrReleaseWaitLost) is the core dropping the association
// (DEVIATION); a command whose UE CONTEXT RELEASE COMPLETE could not be
// sent (engine.ErrReleaseCompleteNotSent) and a request that could not be
// sent are ERROR.
func releaseRequestFailed(r Result, err, ctxErr error) Result {
	switch {
	case ctxErr != nil:
		return r.Errored("timed out waiting for the UE CONTEXT RELEASE COMMAND",
			fmt.Sprintf("%v; the per-test timeout expired during the wait; raise --per-test-timeout", err))
	case errors.Is(err, engine.ErrReleaseWaitLost):
		return r.As(ueCtxReleaseAssociationLost, "association lost waiting for the UE CONTEXT RELEASE COMMAND",
			ueCtxReleaseFailure+" Recorded: "+err.Error())
	case errors.Is(err, engine.ErrReleaseCompleteNotSent):
		return r.Errored("UE CONTEXT RELEASE COMPLETE could not be sent",
			"the core sent the UE CONTEXT RELEASE COMMAND, which was not judged: "+err.Error())
	}
	return r.Errored("UE CONTEXT RELEASE REQUEST could not be sent", err.Error())
}
