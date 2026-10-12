package conformance

import (
	"context"
	"errors"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
)

func init() {
	builtins = append(builtins, ngSetupMissingMandatoryIE{})
}

// ngSetupMissingMandatoryIE sends an NG SETUP REQUEST with the mandatory
// Supported TA List IE (id-102) removed. It is mandatory in the NG SETUP
// message contents (TS 38.413 §9.2.6.1); per clause 10 (missing mandatory IE /
// abstract syntax error) the AMF should reject with NG SETUP FAILURE (or Error
// Indication), not accept the setup and not crash.
//
// Load-bearing assertions: the core (1) does not accept an NG Setup missing a
// mandatory IE, and (2) survives. NG SETUP FAILURE is spec-ideal; a graceful
// drop still passes crash-safety; an NG SETUP RESPONSE (acceptance) is a FAIL;
// an association reset is a FAIL.
type ngSetupMissingMandatoryIE struct{}

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

func (ngSetupMissingMandatoryIE) VerdictClass() Verdict { return Fail }

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

	rctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	resp, err := gnb.ReadPDU(rctx, conn)
	r.Verdict, r.Observed, r.Detail = classifyMissingTAList(resp, err)

	// Crash-safety confirmation for the PASS and DEVIATION paths.
	return confirmAlive(ctx, env, r, "fresh NG Setup failed — the core did not survive")
}

// classifyMissingTAList maps the AMF's reaction to an NG Setup Request
// without the mandatory Supported TA List onto a verdict. 10.3.5 requires
// the procedure to be rejected with the message that reports its
// unsuccessful outcome: NG SETUP FAILURE is PASS, with whether it carries
// Criticality Diagnostics recorded (not asserted). Surviving without that (a
// silent drop, an ERROR INDICATION or another PDU) misses the shall without
// harming the core: DEVIATION. Accepting the setup, or resetting the
// association: FAIL.
func classifyMissingTAList(resp *ngapType.NGAPPDU, err error) (Verdict, string, string) {
	switch {
	case err == nil && isNGSetupFailure(resp):
		if hasCriticalityDiagnostics(resp) {
			return Pass, "NG SETUP FAILURE (rejected)", "Criticality Diagnostics present"
		}
		return Pass, "NG SETUP FAILURE (rejected)", "Criticality Diagnostics absent (10.3.5 asks for it; recorded, not asserted)"
	case err == nil && isNGSetupResponse(resp):
		return Fail, "NG SETUP RESPONSE (accepted!)", "AMF accepted an NG Setup missing the mandatory Supported TA List"
	case err == nil:
		return Deviation, ngapName(resp), "answered with " + ngapName(resp) + " instead of NG SETUP FAILURE; the association survived"
	case errors.Is(err, context.DeadlineExceeded):
		return Deviation, "silently dropped; association survived", "no NG SETUP FAILURE within the wait; the association survived"
	default:
		return Fail, "association reset", err.Error()
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
