package conformance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
)

func init() {
	builtins = append(builtins, endMarkerOnPathSwitch{})
}

// endMarkerOnPathSwitch registers a UE with a PDU session on gNB A, hands it
// over by Xn to gNB B, and asserts the core sends at least one GTP-U End
// Marker (type 254) on the old downlink F-TEID to gNB A's N3 socket, and no
// G-PDU on that tunnel after it (TS 23.501 5.8.2.9.1: "after sending the
// last PDU on the old path"; TS 29.281 7.3.2).
//
// The window opens when the handover starts, before the Path Switch Request:
// per TS 23.502 4.9.1.2.2 the UPF sends the markers right after switching
// (step 5), before the AMF acknowledges the target (step 7), so they usually
// arrive first. It closes EndMarkerGraceTTL after the Acknowledge, the time
// the engine keeps the source tunnel. Who builds the marker (UPF, or SMF per
// TS 23.501 5.8.2.9.2) is not asserted, nor is a QFI on it: TS 29.281 7.3.2
// requires the PDU Session Container only on 5GS-EPS forwarding tunnels, so
// its presence is recorded only.
//
// The check needs the Xn path switch: a PATH SWITCH REQUEST the core does
// not acknowledge switches no path, so no End Marker is due, and the check
// reports BLOCKED, naming NGAP-PATH-SWITCH-ACK-TRANSFER.
type endMarkerOnPathSwitch struct {
	// window overrides EndMarkerGraceTTL (tests only).
	window time.Duration
}

// The keys of the shalls the outcomes of GTPU-END-MARKER-ON-PATH-SWITCH
// rest on.
const (
	endMarkerUPFRef  = "TS 23.501 5.8.2.9.1"
	endMarkerGTPURef = "TS 29.281 7.3.2"
)

// The outcomes of GTPU-END-MARKER-ON-PATH-SWITCH.
var (
	endMarkerSent = Outcome{ID: "end-marker", Verdict: Pass, Refs: []string{endMarkerUPFRef, endMarkerGTPURef},
		Summary: "sent an End Marker on the old F-TEID and no G-PDU on that tunnel after it"}
	endMarkerAbsent = Outcome{ID: "no-end-marker", Verdict: Fail, Refs: []string{endMarkerUPFRef, endMarkerGTPURef},
		Bound:   &Bound{Wait: datapath.EndMarkerGraceTTL, Source: "check bound after the Acknowledge, as long as the source tunnel is kept; TS 23.501 sets no timer for the End Marker"},
		Summary: "sent no End Marker on the old F-TEID"}
	endMarkerGPDUAfter = Outcome{ID: "g-pdu-after-end-marker", Verdict: Fail, Refs: []string{endMarkerUPFRef, endMarkerGTPURef},
		Summary: "sent a G-PDU on the old F-TEID after its End Marker"}
)

// Outcomes declares what the check can report.
func (endMarkerOnPathSwitch) Outcomes() []Outcome {
	return []Outcome{endMarkerSent, endMarkerAbsent, endMarkerGPDUAfter}
}

func (endMarkerOnPathSwitch) ID() string         { return "GTPU-END-MARKER-ON-PATH-SWITCH" }
func (endMarkerOnPathSwitch) Category() Category { return Mobility }
func (endMarkerOnPathSwitch) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 23.501", Clause: "5.8.2.9.1", Strength: Shall, Since: 15, Verified: ts23501Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 15, Note: "surrounding text: \"In case of\" reads \"In the case of\" by V15.13.0; the cited sentence is unchanged Rel-15 through Rel-19"},
				{Release: 16, Note: "surrounding text: the two preamble paragraphs are removed and the with-UPF-change case is reworded between V16.0.0 and V16.20.0"},
			}},
		{Spec: "TS 29.281", Clause: "7.3.2", Strength: Shall, Since: 15, Verified: ts29281EndMarkerVerified,
			Changed: []SpecChange{
				{Release: 15, Note: "message type 254 dates from Rel-8; the 5GS sentence is added in V15.2.0 and the 5GS-EPS forwarding QFI sentence in V15.5.0"},
				{Release: 16, Note: "V16.0.0 adds no End Marker on N19; V16.1.0 splits the clause into 7.3.2.1/7.3.2.2/7.3.2.3; V16.2.0 adds the Dual Connectivity no-QFI case; unchanged V16.2.0 through V19.2.0"},
			}},
		{Spec: "TS 29.244", Clause: "8.2.31", Strength: Shall, Since: 14, Verified: ts29244Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "SxSMReq-Flags renamed PFCPSMReq-Flags in V15.0.0; \"set to 1\" quoted by V15.10.0 (editorial); the SNDEM duty applies when the CP sets the flag"},
			}},
	}
}

// MinRelease is Rel-15: End Markers toward the source NG-RAN start with
// TS 23.501 V15.0.0, though SNDEM dates from Rel-14.
func (endMarkerOnPathSwitch) MinRelease() Release { return 15 }

func (c endMarkerOnPathSwitch) grace() time.Duration {
	if c.window > 0 {
		return c.window
	}
	return datapath.EndMarkerGraceTTL
}

func (c endMarkerOnPathSwitch) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: fmt.Sprintf("GTP-U End Marker on the old F-TEID at the source gNB between Path Switch Request and %v after its Acknowledge, with no G-PDU on that tunnel after it (TS 23.501 5.8.2.9.1; TS 29.281 7.3.2)", c.grace())}
	h, r, ok := setUpMobility(ctx, env, r)
	if !ok {
		return r
	}
	defer h.Close()
	h.StartTraffic(ctx)
	if env.UE.Traffic != "" {
		// Let the flow put downlink on the old path before switching.
		if !sleepCtx(ctx, time.Second) {
			return r.Errored("timed out before the handover", "raise --per-test-timeout above the registration and the handover")
		}
	}
	hctx, cancel := context.WithTimeout(ctx, pathSwitchAckWait)
	err := h.XnHandover(hctx)
	cancel()
	if err != nil {
		if res, local := xnHandoverFailed(ctx, r, err, pathSwitchAckWait); local {
			return res
		}
		_, observed := missOf(err)
		return r.BlockedBy(ProcXnPathSwitch, observed,
			fmt.Sprintf("the core did not acknowledge the PATH SWITCH REQUEST within %s, so no path was switched and no End Marker was due. Recorded: %v", pathSwitchAckWait, err))
	}
	// Wait out the window, plus a moment for packets already read to reach
	// the observers.
	if !sleepCtx(ctx, time.Until(h.Observe().Ack.Add(c.grace()))+50*time.Millisecond) {
		return r.Errored("timed out before the End Marker window closed",
			"raise --per-test-timeout above the registration, the handover and "+c.grace().String())
	}
	return r.As(classifyEndMarker(h.Observe(), c.grace()))
}

// sleepCtx waits d, or until ctx is done (false).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// classifyEndMarker judges the old path around an Xn path switch. Only
// traffic on the old F-TEID at the source gNB's N3 socket counts. An End
// Marker counts when it arrives between the handover's start and grace after
// the Acknowledge; one before the start or after the window, or on another
// tunnel, is recorded only. No counted marker is FAIL (absent); a G-PDU on
// the old tunnel after the first counted marker is FAIL (ordering);
// otherwise PASS. The detail always records the tunnel, the marker counts,
// the QFI container, the downlink before the switch and the traffic.
func classifyEndMarker(o PathObservation, grace time.Duration) (Outcome, string, string) {
	onOld := func(teid uint32, gnb string) bool { return teid == o.OldTEID && sameSocket(o.SourceN3, gnb) }
	closes := o.Ack.Add(grace)

	var counted []datapath.EndMarker
	var early, late, elsewhere int
	for _, em := range o.EndMarkers {
		switch {
		case !onOld(em.TEID, em.GNB):
			elsewhere++
		case em.Arrival.Before(o.Started):
			early++
		case em.Arrival.After(closes):
			late++
		default:
			counted = append(counted, em)
		}
	}
	var first time.Time
	for _, em := range counted {
		if first.IsZero() || em.Arrival.Before(first) {
			first = em.Arrival
		}
	}
	var before, after int
	for _, g := range o.GPDUs {
		if !onOld(g.TEID, g.GNB) {
			continue
		}
		if g.Arrival.Before(o.Ack) {
			before++
		}
		if len(counted) > 0 && g.Arrival.After(first) {
			after++
		}
	}

	facts := []string{
		fmt.Sprintf("old F-TEID %#x at source N3 %s", o.OldTEID, o.SourceN3),
		fmt.Sprintf("End Markers counted %d (before the handover %d, after the window %d, on other tunnels %d)", len(counted), early, late, elsewhere),
		"QFI container " + qfiSummary(counted),
		fmt.Sprintf("downlink G-PDUs on the old path before the Acknowledge %d", before),
		trafficSummary(o.Traffic, before),
	}
	if !o.AckFromEvent {
		facts = append(facts, "Acknowledge time taken from the handover's return")
	}
	detail := strings.Join(facts, "; ")

	switch {
	case len(counted) == 0:
		return endMarkerAbsent, "no End Marker on the old F-TEID",
			fmt.Sprintf("no GTP-U End Marker on the old F-TEID between Path Switch Request and %v after its Acknowledge (absence inferred within this bound). ", grace) +
				"TS 23.501 5.8.2.9.1 (Rel-15 onward, wording unchanged through Rel-19); TS 29.281 7.3.2 (Rel-15 onward; Rel-16: split into 7.3.2.1/7.3.2.3). " +
				"Likely cause: the UPF does not send End Markers, or the SMF did not set SNDEM (TS 29.244 8.2.31). Recorded: " + detail
	case after > 0:
		return endMarkerGPDUAfter, fmt.Sprintf("%d G-PDU(s) on the old F-TEID after the End Marker", after),
			"G-PDU on the old F-TEID after the End Marker: end markers are sent after the last PDU on the old path (TS 23.501 5.8.2.9.1; TS 29.281 7.3.2). Recorded: " + detail
	default:
		return endMarkerSent, fmt.Sprintf("%d End Marker(s) on the old F-TEID, no G-PDU after", len(counted)), detail
	}
}

// qfiSummary records whether the counted markers carried a PDU Session
// Container (recorded, not asserted).
func qfiSummary(ems []datapath.EndMarker) string {
	if len(ems) == 0 {
		return "n/a"
	}
	var with []string
	for _, em := range ems {
		if em.HasQFI {
			with = append(with, fmt.Sprint(em.QFI))
		}
	}
	switch len(with) {
	case 0:
		return "absent (not required on N3; recorded only)"
	case len(ems):
		return "present, QFI " + strings.Join(with, ",") + " (recorded only)"
	default:
		return fmt.Sprintf("on %d of %d markers, QFI %s (recorded only)", len(with), len(ems), strings.Join(with, ","))
	}
}

// trafficSummary says whether traffic was flowing on the old path.
func trafficSummary(peer string, dlBefore int) string {
	switch {
	case peer == "":
		return "no traffic configured"
	case dlBefore > 0:
		return "traffic to " + peer + " flowing on the old path"
	default:
		return "traffic to " + peer + " configured, but no downlink reached the old path"
	}
}
