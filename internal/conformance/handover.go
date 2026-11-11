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
	builtins = append(builtins, ngapPathSwitchAckTransfer{}, ngapHandoverCommandTransfer{})
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
// and is never required. When it is absent the uplink endpoint is unchanged
// (8.4.4.2), so with an echo peer configured (UESpec.Traffic) the check
// also watches for an echo of uplink sent after the switch: none, when the
// peer answered before the switch, is FAIL. Without a peer, or with one
// that never answered, the uplink is recorded as not probed.
type ngapPathSwitchAckTransfer struct {
	// ackWait overrides pathSwitchAckWait and uplinkWindow
	// pathSwitchUplinkWindow (tests only).
	ackWait, uplinkWindow time.Duration
}

func (ngapPathSwitchAckTransfer) ID() string         { return "NGAP-PATH-SWITCH-ACK-TRANSFER" }
func (ngapPathSwitchAckTransfer) Category() Category { return Mobility }
func (ngapPathSwitchAckTransfer) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.4.4.2", Strength: Shall, Since: 15, Verified: ts38413Verified,
			Changed: []SpecChange{
				{Release: 17, Note: "sentences are added around the cited one in Rel-17 and Rel-18 (IAB-MT, MT communication handling, RedCap); the cited sentence is the same Rel-15 through Rel-19"},
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
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run mobility checks"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	if _, err := h.Register(ctx); err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "UE setup on the source gNB failed", err.Error()
		return r
	}
	h.StartTraffic(ctx)
	if env.UE.Traffic != "" {
		// Let the echo peer answer on the old path before switching.
		if !sleepCtx(ctx, time.Second) {
			r.Verdict, r.Observed, r.Detail = Error, "timed out before the handover", ctx.Err().Error()
			return r
		}
	}
	hctx, cancel := context.WithTimeout(ctx, c.waitAck())
	err = h.XnHandover(hctx)
	cancel()
	switch {
	case err != nil && errors.Is(err, engine.ErrPathSwitchNotAcknowledged) && ctx.Err() == nil:
		r.Verdict, r.Observed = Fail, "no PATH SWITCH REQUEST ACKNOWLEDGE"
		r.Detail = fmt.Sprintf("%s Recorded: no Acknowledge within %s of starting the handover: %v", pathSwitchAckFailure, c.waitAck(), err)
		return r
	case err != nil:
		r.Verdict, r.Observed, r.Detail = Error, "Xn path switch failed", err.Error()
		return r
	}
	if env.UE.Traffic != "" {
		// Wait out the window, plus a moment for packets already read to
		// reach the observer.
		if !sleepCtx(ctx, time.Until(h.Observe().Ack.Add(c.window()))+50*time.Millisecond) {
			r.Verdict, r.Observed = Error, "timed out before the uplink window closed"
			r.Detail = "raise --per-test-timeout above the registration, the handover and " + c.window().String()
			return r
		}
	}
	r.Verdict, r.Observed, r.Detail = classifyPathSwitchAck(h.Observe(), int64(h.pdu.PDUSessionID), c.window())
	return r
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
// Switched List, a listed item whose transfer is empty or does not decode,
// the UE's session missing from the list, or no echo after the switch when
// the session's transfer kept the uplink endpoint (no UL NG-U UP TNL
// Information). No echo after a switch that moved the uplink endpoint is
// ERROR: the check asserts nothing about a new endpoint. Otherwise PASS.
func classifyPathSwitchAck(o PathObservation, session int64, window time.Duration) (Verdict, string, string) {
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
	fail := func(observed string) (Verdict, string, string) {
		return Fail, observed, pathSwitchAckFailure + " Recorded: " + detail
	}
	switch {
	case o.Switched == nil || !o.Switched.ListPresent:
		return fail("PATH SWITCH REQUEST ACKNOWLEDGE without a PDU Session Resource Switched List")
	case missing != "":
		return fail(missing)
	case !found:
		return fail(fmt.Sprintf("PDU session %d is not in the PDU Session Resource Switched List", session))
	case up == uplinkSilent && !hasULTNL:
		return fail("uplink not answered after a path switch that kept the uplink endpoint (no UL NG-U UP TNL Information)")
	case up == uplinkSilent:
		return Error, "uplink not answered after a path switch that moved the uplink endpoint",
			"the Acknowledge carries a transfer with UL NG-U UP TNL Information, and no echo came back through the endpoint it names; the check asserts nothing about a new endpoint. Recorded: " + detail
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
	return Pass, observed, detail
}

// n2HandoverWait bounds the N2 handover, target NG Setup included. The
// engine waits up to 3 s of it for the source's release after the Handover
// Notify.
const n2HandoverWait = 8 * time.Second

// handoverCommandFailure is the check's failure message.
const handoverCommandFailure = "HANDOVER COMMAND lacks the PDU Session Resource Handover List / per-item Handover Command Transfer. TS 38.413 8.4.1.2 (Rel-15 V15.5.0 onward), 9.3.4.10. Likely cause: the known SD-Core transfer-IE issue (docs/interop)."

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
type ngapHandoverCommandTransfer struct {
	// wait overrides n2HandoverWait (tests only).
	wait time.Duration
}

func (ngapHandoverCommandTransfer) ID() string         { return "NGAP-HANDOVER-COMMAND-TRANSFER" }
func (ngapHandoverCommandTransfer) Category() Category { return Mobility }
func (ngapHandoverCommandTransfer) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.4.1.2", Strength: Shall, Since: 15, Verified: ts38413HandoverCommandVerified,
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
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run mobility checks"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	if _, err := h.Register(ctx); err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "UE setup on the source gNB failed", err.Error()
		return r
	}
	hctx, cancel := context.WithTimeout(ctx, c.waitHandover())
	cmd, err := h.N2Handover(hctx)
	cancel()
	var refused *gnb.HandoverPreparationFailure
	if cmd == nil && errors.As(err, &refused) {
		r.Verdict, r.Observed = Error, "HANDOVER PREPARATION FAILURE instead of HANDOVER COMMAND"
		r.Detail = fmt.Sprintf("the AMF answered the HANDOVER REQUIRED with HANDOVER PREPARATION FAILURE, cause %s, after the target gNB acknowledged its HANDOVER REQUEST; there is no HANDOVER COMMAND to judge. TS 38.413 8.4.1.3 lets the AMF refuse a handover; its log says why", refused.Cause)
		return r
	}
	if cmd == nil {
		r.Verdict, r.Observed = Error, "handover did not reach Handover Command"
		r.Detail = fmt.Sprintf("no HANDOVER COMMAND within %s of starting the handover: %v", c.waitHandover(), err)
		return r
	}
	r.Verdict, r.Observed, r.Detail = classifyHandoverCommand(*cmd, int64(h.pdu.PDUSessionID))
	if err != nil {
		r.Detail = joinDetail(r.Detail, "the handover did not complete after the command: "+err.Error())
	}
	return r
}

// classifyHandoverCommand judges a HANDOVER COMMAND's PDU Session Resource
// Handover List. FAIL: no list (or one without items), or an item whose
// Handover Command Transfer is empty or does not decode. Otherwise PASS;
// whether the UE's session is among the items is recorded only.
func classifyHandoverCommand(cmd gnb.HandoverCommandInfo, session int64) (Verdict, string, string) {
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
		return Fail, "HANDOVER COMMAND without a PDU Session Resource Handover List", handoverCommandFailure + " Recorded: " + detail
	case missing != "":
		return Fail, missing, handoverCommandFailure + " Recorded: " + detail
	}
	return Pass, fmt.Sprintf("HANDOVER COMMAND lists %d PDU session(s), each with a Handover Command Transfer", len(cmd.Items)), detail
}
