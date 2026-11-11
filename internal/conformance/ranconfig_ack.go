package conformance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/sctp"
)

func init() {
	builtins = append(builtins, ranConfigUpdateAck{})
}

// ranConfigAckWait bounds the wait for the RAN CONFIGURATION UPDATE's
// outcome. Its absence within this bound is what "no answer" reports, and
// the detail says so.
const ranConfigAckWait = 5 * time.Second

// ranConfigAckText is the detail of every outcome other than a RAN
// CONFIGURATION UPDATE ACKNOWLEDGE.
const ranConfigAckText = "no RAN CONFIGURATION UPDATE ACKNOWLEDGE (class-1 procedure, Table 8.1-1). TS 38.413 8.7.2.2 (descriptive; Rel-15 onward). " +
	"Likely cause: AMF does not implement RAN Configuration Update."

// Observed texts of NGAP-RAN-CONFIG-UPDATE-ACK, one per outcome.
const (
	ranConfigAckObserved    = "RAN CONFIGURATION UPDATE ACKNOWLEDGE"
	ranConfigAckFailure     = "RAN CONFIGURATION UPDATE FAILURE"
	ranConfigAckErrorInd    = "ERROR INDICATION"
	ranConfigAckNoAnswer    = "no answer; association survived"
	ranConfigAckAssociation = "association reset"
)

// ranConfigUpdateAck sends, on an association that has completed NG Setup,
// a valid RAN CONFIGURATION UPDATE that restates the Supported TA List the
// NG Setup presented, and reports how the AMF answers. TS 38.413 8.7.2.2
// describes the answer ("The AMF responds with a RAN CONFIGURATION UPDATE
// ACKNOWLEDGE message") and does not require it, and 8.7.2.3 requires a RAN
// CONFIGURATION UPDATE FAILURE only of an AMF that cannot accept the update,
// which cannot be told from outside. So a RAN CONFIGURATION UPDATE
// ACKNOWLEDGE is INFO, and anything else is WARN for availability, the
// procedure not completing for an update that changes nothing, with its own
// outcome: a RAN CONFIGURATION UPDATE FAILURE, an ERROR INDICATION, no
// answer within ranConfigAckWait, or a lost association. PDUs of other
// procedures read meanwhile are passed over. After any outcome but the
// ACKNOWLEDGE a fresh NG Setup is tried (observeAlive): a core that no
// longer completes it did not survive the update and is CRASH, which tells
// it from one that only ignored the update.
type ranConfigUpdateAck struct {
	// wait overrides ranConfigAckWait; tests shorten it.
	wait time.Duration
}

// ranConfigAckRef is the key of the description the acknowledged outcome
// rests on.
const ranConfigAckRef = "TS 38.413 8.7.2.2"

// The outcomes of NGAP-RAN-CONFIG-UPDATE-ACK. Every answer but the
// ACKNOWLEDGE is WARN for availability: no requirement covers it, and the
// core does not complete the procedure for a valid update that restates
// what it accepted at NG Setup.
var (
	ranConfigAcked = Outcome{ID: "acknowledged", Verdict: Info, Refs: []string{ranConfigAckRef},
		Summary: "answered the valid RAN CONFIGURATION UPDATE with RAN CONFIGURATION UPDATE ACKNOWLEDGE, as 8.7.2.2 describes"}
	ranConfigRejected = Outcome{ID: "failure", Verdict: Warn, Criterion: CriterionAvailability,
		Summary: "rejected the valid RAN CONFIGURATION UPDATE with RAN CONFIGURATION UPDATE FAILURE"}
	ranConfigErrorIndicated = Outcome{ID: "error-indication", Verdict: Warn, Criterion: CriterionAvailability,
		Summary: "answered the valid RAN CONFIGURATION UPDATE with an ERROR INDICATION"}
	ranConfigNoAnswer = Outcome{ID: "no-answer", Verdict: Warn, Criterion: CriterionAvailability,
		Bound:   &Bound{Wait: ranConfigAckWait, Source: "check bound; TS 38.413 sets no timer for the RAN Configuration Update outcome"},
		Summary: "sent neither RAN CONFIGURATION UPDATE ACKNOWLEDGE nor FAILURE; the association survived"}
	ranConfigAssociationLost = Outcome{ID: "association-lost", Verdict: Warn, Criterion: CriterionAvailability,
		Summary: "dropped the association instead of answering the valid RAN CONFIGURATION UPDATE"}
)

// Outcomes declares what the check can report.
func (ranConfigUpdateAck) Outcomes() []Outcome {
	return []Outcome{ranConfigAcked, ranConfigRejected, ranConfigErrorIndicated, ranConfigNoAnswer, ranConfigAssociationLost}
}

func (c ranConfigUpdateAck) outcomeWait() time.Duration {
	if c.wait > 0 {
		return c.wait
	}
	return ranConfigAckWait
}

func (ranConfigUpdateAck) ID() string         { return "NGAP-RAN-CONFIG-UPDATE-ACK" }
func (ranConfigUpdateAck) Category() Category { return Procedural }

// SpecRefs cites the description of the answer in 8.7.2.2. The shall in
// 8.7.2.3 and the Class 1 table (8.1) are in the provenance file only: no
// outcome rests on either.
func (ranConfigUpdateAck) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.7.2.2", Strength: Descriptive, Since: 15, Verified: ts38413Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 15, Note: "a sentence on IEs the update does not include follows the two cited ones in V15.13.0 (absent in V15.0.0); the cited ones are unchanged to V18.11.0"},
				{Release: 19, Note: "in V19.4.0 the same words are a bullet under \"If the RAN Configuration Update procedure is executed between the NG-RAN node and the AMF\""},
			}},
	}
}

func (c ranConfigUpdateAck) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "RAN CONFIGURATION UPDATE ACKNOWLEDGE for a valid RAN CONFIGURATION UPDATE (TS 38.413 8.7.2.2, descriptive)"}
	pdu, err := gnb.BuildRANConfigurationUpdate(gnb.WithSupportedTAList(env.GNB))
	if err != nil {
		return r.Errored("stimulus could not be built", err.Error())
	}
	conn, err := env.DialSetup(ctx)
	if err != nil {
		return r.SetupFailed("setup failed", err)
	}
	defer func() { _ = conn.Close() }()

	// RAN CONFIGURATION UPDATE is non-UE-associated: stream 0.
	if err := gnb.SendPDU(conn, sctp.StreamNonUEAssociated, pdu); err != nil {
		return r.Errored("stimulus could not be sent", err.Error())
	}
	sent := time.Now()
	wait := c.outcomeWait()
	rctx, cancel := context.WithTimeout(ctx, wait)
	resp, passed, err := readRANConfigOutcome(rctx, conn)
	cancel()
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
		return r.Errored("timed out waiting for the RAN Configuration Update outcome",
			fmt.Sprintf("the per-test timeout expired before the %s the check waits; raise --per-test-timeout", wait))
	}
	o, observed, detail := classifyRANConfigAck(resp, err, time.Since(sent), wait)
	if passed > 0 {
		detail = joinDetail(detail, fmt.Sprintf("%d PDUs of other procedures were read first", passed))
	}
	r = r.As(o, observed, joinDetail(detail, fmt.Sprintf("stimulus: RAN CONFIGURATION UPDATE restating the NG Setup's Supported TA List (TAC %d, PLMN %s/%s)",
		env.GNB.TAC, env.GNB.MCC, env.GNB.MNC)))
	if o.ID == ranConfigAcked.ID {
		return r
	}
	return observeAlive(ctx, env, r, "the RAN CONFIGURATION UPDATE")
}

// readRANConfigOutcome reads until a PDU that answers a RAN CONFIGURATION
// UPDATE arrives: its ACKNOWLEDGE, its FAILURE or an ERROR INDICATION.
// passed counts the PDUs of other procedures read before it.
func readRANConfigOutcome(ctx context.Context, conn gnb.Transport) (resp *ngapType.NGAPPDU, passed int, err error) {
	for {
		resp, err = gnb.ReadPDU(ctx, conn)
		if err != nil {
			return nil, passed, err
		}
		if gnb.IsRANConfigurationUpdateAcknowledge(resp) || gnb.IsRANConfigurationUpdateFailure(resp) || isErrorIndication(resp) {
			return resp, passed, nil
		}
		passed++
	}
}

// classifyRANConfigAck maps the AMF's answer to the valid RAN CONFIGURATION
// UPDATE onto an outcome; after is how long after the stimulus it came and
// wait the bound a missing answer is inferred within.
func classifyRANConfigAck(resp *ngapType.NGAPPDU, err error, after, wait time.Duration) (o Outcome, observed, detail string) {
	after = after.Round(time.Millisecond)
	switch {
	case err == nil && gnb.IsRANConfigurationUpdateAcknowledge(resp):
		return ranConfigAcked, ranConfigAckObserved, fmt.Sprintf("acknowledged %s after the RAN CONFIGURATION UPDATE", after)
	case err == nil && gnb.IsRANConfigurationUpdateFailure(resp):
		return ranConfigRejected, ranConfigAckFailure,
			joinDetail(fmt.Sprintf("answered with RAN CONFIGURATION UPDATE FAILURE, cause %s, %s after the RAN CONFIGURATION UPDATE", ranConfigFailureCause(resp), after), ranConfigAckText)
	case err == nil:
		return ranConfigErrorIndicated, ranConfigAckErrorInd,
			joinDetail(fmt.Sprintf("answered with ERROR INDICATION, cause %s, %s after the RAN CONFIGURATION UPDATE", errorIndicationCause(resp), after), ranConfigAckText)
	case errors.Is(err, context.DeadlineExceeded):
		return ranConfigNoAnswer, ranConfigAckNoAnswer,
			joinDetail(fmt.Sprintf("no RAN CONFIGURATION UPDATE ACKNOWLEDGE or FAILURE within %s of the RAN CONFIGURATION UPDATE (absence inferred within this bound)", wait), ranConfigAckText)
	default:
		return ranConfigAssociationLost, ranConfigAckAssociation,
			joinDetail(fmt.Sprintf("the association was lost %s after the RAN CONFIGURATION UPDATE (%v)", after, err), ranConfigAckText)
	}
}

// ranConfigFailureCause renders a RAN CONFIGURATION UPDATE FAILURE's Cause
// IE, "absent" without one.
func ranConfigFailureCause(pdu *ngapType.NGAPPDU) string {
	for _, ie := range pdu.UnsuccessfulOutcome.Value.RANConfigurationUpdateFailure.ProtocolIEs.List {
		if ie.Id.Value == ngapType.ProtocolIEIDCause && ie.Value.Cause != nil {
			return causeText(ie.Value.Cause)
		}
	}
	return "absent"
}
