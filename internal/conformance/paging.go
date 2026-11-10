package conformance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
)

func init() {
	builtins = append(builtins, pagingOnDLData{})
}

// pagingOnDLData registers a UE with a PDU session, releases it to CM-IDLE
// by an AN release (UE CONTEXT RELEASE REQUEST, cause release due to
// NG-RAN generated reason), has Env.Downlink send downlink traffic to the
// UE's address, and asserts the core sends an NGAP PAGING on the gNB's
// association whose UE Paging Identity is the UE's 5G-S-TMSI within
// pagingWindow (TS 24.501 5.6.2.2.1).
//
// The trigger follows the release by pagingSettle, so the core has told
// its UPF to buffer. One trigger is sent per run: a UPF de-duplicates its downlink data
// notifications, so a second would show nothing new. The packet that
// triggers paging may be lost by a UPF that does not buffer; its delivery
// is not asserted. Whether the TAI List for Paging holds the serving TAI
// is recorded only: TS 38.413 does not say which TAIs it lists. After a
// PAGING the UE answers with a Service Request so the core stops paging;
// that answer is recorded only.
type pagingOnDLData struct {
	// window overrides pagingWindow and settle pagingSettle (tests only).
	window, settle time.Duration
}

// pagingWindow is how long the check waits for the PAGING after the
// downlink trigger. pagingSettle is the pause between the UE CONTEXT
// RELEASE COMPLETE and the trigger: the core needs it to tell the UPF to
// buffer, and downlink that arrives before goes to the released tunnel
// and raises no notification.
const (
	pagingWindow = 10 * time.Second
	pagingSettle = time.Second
)

func (pagingOnDLData) ID() string         { return "NGAP-PAGING-ON-DL-DATA" }
func (pagingOnDLData) Category() Category { return Procedural }
func (pagingOnDLData) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.6.2.2.1", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 reads \"in 5GMM-IDLE mode\"; \"over 3GPP access\" is added by V15.7.0"},
				{Release: 17, Note: "adds \"and there is no paging restriction applied in the network for that paging\""},
			}},
		{Spec: "TS 23.502", Clause: "4.2.3.3 step 4b", Strength: Descriptive, Since: 15, Verified: ts23502Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 reads \"reachable\" and \"Paging Priority indication\"; \"reachable in 3GPP access\" and \"Paging Priority\" by V15.17.0"},
				{Release: 16, Note: "the Paging message's content adds Enhanced Coverage Restricted information and WUS Assistance Information; \"the AMF sends a Paging message\" is unchanged"},
			}},
		{Spec: "TS 38.413", Clause: "8.5.1.2", Strength: Descriptive, Since: 15, Verified: ts38413Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 reads \"paging procedure\" and \"List of TAIs\"; \"Paging procedure\" and \"TAI List for Paging\" from V15.1.0"},
				{Release: 16, Note: "Rel-16 to Rel-19 add optional-IE sentences only"},
			}},
		{Spec: "TS 38.413", Clause: "9.2.4.1", Strength: Descriptive, Since: 15, Verified: ts38413Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "\"List of TAIs\" is renamed \"TAI List for Paging\" in V15.1.0; V15.0.0 also carries \"UE Identity Index Value\", absent by V19.4.0 (removal version not traced)"},
			}},
		{Spec: "TS 38.413", Clause: "9.3.3.18", Strength: Descriptive, Since: 15, Verified: ts38413Verified, WordingUnchanged: true},
	}
}

func (c pagingOnDLData) wait() time.Duration {
	if c.window > 0 {
		return c.window
	}
	return pagingWindow
}

func (c pagingOnDLData) pause() time.Duration {
	if c.settle > 0 {
		return c.settle
	}
	return pagingSettle
}

// Budget is the paging window plus the registration, the AN release, the
// pause before the trigger, the answer to the paging and the
// deregistration.
func (c pagingOnDLData) Budget() time.Duration { return c.wait() + 20*time.Second }

// pagingRefs is the reference line of the check's FAIL messages.
const pagingRefs = "TS 24.501 5.6.2.2.1 (Rel-15 onward); TS 38.413 8.5.1.2 (Rel-15 onward); TS 23.502 4.2.3.3 step 4b (Rel-15 onward)."

func (c pagingOnDLData) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: fmt.Sprintf("NGAP PAGING naming the UE's 5G-S-TMSI within %v of downlink data to the CM-IDLE UE (TS 24.501 5.6.2.2.1; TS 38.413 8.5.1.2)", c.wait())}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run the checks that register a UE"
		return r
	}
	if env.Downlink == nil {
		r.Verdict, r.Observed = Skip, "no N6 downlink trigger configured"
		r.Detail = "the check needs a responder on the N6 network to send downlink data to the idle UE: name one of the target's responders as the conformance workload's responder"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	reg, err := h.Register(ctx)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "UE setup failed", err.Error()
		return r
	}
	idle, err := h.Idle(ctx)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "the AN release to CM-IDLE did not complete", err.Error()
		return r
	}
	if !sleepCtx(ctx, c.pause()) {
		r.Verdict, r.Observed, r.Detail = Error, "timed out before the downlink trigger", ctx.Err().Error()
		return r
	}
	if err := h.TriggerDownlink(ctx); err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "the downlink trigger failed", err.Error()
		return r
	}
	sent := time.Now()
	seen, err := h.AwaitPaging(ctx, c.wait())
	if err != nil {
		r.Verdict, r.Observed = Error, "the wait for PAGING was cut short"
		r.Detail = fmt.Sprintf("%v; raise --per-test-timeout to at least %s", err, c.Budget())
		return r
	}
	o := pagingObservation{STMSI: idle.STMSI, Serving: gnb.TAI{MCC: env.GNB.MCC, MNC: env.GNB.MNC, TAC: env.GNB.TAC},
		ReleaseCause: idle.Cause, Sent: sent, Seen: seen, MICO: reg.Registration.MICO}
	if len(seen) > 0 && seen[len(seen)-1].ForUE {
		// Answer the paging, so the core stops paging before the
		// deregistration. Recorded only.
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		sr, serr := h.ServiceRequest(actx, engine.WithMobileTerminated())
		cancel()
		switch {
		case serr != nil:
			o.Answer = "Service Request after the PAGING: " + serr.Error()
		case sr.Accepted:
			o.Answer = fmt.Sprintf("Service Request after the PAGING accepted, %d session(s) reactivated", len(sr.Reactivated))
		default:
			o.Answer = fmt.Sprintf("Service Request after the PAGING rejected, 5GMM cause %d", sr.RejectCause)
		}
	}
	r.Verdict, r.Observed, r.Detail = classifyPaging(o, c.wait())
	return r
}

// pagingObservation is what the check saw, for classifyPaging.
type pagingObservation struct {
	// STMSI is the UE's identity and Serving the gNB's tracking area.
	STMSI   gnb.STMSI
	Serving gnb.TAI
	// ReleaseCause is the cause of the release that made the UE idle.
	ReleaseCause string
	// Sent is when the downlink trigger returned; Seen every PAGING read
	// in the window after it.
	Sent time.Time
	Seen []engine.PagingEvent
	// MICO is true when the Registration Accept carried a MICO indication.
	MICO bool
	// Answer says how the Service Request after the PAGING went, "" when
	// none was sent.
	Answer string
}

// classifyPaging judges the window after the downlink trigger: a PAGING
// whose UE Paging Identity is the UE's 5G-S-TMSI is PASS; none, or only
// ones naming other identities, is FAIL. The detail records the identity,
// the release cause, the delay, the TAI list (and whether it holds the
// serving TAI), a MICO indication and the answer to the paging.
func classifyPaging(o pagingObservation, window time.Duration) (Verdict, string, string) {
	facts := []string{
		"UE 5G-S-TMSI " + o.STMSI.String(),
		"AN release cause " + o.ReleaseCause,
	}
	if o.MICO {
		facts = append(facts, "the Registration Accept carried a MICO indication the UE did not ask for")
	}
	var own *engine.PagingEvent
	var others []string
	for i := range o.Seen {
		ev := &o.Seen[i]
		switch {
		case ev.ForUE && own == nil:
			own = ev
		case !ev.ForUE && ev.HasSTMSI:
			others = append(others, ev.STMSI.String())
		case !ev.ForUE:
			others = append(others, "none")
		}
	}
	if len(others) > 0 {
		facts = append(facts, "PAGING for other identities: "+strings.Join(others, ", "))
	}
	if own != nil {
		var tais []string
		serving := false
		for _, t := range own.TAIs {
			tais = append(tais, fmt.Sprintf("%s/%s TAC %d", t.MCC, t.MNC, t.TAC))
			serving = serving || t == o.Serving
		}
		held := "holds"
		if !serving {
			held = "does not hold"
		}
		facts = append(facts,
			fmt.Sprintf("PAGING %v after the downlink trigger", own.At.Sub(o.Sent).Round(time.Millisecond)),
			fmt.Sprintf("TAI List for Paging [%s] %s the serving TAI %s/%s TAC %d (recorded only)", strings.Join(tais, "; "), held, o.Serving.MCC, o.Serving.MNC, o.Serving.TAC))
		if o.Answer != "" {
			facts = append(facts, o.Answer+" (recorded only)")
		}
		return Pass, "PAGING with the UE's 5G-S-TMSI", strings.Join(facts, "; ")
	}
	detail := strings.Join(facts, "; ")
	const cause = "Likely cause: UPF/SMF downlink data notification or AMF N1N2MessageTransfer paging path not implemented."
	if len(others) > 0 {
		return Fail, "PAGING for another identity only",
			fmt.Sprintf("no NGAP PAGING naming the UE's 5G-S-TMSI within %v of downlink data to a CM-IDLE UE: %d PAGING message(s) named another UE Paging Identity. ", window, len(others)) +
				pagingRefs + " TS 38.413 9.3.3.18: the UE Paging Identity is the 5G-S-TMSI. Recorded: " + detail
	}
	return Fail, "no PAGING",
		fmt.Sprintf("no NGAP PAGING within %v of downlink data to a CM-IDLE UE. ", window) + pagingRefs + " " + cause + " Recorded: " + detail
}
