package conformance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/engine"
)

func init() {
	builtins = append(builtins, nasInitialRegistration{})
}

// initialRegWait bounds the wait for the REGISTRATION ACCEPT or REJECT
// that ends an initial registration: the bound the suite's other
// registrations have, inside the 15 s a UE itself waits (T3510, TS 24.501
// Table 10.2.1).
const initialRegWait = initialNASWait

// The keys of the two shalls that give an initial registration request its
// answer.
const (
	initialRegAcceptRef = "TS 24.501 5.5.1.2.4"
	initialRegRejectRef = "TS 24.501 5.5.1.2.5"
)

// ref24501RegAccept and ref24501RegReject are the two shalls: "If the
// initial registration request is accepted by the network, the AMF shall
// send a REGISTRATION ACCEPT message to the UE", and "If the initial
// registration request cannot be accepted by the network, the AMF shall
// send a REGISTRATION REJECT message to the UE including an appropriate
// 5GMM cause value". Each is conditional; together they give every initial
// registration request an answer.
var (
	ref24501RegAccept = SpecRef{Spec: "TS 24.501", Clause: "5.5.1.2.4", Strength: Shall, Since: 15, Verified: ts24501Verified, WordingUnchanged: true}
	ref24501RegReject = SpecRef{Spec: "TS 24.501", Clause: "5.5.1.2.5", Strength: Shall, Since: 15, Verified: ts24501Verified, WordingUnchanged: true}
)

// nasInitialRegistration registers the run's own UE, without a PDU
// session, and deregisters it. It owns the registration: a check whose own
// registration the core does not complete reports BLOCKED and names this
// check.
//
// TS 24.501 5.5.1.2.4 and 5.5.1.2.5 between them give an initial
// registration request its answer: a REGISTRATION ACCEPT when the network
// accepts it, a REGISTRATION REJECT when it cannot. An Accept is PASS. A
// core that sends neither, within initialRegWait or before releasing the
// UE context, violates one of the two shalls, whichever case applied:
// FAIL. A REGISTRATION REJECT, or an AUTHENTICATION REJECT, for the run's
// own subscriber follows the shalls and violates nothing; the core cannot
// complete the procedure for the subscriber the run is configured with, so
// it is WARN, for availability.
type nasInitialRegistration struct {
	// wait overrides initialRegWait (tests only).
	wait time.Duration
}

// The outcomes of NAS-INITIAL-REGISTRATION.
var (
	initialRegAccepted = Outcome{ID: "accepted", Verdict: Pass, Refs: []string{initialRegAcceptRef},
		Summary: "answered the initial REGISTRATION REQUEST with REGISTRATION ACCEPT"}
	// A REGISTRATION REJECT is the answer 5.5.1.2.5 asks of a network that
	// cannot accept the request, so it violates nothing. It is WARN for
	// availability: the core does not complete the registration for the
	// subscriber the run is configured with.
	initialRegRejected = Outcome{ID: "rejected", Verdict: Warn, Criterion: CriterionAvailability, NotCompleted: ProcRegistration,
		Summary: "answered the initial REGISTRATION REQUEST with REGISTRATION REJECT"}
	// An AUTHENTICATION REJECT ends the registration without the answer
	// either shall asks for, but in the authentication procedure, which the
	// shalls the check cites do not cover. WARN for availability, as for
	// the REGISTRATION REJECT.
	initialRegAuthRejected = Outcome{ID: "authentication-rejected", Verdict: Warn, Criterion: CriterionAvailability, NotCompleted: ProcRegistration,
		Summary: "ended the registration with AUTHENTICATION REJECT"}
	initialRegUnanswered = Outcome{ID: "unanswered", Verdict: Fail, Refs: []string{initialRegAcceptRef, initialRegRejectRef}, NotCompleted: ProcRegistration,
		Bound:   &Bound{Wait: initialRegWait, Source: "check bound; inside T3510, the UE's own 15 s wait for the answer (TS 24.501 Table 10.2.1)"},
		Summary: "sent neither REGISTRATION ACCEPT nor REGISTRATION REJECT"}
	initialRegReleased = Outcome{ID: "released", Verdict: Fail, Refs: []string{initialRegAcceptRef, initialRegRejectRef}, NotCompleted: ProcRegistration,
		Summary: "released the UE context without REGISTRATION ACCEPT or REGISTRATION REJECT"}
	// A registration that ended otherwise (the association lost, a message
	// the UE could not use) did not complete, and no clause the check cites
	// says what it violates. WARN for availability: the core did not
	// complete the procedure.
	initialRegBroken = Outcome{ID: "not-completed", Verdict: Warn, Criterion: CriterionAvailability, NotCompleted: ProcRegistration,
		Summary: "ended the registration without completing it, in another way"}
)

func (nasInitialRegistration) ID() string         { return "NAS-INITIAL-REGISTRATION" }
func (nasInitialRegistration) Category() Category { return NAS }

// SpecRefs cites the two shalls that give an initial registration request
// its answer.
func (nasInitialRegistration) SpecRefs() []SpecRef {
	accept := ref24501RegAccept
	accept.Primary = true
	return []SpecRef{accept, ref24501RegReject}
}

// Needs is one UE and the NG Setup: the registration is the check's own.
func (nasInitialRegistration) Needs() CheckNeeds {
	return CheckNeeds{UEs: 1, GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, Procedures: []Procedure{ProcNGSetup}}
}

// Owns is the registration.
func (nasInitialRegistration) Owns() []Procedure { return []Procedure{ProcRegistration} }

// Outcomes declares what the check can report.
func (nasInitialRegistration) Outcomes() []Outcome {
	return []Outcome{initialRegAccepted, initialRegRejected, initialRegAuthRejected, initialRegUnanswered, initialRegReleased, initialRegBroken}
}

func (c nasInitialRegistration) waitTimeout() time.Duration { return orDefault(c.wait, initialRegWait) }

// Budget covers the registration, the deregistration and the liveness
// probe.
func (c nasInitialRegistration) Budget() time.Duration {
	return c.waitTimeout() + initialNASDeregisterWait + 5*time.Second
}

func (c nasInitialRegistration) Run(ctx context.Context, env Env) Result {
	wait := c.waitTimeout()
	r := Result{Expected: fmt.Sprintf("REGISTRATION ACCEPT, or REGISTRATION REJECT, within %s of an initial REGISTRATION REQUEST for the run's own subscriber (TS 24.501 5.5.1.2.4, 5.5.1.2.5)", wait)}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run this check")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	o := registerOnce(ctx, h, wait)
	left := "state left: " + deregisterAfter(h, o)
	switch {
	case o.err != nil && ctx.Err() != nil:
		return r.Errored("timed out during the registration",
			fmt.Sprintf("the per-test timeout expired before the %s the check waits; raise --per-test-timeout to at least %s", wait, c.Budget()))
	case o.err != nil && (errors.Is(o.err, engine.ErrNGSetup) || localFault(o.err)):
		return r.SetupFailed("UE setup failed", o.err)
	}
	o2, observed, detail := classifyInitialRegistration(o, wait)
	r = r.As(o2, observed, joinDetail(detail, left))
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the registration")
}

// registerOnce registers h's UE, without a PDU session, until a
// REGISTRATION ACCEPT or REJECT, an AUTHENTICATION REJECT, a release or
// wait.
func registerOnce(ctx context.Context, h *UEHarness, wait time.Duration) initialNASOutcome {
	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	defer halt()
	var mu sync.Mutex
	var o initialNASOutcome
	timedOut := false
	timer := time.AfterFunc(wait, func() {
		mu.Lock()
		defer mu.Unlock()
		timedOut = true
		halt()
	})
	defer timer.Stop()
	_, err := h.RegisterWithProbe(ctx, &engine.AttachProbe{
		Stop: stop,
		OnNAS: func(ev engine.NASEvent) {
			mu.Lock()
			defer mu.Unlock()
			switch ev.MessageType() {
			case f5nas.MsgTypeSecurityModeCommand:
				o.smcs++
			case f5nas.MsgTypeRegistrationReject:
				if !o.sawReject() {
					o.reject, o.rejectBeforeSMC = readableNAS(ev), o.smcs == 0
					if o.reject == nil {
						o.rejectErr = ev.Err
						if o.rejectErr == nil {
							o.rejectErr = errors.New("no readable message")
						}
					}
				}
				halt()
			case f5nas.MsgTypeAuthenticationReject:
				o.authReject = true
				halt()
			}
		},
	})
	mu.Lock()
	defer mu.Unlock()
	switch {
	case err == nil:
		o.registered = true
	case o.sawReject(), o.authReject:
	case errors.Is(err, engine.ErrReleasedByCore):
		o.released = true
	case timedOut && ctx.Err() == nil:
		o.noAnswer = true
	default:
		o.err = err
	}
	return o
}

// classifyInitialRegistration maps how the registration ended onto the
// check's outcomes: an Accept is PASS; a REGISTRATION or AUTHENTICATION
// REJECT is WARN; no answer within wait, or a release without one, is
// FAIL; any other ending is WARN.
func classifyInitialRegistration(o initialNASOutcome, wait time.Duration) (Outcome, string, string) {
	security := fmt.Sprintf("%d SECURITY MODE COMMAND(s) before it", o.smcs)
	switch {
	case o.registered:
		return initialRegAccepted, "REGISTRATION ACCEPT", "the UE registered"
	case o.authReject:
		return initialRegAuthRejected, "AUTHENTICATION REJECT",
			"the core refused to authenticate the run's own subscriber; no requirement is violated, and the registration did not complete"
	case o.reject != nil:
		return initialRegRejected, fmt.Sprintf("REGISTRATION REJECT, 5GMM cause #%d", o.rejectCause()),
			fmt.Sprintf("answer: %s, %s; TS 24.501 5.5.1.2.5 lets a network that cannot accept the request reject it, so no requirement is violated, and the registration did not complete", o.answer(), security)
	case o.rejectErr != nil:
		return initialRegRejected, "REGISTRATION REJECT that could not be read",
			fmt.Sprintf("%v; %s; the registration did not complete", o.rejectErr, security)
	case o.released:
		return initialRegReleased, "UE CONTEXT RELEASE COMMAND without REGISTRATION ACCEPT or REJECT",
			fmt.Sprintf("the core released the UE context, %s, and sent neither REGISTRATION ACCEPT nor REGISTRATION REJECT; whether it accepted the request or could not, TS 24.501 5.5.1.2.4 or 5.5.1.2.5 required one", security)
	case o.noAnswer:
		return initialRegUnanswered, "no REGISTRATION ACCEPT or REJECT",
			fmt.Sprintf("neither arrived within %s (absence inferred within this bound), %s; whether the network accepted the request or could not, TS 24.501 5.5.1.2.4 or 5.5.1.2.5 required one", wait, security)
	}
	why := "the registration ended without a recorded answer"
	if o.err != nil {
		why = o.err.Error()
	}
	return initialRegBroken, "registration did not complete", fmt.Sprintf("%s; %s", why, security)
}
