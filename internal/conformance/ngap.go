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
func (errorIndicationOnUnknownUE) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "10.6", Strength: Shall, Since: 15, Verified: ts38413Verified,
			Changed: []SpecChange{{Release: 15, Note: "V15.1.0 rewords the lead-in and the local-release sentence; V15.3.0 adds a paragraph for a first returned message"}}},
		{Spec: "TS 38.413", Clause: "8.7.5.2", Strength: Shall, Since: 15, Verified: ts38413Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "example cause names replaced in V15.4.0"},
				{Release: 19, Note: "V19.2.0 adds A-IoT text; V19.3.0 inserts an NB-IoT sentence between the cited sentences"},
			}},
	}
}

func (errorIndicationOnUnknownUE) VerdictClass() Verdict { return Fail }

func (errorIndicationOnUnknownUE) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "ERROR INDICATION for the unknown UE NGAP IDs, and the NG association keeps working (TS 38.413 10.6, 8.7.5.2)"}
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
	r.Verdict, r.Observed, r.Detail = classifyUnknownUE(resp, err)

	// Crash-safety confirmation: a PASS or DEVIATION outcome must leave the
	// AMF able to complete a fresh NG Setup. If not, the core did not survive.
	return confirmAlive(ctx, env, r, "fresh NG Setup failed — the core did not survive the unexpected message")
}

// classifyUnknownUE maps the AMF's reaction to an Uplink NAS Transport for
// unknown UE NGAP IDs onto a verdict. The stimulus is not the last message on
// a UE-associated connection, so 10.6 requires an ERROR INDICATION (not just
// a local release): that is PASS. Surviving without one (a silent drop, or
// another PDU) misses the shall without harming the core: DEVIATION. An
// association reset: FAIL.
func classifyUnknownUE(resp *ngapType.NGAPPDU, err error) (Verdict, string, string) {
	switch {
	case err == nil && isErrorIndication(resp):
		return Pass, "ERROR INDICATION", ""
	case err == nil:
		return Deviation, ngapName(resp), "answered with " + ngapName(resp) + " instead of an ERROR INDICATION; the association survived"
	case errors.Is(err, context.DeadlineExceeded):
		return Deviation, "silently dropped; association survived", "no ERROR INDICATION within the wait; the association survived"
	default:
		return Fail, "association reset", err.Error()
	}
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
