package conformance

import (
	"context"
	"errors"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
)

func init() {
	builtins = append(builtins, ngResetAcknowledged{})
}

// ngResetAcknowledged sends a well-formed NG RESET (reset the whole NG
// interface) and asserts the AMF completes the Reset procedure by replying with
// NG RESET ACKNOWLEDGE. Unlike the negative checks, this is a genuine "shall"
// (TS 38.413 §8.7.4): the Reset procedure is mandatory, so no response or a
// non-ack is a real FAIL — but the pcap must confirm the NG RESET went out
// well-formed before the FAIL is asserted. The association must also survive.
type ngResetAcknowledged struct{}

func (ngResetAcknowledged) ID() string         { return "NGAP-NG-RESET-ACK" }
func (ngResetAcknowledged) Category() Category { return Procedural }
func (ngResetAcknowledged) SpecRef() string {
	return "TS 38.413 §8.7.4 (Reset) / §9.2.6.4-5 (NG RESET / ACKNOWLEDGE)"
}

func (ngResetAcknowledged) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "NG RESET ACKNOWLEDGE (Reset procedure completes)"}
	conn, err := env.DialSetup(ctx)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "setup failed", err.Error()
		return r
	}
	defer conn.Close()

	// NG RESET is non-UE-associated → stream 0.
	if err := gnb.SendPDU(conn, 0, gnb.BuildNGReset()); err != nil {
		r.Verdict, r.Detail = Error, err.Error()
		return r
	}

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := gnb.ReadPDU(rctx, conn)
	switch {
	case err == nil && isNGResetAcknowledge(resp):
		r.Verdict, r.Observed = Pass, "NG RESET ACKNOWLEDGE"
	case err == nil:
		r.Verdict, r.Observed, r.Detail = Fail, ngapName(resp), "expected NG RESET ACKNOWLEDGE"
	case errors.Is(err, context.DeadlineExceeded):
		r.Verdict, r.Observed, r.Detail = Fail, "no response",
			"Reset procedure did not complete (no NG RESET ACKNOWLEDGE)"
	default:
		r.Verdict, r.Observed, r.Detail = Fail, "association reset", err.Error()
	}
	return r
}

// isNGResetAcknowledge reports whether pdu is an NGAP NG RESET ACKNOWLEDGE.
func isNGResetAcknowledge(pdu *ngapType.NGAPPDU) bool {
	return pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome &&
		pdu.SuccessfulOutcome != nil &&
		pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentNGResetAcknowledge
}
