package conformance

import (
	"context"
	"fmt"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
)

func init() {
	builtins = append(builtins, nasDeregAcceptNotSwitchOff{})
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
// Command in its place, is FAIL. The release that follows the Accept is
// recorded, not judged. A core that answered with neither is sent a
// switch-off Deregistration Request before the association closes, so it is
// not left holding the UE.
type nasDeregAcceptNotSwitchOff struct {
	// registerWait, acceptWait and releaseWait override deregRegisterWait,
	// deregAcceptWait and deregReleaseWait (tests only).
	registerWait, acceptWait, releaseWait time.Duration
}

func (nasDeregAcceptNotSwitchOff) ID() string         { return "NAS-DEREG-ACCEPT-NOT-SWITCH-OFF" }
func (nasDeregAcceptNotSwitchOff) Category() Category { return Procedural }
func (nasDeregAcceptNotSwitchOff) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.5.2.2.2", Strength: Shall, Since: 15, Verified: ts24501Verified,
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
	h, done := deregHarness(ctx, env, c.registerTimeout(), &r)
	if done {
		return r
	}
	defer h.Close()

	res, err := h.DeregisterWith(ctx, engine.WithNormalDeregistration(c.acceptTimeout()), engine.WithReleaseWait(c.releaseTimeout()))
	r.Verdict, r.Observed, r.Detail = judgeDeregAccept(res, err, ctx.Err() != nil, c)
	if r.Verdict == Error {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the deregistration")
}

// deregHarness registers env's UE, without a PDU session, for a
// deregistration check. done is true when r already holds the result: SKIP
// without a UE, ERROR when the harness or the registration failed.
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

// judgeDeregAccept turns a normal deregistration's outcome into a verdict,
// Observed and Detail. cut reports that the check's own time ran out.
func judgeDeregAccept(res *engine.DeregisterResult, err error, cut bool, c nasDeregAcceptNotSwitchOff) (Verdict, string, string) {
	switch {
	case err != nil || res == nil:
		return Error, "Deregistration Request not sent", fmt.Sprintf("%v; the association was dropped with the UE still registered", err)
	case res.Accepted:
		return Pass, "Deregistration Accept received",
			fmt.Sprintf("%s after the request; the release is recorded, not judged: %s", res.AcceptAt.Sub(res.SentAt).Round(time.Millisecond), releaseNote(res))
	case cut:
		return Error, "timed out before the Deregistration Accept wait ended",
			fmt.Sprintf("raise --per-test-timeout to at least %s", c.Budget())
	case res.Release != nil && !res.SwitchOffFallback:
		return Fail, "UE Context Release Command without a Deregistration Accept",
			joinDetail(deregAcceptFail, fmt.Sprintf("the release came %s after the request", res.Release.At.Sub(res.SentAt).Round(time.Millisecond)))
	}
	return Fail, fmt.Sprintf("no Deregistration Accept within %s", c.acceptTimeout()),
		joinDetail(deregAcceptFail, "nothing answered the request, so a switch-off Deregistration Request followed: "+releaseNote(res))
}

// releaseNote says what a deregistration's result records about the UE
// Context Release.
func releaseNote(res *engine.DeregisterResult) string {
	if res.Release != nil {
		return "the core released the UE context (UE Context Release Complete sent)"
	}
	return fmt.Sprintf("%v; the association was closed without it", res.ReleaseErr)
}
