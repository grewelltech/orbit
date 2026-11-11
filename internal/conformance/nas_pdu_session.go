package conformance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bgrewell/orbit/internal/nas"
)

func init() {
	builtins = append(builtins, nasPDUSessionEstablishment{})
}

const (
	// pduSessionRegisterWait bounds the registration, up to the PDU Session
	// Establishment Request.
	pduSessionRegisterWait = 10 * time.Second
	// pduSessionAnswerWait bounds the wait for the answer to the request
	// and, after a PDU SESSION ESTABLISHMENT ACCEPT, for the session's
	// setup on N2.
	pduSessionAnswerWait = 10 * time.Second
	// pduSessionDeregisterWait bounds the deregistration that ends the
	// check.
	pduSessionDeregisterWait = 5 * time.Second
)

// nasPDUSessionEstablishment registers one UE and requests a PDU session
// with the run's DNN and slice, as every check that needs a session does,
// and reports whether the core completes the establishment: a PDU SESSION
// ESTABLISHMENT ACCEPT and the session's setup on N2. It owns the PDU
// session establishment: a check whose own session the core does not
// establish reports BLOCKED, naming this check.
//
// TS 24.501 6.4.1.3 and 6.4.1.4.1 leave the SMF two answers: a PDU SESSION
// ESTABLISHMENT ACCEPT when it accepts the connectivity, a PDU SESSION
// ESTABLISHMENT REJECT when it rejects it. The Accept with the session set
// up is PASS. Neither answer (silence, a release, another 5GSM message) is
// FAIL: a network that did not accept the request has rejected it, and
// shall then send the REJECT. A REJECT for the run's own DNN and slice
// violates nothing, since no shall obliges the SMF to accept, and leaves
// the core unable to complete the procedure for the run's configuration:
// WARN for availability. So is the request returned un-forwarded by the
// AMF, and an Accept with no session set up on N2. The UE is deregistered
// at the end.
type nasPDUSessionEstablishment struct {
	// registerWait and answerWait override pduSessionRegisterWait and
	// pduSessionAnswerWait (tests only).
	registerWait, answerWait time.Duration
}

// The keys of the shalls the outcomes of NAS-PDU-SESSION-ESTABLISHMENT rest
// on.
const (
	pduSessionAcceptRef = "TS 24.501 6.4.1.3"
	pduSessionRejectRef = "TS 24.501 6.4.1.4.1 (6.4.1.4 in V15.1.0 to V15.7.0)"
)

// pduSessionAnswerBound is the bound an unanswered request is judged
// against.
var pduSessionAnswerBound = &Bound{Wait: pduSessionAnswerWait, Source: "check bound; T3580, which the UE would retransmit on, is longer"}

// The outcomes of NAS-PDU-SESSION-ESTABLISHMENT. A REJECT, the request
// returned un-forwarded and an Accept without the session's setup on N2 are
// WARN for availability: no shall obliges the network to accept the
// request, and the core cannot complete the procedure for the run's own
// DNN and slice. A downlink whose MAC fails is WARN for interoperability: a
// conformant UE discards it.
var (
	pduSessionEstablished = Outcome{ID: "established", Verdict: Pass, Refs: []string{pduSessionAcceptRef},
		Summary: "accepted the PDU session with a PDU SESSION ESTABLISHMENT ACCEPT and set it up on N2"}
	pduSessionRejected = Outcome{ID: "rejected", Verdict: Warn, Criterion: CriterionAvailability, NotCompleted: ProcPDUSession,
		Summary: "rejected the PDU session for the run's own DNN and slice with a PDU SESSION ESTABLISHMENT REJECT"}
	pduSessionNotForwarded = Outcome{ID: "not-forwarded", Verdict: Warn, Criterion: CriterionAvailability, NotCompleted: ProcPDUSession,
		Summary: "returned the request un-forwarded, with a 5GMM cause, for the run's own DNN and slice"}
	pduSessionNoUserPlane = Outcome{ID: "no-user-plane", Verdict: Warn, Criterion: CriterionAvailability, NotCompleted: ProcPDUSession, Bound: pduSessionAnswerBound,
		Summary: "sent a PDU SESSION ESTABLISHMENT ACCEPT and did not set the session up on N2"}
	pduSessionIntegrityFailed = Outcome{ID: "integrity-failed", Verdict: Warn, Criterion: CriterionInteroperability, NotCompleted: ProcPDUSession,
		Summary: "answered the request with a downlink NAS message that fails the integrity check"}
	pduSessionUnanswered = Outcome{ID: "unanswered", Verdict: Fail, Refs: []string{pduSessionAcceptRef, pduSessionRejectRef}, NotCompleted: ProcPDUSession, Bound: pduSessionAnswerBound,
		Summary: "sent neither a PDU SESSION ESTABLISHMENT ACCEPT nor a PDU SESSION ESTABLISHMENT REJECT"}
	pduSessionReleased = Outcome{ID: "released", Verdict: Fail, Refs: []string{pduSessionAcceptRef, pduSessionRejectRef}, NotCompleted: ProcPDUSession,
		Summary: "released the UE instead of answering the PDU SESSION ESTABLISHMENT REQUEST"}
	pduSessionOtherAnswer = Outcome{ID: "other-answer", Verdict: Fail, Refs: []string{pduSessionRejectRef}, NotCompleted: ProcPDUSession,
		Summary: "answered with a 5GSM message that is neither a PDU SESSION ESTABLISHMENT ACCEPT nor a PDU SESSION ESTABLISHMENT REJECT"}
)

func (nasPDUSessionEstablishment) ID() string         { return "NAS-PDU-SESSION-ESTABLISHMENT" }
func (nasPDUSessionEstablishment) Category() Category { return Procedural }

// SpecRefs cites the SMF's two answers to a UE-requested PDU session
// establishment.
func (nasPDUSessionEstablishment) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "6.4.1.3", Strength: Shall, Since: 15, Verified: ts24501Verified, Primary: true, WordingUnchanged: true},
		{Spec: "TS 24.501", Clause: "6.4.1.4.1 (6.4.1.4 in V15.1.0 to V15.7.0)", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 has 6.4.1.4.1; in V15.7.0 the text sits directly in 6.4.1.4; V15.x reads \"a SM PDU SESSION ESTABLISHMENT REJECT\""},
				{Release: 16, Note: "6.4.1.4.1 again, without \"SM\"; the sentence is otherwise unchanged through V19.8.0"},
			}},
	}
}

// Needs is one UE, registered: the PDU session is the check's stimulus.
func (nasPDUSessionEstablishment) Needs() CheckNeeds { return ueNeeds }

// Owns is the PDU session establishment.
func (nasPDUSessionEstablishment) Owns() []Procedure { return []Procedure{ProcPDUSession} }

// Outcomes declares what the check can report.
func (nasPDUSessionEstablishment) Outcomes() []Outcome {
	return []Outcome{pduSessionEstablished, pduSessionRejected, pduSessionNotForwarded, pduSessionNoUserPlane, pduSessionIntegrityFailed,
		pduSessionUnanswered, pduSessionReleased, pduSessionOtherAnswer}
}

func (c nasPDUSessionEstablishment) registerTimeout() time.Duration {
	return orDefault(c.registerWait, pduSessionRegisterWait)
}

func (c nasPDUSessionEstablishment) answerTimeout() time.Duration {
	return orDefault(c.answerWait, pduSessionAnswerWait)
}

// Budget covers the registration, the answer, the deregistration and the
// liveness probe.
func (c nasPDUSessionEstablishment) Budget() time.Duration {
	return c.registerTimeout() + c.answerTimeout() + pduSessionDeregisterWait + 5*time.Second
}

func (c nasPDUSessionEstablishment) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "a PDU SESSION ESTABLISHMENT ACCEPT and the session set up on N2 for the run's DNN and slice (TS 24.501 6.4.1.3); a network that does not accept the request answers with a PDU SESSION ESTABLISHMENT REJECT (6.4.1.4.1)"}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run the checks that register a UE")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()

	o := probeSession(ctx, h, h.pdu, c.registerTimeout(), c.answerTimeout(), false)
	if o.setupErr != nil {
		r = r.SetupFailed("UE registration failed", o.setupErr)
	} else {
		out, observed, detail := classifyPDUSession(o, c.answerTimeout())
		r = asClassified(r, out, observed, joinDetail(detail, fmt.Sprintf("DNN %q, slice SST %d SD %q", h.pdu.DNN, h.pdu.SST, h.pdu.SD)))
	}
	if o.registered {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pduSessionDeregisterWait)
		derr := h.Deregister(dctx)
		cancel()
		if derr != nil {
			r.Detail = joinDetail(r.Detail, fmt.Sprintf("deregistration failed (%v); the association was dropped", derr))
		} else {
			r.Detail = joinDetail(r.Detail, "UE deregistered")
		}
	} else if o.sent != nil {
		r.Detail = joinDetail(r.Detail, "the attach failed after the request, so its association closed without a deregistration")
	}
	if r.Verdict == Error && ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the PDU session establishment")
}

// classifyPDUSession judges the answer to a PDU SESSION ESTABLISHMENT
// REQUEST for the run's own DNN and slice; wait is the answer wait. An
// attach that failed before the request (setupErr) is the caller's to
// report.
func classifyPDUSession(o dnnOutcome, wait time.Duration) (Outcome, string, string) {
	switch {
	case o.err != nil:
		return classifiedError, "registration or harness failure", o.err.Error()
	case o.regTimedOut:
		return classifiedBlocked(ProcRegistration), "registration did not reach the PDU Session Establishment Request",
			fmt.Sprintf("the UE did not register within %s, so the request was not sent", pduSessionRegisterWait)
	case o.accepted:
		return pduSessionEstablished, "PDU session established",
			"PDU SESSION ESTABLISHMENT ACCEPT, and the session set up on N2 (PDU SESSION RESOURCE SETUP REQUEST answered)"
	case o.released:
		return pduSessionReleased, "UE released after the request, with no answer",
			"a UE CONTEXT RELEASE COMMAND came, and neither a PDU SESSION ESTABLISHMENT ACCEPT nor a REJECT (TS 24.501 6.4.1.3, 6.4.1.4.1)"
	case o.decodeErr != nil && errors.Is(o.decodeErr, nas.ErrIntegrity):
		return pduSessionIntegrityFailed, "downlink NAS after the request failed integrity check" + typeNote(o.decodeType),
			fmt.Sprintf("%v; NAS %x; a UE discards a message whose MAC fails, so the session cannot complete", o.decodeErr, o.decodeRaw)
	case o.decodeErr != nil:
		return classifiedError, "downlink NAS after the request did not decode" + typeNote(o.decodeType), fmt.Sprintf("%v; %s; NAS %x", o.decodeErr, codecGap, o.decodeRaw)
	case o.noUserPlane:
		return pduSessionNoUserPlane, "PDU SESSION ESTABLISHMENT ACCEPT without the session's setup on N2",
			fmt.Sprintf("the Accept came, and no PDU SESSION RESOURCE SETUP REQUEST for the session within %s of the request", wait)
	case o.noAnswer:
		detail := fmt.Sprintf("neither a PDU SESSION ESTABLISHMENT ACCEPT nor a REJECT within %s of the request: a network that does not accept the connectivity shall reject it (TS 24.501 6.4.1.4.1)", wait)
		if o.mmStatus {
			detail += fmt.Sprintf("; a 5GMM STATUS with 5GMM cause %s came", mmCause(o.mmStatusC))
		}
		return pduSessionUnanswered, fmt.Sprintf("no answer within %s", wait), detail
	}
	a := o.answer
	sm := readSM(a.payload)
	switch {
	case a.mmCause != 0:
		return pduSessionNotForwarded, fmt.Sprintf("request not forwarded, 5GMM cause %s", mmCause(a.mmCause)),
			fmt.Sprintf("a DL NAS Transport with 5GMM cause %s carrying %s: the AMF did not forward the request to an SMF for the run's own DNN and slice", mmCause(a.mmCause), sm.name)
	case sm.kind == smReject:
		return pduSessionRejected, sm.name,
			"TS 24.501 6.4.1.4.1 has the SMF answer a request it rejects with this message, so no requirement is violated; no shall obliges it to accept the run's own DNN and slice, and no session was established"
	case sm.kind == smUndecodable:
		return classifiedError, "answer did not decode", fmt.Sprintf("a DL NAS Transport carrying a %s that did not decode: %v; %s; payload %x", sm.name, sm.decodeErr, codecGap, a.payload)
	}
	return pduSessionOtherAnswer, "unexpected answer: " + sm.name,
		fmt.Sprintf("a DL NAS Transport carrying %s: neither the PDU SESSION ESTABLISHMENT ACCEPT nor the REJECT a network that does not accept the request shall send (TS 24.501 6.4.1.4.1)", sm.name)
}
