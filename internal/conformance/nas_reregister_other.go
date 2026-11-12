package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/ue"
)

func init() {
	builtins = append(builtins, nasRegistrationDuringOtherRelease{})
}

// regOtherNGSetupWait bounds the NG Setup of the association the other UE
// registers on, which comes before the first UE's deregistration.
const regOtherNGSetupWait = ngSetupWait

// regOtherGaps are the pauses between one UE's UE CONTEXT RELEASE COMPLETE
// and another UE's INITIAL UE MESSAGE, one attempt each: 0 to 50 ms in
// even 10 ms steps. Another UE's registration can arrive at any moment
// relative to a release, and the AMF must answer it whenever it does, so
// no gap is singled out: the sweep samples the first 50 ms after the
// complete evenly, the stretch in which the AMF can still be acting on the
// release it has just been told is complete.
var regOtherGaps = []time.Duration{0, 10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond, 40 * time.Millisecond, 50 * time.Millisecond}

// The outcomes of NAS-REGISTRATION-DURING-OTHER-RELEASE.
var (
	regOtherOutAnswered = Outcome{ID: "answered", Verdict: Pass, Refs: []string{initialRegAcceptRef, initialRegRejectRef},
		Summary: "answered every REGISTRATION REQUEST another UE sent, on its own association, right after a UE CONTEXT RELEASE COMPLETE"}
	regOtherOutUnanswered = Outcome{ID: "unanswered", Verdict: Fail, Refs: []string{initialRegAcceptRef, initialRegRejectRef},
		Bound:   &Bound{Wait: regAfterReleaseWait, Source: initialNASBound},
		Summary: "sent no NAS message for a REGISTRATION REQUEST another UE sent, on its own association, right after a UE CONTEXT RELEASE COMPLETE"}
)

// Outcomes declares what the check can report.
func (nasRegistrationDuringOtherRelease) Outcomes() []Outcome {
	return []Outcome{regOtherOutAnswered, regOtherOutUnanswered}
}

// Texts of NAS-REGISTRATION-DURING-OTHER-RELEASE.
const (
	regOtherAnswered   = "another UE's REGISTRATION REQUEST sent right after a UE CONTEXT RELEASE COMPLETE answered every time"
	regOtherUnanswered = "no NAS answer to another UE's REGISTRATION REQUEST sent right after a UE CONTEXT RELEASE COMPLETE"
	regOtherNoRelease  = "core sent no UE CONTEXT RELEASE COMMAND after the switch-off deregistration; the other UE's registration could not be sent"
	regOtherNoNGSetup  = "the NG Setup of the other UE's gNB did not complete"
	regOtherFailure    = "the AMF did not answer an initial REGISTRATION REQUEST that one UE sent, on its own NG association, right after the UE CONTEXT RELEASE COMPLETE for another UE. TS 24.501 5.5.1.2.4, 5.5.1.2.5. Likely cause: the AMF takes an INITIAL UE MESSAGE that arrives while it clears another UE's context as part of that context, and drops it."
)

// nasRegistrationDuringOtherRelease registers one UE on the check's own
// gNB and sets up a second gNB, with its own gNB ID and NG association. It
// then deregisters the first UE (switch-off) and, when the core's UE
// CONTEXT RELEASE COMMAND arrives, the first gNB sends the UE CONTEXT
// RELEASE COMPLETE and the second, gap later, an INITIAL UE MESSAGE with
// the initial REGISTRATION REQUEST of another subscriber. It does so once
// per gap of regOtherGaps. The UE that registered becomes the one the next
// attempt deregisters, so the two subscribers and the two gNBs take turns.
//
// The other UE's request is an ordinary initial registration, and TS
// 24.501 5.5.1.2.4 and 5.5.1.2.5 between them give it an answer, as in
// NAS-REGISTRATION-AFTER-RELEASE: any NAS message for it within
// regAfterReleaseWait is PASS, none is FAIL. A missing release is BLOCKED
// on the release after switch-off, a second gNB the core does not set up
// is BLOCKED on the NG Setup, and a registration that does not complete,
// at the start or between attempts, is BLOCKED on the registration.
//
// A registration that did not complete is followed, after a pause, by a
// fresh registration of the same subscriber on the check's own gNB. The
// registered UE is deregistered at the end.
type nasRegistrationDuringOtherRelease struct {
	// registerWait, wait, settle and linger override deregRegisterWait,
	// regAfterReleaseWait, regAfterReleaseSettle and
	// regAfterReleaseLinger; gaps overrides regOtherGaps (tests only).
	registerWait, wait, settle, linger time.Duration
	gaps                               []time.Duration
}

func (nasRegistrationDuringOtherRelease) ID() string { return "NAS-REGISTRATION-DURING-OTHER-RELEASE" }

// SpecRefs cites the two shalls that give an initial registration request
// its answer, and as context the sentence that completes a switch-off
// deregistration at the request and the UE's own wait for the answer: the
// references of NAS-REGISTRATION-AFTER-RELEASE.
func (nasRegistrationDuringOtherRelease) SpecRefs() []SpecRef {
	return nasRegistrationAfterRelease{}.SpecRefs()
}

// Needs is two subscribers, registered one after the other, and a second
// gNB for the other UE.
func (nasRegistrationDuringOtherRelease) Needs() CheckNeeds {
	return CheckNeeds{UEs: 1, Subscribers: 2, GNBRoles: []GNBRole{RoleOwn, RoleHandover, RoleLiveness},
		Procedures: append(append([]Procedure(nil), registrationFirst...), ProcSwitchOffRelease)}
}

// sibling is NAS-REGISTRATION-AFTER-RELEASE with the check's waits, whose
// fresh registration the check uses.
func (c nasRegistrationDuringOtherRelease) sibling() nasRegistrationAfterRelease {
	return nasRegistrationAfterRelease{registerWait: c.registerWait, wait: c.wait, settle: c.settle, linger: c.linger}
}

func (c nasRegistrationDuringOtherRelease) attemptGaps() []time.Duration {
	if c.gaps != nil {
		return c.gaps
	}
	return regOtherGaps
}

// Budget covers the first registration, each attempt (the second gNB's NG
// Setup, the release, the gap, the other UE's registration and, should it
// not complete, the pause and a fresh registration), the deregistration
// and the liveness probe.
func (c nasRegistrationDuringOtherRelease) Budget() time.Duration {
	s := c.sibling()
	b := s.registerTimeout() + regAfterReleaseCleanup + 5*time.Second
	for _, gap := range c.attemptGaps() {
		b += regOtherNGSetupWait + s.lingerTime() + gap + s.waitTimeout() + s.settleTime() + s.registerTimeout()
	}
	return b.Round(time.Second)
}

// otherAttempt is one registration of another UE sent after a release.
type otherAttempt struct {
	regAttempt
	// ngSetupErr says why the second gNB was not set up, nil when it was.
	ngSetupErr error
	// releaseAfter is how long the core took from the DEREGISTRATION
	// REQUEST to the UE CONTEXT RELEASE COMMAND, zero when unknown.
	releaseAfter time.Duration
}

// String renders the attempt for the detail.
func (a otherAttempt) String() string {
	s := a.regAttempt.String()
	if a.releaseAfter > 0 {
		s += fmt.Sprintf(" [the release came %s after the DEREGISTRATION REQUEST]", a.releaseAfter.Round(100*time.Microsecond))
	}
	return s
}

func (c nasRegistrationDuringOtherRelease) Run(ctx context.Context, env Env) Result {
	s := c.sibling()
	wait := s.waitTimeout()
	r := Result{Expected: fmt.Sprintf("a NAS answer (the REGISTRATION ACCEPT, or a message of the procedure such as the AUTHENTICATION REQUEST) within %s to an initial REGISTRATION REQUEST another UE sends, on its own NG association, right after the UE CONTEXT RELEASE COMPLETE that ends a UE's switch-off deregistration (TS 24.501 5.5.1.2.4, 5.5.1.2.5)", wait)}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run checks that register a UE")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	rctx, cancel := context.WithTimeout(ctx, s.registerTimeout())
	_, err = h.RegisterWithProbe(rctx, nil)
	cancel()
	if err != nil {
		return r.SetupFailed("registration failed", err)
	}

	gaps := c.attemptGaps()
	var attempts []otherAttempt
	for i, gap := range gaps {
		if i > 0 && !h.registered {
			if why, blocked := s.registerAfresh(ctx, h); why != "" {
				for _, g := range gaps[i:] {
					attempts = append(attempts, otherAttempt{regAttempt: regAttempt{gap: g, notRun: why, blocked: blocked}})
				}
				break
			}
		}
		a := c.attempt(ctx, h, gap)
		attempts = append(attempts, a)
		if a.noRelease || a.ngSetupErr != nil {
			// A core that sent no release, or did not set the second gNB
			// up, once is not asked again.
			why := "not tried after a deregistration without a release"
			if a.ngSetupErr != nil {
				why = "not tried after the second gNB's NG Setup did not complete"
			}
			for _, g := range gaps[i+1:] {
				attempts = append(attempts, otherAttempt{regAttempt: regAttempt{gap: g, notRun: why, noRelease: a.noRelease}, ngSetupErr: a.ngSetupErr})
			}
			break
		}
	}

	// The UE leaves deregistered. A last registration that did not
	// complete is followed by a fresh one, which shows whether the core
	// still registers that subscriber and lets the UE deregister properly.
	var left []string
	if n := len(attempts); n > 0 && !h.registered && attempts[n-1].notRun == "" && !attempts[n-1].rejected && ctx.Err() == nil {
		if why, _ := s.registerAfresh(ctx, h); why != "" {
			left = append(left, "after the last registration, "+why)
		} else {
			left = append(left, fmt.Sprintf("a registration of the same SUPI %s after the last one completed", s.settleTime()))
		}
	}
	if h.registered {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), regAfterReleaseCleanup)
		res, err := h.DeregisterWith(dctx, engine.WithReleaseWait(s.lingerTime()))
		cancel()
		if err != nil || res == nil {
			left = append(left, fmt.Sprintf("the final deregistration was not sent (%v) and the association was dropped", err))
		} else {
			left = append(left, "the UE was deregistered (switch-off): "+releaseNote(res))
		}
	} else {
		left = append(left, "the associations were closed with no UE registered")
	}

	r = judgeRegOther(r, attempts, wait)
	if env.UE.Subscribers <= 1 {
		r.Detail = joinDetail(r.Detail, "the subscriber pool has one SUPI, so the other UE was the same subscriber on its own association")
	}
	r.Detail = joinDetail(r.Detail, "state left: "+strings.Join(left, "; "))
	if r.Verdict == Error || r.Verdict == Blocked || ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the registrations that followed another UE's release")
}

// attempt deregisters the registered UE and registers the other subscriber
// on the other gNB gap after the UE CONTEXT RELEASE COMPLETE, recording the
// first NAS message the registration draws. The registration is given the
// check's wait to complete.
func (c nasRegistrationDuringOtherRelease) attempt(ctx context.Context, h *UEHarness, gap time.Duration) otherAttempt {
	s := c.sibling()
	a := otherAttempt{regAttempt: regAttempt{gap: gap}}
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
			timer = time.AfterFunc(s.waitTimeout(), halt)
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
	bound := regOtherNGSetupWait + s.lingerTime() + gap + s.waitTimeout() + 2*time.Second
	res, err := h.registerOtherAfterRelease(ctx, probe, gap, bound, engine.WithReleaseWait(s.lingerTime()))
	mu.Lock()
	defer mu.Unlock()
	if res != nil && res.Deregistration != nil && res.Deregistration.Release != nil {
		a.releaseAfter = res.Deregistration.Release.At.Sub(res.Deregistration.SentAt)
	}
	switch {
	case errors.Is(err, engine.ErrNGSetup):
		a.notRun, a.ngSetupErr = err.Error(), err
		return a
	case err != nil && res != nil && res.Deregistration != nil && res.Deregistration.ReleaseErr != nil:
		a.notRun, a.noRelease = releaseNotRun(res.Deregistration)
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
	const closed = "its association was closed with the UE not registered"
	switch {
	case res.Attach != nil:
		a.registered = true
	case errors.Is(res.AttachErr, engine.ErrRegistrationRejected):
		a.rejected, a.ended = true, "the registration was rejected; "+closed
	case errors.Is(res.AttachErr, engine.ErrReleasedByCore):
		a.ended = "the core released the new UE context; " + closed
	case errors.Is(res.AttachErr, engine.ErrProbeStopped) && a.answer == "":
		a.ended = fmt.Sprintf("none within %s; the other UE's context was released and this request drew nothing, so the core was left with a REGISTRATION REQUEST it did not answer; %s", s.waitTimeout(), closed)
	case errors.Is(res.AttachErr, engine.ErrProbeStopped):
		a.ended = fmt.Sprintf("the registration did not complete within %s; %s", s.waitTimeout(), closed)
	default:
		a.ended = fmt.Sprintf("the registration ended with: %v; %s", res.AttachErr, closed)
	}
	return a
}

// judgeRegOther returns r with the attempts' outcome, as
// judgeRegAfterRelease does: an attempt that drew no NAS answer is FAIL
// whatever the others did. Otherwise a missing UE CONTEXT RELEASE COMMAND
// is BLOCKED on the release after switch-off, a second gNB the core did not
// set up BLOCKED on the NG Setup (ERROR for a local fault), a registration
// the core did not complete BLOCKED on the registration, any other attempt
// that could not be run ERROR, and every attempt answered PASS.
func judgeRegOther(r Result, attempts []otherAttempt, wait time.Duration) Result {
	lines := make([]string, len(attempts))
	var unanswered, noRelease, blocked, notRun, answered int
	var ngSetupErr error
	for i, a := range attempts {
		lines[i] = a.String()
		switch {
		case a.ngSetupErr != nil:
			ngSetupErr = a.ngSetupErr
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
		return r.As(regOtherOutUnanswered, regOtherUnanswered,
			fmt.Sprintf("%s %d of %d registrations drew no NAS message within %s (absence inferred within this bound). Recorded: %s", regOtherFailure, unanswered, len(attempts), wait, detail))
	case noRelease > 0:
		return r.BlockedBy(ProcSwitchOffRelease, regOtherNoRelease,
			fmt.Sprintf("%d of %d registrations answered; no UE CONTEXT RELEASE COMMAND within the wait after a switch-off deregistration; %s", answered, len(attempts), detail))
	case ngSetupErr != nil:
		r = r.SetupFailed(regOtherNoNGSetup, ngSetupErr)
		r.Detail = fmt.Sprintf("%d of %d registrations answered; %s", answered, len(attempts), detail)
		return r
	case blocked > 0:
		return r.BlockedBy(ProcRegistration, "a registration of the same SUPI did not complete", detail)
	case notRun > 0:
		return r.Errored("a registration after the release could not be sent", detail)
	}
	return r.As(regOtherOutAnswered, regOtherAnswered, detail)
}

// otherGNB is the gNB the other UE registers through: the check's own when
// the registered UE is on the second, else the second (Env.TargetGNBID).
func (h *UEHarness) otherGNB(serving uint32) gnb.Config {
	if serving != h.env.GNB.ID {
		return h.env.GNB
	}
	g := h.env.GNB
	g.ID = h.env.targetGNBID()
	g.Name = "orbit-conf-second-gnb"
	return g
}

// registerOtherAfterRelease deregisters the registered UE (switch-off) and,
// at its release, registers the other subscriber of the check's first two
// through the other gNB (engine.Manager.RegisterAfterOtherRelease), with no
// PDU session and with probe; bound limits the whole call. The
// harness then holds the other subscriber: registered only when the
// result's Attach is set. When the other gNB's NG Setup did not complete
// (engine.ErrNGSetup) the harness keeps the registered UE as it was. With
// no UE registered it returns an error.
func (h *UEHarness) registerOtherAfterRelease(ctx context.Context, probe *engine.AttachProbe, gap, bound time.Duration, opts ...engine.DeregisterOption) (*engine.OtherRegistration, error) {
	if !h.registered {
		return nil, errors.New("no UE registered")
	}
	sess, err := h.m.Status(h.supi)
	if err != nil {
		return nil, err
	}
	first, err := supiAt(h.env.UE.SUPI, 0)
	if err != nil {
		return nil, err
	}
	other := 0
	if h.supi == first {
		other = 1
	}
	otherSUPI, err := supiAt(h.env.UE.SUPI, other%max(h.env.UE.Subscribers, 1))
	if err != nil {
		return nil, err
	}
	id, err := ue.ParseIdentity(otherSUPI, h.env.GNB.MCC, h.env.GNB.MNC, "0")
	if err != nil {
		return nil, err
	}
	sub := h.sub
	sub.SUPI = otherSUPI
	g := engine.GNBSpec{Config: h.otherGNB(sess.ServingGNB().ID), AMFAddr: h.env.AMFAddr, BindAddr: h.env.Bind}

	// The whole attempt is bounded: the NG Setup, the release, the gap and
	// the registration, which the probe's Stop ends.
	actx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	res, err := h.m.RegisterAfterOtherRelease(actx, h.supi,
		g, engine.UEConfig{Identity: id, Sub: sub, Probe: probe, SecurityCapabilities: h.caps}, gap, opts...)
	if errors.Is(err, engine.ErrNGSetup) {
		return res, err
	}
	h.supi, h.id, h.sub = otherSUPI, id, sub
	h.registered = err == nil && res.Attach != nil
	return res, err
}
