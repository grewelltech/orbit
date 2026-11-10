package conformance

import (
	"context"
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
type ngapUECtxReleaseOnRequest struct {
	// wait overrides ueCtxReleaseWait (tests only).
	wait time.Duration
}

func (ngapUECtxReleaseOnRequest) ID() string         { return "NGAP-UE-CTX-RELEASE-ON-REQUEST" }
func (ngapUECtxReleaseOnRequest) Category() Category { return Procedural }
func (ngapUECtxReleaseOnRequest) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.3.2.2", Strength: Should, Since: 15, Verified: ts38413ReleaseRequestVerified,
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
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run this check"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	res, err := h.RegisterWithProbe(ctx, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "UE registration failed", fmt.Sprintf("registration on gNB %#x: %v", env.GNB.ID, err)
		return r
	}
	if !res.Registered {
		r.Verdict, r.Observed, r.Detail = Error, "UE registration failed", fmt.Sprintf("registration on gNB %#x: not registered", env.GNB.ID)
		return r
	}
	rel, err := h.RequestContextRelease(ctx, wait)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "UE Context Release Request could not be sent or waited for", err.Error()
		return r
	}
	// Deregister here rather than in Close, so the detail says how the
	// cleanup went; the check's own context may be nearly spent.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	derr := h.Deregister(dctx)
	cancel()
	r.Verdict, r.Observed, r.Detail = classifyUECtxRelease(rel, wait, derr)
	return r
}

// classifyUECtxRelease judges the answer to a UE CONTEXT RELEASE REQUEST:
// PASS with the command, DEVIATION without it. The detail records the
// command's cause, what else was read while waiting, and how the UE's
// deregistration afterwards went (derr).
func classifyUECtxRelease(rel engine.ContextReleaseResult, wait time.Duration, derr error) (Verdict, string, string) {
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
		return Pass, fmt.Sprintf("UE CONTEXT RELEASE COMMAND after %s", rel.After.Round(time.Millisecond)), detail
	}
	return Deviation, fmt.Sprintf("no UE CONTEXT RELEASE COMMAND within %s", wait), ueCtxReleaseFailure + " Recorded: " + detail
}
