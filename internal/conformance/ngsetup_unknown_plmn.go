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
	builtins = append(builtins, ngSetupUnknownPLMN{})
}

// unservedMCC and unservedMNC are the PLMN the stimulus presents: a test
// PLMN no lab core is expected to serve. When the run's own PLMN is this
// one, alternateMCC/alternateMNC are used instead.
const (
	unservedMCC, unservedMNC   = "999", "99"
	alternateMCC, alternateMNC = "998", "98"
)

// unservedTACOffset keeps the stimulus's TAC distinct from the served one.
const unservedTACOffset = 0x100

// ngSetupUnknownPLMN sends an NG SETUP REQUEST whose Global RAN Node ID and
// Supported TA List name only a PLMN the AMF does not serve, with a TAC
// distinct from the served one. TS 38.413 8.7.1.4: "If the AMF does not
// identify any of the PLMNs/SNPNs indicated in the NG SETUP REQUEST message,
// it shall reject the NG Setup procedure with an appropriate cause value."
// Rejecting the procedure means its unsuccessful-outcome message (8.7.1.3),
// so NG SETUP FAILURE is PASS, its cause recorded and not asserted.
// Anything else misses the shall and is FAIL, with Observed saying which:
// acceptance (NG SETUP RESPONSE), another PDU (an ERROR INDICATION
// included), a silent drop within ngSetupWait, an association reset, or a
// fresh NG Setup (with the real, served PLMN) that no longer completes.
//
// The stimulus never lists a served PLMN with an unknown TAC: an AMF that
// matches whole TAIs rejects that with the same cause, so a PASS would not
// show that it checks the PLMN.
type ngSetupUnknownPLMN struct{}

func (ngSetupUnknownPLMN) ID() string         { return "NGAP-NGSETUP-UNKNOWN-PLMN" }
func (ngSetupUnknownPLMN) Category() Category { return NegativeIE }
func (ngSetupUnknownPLMN) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.7.1.4", Strength: Shall, Since: 15, Verified: ts38413NGSetupAbnormalVerified,
			Changed: []SpecChange{
				{Release: 15, Note: "added in V15.2.0 (Void in V15.0.0 and V15.1.0), worded for PLMN Identity IEs"},
				{Release: 16, Note: "reworded to the PLMNs/SNPNs form in V16.9.0"},
				{Release: 19, Note: "V19.0.0 adds that the AMF text applies to an AIOTF; the quoted sentence is unchanged"},
			}},
		{Spec: "TS 38.413", Clause: "9.3.1.2", Strength: Descriptive, Since: 15, Verified: ts38413Verified,
			Changed: []SpecChange{{Release: 16, Note: "Misc cause \"Unknown PLMN\" renamed \"Unknown PLMN or SNPN\" in V16.5.0"}}},
	}
}

// unservedPLMN returns the PLMN the stimulus presents: never the run's own.
func unservedPLMN(served gnb.Config) (mcc, mnc string) {
	if served.MCC == unservedMCC && served.MNC == unservedMNC {
		return alternateMCC, alternateMNC
	}
	return unservedMCC, unservedMNC
}

func (ngSetupUnknownPLMN) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "NG SETUP FAILURE rejecting a gNB whose PLMNs the AMF does not serve, and the core survives (TS 38.413 8.7.1.4)"}
	conn, err := env.Dial(ctx) // raw association: the NG Setup is the stimulus
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "dial failed", err.Error()
		return r
	}
	defer func() { _ = conn.Close() }()

	stim := env.GNB
	stim.MCC, stim.MNC = unservedPLMN(env.GNB)
	stim.TAC = (env.GNB.TAC + unservedTACOffset) & 0xffffff
	pdu, err := gnb.BuildNGSetupRequest(stim)
	if err != nil {
		r.Verdict, r.Detail = Error, err.Error()
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
	r.Verdict, r.Observed, r.Detail, survived = classifyUnknownPLMN(resp, err, time.Since(sent))
	r.Detail = joinDetail(fmt.Sprintf("presented PLMN %s/%s, TAC %d (served: %s/%s, TAC %d)",
		stim.MCC, stim.MNC, stim.TAC, env.GNB.MCC, env.GNB.MNC, env.GNB.TAC), r.Detail)

	// Crash-safety confirmation whenever the association survived; the
	// probe presents the real, served PLMN.
	return confirmAlive(ctx, env, r, survived, "fresh NG Setup with the served PLMN failed — the core did not survive")
}

// unknownPLMNFailText is the FAIL detail for an accepted setup.
const unknownPLMNFailText = "AMF accepted NG Setup whose PLMNs it does not serve. TS 38.413 8.7.1.4 (Rel-15 V15.2.0 onward; " +
	"Rel-16 V16.9.0: reworded to PLMNs/SNPNs). Likely cause: AMF does not compare the Broadcast PLMN List against its served PLMNs."

// classifyUnknownPLMN maps the AMF's reaction to an NG Setup naming only
// unserved PLMNs onto a verdict; after is how long after the request the
// reaction came. NG SETUP FAILURE is PASS with its cause recorded. Anything
// else misses the 8.7.1.4 shall and is FAIL. survived reports whether the
// association is still up, so the caller confirms the core.
func classifyUnknownPLMN(resp *ngapType.NGAPPDU, err error, after time.Duration) (v Verdict, observed, detail string, survived bool) {
	switch {
	case err == nil && isNGSetupFailure(resp):
		return Pass, "NG SETUP FAILURE (rejected)", "cause " + ngSetupFailureCause(resp) + " (recorded, not asserted)", true
	case err == nil && isNGSetupResponse(resp):
		return Fail, "NG SETUP RESPONSE (accepted!)", unknownPLMNFailText, true
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

// ngSetupFailureCause renders an NG SETUP FAILURE's Cause IE, naming the
// Misc cause 8.7.1.4 points at ("Unknown PLMN or SNPN", 9.3.1.2).
func ngSetupFailureCause(pdu *ngapType.NGAPPDU) string {
	for _, ie := range pdu.UnsuccessfulOutcome.Value.NGSetupFailure.ProtocolIEs.List {
		if ie.Id.Value != ngapType.ProtocolIEIDCause || ie.Value.Cause == nil {
			continue
		}
		c := ie.Value.Cause
		switch c.Present {
		case ngapType.CausePresentMisc:
			if c.Misc.Value == ngapType.CauseMiscPresentUnknownPLMN {
				return "Misc unknown-PLMN-or-SNPN"
			}
			return fmt.Sprintf("Misc %d", c.Misc.Value)
		case ngapType.CausePresentRadioNetwork:
			return fmt.Sprintf("RadioNetwork %d", c.RadioNetwork.Value)
		case ngapType.CausePresentTransport:
			return fmt.Sprintf("Transport %d", c.Transport.Value)
		case ngapType.CausePresentNas:
			return fmt.Sprintf("NAS %d", c.Nas.Value)
		case ngapType.CausePresentProtocol:
			return fmt.Sprintf("Protocol %d", c.Protocol.Value)
		}
	}
	return "absent"
}
