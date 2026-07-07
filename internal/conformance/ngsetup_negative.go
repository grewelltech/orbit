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
func (ngSetupMissingMandatoryIE) SpecRef() string {
	return "TS 38.413 §9.2.6.1 (NG SETUP mandatory IEs) / §10 (missing mandatory IE)"
}

func (ngSetupMissingMandatoryIE) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "reject (NG SETUP FAILURE ideal) and survive; must not accept a setup missing a mandatory IE"}
	conn, err := env.Dial() // raw association — we send our own (malformed) NG Setup
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
	switch {
	case err == nil && isNGSetupFailure(resp):
		r.Verdict, r.Observed = Pass, "NG SETUP FAILURE (rejected)"
	case err == nil && isErrorIndication(resp):
		r.Verdict, r.Observed = Pass, "Error Indication (rejected)"
	case err == nil && isNGSetupResponse(resp):
		r.Verdict, r.Observed, r.Detail = Fail, "NG SETUP RESPONSE (accepted!)",
			"AMF accepted an NG Setup missing the mandatory Supported TA List"
	case err == nil:
		r.Verdict, r.Observed = Pass, ngapName(resp)
		r.Detail = "responded (not acceptance) without crashing"
	case errors.Is(err, context.DeadlineExceeded):
		r.Verdict, r.Observed = Pass, "silently dropped; association survived"
		r.Detail = "graceful drop of the malformed setup; NG SETUP FAILURE would be spec-ideal"
	default:
		r.Verdict, r.Observed, r.Detail = Fail, "association reset", err.Error()
	}

	// Crash-safety confirmation for the non-failure paths.
	if r.Verdict == Pass && !env.Alive(ctx) {
		r.Verdict = Fail
		r.Observed = "AMF unresponsive after the message (" + r.Observed + ")"
		r.Detail = "fresh NG Setup failed — the core did not survive"
	}
	return r
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
