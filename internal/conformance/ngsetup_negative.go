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
	builtins = append(builtins, ngSetupMissingMandatoryIE{})
}

// ngSetupMissingMandatoryIE sends an NG SETUP REQUEST with the mandatory
// Supported TA List IE (id-102) removed. It is mandatory in the NG SETUP
// message contents (TS 38.413 9.2.6.1), and 10.3.5 requires the AMF to reject
// the procedure with NG SETUP FAILURE.
//
// NG SETUP FAILURE is PASS. Everything else misses the shall and is FAIL,
// with its own outcome: acceptance (NG SETUP RESPONSE), another PDU (an
// ERROR INDICATION included), no answer within ngSetupWait, or an
// association reset. A fresh NG Setup afterwards confirms the core
// (confirmAlive): one that no longer completes is CRASH.
type ngSetupMissingMandatoryIE struct{}

// ngSetupWait is how long the check waits for the NG SETUP FAILURE. A silent
// drop is inferred from its absence within this bound, and the detail says
// so.
const ngSetupWait = 4 * time.Second

// missingTAListRef is the key of the shall every outcome of the check rests
// on.
const missingTAListRef = "TS 38.413 10.3.5 Reject IE bullet 1"

// The outcomes of NGAP-NGSETUP-MISSING-TALIST.
var (
	missingTAListRejected = Outcome{ID: "setup-failure", Verdict: Pass, Refs: []string{missingTAListRef},
		Summary: "rejected the NG SETUP REQUEST missing the Supported TA List with NG SETUP FAILURE"}
	missingTAListAccepted = Outcome{ID: "accepted", Verdict: Fail, Refs: []string{missingTAListRef},
		Summary: "accepted the NG SETUP REQUEST missing the Supported TA List"}
	missingTAListOtherPDU = Outcome{ID: "other-pdu", Verdict: Fail, Refs: []string{missingTAListRef},
		Summary: "answered with another PDU instead of NG SETUP FAILURE; the association survived"}
	missingTAListSilent = Outcome{ID: "silent-drop", Verdict: Fail, Refs: []string{missingTAListRef},
		Bound:   &Bound{Wait: ngSetupWait, Source: "check bound; TS 38.413 sets no timer for the NG Setup outcome"},
		Summary: "sent no NG SETUP FAILURE; the association survived"}
	missingTAListReset = Outcome{ID: "association-reset", Verdict: Fail, Refs: []string{missingTAListRef},
		Summary: "reset the association instead of sending NG SETUP FAILURE"}
)

// Outcomes declares what the check can report.
func (ngSetupMissingMandatoryIE) Outcomes() []Outcome {
	return []Outcome{missingTAListRejected, missingTAListAccepted, missingTAListOtherPDU, missingTAListSilent, missingTAListReset}
}

func (ngSetupMissingMandatoryIE) ID() string { return "NGAP-NGSETUP-MISSING-TALIST" }
func (ngSetupMissingMandatoryIE) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "9.2.6.1 Supported TA List", Strength: Descriptive, Since: 15, Verified: ts38413Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "criticality column reads \"GLOBAL reject\" in V15.0.0, \"YES reject\" by V15.13.0"},
				{Release: 19, Note: "semantics add that the IE is ignored for an A-IoT-only node; still mandatory, criticality reject"},
			}},
		{Spec: "TS 38.413", Clause: "10.3.5 Reject IE bullet 1", Strength: Shall, Since: 15, Verified: ts38413Verified, WordingUnchanged: true, Primary: true},
	}
}

func (ngSetupMissingMandatoryIE) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "NG SETUP FAILURE reporting the missing Supported TA List, and the core survives (TS 38.413 10.3.5)"}
	conn, err := env.Dial(ctx) // raw association — we send our own (malformed) NG Setup
	if err != nil {
		return r.Errored("dial failed", err.Error())
	}
	defer conn.Close()

	pdu, err := gnb.BuildNGSetupRequest(env.GNB)
	if err != nil {
		return r.Errored("stimulus could not be built", err.Error())
	}
	// Drop the mandatory Supported TA List IE.
	ies := &pdu.InitiatingMessage.Value.NGSetupRequest.ProtocolIEs.List
	kept := (*ies)[:0]
	dropped := false
	for _, ie := range *ies {
		if ie.Id.Value == ngapType.ProtocolIEIDSupportedTAList {
			dropped = true
			continue
		}
		kept = append(kept, ie)
	}
	*ies = kept
	if !dropped {
		return r.Errored("stimulus could not be built", "Supported TA List IE not present to drop")
	}

	if err := gnb.SendPDU(conn, 0, pdu); err != nil {
		return r.Errored("stimulus could not be sent", err.Error())
	}

	sent := time.Now()
	rctx, cancel := context.WithTimeout(ctx, ngSetupWait)
	defer cancel()
	resp, err := gnb.ReadPDU(rctx, conn)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
		return r.Errored("timed out waiting for the NG SETUP FAILURE",
			fmt.Sprintf("the per-test timeout expired before the %s the check waits; raise --per-test-timeout", ngSetupWait))
	}
	o, observed, detail, survived := classifyMissingTAList(resp, err, time.Since(sent))
	r = r.As(o, observed, detail)

	// Crash-safety confirmation whenever the association survived.
	return confirmAlive(ctx, env, r, survived, "fresh NG Setup failed — the core did not survive")
}

// classifyMissingTAList maps the AMF's reaction to an NG Setup Request
// without the mandatory Supported TA List onto an outcome; after is how long
// after the request the reaction came. 10.3.5 requires the procedure to be
// rejected with the message that reports its unsuccessful outcome: NG SETUP
// FAILURE is PASS, with whether it carries Criticality Diagnostics recorded
// (not asserted). Anything else misses the shall: FAIL, with its own
// outcome for a silent drop, another PDU (an ERROR INDICATION included),
// acceptance and a reset. survived reports whether the association is still
// up, so the caller confirms the core with a fresh NG Setup.
func classifyMissingTAList(resp *ngapType.NGAPPDU, err error, after time.Duration) (o Outcome, observed, detail string, survived bool) {
	switch {
	case err == nil && isNGSetupFailure(resp):
		if hasCriticalityDiagnostics(resp) {
			return missingTAListRejected, "NG SETUP FAILURE (rejected)", "Criticality Diagnostics present", true
		}
		return missingTAListRejected, "NG SETUP FAILURE (rejected)", "Criticality Diagnostics absent (10.3.5 asks for it; recorded, not asserted)", true
	case err == nil && isNGSetupResponse(resp):
		return missingTAListAccepted, "NG SETUP RESPONSE (accepted!)", "AMF accepted an NG Setup missing the mandatory Supported TA List", true
	case err == nil:
		return missingTAListOtherPDU, ngapName(resp), fmt.Sprintf("answered with %s %s after the NG SETUP REQUEST instead of NG SETUP FAILURE; the association survived",
			ngapName(resp), after.Round(time.Millisecond)), true
	case errors.Is(err, context.DeadlineExceeded):
		return missingTAListSilent, "silently dropped; association survived",
			fmt.Sprintf("no NG SETUP FAILURE within %s of the NG SETUP REQUEST (absence inferred within this bound); the association survived", ngSetupWait), true
	default:
		return missingTAListReset, "association reset", err.Error(), false
	}
}

// hasCriticalityDiagnostics reports whether an NG SETUP FAILURE carries the
// Criticality Diagnostics IE.
func hasCriticalityDiagnostics(pdu *ngapType.NGAPPDU) bool {
	for _, ie := range pdu.UnsuccessfulOutcome.Value.NGSetupFailure.ProtocolIEs.List {
		if ie.Id.Value == ngapType.ProtocolIEIDCriticalityDiagnostics && ie.Value.CriticalityDiagnostics != nil {
			return true
		}
	}
	return false
}

func isNGSetupFailure(pdu *ngapType.NGAPPDU) bool {
	return pdu.Present == ngapType.NGAPPDUPresentUnsuccessfulOutcome &&
		pdu.UnsuccessfulOutcome != nil &&
		pdu.UnsuccessfulOutcome.Value.Present == ngapType.UnsuccessfulOutcomePresentNGSetupFailure
}

func isNGSetupResponse(pdu *ngapType.NGAPPDU) bool {
	return pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome &&
		pdu.SuccessfulOutcome != nil &&
		pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentNGSetupResponse
}
