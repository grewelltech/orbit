package conformance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
)

func init() {
	builtins = append(builtins, ngSetupCompletes{})
}

// ngSetupCompletes sends one valid NG SETUP REQUEST with the run's own
// identity (PLMN, tracking area, slices) and reports whether the AMF
// completes the NG Setup. It owns the NG Setup: a check whose own NG Setup
// the core does not complete reports BLOCKED, naming this check.
//
// TS 38.413 8.7.1.2 describes the answer ("The AMF responds with an NG
// SETUP RESPONSE message") without requiring it, so the response is INFO. A
// rejection (NG SETUP FAILURE) is the answer 8.7.1.3 asks of an AMF that
// cannot accept the setup; it violates nothing, and the core cannot serve
// the gNB the run presents: WARN, for availability. An AMF that neither
// accepts nor answers with NG SETUP FAILURE (no answer within ngSetupWait,
// another PDU, a lost association) has not accepted the setup and has not
// sent the failure 8.7.1.3 says it should: DEVIATION.
type ngSetupCompletes struct{}

// The keys of the references the outcomes of NGAP-NG-SETUP rest on.
const (
	ngSetupSuccessRef = "TS 38.413 8.7.1.2"
	ngSetupFailureRef = "TS 38.413 8.7.1.3"
)

// The outcomes of NGAP-NG-SETUP.
var (
	ngSetupAccepted = Outcome{ID: "accepted", Verdict: Info, Refs: []string{ngSetupSuccessRef},
		Summary: "answered the NG SETUP REQUEST with NG SETUP RESPONSE, as 8.7.1.2 describes"}
	// A rejection is WARN for availability: the core cannot complete the
	// procedure for the gNB identity the run presents. It follows the
	// should of 8.7.1.3, so it is not a DEVIATION.
	ngSetupRejected = Outcome{ID: "rejected", Verdict: Warn, Criterion: CriterionAvailability, NotCompleted: ProcNGSetup,
		Summary: "rejected the NG Setup with NG SETUP FAILURE"}
	ngSetupUnanswered = Outcome{ID: "unanswered", Verdict: Deviation, Refs: []string{ngSetupFailureRef}, NotCompleted: ProcNGSetup,
		Bound:   &Bound{Wait: ngSetupWait, Source: "check bound; TS 38.413 sets no timer for the NG Setup outcome"},
		Summary: "sent neither NG SETUP RESPONSE nor NG SETUP FAILURE"}
	ngSetupOtherPDU = Outcome{ID: "other-pdu", Verdict: Deviation, Refs: []string{ngSetupFailureRef}, NotCompleted: ProcNGSetup,
		Summary: "answered the NG SETUP REQUEST with a PDU that is neither NG SETUP RESPONSE nor NG SETUP FAILURE"}
	ngSetupAssociationLost = Outcome{ID: "association-lost", Verdict: Deviation, Refs: []string{ngSetupFailureRef}, NotCompleted: ProcNGSetup,
		Summary: "dropped the association instead of answering the NG SETUP REQUEST"}
)

func (ngSetupCompletes) ID() string { return "NGAP-NG-SETUP" }

// SpecRefs cites the description of the successful NG Setup and the should
// for an AMF that cannot accept one.
func (ngSetupCompletes) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.7.1.2", Strength: Descriptive, Since: 15, Verified: ts38413Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 19, Note: "in V19.4.0 the same words are a bullet under \"If the NG Setup procedure is executed between the NG-RAN node and the AMF\""},
			}},
		{Spec: "TS 38.413", Clause: "8.7.1.3", Strength: Should, Since: 15, Verified: ts38413Verified, WordingUnchanged: true},
	}
}

// Needs is no procedure: the NG Setup is the check's stimulus.
func (ngSetupCompletes) Needs() CheckNeeds { return rawNeeds }

// Owns is the NG Setup.
func (ngSetupCompletes) Owns() []Procedure { return []Procedure{ProcNGSetup} }

// Outcomes declares what the check can report.
func (ngSetupCompletes) Outcomes() []Outcome {
	return []Outcome{ngSetupAccepted, ngSetupRejected, ngSetupUnanswered, ngSetupOtherPDU, ngSetupAssociationLost}
}

func (ngSetupCompletes) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "NG SETUP RESPONSE for a valid NG SETUP REQUEST (TS 38.413 8.7.1.2, descriptive)"}
	conn, err := env.Dial(ctx)
	if err != nil {
		return r.Errored("dial failed", err.Error())
	}
	defer func() { _ = conn.Close() }()
	pdu, err := gnb.BuildNGSetupRequest(env.GNB)
	if err != nil {
		return r.Errored("NG SETUP REQUEST could not be built", err.Error())
	}
	if err := gnb.SendPDU(conn, 0, pdu); err != nil {
		return r.Errored("NG SETUP REQUEST could not be sent", err.Error())
	}
	sent := time.Now()
	rctx, cancel := context.WithTimeout(ctx, ngSetupWait)
	defer cancel()
	resp, err := gnb.ReadPDU(rctx, conn)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
		return r.Errored("timed out waiting for the NG Setup outcome",
			fmt.Sprintf("the per-test timeout expired before the %s the check waits; raise --per-test-timeout", ngSetupWait))
	}
	o, observed, detail := classifyNGSetup(resp, err, time.Since(sent))
	return r.As(o, observed, joinDetail(detail, fmt.Sprintf("presented gNB ID %d, PLMN %s/%s, TAC %d", env.GNB.ID, env.GNB.MCC, env.GNB.MNC, env.GNB.TAC)))
}

// classifyNGSetup maps the AMF's answer to a valid NG SETUP REQUEST onto an
// outcome; after is how long after the request the answer came.
func classifyNGSetup(resp *ngapType.NGAPPDU, err error, after time.Duration) (o Outcome, observed, detail string) {
	after = after.Round(time.Millisecond)
	switch {
	case err == nil && isNGSetupResponse(resp):
		return ngSetupAccepted, "NG SETUP RESPONSE", fmt.Sprintf("accepted %s after the NG SETUP REQUEST", after)
	case err == nil && isNGSetupFailure(resp):
		return ngSetupRejected, "NG SETUP FAILURE", fmt.Sprintf("rejected %s after the NG SETUP REQUEST, cause %s; "+
			"TS 38.413 8.7.1.3 lets an AMF that cannot accept the setup reject it, so no requirement is violated", after, ngSetupFailureCause(resp))
	case err == nil:
		return ngSetupOtherPDU, ngapName(resp), fmt.Sprintf("answered with %s %s after the NG SETUP REQUEST: neither NG SETUP RESPONSE nor the NG SETUP FAILURE of 8.7.1.3", ngapName(resp), after)
	case errors.Is(err, context.DeadlineExceeded):
		return ngSetupUnanswered, "no answer; association survived",
			fmt.Sprintf("neither NG SETUP RESPONSE nor NG SETUP FAILURE within %s of the NG SETUP REQUEST (absence inferred within this bound)", ngSetupWait)
	default:
		return ngSetupAssociationLost, "association reset", fmt.Sprintf("the association was lost %s after the NG SETUP REQUEST (%v)", after, err)
	}
}
