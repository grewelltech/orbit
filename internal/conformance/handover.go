package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
)

func init() {
	builtins = append(builtins, ngapPathSwitchAckTransfer{}, ngapHandoverCommandTransfer{}, ngapHandoverCancelAck{})
}

const (
	// pathSwitchAckWait bounds the Xn handover, target NG Setup included:
	// an AMF acknowledges a Path Switch Request once the SMF and UPF have
	// switched, well within it.
	pathSwitchAckWait = 5 * time.Second
	// pathSwitchUplinkWindow is how long after the Acknowledge the echo
	// flow is watched for an answer to uplink sent after the switch.
	pathSwitchUplinkWindow = 2 * time.Second
)

// pathSwitchAckFailure is the check's failure message.
const pathSwitchAckFailure = "PATH SWITCH REQUEST ACKNOWLEDGE missing / per-session transfer missing. TS 38.413 8.4.4.2, 9.3.4.9 (UL NG-U UP TNL optional from V15.1.0). Likely cause: AMF/SMF path-switch handling."

// ngapPathSwitchAckTransfer registers a UE with a PDU session on gNB A,
// hands it over by Xn to gNB B, and asserts TS 38.413 8.4.4.2: the AMF
// answers the Path Switch Request with a PATH SWITCH REQUEST ACKNOWLEDGE,
// whose PDU Session Resource Switched List carries a Path Switch Request
// Acknowledge Transfer for the switched session (9.2.3.9: mandatory per
// item).
//
// UL NG-U UP TNL Information inside the transfer is optional from V15.1.0
// and is never required. With an echo peer configured (UESpec.Traffic) the
// check also watches for an echo of uplink sent after the switch: none,
// when the peer answered before the switch, is WARN for availability, as no
// shall obliges the core to carry the flow. Without a peer, or with one
// that never answered, the uplink is recorded as not probed.
//
// The check owns the Xn path switch: a check that needs one reports
// BLOCKED, naming this check, when the core does not acknowledge it. A
// PATH SWITCH REQUEST FAILURE is the answer 8.4.4.3 requires when the 5GC
// fails to switch, and no shall obliges it to switch: WARN, for
// availability. No answer, or another one, violates one of the two shalls.
type ngapPathSwitchAckTransfer struct {
	// ackWait overrides pathSwitchAckWait and uplinkWindow
	// pathSwitchUplinkWindow (tests only).
	ackWait, uplinkWindow time.Duration
}

// The keys of the references the outcomes of NGAP-PATH-SWITCH-ACK-TRANSFER
// and NGAP-HANDOVER-COMMAND-TRANSFER rest on.
const (
	pathSwitchAckRef     = "TS 38.413 8.4.4.2"
	pathSwitchFailureRef = "TS 38.413 8.4.4.3"
	ngapMandatoryIERef   = "TS 38.413 9.1.1"
)

// ngapMandatoryIESpecRef is TS 38.413 9.1.1, the shall that an IE marked
// mandatory is always included.
var ngapMandatoryIESpecRef = SpecRef{Spec: "TS 38.413", Clause: "9.1.1", Strength: Shall, Since: 15, Verified: ts38413Verified, WordingUnchanged: true}

// The outcomes of NGAP-PATH-SWITCH-ACK-TRANSFER.
var (
	pathSwitchAcked = Outcome{ID: "acknowledged", Verdict: Pass, Refs: []string{pathSwitchAckRef, ngapMandatoryIERef},
		Summary: "answered the PATH SWITCH REQUEST with an Acknowledge carrying a transfer for the switched PDU session"}
	pathSwitchNoList = Outcome{ID: "no-switched-list", Verdict: Fail, Refs: []string{ngapMandatoryIERef},
		Summary: "sent the PATH SWITCH REQUEST ACKNOWLEDGE without the mandatory PDU Session Resource Switched List"}
	pathSwitchNoTransfer = Outcome{ID: "transfer-missing", Verdict: Fail, Refs: []string{ngapMandatoryIERef},
		Summary: "listed a switched PDU session without a Path Switch Request Acknowledge Transfer that decodes"}
	// The Acknowledge says the path of at least one session was switched,
	// yet does not list the UE's only session, so the gNB cannot tell that
	// the session's user plane survived. No shall covers the list's content
	// beyond its presence: availability, as the session may be lost.
	pathSwitchSessionNotListed = Outcome{ID: "session-not-listed", Verdict: Warn, Criterion: CriterionAvailability,
		Summary: "acknowledged the path switch without listing the UE's PDU session as switched"}
	// No shall obliges the core to keep the switched session's user plane
	// carrying the flow; a flow that stops after the switch is lost user
	// data: availability.
	pathSwitchUplinkSilent = Outcome{ID: "uplink-not-answered", Verdict: Warn, Criterion: CriterionAvailability,
		Bound:   &Bound{Wait: pathSwitchUplinkWindow, Source: "check bound; echoes are counted from half of it to all of it after the Acknowledge"},
		Summary: "carried no echo of uplink sent after the path switch, where the echo peer answered before it"}
	// 8.4.4.3 requires the FAILURE when the 5GC fails to switch, and no
	// shall obliges it to switch: the core cannot complete the procedure,
	// availability.
	pathSwitchRefused = Outcome{ID: "refused", Verdict: Warn, Criterion: CriterionAvailability, NotCompleted: ProcXnPathSwitch,
		Summary: "answered the PATH SWITCH REQUEST with PATH SWITCH REQUEST FAILURE"}
	pathSwitchUnanswered = Outcome{ID: "unanswered", Verdict: Fail, Refs: []string{pathSwitchAckRef, pathSwitchFailureRef}, NotCompleted: ProcXnPathSwitch,
		Bound:   &Bound{Wait: pathSwitchAckWait, Source: "check bound, the target NG Setup included; TS 38.413 sets no timer for the answer"},
		Summary: "sent neither PATH SWITCH REQUEST ACKNOWLEDGE nor PATH SWITCH REQUEST FAILURE"}
	pathSwitchOtherAnswer = Outcome{ID: "other-answer", Verdict: Fail, Refs: []string{pathSwitchAckRef, pathSwitchFailureRef}, NotCompleted: ProcXnPathSwitch,
		Summary: "answered the PATH SWITCH REQUEST with another message, or lost the association, instead of ACKNOWLEDGE or FAILURE"}
)

// Outcomes declares what the check can report.
func (ngapPathSwitchAckTransfer) Outcomes() []Outcome {
	return []Outcome{pathSwitchAcked, pathSwitchNoList, pathSwitchNoTransfer, pathSwitchSessionNotListed, pathSwitchUplinkSilent,
		pathSwitchRefused, pathSwitchUnanswered, pathSwitchOtherAnswer}
}

func (ngapPathSwitchAckTransfer) ID() string         { return "NGAP-PATH-SWITCH-ACK-TRANSFER" }
func (ngapPathSwitchAckTransfer) Category() Category { return Mobility }
func (ngapPathSwitchAckTransfer) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.4.4.2", Strength: Shall, Since: 15, Verified: ts38413Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 17, Note: "sentences are added around the cited one in Rel-17 and Rel-18 (IAB-MT, MT communication handling, RedCap); the cited sentence is the same Rel-15 through Rel-19"},
			}},
		{Spec: "TS 38.413", Clause: "8.4.4.3", Strength: Shall, Since: 15, Verified: ts38413Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 reads \"fails to switch the downlink GTP tunnel endpoint towards a new GTP tunnel endpoint\" and ends \"with an appropriate cause value\"; by V15.13.0 it reads \"fails to switch the downlink termination point of the NG-U transport bearer towards a new termination point\", unchanged through V19.4.0"},
			}},
		{Spec: "TS 38.413", Clause: "9.3.4.9", Strength: Descriptive, Since: 15, Verified: ts38413PathSwitchAckVerified,
			Changed: []SpecChange{
				{Release: 15, Note: "UL NG-U UP TNL Information is M in V15.0.0 and O from V15.1.0; its semantics text dates from V15.3.0"},
				{Release: 16, Note: "the UL NG-U UP TNL Information row is unchanged Rel-16 through Rel-19; other optional IEs are added (redundant TNL, QoS Flow Parameters List)"},
			}},
		{Spec: "TS 38.413", Clause: "9.2.3.9", Strength: Descriptive, Since: 15, Verified: ts38413PathSwitchAckVerified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 has a PDU Session Resource to be Switched in Uplink List (0..1); the PDU Session Resource Switched List with a mandatory Path Switch Request Acknowledge Transfer per item dates from V15.1.0"},
			}},
		ngapMandatoryIESpecRef,
	}
}

func (c ngapPathSwitchAckTransfer) waitAck() time.Duration {
	if c.ackWait > 0 {
		return c.ackWait
	}
	return pathSwitchAckWait
}

func (c ngapPathSwitchAckTransfer) window() time.Duration {
	if c.uplinkWindow > 0 {
		return c.uplinkWindow
	}
	return pathSwitchUplinkWindow
}

func (c ngapPathSwitchAckTransfer) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "PATH SWITCH REQUEST ACKNOWLEDGE to an Xn Path Switch Request, with a Path Switch Request Acknowledge Transfer for the switched PDU session; uplink still answered when the transfer carries no UL NG-U UP TNL Information (TS 38.413 8.4.4.2, 9.3.4.9)"}
	h, r, ok := setUpMobility(ctx, env, r)
	if !ok {
		return r
	}
	defer h.Close()
	h.StartTraffic(ctx)
	if env.UE.Traffic != "" {
		// Let the echo peer answer on the old path before switching.
		if !sleepCtx(ctx, time.Second) {
			return r.Errored("timed out before the handover", "raise --per-test-timeout above the registration and the handover")
		}
	}
	hctx, cancel := context.WithTimeout(ctx, c.waitAck())
	err := h.XnHandover(hctx)
	cancel()
	if err != nil {
		if res, local := xnHandoverFailed(ctx, r, err, c.waitAck()); local {
			return res
		}
		return r.As(classifyPathSwitchUnacknowledged(err, c.waitAck()))
	}
	if env.UE.Traffic != "" {
		// Wait out the window, plus a moment for packets already read to
		// reach the observer.
		if !sleepCtx(ctx, time.Until(h.Observe().Ack.Add(c.window()))+50*time.Millisecond) {
			return r.Errored("timed out before the uplink window closed",
				"raise --per-test-timeout above the registration, the handover and "+c.window().String())
		}
	}
	return r.As(classifyPathSwitchAck(h.Observe(), int64(h.pdu.PDUSessionID), c.window()))
}

// setUpMobility starts a mobility check: the UE harness, and the UE
// registered with its PDU session on gNB A. Without a UE r is the SKIP for
// the missing input; a harness the env refuses is ERROR, and a setup the
// core does not complete BLOCKED (Result.SetupFailed). ok is false with r
// then final; otherwise the caller closes h.
func setUpMobility(ctx context.Context, env Env, r Result) (_ *UEHarness, _ Result, ok bool) {
	if !env.UE.Configured() {
		return nil, r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run mobility checks"), false
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return nil, r.Errored("harness setup failed", err.Error()), false
	}
	if _, err := h.Register(ctx); err != nil {
		h.Close()
		return nil, r.SetupFailed("UE setup on the source gNB failed", err), false
	}
	return h, r, true
}

// xnHandoverFailed handles an Xn handover that ended with err before an
// Acknowledge, when the error is not the core's answer to the PATH SWITCH
// REQUEST: ERROR when the per-test timeout ended the check's wait for the
// answer (wait); BLOCKED on the NG Setup when the core did not complete the
// target gNB's (ERROR when that failed on this host); ERROR when the
// handover could not be started. local is false when err is the core's: a
// PATH SWITCH REQUEST it did not acknowledge, which the caller judges.
func xnHandoverFailed(ctx context.Context, r Result, err error, wait time.Duration) (_ Result, local bool) {
	switch {
	case ctx.Err() != nil:
		return r.Errored("timed out before the path switch was answered",
			fmt.Sprintf("the per-test timeout expired before the %s wait for the PATH SWITCH REQUEST ACKNOWLEDGE ended, so the core's answer is not known: %v; raise --per-test-timeout", wait, err)), true
	case errors.Is(err, engine.ErrPathSwitchNotAcknowledged):
		return r, false
	case errors.Is(err, engine.ErrNGSetup):
		return r.SetupFailed("the target gNB's NG Setup failed", err), true
	}
	return r.Errored("Xn path switch could not be started", err.Error()), true
}

// pathSwitchMiss is how the core left a PATH SWITCH REQUEST
// unacknowledged.
type pathSwitchMiss int

const (
	// pathSwitchMissRefused: a PATH SWITCH REQUEST FAILURE.
	pathSwitchMissRefused pathSwitchMiss = iota
	// pathSwitchMissUnanswered: no answer within the wait.
	pathSwitchMissUnanswered
	// pathSwitchMissOther: another message, or the association lost.
	pathSwitchMissOther
)

// missOf reads how the core left a PATH SWITCH REQUEST unacknowledged from
// err, an error wrapping engine.ErrPathSwitchNotAcknowledged, with the
// observed text for it.
func missOf(err error) (pathSwitchMiss, string) {
	switch {
	case errors.Is(err, engine.ErrPathSwitchRefused):
		return pathSwitchMissRefused, "PATH SWITCH REQUEST FAILURE instead of PATH SWITCH REQUEST ACKNOWLEDGE"
	case errors.Is(err, context.DeadlineExceeded):
		return pathSwitchMissUnanswered, "no PATH SWITCH REQUEST ACKNOWLEDGE"
	}
	return pathSwitchMissOther, "no PATH SWITCH REQUEST ACKNOWLEDGE (another answer, or the association lost)"
}

// classifyPathSwitchUnacknowledged judges a PATH SWITCH REQUEST the core
// did not acknowledge within wait (err wraps
// engine.ErrPathSwitchNotAcknowledged): WARN for a PATH SWITCH REQUEST
// FAILURE, FAIL for no answer and for another one.
func classifyPathSwitchUnacknowledged(err error, wait time.Duration) (Outcome, string, string) {
	miss, observed := missOf(err)
	switch miss {
	case pathSwitchMissRefused:
		return pathSwitchRefused, observed,
			fmt.Sprintf("the core did not switch the path and answered with PATH SWITCH REQUEST FAILURE, the answer TS 38.413 8.4.4.3 requires when the 5GC fails to switch; no requirement obliges it to switch. Recorded: %v", err)
	case pathSwitchMissUnanswered:
		return pathSwitchUnanswered, observed,
			fmt.Sprintf("%s Recorded: neither PATH SWITCH REQUEST ACKNOWLEDGE nor PATH SWITCH REQUEST FAILURE within %s of starting the handover (absence inferred within this bound): %v", pathSwitchAckFailure, wait, err)
	}
	return pathSwitchOtherAnswer, observed,
		fmt.Sprintf("%s Recorded: neither PATH SWITCH REQUEST ACKNOWLEDGE nor PATH SWITCH REQUEST FAILURE: %v", pathSwitchAckFailure, err)
}

// uplinkState is what the echo flow showed about the uplink after a path
// switch.
type uplinkState int

const (
	// uplinkNotProbed: no echo peer, or one that did not answer before the
	// switch either, so its silence after it shows nothing.
	uplinkNotProbed uplinkState = iota
	// uplinkAnswered: an echo arrived late enough in the window to answer
	// uplink sent after the switch.
	uplinkAnswered
	// uplinkSilent: the peer answered before the switch and not after it.
	uplinkSilent
)

// probeUplink judges the uplink from the echo flow's downlink: the peer
// answered before the handover started (any G-PDU by then), and an echo
// arrived in the second half of the window after the Acknowledge, late
// enough not to answer uplink sent before the switch. An echo on either
// path counts: where the downlink arrives is not the uplink's concern.
func probeUplink(o PathObservation, window time.Duration) (uplinkState, string) {
	if o.Traffic == "" {
		return uplinkNotProbed, "uplink not probed (no --traffic echo peer)"
	}
	from, to := o.Ack.Add(window/2), o.Ack.Add(window)
	var before, onNew, onOld int
	for _, g := range o.GPDUs {
		switch {
		case g.Arrival.Before(o.Started):
			before++
		case g.Arrival.Before(from) || g.Arrival.After(to):
		case g.TEID == o.OldTEID:
			onOld++
		default:
			onNew++
		}
	}
	counts := fmt.Sprintf("echoes from %s: %d before the handover, %d on the new path and %d on the old path between %s and %s after the Acknowledge",
		o.Traffic, before, onNew, onOld, window/2, window)
	switch {
	case before == 0:
		return uplinkNotProbed, "uplink not probed (the echo peer did not answer before the handover); " + counts
	case onNew+onOld > 0:
		return uplinkAnswered, "uplink answered after the path switch; " + counts
	default:
		return uplinkSilent, "uplink not answered after the path switch; " + counts
	}
}

// classifyPathSwitchAck judges an acknowledged path switch. FAIL: no
// Switched List, or a listed item whose transfer is empty or does not
// decode (TS 38.413 9.1.1: both are mandatory). WARN: the UE's session
// missing from the list, or no echo after the switch, whether the
// session's transfer kept the uplink endpoint (no UL NG-U UP TNL
// Information) or moved it. Otherwise PASS.
func classifyPathSwitchAck(o PathObservation, session int64, window time.Duration) (Outcome, string, string) {
	up, upText := probeUplink(o, window)
	var items []string
	found, hasULTNL := false, false
	missing := ""
	if o.Switched != nil {
		for _, it := range o.Switched.Items {
			tnl := "absent"
			if it.HasULTNL {
				tnl = "present"
			}
			switch {
			case it.TransferLen == 0:
				items = append(items, fmt.Sprintf("PDU session %d: no transfer", it.PDUSessionID))
				if missing == "" {
					missing = fmt.Sprintf("PDU session %d switched without a Path Switch Request Acknowledge Transfer", it.PDUSessionID)
				}
			case it.TransferErr != "":
				items = append(items, fmt.Sprintf("PDU session %d: transfer of %d octets does not decode (%s)", it.PDUSessionID, it.TransferLen, it.TransferErr))
				if missing == "" {
					missing = fmt.Sprintf("PDU session %d switched with a Path Switch Request Acknowledge Transfer that does not decode", it.PDUSessionID)
				}
			default:
				items = append(items, fmt.Sprintf("PDU session %d: transfer of %d octets, UL NG-U UP TNL Information %s", it.PDUSessionID, it.TransferLen, tnl))
			}
			if it.PDUSessionID == session {
				found, hasULTNL = true, it.HasULTNL
			}
		}
	}
	list := "Switched List: none"
	if len(items) > 0 {
		list = "Switched List: " + strings.Join(items, ", ")
	}
	facts := []string{list, upText}
	if !o.AckFromEvent {
		facts = append(facts, "Acknowledge time taken from the handover's return")
	}
	detail := strings.Join(facts, "; ")
	fail := func(o Outcome, observed string) (Outcome, string, string) {
		return o, observed, pathSwitchAckFailure + " Recorded: " + detail
	}
	switch {
	case o.Switched == nil || !o.Switched.ListPresent:
		return fail(pathSwitchNoList, "PATH SWITCH REQUEST ACKNOWLEDGE without a PDU Session Resource Switched List")
	case missing != "":
		return fail(pathSwitchNoTransfer, missing)
	case !found:
		return pathSwitchSessionNotListed, fmt.Sprintf("PDU session %d is not in the PDU Session Resource Switched List", session),
			"the Acknowledge does not list the UE's only PDU session as switched. Recorded: " + detail
	case up == uplinkSilent && !hasULTNL:
		return pathSwitchUplinkSilent, "uplink not answered after a path switch that kept the uplink endpoint (no UL NG-U UP TNL Information)",
			"an absent UL NG-U UP TNL Information leaves the uplink endpoint as it was (TS 38.413 8.4.4.2), and the user plane the Acknowledge reports as switched carries no echo of the flow after the switch. Recorded: " + detail
	case up == uplinkSilent:
		return pathSwitchUplinkSilent, "uplink not answered after a path switch that moved the uplink endpoint",
			"the Acknowledge carries a transfer with UL NG-U UP TNL Information, the uplink goes to the endpoint it names, and no echo of the flow came back after the switch. Recorded: " + detail
	}
	observed := "Acknowledge with a transfer for the switched PDU session"
	switch {
	case up == uplinkAnswered:
		observed += "; uplink answered after the switch"
	case hasULTNL:
		observed += "; UL NG-U UP TNL Information present, uplink not probed"
	default:
		observed += "; UL NG-U UP TNL Information absent, uplink not probed"
	}
	return pathSwitchAcked, observed, detail
}

// n2HandoverWait bounds the N2 handover, target NG Setup included. The
// engine waits up to 3 s of it for the source's release after the Handover
// Notify.
const n2HandoverWait = 8 * time.Second

// handoverCommandFailure is the check's failure message.
const handoverCommandFailure = "HANDOVER COMMAND lacks the PDU Session Resource Handover List / per-item Handover Command Transfer. TS 38.413 8.4.1.2 (Rel-15 V15.5.0 onward), 9.2.3.2, 9.1.1."

// ngapHandoverCommandTransfer registers a UE with a PDU session on gNB A,
// hands it over by N2 to gNB B, and asserts TS 38.413 8.4.1.2 for an
// intra-system handover: the HANDOVER COMMAND carries the PDU Session
// Resource Handover List, and each item in it a Handover Command Transfer
// (9.2.3.2: mandatory per item) that decodes.
//
// Nothing inside the transfer is asserted: every IE of 9.3.4.10 is
// optional, forwarding tunnels included. Nor is every session required to
// be listed. The target gNB's Handover Request Acknowledge is the conformant
// encoding (the strict-3gpp core profile), so a core that needs a quirk to
// read it is measured as it behaves without one.
//
// The check owns the N2 handover preparation: a check that needs a
// prepared handover reports BLOCKED, naming this check, when the core does
// not prepare it. 8.4.1.2 and 8.4.1.3 describe the AMF's answer to a
// HANDOVER REQUIRED without obliging it to prepare the handover, so a core
// that refuses it (HANDOVER PREPARATION FAILURE) or sends no HANDOVER
// COMMAND is WARN, for availability, never FAIL.
type ngapHandoverCommandTransfer struct {
	// wait overrides n2HandoverWait (tests only).
	wait time.Duration
}

// handoverCommandRef is the key of the shall for the HANDOVER COMMAND's
// list.
const handoverCommandRef = "TS 38.413 8.4.1.2"

// The outcomes of NGAP-HANDOVER-COMMAND-TRANSFER. The three that report
// no HANDOVER COMMAND are WARN for availability: the core cannot complete
// the procedure, and no shall obliges it to (8.4.1.2 and 8.4.1.3 are
// descriptive on the answer).
var (
	handoverCommanded = Outcome{ID: "command-with-transfers", Verdict: Pass, Refs: []string{handoverCommandRef, ngapMandatoryIERef},
		Summary: "answered the HANDOVER REQUIRED with a HANDOVER COMMAND listing PDU sessions, each with a Handover Command Transfer"}
	handoverNoList = Outcome{ID: "no-handover-list", Verdict: Fail, Refs: []string{handoverCommandRef},
		Summary: "sent the HANDOVER COMMAND of an intra-system handover without the PDU Session Resource Handover List"}
	handoverNoTransfer = Outcome{ID: "transfer-missing", Verdict: Fail, Refs: []string{ngapMandatoryIERef},
		Summary: "listed a PDU session in the HANDOVER COMMAND without a Handover Command Transfer that decodes"}
	handoverRefused = Outcome{ID: "refused", Verdict: Warn, Criterion: CriterionAvailability, NotCompleted: ProcN2HandoverPreparation,
		Summary: "answered the HANDOVER REQUIRED with HANDOVER PREPARATION FAILURE"}
	handoverUnanswered = Outcome{ID: "unanswered", Verdict: Warn, Criterion: CriterionAvailability, NotCompleted: ProcN2HandoverPreparation,
		Bound:   &Bound{Wait: n2HandoverWait, Source: "check bound, the target NG Setup included; TS 38.413 leaves the value of TNGRELOCprep, the source's timer for the preparation, to the implementation"},
		Summary: "sent neither HANDOVER COMMAND nor HANDOVER PREPARATION FAILURE for the HANDOVER REQUIRED"}
	handoverOtherAnswer = Outcome{ID: "no-command", Verdict: Warn, Criterion: CriterionAvailability, NotCompleted: ProcN2HandoverPreparation,
		Summary: "answered the HANDOVER REQUIRED with another message, or lost an association, instead of HANDOVER COMMAND"}
)

// Outcomes declares what the check can report.
func (ngapHandoverCommandTransfer) Outcomes() []Outcome {
	return []Outcome{handoverCommanded, handoverNoList, handoverNoTransfer, handoverRefused, handoverUnanswered, handoverOtherAnswer}
}

func (ngapHandoverCommandTransfer) ID() string         { return "NGAP-HANDOVER-COMMAND-TRANSFER" }
func (ngapHandoverCommandTransfer) Category() Category { return Mobility }
func (ngapHandoverCommandTransfer) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.4.1.2", Strength: Shall, Since: 15, Verified: ts38413HandoverCommandVerified, Primary: true,
			Changed: []SpecChange{
				{Release: 15, Note: "the intra-system \"shall include\" sentence is added in V15.5.0; before it V15.0.0 has a PDU Session Resource Subject to Forwarding List (0..1), V15.1.0 to V15.3.0 a mandatory PDU Session Resource Handover List, and V15.4.0 the list as 0..1 with no obligation in the text; the cited sentences are unchanged Rel-16 through Rel-19"},
			}},
		{Spec: "TS 38.413", Clause: "9.2.3.2", Strength: Descriptive, Since: 15, Verified: ts38413HandoverCommandVerified,
			Changed: []SpecChange{
				{Release: 15, Note: "Handover Command Transfer is O per item in V15.0.0 and M from V15.1.0"},
				{Release: 18, Note: "\"subclause 9.3.4.10\" reads \"clause 9.3.4.10\" from Rel-18 (editorial)"},
			}},
		{Spec: "TS 38.413", Clause: "9.3.4.10", Strength: Descriptive, Since: 15, Verified: ts38413HandoverCommandVerified,
			Changed: []SpecChange{
				{Release: 15, Note: "UL forwarding and additional tunnels are added within Rel-15; every IE of the transfer is optional in every version read"},
				{Release: 16, Note: "adds the Data Forwarding Response E-RAB List"},
				{Release: 19, Note: "the QoS Flow Failed to Setup List is present by V19.4.0"},
			}},
		ngapMandatoryIESpecRef,
	}
}

func (c ngapHandoverCommandTransfer) waitHandover() time.Duration {
	if c.wait > 0 {
		return c.wait
	}
	return n2HandoverWait
}

func (c ngapHandoverCommandTransfer) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "HANDOVER COMMAND to an intra-system N2 handover carrying the PDU Session Resource Handover List, each item with a Handover Command Transfer that decodes (TS 38.413 8.4.1.2, 9.2.3.2)"}
	h, r, ok := setUpMobility(ctx, env, r)
	if !ok {
		return r
	}
	defer h.Close()
	hctx, cancel := context.WithTimeout(ctx, c.waitHandover())
	cmd, err := h.N2Handover(hctx)
	cancel()
	if cmd == nil {
		if res, local := n2HandoverFailed(ctx, r, err, c.waitHandover()); local {
			return res
		}
		return r.As(classifyHandoverNotCommanded(err, c.waitHandover()))
	}
	r = r.As(classifyHandoverCommand(*cmd, int64(h.pdu.PDUSessionID)))
	if err != nil {
		r.Detail = joinDetail(r.Detail, "the handover did not complete after the command: "+err.Error())
	}
	return r
}

// n2HandoverFailed handles an N2 handover that ended with err before a
// HANDOVER COMMAND, when the error is not the core's answer to the
// HANDOVER REQUIRED: ERROR when the per-test timeout ended the check's wait
// for the answer (wait); BLOCKED on the NG Setup when the core did not
// complete the target gNB's (ERROR when that failed on this host); ERROR
// when the handover could not be started. local is false when err is the
// core's: a HANDOVER REQUIRED it did not carry to a command
// (engine.ErrHandoverNotCommanded), which the caller judges.
func n2HandoverFailed(ctx context.Context, r Result, err error, wait time.Duration) (_ Result, local bool) {
	switch {
	case ctx.Err() != nil:
		return r.Errored("timed out before the handover was answered",
			fmt.Sprintf("the per-test timeout expired before the %s wait for the HANDOVER COMMAND ended, so the core's answer is not known: %v; raise --per-test-timeout", wait, err)), true
	case errors.Is(err, engine.ErrHandoverNotCommanded):
		return r, false
	case errors.Is(err, engine.ErrNGSetup):
		return r.SetupFailed("the target gNB's NG Setup failed", err), true
	}
	return r.Errored("handover could not be started",
		fmt.Sprintf("the handover stopped on this side before the HANDOVER REQUIRED was answered (a message that could not be built or sent), so the core's answer is not known: %v", err)), true
}

// handoverMissOf reads what the core did with a HANDOVER REQUIRED it did
// not carry to a HANDOVER COMMAND from err (engine.ErrHandoverNotCommanded):
// the outcome of NGAP-HANDOVER-COMMAND-TRANSFER for it and the observed
// text, which names the cause of a HANDOVER PREPARATION FAILURE.
func handoverMissOf(err error) (Outcome, string) {
	var refused *gnb.HandoverPreparationFailure
	switch {
	case errors.As(err, &refused):
		return handoverRefused, fmt.Sprintf("HANDOVER PREPARATION FAILURE (cause %s) instead of HANDOVER COMMAND", refused.Cause)
	case errors.Is(err, context.DeadlineExceeded):
		return handoverUnanswered, "no HANDOVER COMMAND"
	}
	return handoverOtherAnswer, "no HANDOVER COMMAND (another answer, or an association lost)"
}

// classifyHandoverNotCommanded judges an N2 handover whose HANDOVER
// REQUIRED the core did not carry to a HANDOVER COMMAND within wait (err
// wraps engine.ErrHandoverNotCommanded). Every such outcome is WARN for
// availability: no shall obliges the core to prepare the handover.
func classifyHandoverNotCommanded(err error, wait time.Duration) (Outcome, string, string) {
	o, observed := handoverMissOf(err)
	const why = "TS 38.413 8.4.1.2 and 8.4.1.3 describe the AMF's answer to a HANDOVER REQUIRED without obliging it to prepare the handover, and the core did not prepare this one"
	switch o.ID {
	case handoverRefused.ID:
		return o, observed, fmt.Sprintf("%s: it answered with HANDOVER PREPARATION FAILURE. Recorded: %v", why, err)
	case handoverUnanswered.ID:
		return o, observed, fmt.Sprintf("%s: it sent neither HANDOVER COMMAND nor HANDOVER PREPARATION FAILURE within %s of starting the handover (absence inferred within this bound). Recorded: %v", why, wait, err)
	}
	return o, observed, fmt.Sprintf("%s. Recorded: %v", why, err)
}

// classifyHandoverCommand judges a HANDOVER COMMAND's PDU Session Resource
// Handover List. FAIL: no list (or one without items), or an item whose
// Handover Command Transfer is empty or does not decode. Otherwise PASS;
// whether the UE's session is among the items is recorded only.
func classifyHandoverCommand(cmd gnb.HandoverCommandInfo, session int64) (Outcome, string, string) {
	var items []string
	found := false
	missing := ""
	for _, it := range cmd.Items {
		switch {
		case it.TransferLen == 0:
			items = append(items, fmt.Sprintf("PDU session %d: no transfer", it.PDUSessionID))
			if missing == "" {
				missing = fmt.Sprintf("PDU session %d listed without a Handover Command Transfer", it.PDUSessionID)
			}
		case it.TransferErr != "":
			items = append(items, fmt.Sprintf("PDU session %d: transfer of %d octets does not decode (%s)", it.PDUSessionID, it.TransferLen, it.TransferErr))
			if missing == "" {
				missing = fmt.Sprintf("PDU session %d listed with a Handover Command Transfer that does not decode", it.PDUSessionID)
			}
		default:
			items = append(items, fmt.Sprintf("PDU session %d: transfer of %d octets", it.PDUSessionID, it.TransferLen))
		}
		found = found || it.PDUSessionID == session
	}
	list := "Handover List: none"
	if len(items) > 0 {
		list = "Handover List: " + strings.Join(items, ", ")
	}
	facts := []string{list}
	if len(items) > 0 && !found {
		facts = append(facts, fmt.Sprintf("the UE's PDU session %d is not listed (not required; recorded only)", session))
	}
	facts = append(facts, "target gNB under the strict-3gpp profile (conformant Handover Request Acknowledge Transfer)")
	detail := strings.Join(facts, "; ")
	switch {
	case !cmd.ListPresent || len(cmd.Items) == 0:
		return handoverNoList, "HANDOVER COMMAND without a PDU Session Resource Handover List", handoverCommandFailure + " Recorded: " + detail
	case missing != "":
		return handoverNoTransfer, missing, handoverCommandFailure + " Recorded: " + detail
	}
	return handoverCommanded, fmt.Sprintf("HANDOVER COMMAND lists %d PDU session(s), each with a Handover Command Transfer", len(cmd.Items)), detail
}

const (
	// handoverPreparationWait bounds the preparation of the handover the
	// cancel check cancels, target NG Setup included.
	handoverPreparationWait = 5 * time.Second
	// handoverCancelAckWait is the bound within which the HANDOVER CANCEL
	// ACKNOWLEDGE is expected.
	handoverCancelAckWait = 5 * time.Second
)

// handoverCancelText is the detail of a missing acknowledge.
const handoverCancelText = "no HANDOVER CANCEL ACKNOWLEDGE (class-1 procedure, Table 8.1-1; 9.2.3.12). TS 38.413 8.4.5 (descriptive; Rel-15 onward). Likely cause: AMF does not answer Handover Cancel."

// handoverCancelRef is the key of the description of the cancel.
const handoverCancelRef = "TS 38.413 8.4.5.2"

// The outcomes of NGAP-HANDOVER-CANCEL-ACK. A missing acknowledge is INFO:
// 8.4.5.4 has the source take it as a successful cancellation, so no
// conformant peer is broken and no state is lost.
var (
	handoverCancelAcked = Outcome{ID: "acknowledged", Verdict: Info, Refs: []string{handoverCancelRef, "TS 38.413 9.2.3.12"},
		Summary: "answered the HANDOVER CANCEL with HANDOVER CANCEL ACKNOWLEDGE"}
	handoverCancelNoAck = Outcome{ID: "not-acknowledged", Verdict: Info, Refs: []string{handoverCancelRef},
		Bound:   &Bound{Wait: handoverCancelAckWait, Source: "check bound; TS 38.413 sets no timer for the acknowledge"},
		Summary: "sent no HANDOVER CANCEL ACKNOWLEDGE for the HANDOVER CANCEL"}
	// The core dropped the source association of a UE whose handover was
	// cancelled, so the UE's NG connection is lost: availability.
	handoverCancelSourceLost = Outcome{ID: "association-lost", Verdict: Warn, Criterion: CriterionAvailability,
		Summary: "dropped the source gNB's association after the HANDOVER CANCEL"}
)

// Outcomes declares what the check can report.
func (ngapHandoverCancelAck) Outcomes() []Outcome {
	return []Outcome{handoverCancelAcked, handoverCancelNoAck, handoverCancelSourceLost}
}

// ngapHandoverCancelAck registers a UE with a PDU session on gNB A,
// prepares an N2 handover to gNB B up to the HANDOVER COMMAND, cancels it
// with a HANDOVER CANCEL and reports whether the AMF answers with a
// HANDOVER CANCEL ACKNOWLEDGE.
//
// TS 38.413 has no "shall" and no "should" for the acknowledge: 8.4.5.2
// only describes the source's message, the acknowledge is the response of a
// class 1 procedure (Table 8.1-1) and 9.2.3.12 says what it confirms, while
// 8.4.5.4 has the source take a missing one as a successful cancellation.
// So the check is a characterisation: the acknowledge within the wait and
// its absence are both INFO, and a source association the core drops is
// WARN. A handover the core does not prepare is BLOCKED on the N2 handover
// preparation, which NGAP-HANDOVER-COMMAND-TRANSFER owns. The UE stays on
// gNB A and is deregistered there; a fresh NG Setup is then tried
// (observeAlive), and a core that no longer completes it did not survive
// the cancel: CRASH.
type ngapHandoverCancelAck struct {
	// prepWait overrides handoverPreparationWait and ackWait
	// handoverCancelAckWait (tests only).
	prepWait, ackWait time.Duration
}

func (ngapHandoverCancelAck) ID() string         { return "NGAP-HANDOVER-CANCEL-ACK" }
func (ngapHandoverCancelAck) Category() Category { return Mobility }

// SpecRefs cites the description of the procedure (8.4.5.2), its row in
// the table of class 1 procedures (Table 8.1-1) and the description of the
// acknowledge (9.2.3.12). None requires the acknowledge of the AMF.
func (ngapHandoverCancelAck) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.4.5.2", Strength: Descriptive, Since: 15, Verified: ts38413Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 15, Note: "the sentence is unchanged from V15.0.0 through V19.4.0; by V15.13.0 8.4.5.4 is added, under which the source NG-RAN node takes a missing acknowledge as a successful cancellation"},
			}},
		{Spec: "TS 38.413", Clause: "Table 8.1-1", Strength: Descriptive, Since: 15, Verified: ts38413Verified, WordingUnchanged: true},
		{Spec: "TS 38.413", Clause: "9.2.3.12", Strength: Descriptive, Since: 15, Verified: ts38413Verified, WordingUnchanged: true},
	}
}

func (c ngapHandoverCancelAck) waits() (prep, ack time.Duration) {
	prep, ack = handoverPreparationWait, handoverCancelAckWait
	if c.prepWait > 0 {
		prep = c.prepWait
	}
	if c.ackWait > 0 {
		ack = c.ackWait
	}
	return prep, ack
}

func (c ngapHandoverCancelAck) Run(ctx context.Context, env Env) Result {
	prepWait, ackWait := c.waits()
	r := Result{Expected: fmt.Sprintf("HANDOVER CANCEL ACKNOWLEDGE within %s of a HANDOVER CANCEL sent after the HANDOVER COMMAND (TS 38.413 8.4.5.2, Table 8.1-1, 9.2.3.12, descriptive)", ackWait)}
	h, r, ok := setUpMobility(ctx, env, r)
	if !ok {
		return r
	}
	defer h.Close()
	// One bound for the preparation and the wait: the engine takes the
	// wait for the acknowledge from it once the command has arrived.
	hctx, cancel := context.WithTimeout(ctx, prepWait+ackWait+time.Second)
	res, err := h.CancelN2Handover(hctx, ackWait)
	cancel()
	switch {
	case errors.Is(err, engine.ErrHandoverNotPrepared):
		if res, local := n2HandoverFailed(ctx, r, err, prepWait); local {
			return res
		}
		_, observed := handoverMissOf(err)
		return r.BlockedBy(ProcN2HandoverPreparation, observed,
			"the core did not prepare the handover, so there was nothing to cancel and no HANDOVER CANCEL was sent. Recorded: "+err.Error())
	case err != nil:
		return r.Errored("Handover Cancel could not be sent or waited for", err.Error())
	}
	r = r.As(classifyHandoverCancel(res, ackWait))
	// The UE leaves before the probe, so the probe's NG Setup meets no
	// association of the check.
	h.Close()
	return observeAlive(ctx, env, r, "the HANDOVER CANCEL")
}

// classifyHandoverCancel judges a cancelled handover: INFO with the
// acknowledge and without it, WARN when the source association was lost
// while waiting. The detail records what else the source read while
// waiting and whether the AMF released the target.
func classifyHandoverCancel(res engine.HandoverCancelResult, wait time.Duration) (Outcome, string, string) {
	var facts []string
	if len(res.Skipped) > 0 {
		facts = append(facts, "other PDUs on the source association while waiting: "+strings.Join(res.Skipped, ", "))
	}
	if res.TargetReleased {
		facts = append(facts, "the AMF released the UE context prepared at the target gNB (recorded only)")
	} else {
		facts = append(facts, "no UE Context Release Command at the target gNB before its association closed (recorded only)")
	}
	detail := strings.Join(facts, "; ")
	if res.SourceLost != nil {
		return handoverCancelSourceLost, "association lost after the HANDOVER CANCEL",
			fmt.Sprintf("the source association ended before a HANDOVER CANCEL ACKNOWLEDGE (%v). Recorded: %s", res.SourceLost, detail)
	}
	if res.Acknowledged {
		return handoverCancelAcked, fmt.Sprintf("HANDOVER CANCEL ACKNOWLEDGE after %s", res.AckAfter.Round(time.Millisecond)), detail
	}
	return handoverCancelNoAck, fmt.Sprintf("no HANDOVER CANCEL ACKNOWLEDGE within %s", wait), handoverCancelText + " Recorded: " + detail
}
