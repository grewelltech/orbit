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
// NG SETUP FAILURE with a working core afterwards is PASS. Everything else
// misses the shall and is FAIL, with Observed saying which: a silent drop
// within ngSetupWait, another PDU (an ERROR INDICATION included), acceptance
// (NG SETUP RESPONSE), an association reset, or a fresh NG Setup that no
// longer completes.
type ngSetupMissingMandatoryIE struct{}

// ngSetupWait is how long the check waits for the NG SETUP FAILURE. A silent
// drop is inferred from its absence within this bound, and the detail says
// so.
const ngSetupWait = 4 * time.Second

func (ngSetupMissingMandatoryIE) ID() string         { return "NGAP-NGSETUP-MISSING-TALIST" }
func (ngSetupMissingMandatoryIE) Category() Category { return NegativeIE }
func (ngSetupMissingMandatoryIE) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "9.2.6.1 Supported TA List", Strength: Descriptive, Since: 15, Verified: ts38413Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "criticality column reads \"GLOBAL reject\" in V15.0.0, \"YES reject\" by V15.13.0"},
				{Release: 19, Note: "semantics add that the IE is ignored for an A-IoT-only node; still mandatory, criticality reject"},
			}},
		{Spec: "TS 38.413", Clause: "10.3.5 Reject IE bullet 1", Strength: Shall, Since: 15, Verified: ts38413Verified, WordingUnchanged: true},
	}
}

func (ngSetupMissingMandatoryIE) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "NG SETUP FAILURE reporting the missing Supported TA List, and the core survives (TS 38.413 10.3.5)"}
	conn, err := env.Dial(ctx) // raw association — we send our own (malformed) NG Setup
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "dial failed", err.Error()
		return r
	}
	defer conn.Close()

	pdu, err := gnb.BuildNGSetupRequest(env.GNB)
	if err != nil {
		r.Verdict, r.Detail = Error, err.Error()
		return r
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
		r.Verdict, r.Detail = Error, "Supported TA List IE not present to drop"
		return r
	}

	if err := gnb.SendPDU(conn, 0, pdu); err != nil {
		r.Verdict, r.Detail = Error, err.Error()
		return r
	}

	sent := time.Now()
	rctx, cancel := context.WithTimeout(ctx, ngSetupWait)
	defer cancel()
	resp, err := gnb.ReadPDU(rctx, conn)
	var survived bool
	r.Verdict, r.Observed, r.Detail, survived = classifyMissingTAList(resp, err, time.Since(sent))

	// Crash-safety confirmation whenever the association survived.
	return confirmAlive(ctx, env, r, survived, "fresh NG Setup failed — the core did not survive")
}

// classifyMissingTAList maps the AMF's reaction to an NG Setup Request
// without the mandatory Supported TA List onto a verdict; after is how long
// after the request the reaction came. 10.3.5 requires the procedure to be
// rejected with the message that reports its unsuccessful outcome: NG SETUP
// FAILURE is PASS, with whether it carries Criticality Diagnostics recorded
// (not asserted). Anything else misses the shall: FAIL, with Observed keeping
// a silent drop, another PDU (an ERROR INDICATION included), acceptance and a
// reset apart. survived reports whether the association is still up, so the
// caller confirms the core with a fresh NG Setup.
func classifyMissingTAList(resp *ngapType.NGAPPDU, err error, after time.Duration) (v Verdict, observed, detail string, survived bool) {
	switch {
	case err == nil && isNGSetupFailure(resp):
		if hasCriticalityDiagnostics(resp) {
			return Pass, "NG SETUP FAILURE (rejected)", "Criticality Diagnostics present", true
		}
		return Pass, "NG SETUP FAILURE (rejected)", "Criticality Diagnostics absent (10.3.5 asks for it; recorded, not asserted)", true
	case err == nil && isNGSetupResponse(resp):
		return Fail, "NG SETUP RESPONSE (accepted!)", "AMF accepted an NG Setup missing the mandatory Supported TA List", true
	case err == nil:
		return Fail, ngapName(resp), fmt.Sprintf("answered with %s %s after the NG SETUP REQUEST instead of NG SETUP FAILURE; the association survived",
			ngapName(resp), after.Round(time.Millisecond)), true
	case errors.Is(err, context.DeadlineExceeded):
		return Fail, "silently dropped; association survived",
			fmt.Sprintf("no NG SETUP FAILURE within %s of the NG SETUP REQUEST (absence inferred within this bound); the association survived", ngSetupWait), true
	default:
		return Fail, "association reset", err.Error(), false
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
