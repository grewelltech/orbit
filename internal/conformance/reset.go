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
	builtins = append(builtins, ngResetAcknowledged{})
}

// ngResetAcknowledged sends a well-formed NG RESET (reset the whole NG
// interface) and asserts the AMF completes the Reset procedure by replying with
// NG RESET ACKNOWLEDGE. TS 38.413 8.7.4.2.2 requires the acknowledge ("the
// AMF ... shall respond with the NG RESET ACKNOWLEDGE message"), so another
// PDU, no answer within ngResetWait or a lost association is FAIL.
type ngResetAcknowledged struct{}

// ngResetWait is how long the check waits for the NG RESET ACKNOWLEDGE; its
// absence is inferred within this bound.
const ngResetWait = 5 * time.Second

// ngResetRef is the key of the shall every outcome of the check rests on.
const ngResetRef = "TS 38.413 8.7.4.2.2"

// The outcomes of NGAP-NG-RESET-ACK.
var (
	ngResetAcked = Outcome{ID: "acknowledged", Verdict: Pass, Refs: []string{ngResetRef},
		Summary: "answered the NG RESET with NG RESET ACKNOWLEDGE"}
	ngResetOtherPDU = Outcome{ID: "other-pdu", Verdict: Fail, Refs: []string{ngResetRef},
		Summary: "answered the NG RESET with another PDU"}
	ngResetNoAnswer = Outcome{ID: "no-answer", Verdict: Fail, Refs: []string{ngResetRef},
		Bound:   &Bound{Wait: ngResetWait, Source: "check bound; TS 38.413 sets no timer for the acknowledge"},
		Summary: "sent no NG RESET ACKNOWLEDGE"}
	ngResetAssociationLost = Outcome{ID: "association-lost", Verdict: Fail, Refs: []string{ngResetRef},
		Summary: "dropped the association instead of acknowledging the NG RESET"}
)

// Outcomes declares what the check can report.
func (ngResetAcknowledged) Outcomes() []Outcome {
	return []Outcome{ngResetAcked, ngResetOtherPDU, ngResetNoAnswer, ngResetAssociationLost}
}

func (ngResetAcknowledged) ID() string         { return "NGAP-NG-RESET-ACK" }
func (ngResetAcknowledged) Category() Category { return Procedural }

// SpecRefs cites the shall in 8.7.4.2.2. The NG RESET and NG RESET
// ACKNOWLEDGE message clauses (9.2.6.11, 9.2.6.12) are cited in the
// provenance file only: their numbering was verified V15.0.0 to V19.4.0 but
// their IE tables were not compared release by release.
func (ngResetAcknowledged) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.7.4.2.2", Strength: Shall, Since: 15, Verified: ts38413Verified, WordingUnchanged: true, Primary: true},
	}
}

func (ngResetAcknowledged) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "NG RESET ACKNOWLEDGE (Reset procedure completes)"}
	conn, err := env.DialSetup(ctx)
	if err != nil {
		return r.SetupFailed("setup failed", err)
	}
	defer conn.Close()

	// NG RESET is non-UE-associated → stream 0.
	if err := gnb.SendPDU(conn, 0, gnb.BuildNGReset()); err != nil {
		return r.Errored("NG RESET could not be sent", err.Error())
	}

	rctx, cancel := context.WithTimeout(ctx, ngResetWait)
	defer cancel()
	resp, err := gnb.ReadPDU(rctx, conn)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
		return r.Errored("timed out waiting for the NG RESET ACKNOWLEDGE",
			fmt.Sprintf("the per-test timeout expired before the %s the check waits for the acknowledge; raise --per-test-timeout", ngResetWait))
	}
	return r.As(classifyNGReset(resp, err))
}

// classifyNGReset maps the AMF's reaction to the NG RESET onto an outcome:
// the acknowledge 8.7.4.2.2 requires, another PDU, no answer within
// ngResetWait, or a lost association.
func classifyNGReset(resp *ngapType.NGAPPDU, err error) (o Outcome, observed, detail string) {
	switch {
	case err == nil && isNGResetAcknowledge(resp):
		return ngResetAcked, "NG RESET ACKNOWLEDGE", ""
	case err == nil:
		return ngResetOtherPDU, ngapName(resp), "expected NG RESET ACKNOWLEDGE"
	case errors.Is(err, context.DeadlineExceeded):
		return ngResetNoAnswer, "no response",
			fmt.Sprintf("no NG RESET ACKNOWLEDGE within %s of the NG RESET (absence inferred within this bound)", ngResetWait)
	default:
		return ngResetAssociationLost, "association reset", err.Error()
	}
}

// isNGResetAcknowledge reports whether pdu is an NGAP NG RESET ACKNOWLEDGE.
func isNGResetAcknowledge(pdu *ngapType.NGAPPDU) bool {
	return pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome &&
		pdu.SuccessfulOutcome != nil &&
		pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentNGResetAcknowledge
}
