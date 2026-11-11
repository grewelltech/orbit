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
// which cannot be told from outside. So the check is informational: a RAN
// CONFIGURATION UPDATE ACKNOWLEDGE is INFO, and anything else is INFO-DIFF
// with Observed saying which: a RAN CONFIGURATION UPDATE FAILURE, an ERROR
// INDICATION, no answer within ranConfigAckWait, or a lost association.
// PDUs of other procedures read meanwhile are passed over. After any
// outcome but the ACKNOWLEDGE a fresh NG Setup is tried (observeAlive): a
// core that no longer completes it did not survive the update and is FAIL,
// which tells it from one that only ignored the update.
type ranConfigUpdateAck struct {
	// wait overrides ranConfigAckWait; tests shorten it.
	wait time.Duration
}

func (c ranConfigUpdateAck) outcomeWait() time.Duration {
	if c.wait > 0 {
		return c.wait
	}
	return ranConfigAckWait
}

func (ranConfigUpdateAck) ID() string         { return "NGAP-RAN-CONFIG-UPDATE-ACK" }
func (ranConfigUpdateAck) Category() Category { return Procedural }

// Informational declares the check informational: it cites descriptive
// text only.
func (ranConfigUpdateAck) Informational() bool { return true }

// SpecRefs cites the description of the answer in 8.7.2.2. The shall in
// 8.7.2.3 and the Class 1 table (8.1) are in the provenance file only: the
// check grades neither.
func (ranConfigUpdateAck) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.7.2.2", Strength: Descriptive, Since: 15, Verified: ts38413Verified,
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
		r.Verdict, r.Observed, r.Detail = Error, "stimulus could not be built", err.Error()
		return r
	}
	conn, err := env.DialSetup(ctx)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "setup failed", err.Error()
		return r
	}
	defer func() { _ = conn.Close() }()

	// RAN CONFIGURATION UPDATE is non-UE-associated: stream 0.
	if err := gnb.SendPDU(conn, sctp.StreamNonUEAssociated, pdu); err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "stimulus could not be sent", err.Error()
		return r
	}
	sent := time.Now()
	wait := c.outcomeWait()
	rctx, cancel := context.WithTimeout(ctx, wait)
	resp, passed, err := readRANConfigOutcome(rctx, conn)
	cancel()
	r.Verdict, r.Observed, r.Detail = classifyRANConfigAck(resp, err, time.Since(sent), wait)
	if passed > 0 {
		r.Detail = joinDetail(r.Detail, fmt.Sprintf("%d PDUs of other procedures were read first", passed))
	}
	r.Detail = joinDetail(r.Detail, fmt.Sprintf("stimulus: RAN CONFIGURATION UPDATE restating the NG Setup's Supported TA List (TAC %d, PLMN %s/%s)",
		env.GNB.TAC, env.GNB.MCC, env.GNB.MNC))
	if r.Verdict == Info {
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
// UPDATE onto the informational verdicts; after is how long after the
// stimulus it came and wait the bound a missing answer is inferred within.
func classifyRANConfigAck(resp *ngapType.NGAPPDU, err error, after, wait time.Duration) (v Verdict, observed, detail string) {
	after = after.Round(time.Millisecond)
	switch {
	case err == nil && gnb.IsRANConfigurationUpdateAcknowledge(resp):
		return Info, ranConfigAckObserved, fmt.Sprintf("acknowledged %s after the RAN CONFIGURATION UPDATE", after)
	case err == nil && gnb.IsRANConfigurationUpdateFailure(resp):
		return infoDiffers, ranConfigAckFailure,
			joinDetail(fmt.Sprintf("answered with RAN CONFIGURATION UPDATE FAILURE, cause %s, %s after the RAN CONFIGURATION UPDATE", ranConfigFailureCause(resp), after), ranConfigAckText)
	case err == nil:
		return infoDiffers, ranConfigAckErrorInd,
			joinDetail(fmt.Sprintf("answered with ERROR INDICATION, cause %s, %s after the RAN CONFIGURATION UPDATE", errorIndicationCause(resp), after), ranConfigAckText)
	case errors.Is(err, context.DeadlineExceeded):
		return infoDiffers, ranConfigAckNoAnswer,
			joinDetail(fmt.Sprintf("no RAN CONFIGURATION UPDATE ACKNOWLEDGE or FAILURE within %s of the RAN CONFIGURATION UPDATE (absence inferred within this bound)", wait), ranConfigAckText)
	default:
		return infoDiffers, ranConfigAckAssociation,
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
