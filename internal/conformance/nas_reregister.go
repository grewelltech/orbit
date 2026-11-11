package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
)

func init() {
	builtins = append(builtins, nasRegistrationAfterRelease{})
}

const (
	// regAfterReleaseWait bounds the wait for the answer to the
	// registration that follows the release, and for that registration to
	// complete: the bound the suite's other registrations have, inside the
	// 15 s a UE itself waits (T3510, TS 24.501 Table 10.2.1).
	regAfterReleaseWait = initialNASWait
	// regAfterReleaseSettle is the pause before the check registers its
	// subscriber afresh after a registration that did not complete. The
	// check is about one SUPI, so the same one must register again, and the
	// core is given this long to finish with what it left unanswered.
	regAfterReleaseSettle = time.Second
	// regAfterReleaseCleanup bounds the deregistration that ends the check.
	regAfterReleaseCleanup = 5 * time.Second
	// regAfterReleaseLinger bounds the wait for the UE CONTEXT RELEASE
	// COMMAND that answers each switch-off deregistration.
	regAfterReleaseLinger = deregReleaseWait
)

// regAfterReleaseGaps are the pauses between the UE CONTEXT RELEASE
// COMPLETE and the new REGISTRATION REQUEST, one attempt each: none, then
// three that step through the first tens of milliseconds after the
// release, since a core can answer a request that arrives with the complete
// and still miss one that arrives while it clears the UE context.
var regAfterReleaseGaps = []time.Duration{0, 5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}

// The outcomes of NAS-REGISTRATION-AFTER-RELEASE.
var (
	regAfterReleaseOutAnswered = Outcome{ID: "answered", Verdict: Pass, Refs: []string{initialRegAcceptRef, initialRegRejectRef},
		Summary: "answered every REGISTRATION REQUEST sent right after the same UE's UE CONTEXT RELEASE COMPLETE"}
	regAfterReleaseOutUnanswered = Outcome{ID: "unanswered", Verdict: Fail, Refs: []string{initialRegAcceptRef, initialRegRejectRef},
		Bound:   &Bound{Wait: regAfterReleaseWait, Source: initialNASBound},
		Summary: "sent no NAS message for a REGISTRATION REQUEST sent right after the same UE's UE CONTEXT RELEASE COMPLETE"}
)

// Outcomes declares what the check can report.
func (nasRegistrationAfterRelease) Outcomes() []Outcome {
	return []Outcome{regAfterReleaseOutAnswered, regAfterReleaseOutUnanswered}
}

// Texts of NAS-REGISTRATION-AFTER-RELEASE.
const (
	regAfterReleaseAnswered   = "REGISTRATION REQUEST sent right after the same UE's UE CONTEXT RELEASE COMPLETE answered every time"
	regAfterReleaseUnanswered = "no NAS answer to a REGISTRATION REQUEST sent right after the same UE's UE CONTEXT RELEASE COMPLETE"
	regAfterReleaseNoRelease  = "core sent no UE CONTEXT RELEASE COMMAND after the switch-off deregistration; the registration after the release could not be sent"
	regAfterReleaseFailure    = "the AMF did not answer an initial REGISTRATION REQUEST that arrived right after the UE CONTEXT RELEASE COMPLETE for the same subscriber's previous UE context. TS 24.501 5.5.1.2.4, 5.5.1.2.5. Likely cause: the AMF drops a registration that arrives while it is still clearing the released UE context."
)

// nasRegistrationAfterRelease registers the UE, deregisters it (switch-off)
// and, when the core's UE CONTEXT RELEASE COMMAND arrives, sends the UE
// CONTEXT RELEASE COMPLETE and at once a new initial REGISTRATION REQUEST
// for the same SUPI, on the same association under a new RAN UE NGAP ID.
// It does so once per gap of regAfterReleaseGaps, the gap being the pause
// between the two messages.
//
// A switch-off deregistration is complete when the AMF receives the request
// (TS 24.501 5.5.2.2.2), so the new request is an ordinary initial
// registration, and 5.5.1.2.4 and 5.5.1.2.5 between them give it an answer:
// a REGISTRATION ACCEPT when the network accepts it, a REGISTRATION REJECT
// when it cannot. The check asks for less than either: any NAS message for
// the new registration within regAfterReleaseWait, such as the
// AUTHENTICATION REQUEST that starts the procedure. Every attempt answered
// is PASS; one that drew no NAS message is FAIL. A core that sends no
// release for a switch-off is BLOCKED on the release after switch-off,
// which NGAP-CTX-RELEASE-AFTER-SWITCH-OFF owns: the stimulus follows it. A
// registration of the SUPI that does not complete, at the start or
// between attempts, is BLOCKED on the registration.
//
// An answered registration is completed, and the next attempt deregisters
// it; one that did not complete is followed, after a pause, by a fresh
// registration of the same SUPI. The UE is deregistered at the end, and the
// detail says what state each unanswered registration left.
type nasRegistrationAfterRelease struct {
	// registerWait, wait, settle and linger override deregRegisterWait,
	// regAfterReleaseWait, regAfterReleaseSettle and
	// regAfterReleaseLinger; gaps overrides regAfterReleaseGaps (tests
	// only).
	registerWait, wait, settle, linger time.Duration
	gaps                               []time.Duration
}

func (nasRegistrationAfterRelease) ID() string         { return "NAS-REGISTRATION-AFTER-RELEASE" }
func (nasRegistrationAfterRelease) Category() Category { return NAS }

// SpecRefs cites the two shalls that give an initial registration request
// its answer, and as context the sentence that completes a switch-off
// deregistration at the request and the UE's own wait for the answer.
func (nasRegistrationAfterRelease) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.5.1.2.4", Strength: Shall, Since: 15, Verified: ts24501Verified, WordingUnchanged: true, Primary: true},
		{Spec: "TS 24.501", Clause: "5.5.1.2.5", Strength: Shall, Since: 15, Verified: ts24501Verified, WordingUnchanged: true},
		{Spec: "TS 24.501", Clause: "5.5.2.2.2", Strength: Descriptive, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 titles the clause \"UE-initiated de-registration procedure completion - common procedure\", \"UE-initiated de-registration procedure completion\" by V15.7.0; the switch-off sentence is unchanged from V15.0.0 through V19.8.0"},
			}},
		{Spec: "TS 24.501", Clause: "Table 10.2.1 T3510", Strength: Descriptive, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 16, Note: "adds a WB-N1/CE mode value of 85s; the normal value stays 15s"},
				{Release: 17, Note: "adds 27s for access via a satellite NG-RAN cell; the normal value stays 15s"},
			}},
	}
}

func (c nasRegistrationAfterRelease) registerTimeout() time.Duration {
	return orDefault(c.registerWait, deregRegisterWait)
}
func (c nasRegistrationAfterRelease) waitTimeout() time.Duration {
	return orDefault(c.wait, regAfterReleaseWait)
}
func (c nasRegistrationAfterRelease) settleTime() time.Duration {
	return orDefault(c.settle, regAfterReleaseSettle)
}
func (c nasRegistrationAfterRelease) lingerTime() time.Duration {
	return orDefault(c.linger, regAfterReleaseLinger)
}
func (c nasRegistrationAfterRelease) attemptGaps() []time.Duration {
	if c.gaps != nil {
		return c.gaps
	}
	return regAfterReleaseGaps
}

// Budget covers the first registration, each attempt (the release, the gap,
// the new registration and, should it not complete, the pause and a fresh
// registration), the same after the last attempt, the deregistration and
// the liveness probe.
func (c nasRegistrationAfterRelease) Budget() time.Duration {
	b := c.registerTimeout() + regAfterReleaseCleanup + 5*time.Second
	for _, gap := range c.attemptGaps() {
		b += c.lingerTime() + gap + c.waitTimeout() + c.settleTime() + c.registerTimeout()
	}
	return b.Round(time.Second)
}

// regAttempt is one registration sent after a release.
type regAttempt struct {
	// gap is the pause asked for and measured the one taken, between the
	// UE CONTEXT RELEASE COMPLETE and the INITIAL UE MESSAGE as written.
	gap, measured time.Duration
	// notRun says why the registration was not sent ("" when it was),
	// noRelease that the reason is a missing UE CONTEXT RELEASE COMMAND,
	// and blocked that it is a registration of the SUPI the core did not
	// complete.
	notRun             string
	noRelease, blocked bool
	// answer names the first NAS message for the new registration, ""
	// when none came within the wait, and after is how long it took.
	answer string
	after  time.Duration
	// other names what came in place of a NAS answer, if anything.
	other string
	// registered says the registration completed, and rejected that it
	// ended with a REGISTRATION REJECT.
	registered, rejected bool
	// ended says how a registration that did not complete ended.
	ended string
}

// answered reports whether the registration drew a NAS answer.
func (a regAttempt) answered() bool { return a.notRun == "" && a.answer != "" }

// String renders the attempt for the detail.
func (a regAttempt) String() string {
	head := fmt.Sprintf("gap %s", a.gap)
	if a.notRun != "" {
		return head + ": not run: " + a.notRun
	}
	head += fmt.Sprintf(" (REGISTRATION REQUEST written %s after the UE CONTEXT RELEASE COMPLETE)", a.measured.Round(10*time.Microsecond))
	switch {
	case a.answer == "" && a.other != "":
		return fmt.Sprintf("%s: no NAS answer; %s; %s", head, a.other, a.ended)
	case a.answer == "":
		return fmt.Sprintf("%s: no NAS answer; %s", head, a.ended)
	case a.registered:
		return fmt.Sprintf("%s: %s after %s, registration completed", head, a.answer, a.after.Round(time.Millisecond))
	}
	return fmt.Sprintf("%s: %s after %s; %s", head, a.answer, a.after.Round(time.Millisecond), a.ended)
}

func (c nasRegistrationAfterRelease) Run(ctx context.Context, env Env) Result {
	wait := c.waitTimeout()
	r := Result{Expected: fmt.Sprintf("a NAS answer (the REGISTRATION ACCEPT, or a message of the procedure such as the AUTHENTICATION REQUEST) within %s to an initial REGISTRATION REQUEST sent right after the UE CONTEXT RELEASE COMPLETE that ends the same UE's switch-off deregistration (TS 24.501 5.5.1.2.4, 5.5.1.2.5)", wait)}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run checks that register a UE")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	rctx, cancel := context.WithTimeout(ctx, c.registerTimeout())
	_, err = h.RegisterWithProbe(rctx, nil)
	cancel()
	if err != nil {
		return r.SetupFailed("registration failed", err)
	}

	gaps := c.attemptGaps()
	var attempts []regAttempt
	for i, gap := range gaps {
		if i > 0 && !h.registered {
			if why, blocked := c.registerAfresh(ctx, h); why != "" {
				for _, g := range gaps[i:] {
					attempts = append(attempts, regAttempt{gap: g, notRun: why, blocked: blocked})
				}
				break
			}
		}
		a := c.attempt(ctx, h, gap)
		attempts = append(attempts, a)
		if a.noRelease {
			// A core that sent no release once is not asked again.
			for _, g := range gaps[i+1:] {
				attempts = append(attempts, regAttempt{gap: g, notRun: "not tried after a deregistration without a release", noRelease: true})
			}
			break
		}
	}

	// The UE leaves deregistered. A last registration that did not
	// complete is followed by a fresh one, which shows whether the core
	// still registers the SUPI and lets the UE deregister properly.
	var left []string
	if n := len(attempts); n > 0 && !h.registered && attempts[n-1].notRun == "" && !attempts[n-1].rejected && ctx.Err() == nil {
		if why, _ := c.registerAfresh(ctx, h); why != "" {
			left = append(left, "after the last registration, "+why)
		} else {
			left = append(left, fmt.Sprintf("a registration of the same SUPI %s after the last one completed", c.settleTime()))
		}
	}
	if h.registered {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), regAfterReleaseCleanup)
		res, err := h.DeregisterWith(dctx, engine.WithReleaseWait(c.lingerTime()))
		cancel()
		if err != nil || res == nil {
			left = append(left, fmt.Sprintf("the final deregistration was not sent (%v) and the association was dropped", err))
		} else {
			left = append(left, "the UE was deregistered (switch-off): "+releaseNote(res))
		}
	} else {
		left = append(left, "the association was closed with the UE not registered")
	}

	r = judgeRegAfterRelease(r, attempts, wait)
	r.Detail = joinDetail(r.Detail, "state left: "+strings.Join(left, "; "))
	if r.Verdict == Error || r.Verdict == Blocked || ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the registrations that followed a release")
}

// registerAfresh pauses for the settle and registers the harness's UE on a
// new association, returning "" or why that failed, and whether the
// failure is the core's: a registration that did not complete, with the
// check's time left and no local fault.
func (c nasRegistrationAfterRelease) registerAfresh(ctx context.Context, h *UEHarness) (string, bool) {
	if !sleepCtx(ctx, c.settleTime()) {
		return "the check's time ran out before the UE could register again", false
	}
	rctx, cancel := context.WithTimeout(ctx, c.registerTimeout())
	defer cancel()
	if _, err := h.RegisterWithProbe(rctx, nil); err != nil {
		return fmt.Sprintf("a registration of the same SUPI %s later did not complete (%v)", c.settleTime(), err), ctx.Err() == nil && !localFault(err)
	}
	return "", false
}

// attempt deregisters the registered UE and registers it again gap after
// the UE CONTEXT RELEASE COMPLETE, recording the first NAS message the new
// registration draws. The registration is given the check's wait to
// complete.
func (c nasRegistrationAfterRelease) attempt(ctx context.Context, h *UEHarness, gap time.Duration) regAttempt {
	a := regAttempt{gap: gap}
	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	defer halt()
	var (
		mu        sync.Mutex
		firstNAS  time.Time
		firstType uint8
		others    []string
	)
	// The wait runs from just before the request is written.
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	probe := &engine.AttachProbe{
		Stop: stop,
		BeforeRegistration: func(context.Context, gnb.Transport) error {
			timer = time.AfterFunc(c.waitTimeout(), halt)
			return nil
		},
		OnDownlink: func(ev engine.DownlinkEvent) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case ev.NAS != nil && firstNAS.IsZero():
				firstNAS, firstType = ev.At, ev.NASType
			case ev.NAS == nil && firstNAS.IsZero():
				others = append(others, ev.Procedure)
			}
		},
	}
	res, err := h.ReregisterAfterRelease(ctx, probe, gap, engine.WithReleaseWait(c.lingerTime()))
	mu.Lock()
	defer mu.Unlock()
	switch {
	case err != nil && res != nil && res.Deregistration != nil && res.Deregistration.ReleaseErr != nil:
		a.notRun, a.noRelease = res.Deregistration.ReleaseErr.Error(), true
		return a
	case err != nil:
		a.notRun = err.Error()
		return a
	}
	a.measured = res.RequestedAt.Sub(res.CompletedAt)
	if !firstNAS.IsZero() {
		a.answer, a.after = nasAnswerName(firstType), firstNAS.Sub(res.RequestedAt)
	}
	if len(others) > 0 {
		a.other = "received instead: " + strings.Join(others, ", ")
	}
	const closed = "the association was closed with the UE not registered"
	switch {
	case res.Attach != nil:
		a.registered = true
	case errors.Is(res.AttachErr, engine.ErrRegistrationRejected):
		a.rejected, a.ended = true, "the registration was rejected; "+closed
	case errors.Is(res.AttachErr, engine.ErrReleasedByCore):
		a.ended = "the core released the new UE context; " + closed
	case errors.Is(res.AttachErr, engine.ErrProbeStopped) && a.answer == "":
		a.ended = fmt.Sprintf("none within %s; the previous UE context was released and the new request drew nothing, so the core was left with a REGISTRATION REQUEST it did not answer; %s", c.waitTimeout(), closed)
	case errors.Is(res.AttachErr, engine.ErrProbeStopped):
		a.ended = fmt.Sprintf("the registration did not complete within %s; %s", c.waitTimeout(), closed)
	default:
		a.ended = fmt.Sprintf("the registration ended with: %v; %s", res.AttachErr, closed)
	}
	return a
}

// nasAnswerName names the 5GMM message that answered a registration, from
// its message type; 0 is a message whose type could not be read.
func nasAnswerName(t uint8) string {
	switch t {
	case f5nas.MsgTypeAuthenticationRequest:
		return "AUTHENTICATION REQUEST"
	case f5nas.MsgTypeIdentityRequest:
		return "IDENTITY REQUEST"
	case f5nas.MsgTypeSecurityModeCommand:
		return "SECURITY MODE COMMAND"
	case f5nas.MsgTypeRegistrationAccept:
		return "REGISTRATION ACCEPT"
	case f5nas.MsgTypeRegistrationReject:
		return "REGISTRATION REJECT"
	case f5nas.MsgTypeAuthenticationReject:
		return "AUTHENTICATION REJECT"
	case 0:
		return "a NAS message of unreadable type"
	}
	return fmt.Sprintf("5GMM message type %#02x", t)
}

// judgeRegAfterRelease returns r with the attempts' outcome. An attempt
// that drew no NAS answer is FAIL whatever the others did. Otherwise a
// missing UE CONTEXT RELEASE COMMAND, which leaves nothing to send the
// registration after, is INFO; a registration of the SUPI the core did not
// complete is BLOCKED on the registration; any other attempt that could
// not be run is ERROR; and every attempt answered is PASS.
func judgeRegAfterRelease(r Result, attempts []regAttempt, wait time.Duration) Result {
	lines := make([]string, len(attempts))
	var unanswered, noRelease, blocked, notRun, answered int
	for i, a := range attempts {
		lines[i] = a.String()
		switch {
		case a.noRelease:
			noRelease++
		case a.blocked:
			blocked++
		case a.notRun != "":
			notRun++
		case a.answered():
			answered++
		default:
			unanswered++
		}
	}
	detail := strings.Join(lines, "; ")
	switch {
	case len(attempts) == 0:
		return r.Errored("no registration attempted", "the check has no gaps to try")
	case unanswered > 0:
		return r.As(regAfterReleaseOutUnanswered, regAfterReleaseUnanswered,
			fmt.Sprintf("%s %d of %d registrations drew no NAS message within %s (absence inferred within this bound). Recorded: %s", regAfterReleaseFailure, unanswered, len(attempts), wait, detail))
	case noRelease > 0:
		return r.BlockedBy(ProcSwitchOffRelease, regAfterReleaseNoRelease,
			fmt.Sprintf("%d of %d registrations answered; no UE CONTEXT RELEASE COMMAND within %s of a switch-off deregistration; %s", answered, len(attempts), regAfterReleaseLinger, detail))
	case blocked > 0:
		return r.BlockedBy(ProcRegistration, "a registration of the same SUPI did not complete", detail)
	case notRun > 0:
		return r.Errored("a registration after the release could not be sent", detail)
	}
	return r.As(regAfterReleaseOutAnswered, regAfterReleaseAnswered, detail)
}
