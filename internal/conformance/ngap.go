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
	builtins = append(builtins, errorIndicationOnUnknownUE{})
}

// errorIndicationOnUnknownUE sends a UE-associated message (Uplink NAS
// Transport) referencing a UE-NGAP-ID pair the AMF never established. TS 38.413
// §8.7.1 / §10.6 have the AMF *ideally* reply with an Error Indication; the
// load-bearing regression assertion, though, is crash-safety — the core must
// survive the unexpected message (association stays up). This is the class of
// defect behind omec AMF #672/#673 (crashes, fixed in v2.2.1), so on the
// deployed v3.1.0 this should PASS. A graceful silent drop is acceptable;
// only an association reset is a FAIL.
type errorIndicationOnUnknownUE struct{}

func (errorIndicationOnUnknownUE) ID() string         { return "NGAP-UNKNOWN-UE-SURVIVES" }
func (errorIndicationOnUnknownUE) Category() Category { return NegativeIE }
func (errorIndicationOnUnknownUE) SpecRef() string {
	return "TS 38.413 §8.7.1 (Error Indication) / §10.6 (AP ID errors)"
}

func (errorIndicationOnUnknownUE) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "core survives the unexpected message (Error Indication ideal; graceful drop OK)"}
	conn, err := env.DialSetup(ctx)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "setup failed", err.Error()
		return r
	}
	defer conn.Close()

	// Uplink NAS Transport for a UE that was never set up. The NAS payload is
	// irrelevant — the AMF should reject on the unknown UE-NGAP-ID pair before
	// looking at it.
	pdu, err := gnb.BuildUplinkNASTransport(env.GNB, 0x7fffffff, 0x00ffffff, []byte{0x7e, 0x00, 0x5c})
	if err != nil {
		r.Verdict, r.Detail = Error, err.Error()
		return r
	}
	if err := gnb.SendPDU(conn, 1, pdu); err != nil {
		r.Verdict, r.Detail = Error, err.Error()
		return r
	}

	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := gnb.ReadPDU(rctx, conn)
	switch {
	case err == nil && isErrorIndication(resp):
		r.Verdict, r.Observed = Pass, "Error Indication (spec-ideal)"
	case err == nil:
		r.Verdict, r.Observed = Pass, ngapName(resp)
		r.Detail = "responded without crashing (not an Error Indication)"
	case errors.Is(err, context.DeadlineExceeded):
		// No response within the wait, but the association is still up — the
		// AMF dropped the message gracefully.
		r.Verdict, r.Observed = Pass, "silently dropped; association survived"
		r.Detail = "spec-ideal is an Error Indication; a graceful drop still passes the crash-safety guard"
	default:
		// The association errored (reset/EOF) — the core did not survive.
		r.Verdict, r.Observed, r.Detail = Fail, "association reset", err.Error()
	}

	// Crash-safety confirmation: even a "graceful" outcome must leave the AMF
	// able to complete a fresh NG Setup. If not, the core did not survive.
	if r.Verdict == Pass && !env.Alive(ctx) {
		r.Verdict = Fail
		r.Observed = "AMF unresponsive after the message (" + r.Observed + ")"
		r.Detail = "fresh NG Setup failed — the core did not survive the unexpected message"
	}
	return r
}

// isErrorIndication reports whether pdu is an NGAP Error Indication.
func isErrorIndication(pdu *ngapType.NGAPPDU) bool {
	return pdu.Present == ngapType.NGAPPDUPresentInitiatingMessage &&
		pdu.InitiatingMessage != nil &&
		pdu.InitiatingMessage.Value.Present == ngapType.InitiatingMessagePresentErrorIndication
}

// ngapName returns a short human name for a decoded NGAP PDU, for the observed
// field of a result.
func ngapName(pdu *ngapType.NGAPPDU) string {
	switch pdu.Present {
	case ngapType.NGAPPDUPresentInitiatingMessage:
		if pdu.InitiatingMessage != nil {
			return fmt.Sprintf("InitiatingMessage(procedure %d)", pdu.InitiatingMessage.ProcedureCode.Value)
		}
	case ngapType.NGAPPDUPresentSuccessfulOutcome:
		if pdu.SuccessfulOutcome != nil {
			return fmt.Sprintf("SuccessfulOutcome(procedure %d)", pdu.SuccessfulOutcome.ProcedureCode.Value)
		}
	case ngapType.NGAPPDUPresentUnsuccessfulOutcome:
		if pdu.UnsuccessfulOutcome != nil {
			return fmt.Sprintf("UnsuccessfulOutcome(procedure %d)", pdu.UnsuccessfulOutcome.ProcedureCode.Value)
		}
	}
	return "unknown NGAP PDU"
}
