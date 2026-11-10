package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/engine"
)

func init() {
	builtins = append(builtins, serviceRequestReactivates{})
}

// serviceRequestReactivates registers a UE with a PDU session, releases it
// to CM-IDLE by an AN release, sends a Service Request naming the session
// in its Uplink data status, and asserts the core answers Service Accept
// and re-establishes the session's user plane: it sets the N3 tunnel up
// again on N2, and downlink traffic Env.Downlink sends to the UE's address
// arrives on the gNB's new tunnel (TS 24.501 5.6.1.4.1, 5.6.1.4 in Rel-15).
//
// The same trigger is sent once before the release, on the tunnel of the
// registration: when that traffic does not reach the UE the verdict is an
// ERROR, since its absence afterwards would say nothing about the core.
// The release follows once that traffic has stopped, so none of it reaches
// the core while the UE is idle. The Service Accept's PDU session status
// and PDU session reactivation result are recorded only.
type serviceRequestReactivates struct {
	// Overrides of serviceTrafficWindow, serviceQuietGap, serviceSettle,
	// serviceAnswerWindow and serviceUserPlaneWait (tests only).
	traffic, quiet, settle, answer, userPlane time.Duration
}

const (
	// serviceTrafficWindow is how long the check waits for downlink
	// traffic after each trigger, and at most for that traffic to stop.
	serviceTrafficWindow = 5 * time.Second
	// serviceQuietGap is how long the downlink must be silent before the
	// release.
	serviceQuietGap = 500 * time.Millisecond
	// serviceSettle is the pause between the UE CONTEXT RELEASE COMPLETE
	// and the Service Request, in which the core finishes its own release.
	serviceSettle = time.Second
	// serviceAnswerWindow is how long the check waits for the Service
	// Accept or Reject, and serviceUserPlaneWait how long after it for the
	// N3 tunnel's setup on N2.
	serviceAnswerWindow  = 10 * time.Second
	serviceUserPlaneWait = 5 * time.Second
)

func (serviceRequestReactivates) ID() string         { return "NAS-SERVICE-REQUEST-REACTIVATES" }
func (serviceRequestReactivates) Category() Category { return Procedural }
func (serviceRequestReactivates) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.6.1.4.1 (5.6.1.4 in Rel-15) Uplink data status a)", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 16, Note: "5.6.1.4 is split and the text moves to 5.6.1.4.1; the condition adds \"and the UE is: a) not in NB-N1 mode; or b) in NB-N1 mode and ...\" before \"the AMF shall:\"; item a) is unchanged in every version"},
			}},
		{Spec: "TS 24.501", Clause: "5.6.1.5 first sentence", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 16, Note: "V15.0.0 and V15.7.0 end \"and stop timer T3517\"; absent by V16.15.0"},
			}},
		{Spec: "TS 23.502", Clause: "4.2.3.2 steps 12 and 18a", Strength: Descriptive, Since: 15, Verified: ts23502Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 reads \"forwarded to RAN and UE\" in step 18a; \"NG-RAN and UE\" by V15.17.0, unchanged through V19.9.0; the N2 Request's content in step 12 grows in every release, \"N2 SM information received from SMF\" and \"MM NAS Service Accept\" stay"},
			}},
	}
}

func (c serviceRequestReactivates) trafficWait() time.Duration {
	return orDefault(c.traffic, serviceTrafficWindow)
}
func (c serviceRequestReactivates) quietGap() time.Duration {
	return orDefault(c.quiet, serviceQuietGap)
}
func (c serviceRequestReactivates) pause() time.Duration {
	return orDefault(c.settle, serviceSettle)
}
func (c serviceRequestReactivates) answerWait() time.Duration {
	return orDefault(c.answer, serviceAnswerWindow)
}
func (c serviceRequestReactivates) userPlaneWait() time.Duration {
	return orDefault(c.userPlane, serviceUserPlaneWait)
}

// Budget is the three traffic waits (before the release, for that traffic
// to stop, and after the Service Request), the pause, the answer and
// user-plane waits, plus the registration, the AN release and the
// deregistration.
func (c serviceRequestReactivates) Budget() time.Duration {
	return 3*c.trafficWait() + c.pause() + c.answerWait() + c.userPlaneWait() + 15*time.Second
}

// serviceFailure is the check's FAIL message; what was seen follows it.
const serviceFailure = "Service Request did not re-establish the PDU session user plane. TS 24.501 5.6.1.4.1 (5.6.1.4 in Rel-15; Rel-15 onward). Likely cause: AMF/SMF reactivation path."

func (c serviceRequestReactivates) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "Service Accept and the PDU session's user plane re-established after a Service Request from CM-IDLE with the session in its Uplink data status: the N3 tunnel set up again and downlink traffic on it (TS 24.501 5.6.1.4.1)"}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run the checks that register a UE"
		return r
	}
	if env.Downlink == nil {
		r.Verdict, r.Observed = Skip, "no N6 downlink trigger configured"
		r.Detail = "the check needs a responder on the N6 network to send downlink data to the UE, as the traffic that must flow again: name one of the target's responders as the conformance workload's responder"
		return r
	}
	fail := func(observed string, err error) Result {
		r.Verdict, r.Observed, r.Detail = Error, observed, err.Error()
		return r
	}
	cutShort := func(what string, err error) Result {
		r.Verdict, r.Observed = Error, what+" was cut short"
		r.Detail = fmt.Sprintf("%v; raise --per-test-timeout to at least %s", err, c.Budget())
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return fail("harness setup failed", err)
	}
	defer h.Close()
	reg, err := h.Register(ctx)
	if err != nil {
		return fail("UE setup failed", err)
	}
	o := serviceObservation{Session: h.PDUSessionID(), AnswerWindow: c.answerWait() + c.userPlaneWait(), TrafficWindow: c.trafficWait()}

	if err := h.TriggerDownlink(ctx); err != nil {
		return fail("the downlink trigger failed", err)
	}
	sent := time.Now()
	before, err := h.AwaitDownlink(ctx, c.trafficWait(), reg.DLTEID)
	if err != nil {
		return cutShort("the wait for downlink traffic before the release", err)
	}
	if before == nil {
		r.Verdict, r.Observed = Error, "no downlink traffic reached the UE before the release"
		r.Detail = fmt.Sprintf("the trigger's traffic to the UE's address %s did not arrive on its N3 tunnel within %v while the UE was connected, so its absence after the Service Request would say nothing about the core; check the responder's route to the UE pool through the UPF's N6", reg.PDUAddress, c.trafficWait())
		return r
	}
	o.Before = before.Arrival.Sub(sent)
	quiet, err := h.AwaitDownlinkQuiet(ctx, c.quietGap(), c.trafficWait())
	if err != nil {
		return cutShort("the wait for the downlink traffic to stop", err)
	}
	if !quiet {
		r.Verdict, r.Observed = Error, "the downlink traffic did not stop before the release"
		r.Detail = fmt.Sprintf("downlink to the UE was still arriving %v after the trigger; released now, the UE would be paged for it", c.trafficWait())
		return r
	}

	idle, err := h.Idle(ctx)
	if err != nil {
		return fail("the AN release to CM-IDLE did not complete", err)
	}
	o.ReleaseCause = idle.Cause
	if !sleepCtx(ctx, c.pause()) {
		return cutShort("the pause before the Service Request", ctx.Err())
	}

	sctx, cancel := context.WithTimeout(ctx, o.AnswerWindow)
	sr, err := h.ServiceRequest(sctx, engine.WithUplinkDataStatus(o.Session), engine.WithUserPlaneWait(c.userPlaneWait()))
	cancel()
	switch {
	case ctx.Err() != nil:
		return cutShort("the Service Request", ctx.Err())
	case errors.Is(err, engine.ErrServiceUnanswered) && errors.Is(err, context.DeadlineExceeded):
		o.Unanswered = true
	case err != nil:
		return fail("the Service Request procedure failed", err)
	}
	o.Result = sr

	if sr != nil && sr.Accepted && !sr.Released && sr.UserPlane(o.Session) {
		if err := h.WatchDownlink(); err != nil {
			return fail("the reactivated tunnel's data path did not open", err)
		}
		if err := h.TriggerDownlink(ctx); err != nil {
			return fail("the downlink trigger failed", err)
		}
		o.Triggered = time.Now()
		if o.Traffic, err = h.AwaitDownlink(ctx, c.trafficWait(), o.tunnel().DLTEID); err != nil {
			return cutShort("the wait for downlink traffic on the reactivated tunnel", err)
		}
	}
	r.Verdict, r.Observed, r.Detail = classifyService(o)
	return r
}

// serviceObservation is what the check saw, for classifyService.
type serviceObservation struct {
	// Session is the PDU session the Uplink data status named.
	Session uint8
	// ReleaseCause is the cause of the release that made the UE idle.
	ReleaseCause string
	// Before is how long after the first trigger downlink reached the UE
	// on the tunnel of its registration.
	Before time.Duration
	// Result is the Service Request procedure's outcome; nil, with
	// Unanswered set, when no answer came within AnswerWindow.
	Result       *engine.ServiceResult
	Unanswered   bool
	AnswerWindow time.Duration
	// Triggered is when the trigger after the Service Accept returned, and
	// Traffic the first G-PDU on the reactivated tunnel within
	// TrafficWindow of it, nil when none came.
	Triggered     time.Time
	Traffic       *datapath.GPDUArrival
	TrafficWindow time.Duration
}

// tunnel is the session's reactivated N3 tunnel, zero when the core set
// none up.
func (o serviceObservation) tunnel() engine.SessionResult {
	if o.Result != nil {
		for _, s := range o.Result.Reactivated {
			if s.PDUSessionID == o.Session {
				return s
			}
		}
	}
	return engine.SessionResult{}
}

// classifyService judges a Service Request from CM-IDLE: a Service Accept,
// the session's N3 tunnel set up again and downlink traffic on it is PASS.
// No answer, a Service Reject, an Accept followed by a release, an Accept
// without the tunnel, and a tunnel no downlink arrives on are FAIL. The
// detail records the release cause, the downlink seen before the release,
// the answer's delay and IEs, how the tunnel was set up and its endpoints.
func classifyService(o serviceObservation) (Verdict, string, string) {
	facts := []string{
		fmt.Sprintf("PDU session %d in the Uplink data status", o.Session),
		"AN release cause " + o.ReleaseCause,
		fmt.Sprintf("downlink reached the UE %v after the trigger before the release", o.Before.Round(time.Millisecond)),
	}
	failed := func(observed, seen string) (Verdict, string, string) {
		return Fail, observed, serviceFailure + " Seen: " + seen + ". Recorded: " + strings.Join(facts, "; ")
	}
	res := o.Result
	if o.Unanswered || res == nil {
		return failed("no answer to the Service Request",
			fmt.Sprintf("neither Service Accept nor Service Reject within %v (TS 24.501 5.6.1.5: a request that cannot be accepted is answered with SERVICE REJECT)", o.AnswerWindow))
	}
	if res.Rejected {
		if res.Released {
			facts = append(facts, "the core released the UE context after the reject")
		}
		return failed("Service Reject", fmt.Sprintf("Service Reject with 5GMM cause #%d, %v after the Service Request",
			res.RejectCause, res.Answered.Sub(res.Sent).Round(time.Millisecond)))
	}
	facts = append(facts, fmt.Sprintf("Service Accept %v after the Service Request", res.Answered.Sub(res.Sent).Round(time.Millisecond)))
	if a := res.Accept; a != nil {
		if a.HasPDUSessionStatus {
			facts = append(facts, fmt.Sprintf("PDU session status %#04x", a.PDUSessionStatus))
		}
		switch {
		case !a.HasReactivationResult:
			facts = append(facts, "no PDU session reactivation result IE (recorded only)")
		case a.ReactivationResult&(1<<(o.Session&0x0f)) != 0:
			facts = append(facts, fmt.Sprintf("PDU session reactivation result %#04x reports the session not re-established (recorded only)", a.ReactivationResult))
		default:
			facts = append(facts, fmt.Sprintf("PDU session reactivation result %#04x reports the session re-established (recorded only)", a.ReactivationResult))
		}
		if len(a.ReactivationErrors) > 0 {
			facts = append(facts, fmt.Sprintf("PDU session reactivation result error cause % x", a.ReactivationErrors))
		}
	}
	if res.Released {
		return failed("Service Accept, then the UE context released", "the core released the UE context after its Service Accept")
	}
	if !res.UserPlane(o.Session) {
		how := "no Initial Context Setup or PDU Session Resource Setup"
		if res.ContextSetup {
			how = "an Initial Context Setup without the session, and no PDU Session Resource Setup"
		}
		return failed("Service Accept without user plane",
			fmt.Sprintf("Service Accept, and %s for PDU session %d within %v", how, o.Session, o.AnswerWindow))
	}
	t := o.tunnel()
	by := "PDU Session Resource Setup"
	if res.ContextSetup {
		by = "Initial Context Setup"
	}
	facts = append(facts, fmt.Sprintf("N3 tunnel set up again by %s: gNB downlink TEID %#x, UPF %s uplink TEID %#x", by, t.DLTEID, t.UPFAddress, t.UPFTEID))
	if o.Traffic == nil {
		return failed("Service Accept and N3 tunnel, no downlink traffic",
			fmt.Sprintf("Service Accept and the N3 tunnel set up again on N2, and no downlink G-PDU on it within %v of downlink data sent to the UE (TS 23.502 4.2.3.2 step 18a: the SMF gives the UPF the AN Tunnel Info)", o.TrafficWindow))
	}
	facts = append(facts, fmt.Sprintf("downlink G-PDU on the reactivated tunnel %v after the trigger", o.Traffic.Arrival.Sub(o.Triggered).Round(time.Millisecond)))
	return Pass, "Service Accept and downlink traffic on the reactivated N3 tunnel", strings.Join(facts, "; ")
}
