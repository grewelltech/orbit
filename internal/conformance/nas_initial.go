package conformance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
)

func init() {
	builtins = append(builtins, nasInitialMsgContainerWins{}, nasInitialMsgNoContainer{}, nasInitialMsgNonCleartextAccepted{})
}

const (
	// initialNASWait bounds one registration's wait for its answer.
	initialNASWait = 10 * time.Second
	// initialNASDeregisterWait bounds the deregistration after an Accept.
	initialNASDeregisterWait = 5 * time.Second
	// initialNASSettle is the pause before a registration of the subscriber
	// the registration before it used, which happens only when the check's
	// subscriber block is smaller than its registrations (nextSubscriber).
	// The core finishes releasing the UE context in it: a registration of
	// the same UE that reaches the core during that release can go
	// unanswered.
	initialNASSettle = time.Second
)

// initialNASWaits holds the overrides of initialNASWait and
// initialNASSettle that the initial-NAS checks share (tests only).
type initialNASWaits struct {
	wait, settle time.Duration
}

func (w initialNASWaits) waitTimeout() time.Duration {
	if w.wait > 0 {
		return w.wait
	}
	return initialNASWait
}

func (w initialNASWaits) settleTime() time.Duration {
	if w.settle > 0 {
		return w.settle
	}
	return initialNASSettle
}

// budget is the time n registrations may take, each with its
// deregistration, with a settle between two of them (taken only when they
// share a subscriber), plus the liveness probe.
func (w initialNASWaits) budget(n int) time.Duration {
	return time.Duration(n)*(w.waitTimeout()+initialNASDeregisterWait) + time.Duration(n-1)*w.settleTime() + 5*time.Second
}

// nextSubscriber switches h to subscriber i of the check's block for its
// next registration. When the block is too small to give that registration
// a subscriber of its own, so that it registers the one just used, it first
// pauses for settle (or until ctx ends): the core may still be releasing
// that UE's context, and a registration that meets the release can go
// unanswered.
func nextSubscriber(ctx context.Context, h *UEHarness, i int, settle time.Duration) error {
	prev := h.supi
	if err := h.UseSubscriber(i); err != nil {
		return err
	}
	if h.supi == prev {
		sleepCtx(ctx, settle)
	}
	return nil
}

// initialNASStimulus is one registration of the initial-NAS checks: what
// the UE requests, and where its messages leave TS 24.501 4.4.6.
type initialNASStimulus struct {
	// requested is the Requested NSSAI of the complete Registration
	// Request, the one the Security Mode Complete's NAS message container
	// carries.
	requested ue.SNSSAI
	// plainComplete sends the complete Registration Request, non-cleartext
	// IEs included, as the initial NAS message, where a UE without a NAS
	// security context sends the cleartext IEs only.
	plainComplete bool
	// plainRequested, with plainComplete, replaces the Requested NSSAI of
	// that initial message, so the unprotected copy and the protected one
	// differ.
	plainRequested *ue.SNSSAI
	// omitContainer leaves the NAS message container out of the Security
	// Mode Complete, and stopAtSecondSMC ends the registration at a second
	// Security Mode Command.
	omitContainer, stopAtSecondSMC bool
	// omitIMEISV leaves a requested IMEISV out of the Security Mode
	// Complete.
	omitIMEISV bool
}

// initialNASOutcome is what one such registration saw.
type initialNASOutcome struct {
	// smcs counts the Security Mode Commands; imeisvRequested is the first
	// one's IMEISV request and secondRINMR the second one's RINMR bit.
	smcs            int
	imeisvRequested bool
	secondRINMR     bool
	// identities are the types of identity the Identity Requests after the
	// first Security Mode Command asked for.
	identities []uint8
	// reject is a Registration Reject's plain message; rejectBeforeSMC
	// says it came before any Security Mode Command, and rejectErr why one
	// could not be read (reject is then nil).
	reject          []byte
	rejectBeforeSMC bool
	rejectErr       error
	accept          *nasMessage.RegistrationAccept
	authReject      bool  // an Authentication Reject ended the registration
	released        bool  // a UE Context Release Command ended it
	noAnswer        bool  // nothing of the above within the wait
	registered      bool  // the attach returned a REGISTERED UE
	err             error // another failure
}

// sawReject reports whether a Registration Reject arrived, readable or not.
func (o initialNASOutcome) sawReject() bool { return o.reject != nil || o.rejectErr != nil }

// answered reports whether the core gave the registration an answer the
// check can describe: an Accept or a readable Reject.
func (o initialNASOutcome) answered() bool {
	return o.err == nil && (o.reject != nil || (o.registered && o.accept != nil))
}

// rejectCause is a readable Reject's 5GMM cause.
func (o initialNASOutcome) rejectCause() uint8 {
	cause, _, _ := rejectIEs(o.reject)
	return cause
}

// answer renders the outcome on one line that is the same for the same
// answer: the Accept with its allowed and rejected NSSAI, the Reject with
// its cause and rejected NSSAI, or what else ended the registration.
func (o initialNASOutcome) answer() string {
	switch {
	case o.err != nil:
		return "UE setup failed (" + o.err.Error() + ")"
	case o.authReject:
		return "Authentication Reject"
	case o.reject != nil:
		cause, ies, err := rejectIEs(o.reject)
		if err != nil {
			return fmt.Sprintf("Registration Reject that does not decode (%x)", o.reject)
		}
		var rejected []string
		for _, ie := range ies {
			var list []rejectedSNSSAI
			switch ie.iei {
			case ieiRejectedNSSAI:
				list, _ = decodeRejectedNSSAI(ie.value)
			case ieiExtendedRejectedNSSAI:
				list, _ = decodeExtendedRejectedNSSAI(ie.value)
			}
			for _, r := range list {
				rejected = append(rejected, r.String())
			}
		}
		return fmt.Sprintf("Registration Reject, 5GMM cause #%d, rejected NSSAI %s", cause, listOrNone(rejected))
	case o.rejectErr != nil:
		return "Registration Reject that could not be read"
	case o.registered && o.accept != nil:
		var allowed, rejected []string
		if a := o.accept.AllowedNSSAI; a != nil {
			for _, s := range allowedSNSSAIs(a.Buffer[:min(int(a.GetLen()), len(a.Buffer))]) {
				allowed = append(allowed, fmt.Sprintf("%x", s))
			}
		}
		if r := o.accept.RejectedNSSAI; r != nil {
			list, _ := decodeRejectedNSSAI(r.Buffer[:min(int(r.GetLen()), len(r.Buffer))])
			for _, s := range list {
				rejected = append(rejected, s.String())
			}
		}
		return fmt.Sprintf("Registration Accept, allowed NSSAI %s, rejected NSSAI %s", listOrNone(allowed), listOrNone(rejected))
	case o.released:
		return "UE Context Release Command, no Registration Accept or Reject"
	case o.noAnswer:
		return "no Registration Accept or Reject"
	}
	return "no recorded answer"
}

// listOrNone renders a sorted list, "none" when empty.
func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	sort.Strings(items)
	return strings.Join(items, ", ")
}

// withRequestedNSSAI is the plain Registration Request complete with its
// Requested NSSAI replaced by s alone.
func withRequestedNSSAI(complete []byte, s ue.SNSSAI) ([]byte, error) {
	m, err := nas.DecodePlain(complete)
	if err != nil {
		return nil, err
	}
	if m.GmmMessage == nil || m.RegistrationRequest == nil {
		return nil, errors.New("not a Registration Request")
	}
	n, err := ue.BuildRequestedNSSAI([]ue.SNSSAI{s})
	if err != nil {
		return nil, err
	}
	m.RegistrationRequest.RequestedNSSAI = n
	return m.PlainNasEncode()
}

// probeInitialNAS runs one registration as stim describes, without a PDU
// session, until a Registration Accept or Reject, an Authentication Reject,
// a release, a second Security Mode Command (stopAtSecondSMC) or wait. The
// stimulus 5GMM capability goes with it, so the complete request always has
// a non-cleartext IE.
func probeInitialNAS(ctx context.Context, h *UEHarness, stim initialNASStimulus, wait time.Duration) initialNASOutcome {
	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	defer halt()
	var mu sync.Mutex
	var o initialNASOutcome
	timedOut, stoppedAtSMC := false, false
	timer := time.AfterFunc(wait, func() {
		mu.Lock()
		defer mu.Unlock()
		timedOut = true
		halt()
	})
	defer timer.Stop()
	probe := &engine.AttachProbe{
		Stop:                    stop,
		OmitNASMessageContainer: stim.omitContainer,
		OmitIMEISV:              stim.omitIMEISV,
		OnNAS: func(ev engine.NASEvent) {
			mu.Lock()
			defer mu.Unlock()
			switch ev.MessageType() {
			case f5nas.MsgTypeSecurityModeCommand:
				if ev.Msg == nil || ev.Msg.GmmMessage == nil || ev.Msg.SecurityModeCommand == nil {
					return
				}
				smc := ev.Msg.SecurityModeCommand
				o.smcs++
				switch o.smcs {
				case 1:
					o.imeisvRequested = smc.IMEISVRequest != nil && smc.GetIMEISVRequestValue() == nasMessage.IMEISVRequested
				case 2:
					o.secondRINMR = smc.Additional5GSecurityInformation != nil && smc.GetRINMR() == 1
					if stim.stopAtSecondSMC {
						stoppedAtSMC = true
						halt()
					}
				}
			case f5nas.MsgTypeIdentityRequest:
				if o.smcs > 0 && ev.Msg != nil && ev.Msg.GmmMessage != nil && ev.Msg.IdentityRequest != nil {
					o.identities = append(o.identities, ev.Msg.IdentityRequest.GetTypeOfIdentity())
				}
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
				// A decoded Reject ends the attach by itself; one the codec
				// refused is skipped, so stop here.
				halt()
			case f5nas.MsgTypeRegistrationAccept:
				if ev.Err == nil && ev.Msg != nil && ev.Msg.GmmMessage != nil {
					o.accept = ev.Msg.RegistrationAccept
				}
			case f5nas.MsgTypeAuthenticationReject:
				o.authReject = true
				halt()
			}
		},
	}
	switch {
	case stim.plainComplete && stim.plainRequested != nil:
		probe.InitialNAS = func(complete []byte) ([]byte, error) { return withRequestedNSSAI(complete, *stim.plainRequested) }
	case stim.plainComplete:
		probe.InitialNAS = func(complete []byte) ([]byte, error) { return complete, nil }
	}
	_, err := h.RegisterWithProbe(ctx, probe, func(cfg *engine.UEConfig) {
		cfg.RequestedNSSAI = []ue.SNSSAI{stim.requested}
		cfg.Capability5GMM = stimulusCapability5GMM
	})
	mu.Lock()
	defer mu.Unlock()
	switch {
	case err == nil:
		o.registered = true
		if o.accept == nil {
			o.err = errors.New("registered without a Registration Accept the probe saw")
		}
	case o.sawReject(), o.authReject, stoppedAtSMC:
	case errors.Is(err, engine.ErrReleasedByCore):
		o.released = true
	case timedOut && ctx.Err() == nil:
		o.noAnswer = true
	default:
		o.err = err
	}
	return o
}

// deregisterAfter deregisters the UE when o left it registered, and says in
// one clause what state the core was left in for this registration.
func deregisterAfter(h *UEHarness, o initialNASOutcome) string {
	left := "the association was closed with the UE not registered"
	if o.registered {
		dctx, cancel := context.WithTimeout(context.Background(), initialNASDeregisterWait)
		err := h.Deregister(dctx)
		cancel()
		left = "the UE was deregistered"
		if err != nil {
			left = fmt.Sprintf("deregistration failed (%v) and the association was dropped", err)
		}
	}
	return left
}

// describeSNSSAI renders an S-NSSAI for a detail line.
func describeSNSSAI(s ue.SNSSAI) string {
	if s.SD == "" {
		return fmt.Sprintf("SST %d", s.SST)
	}
	return fmt.Sprintf("SST %d SD %s", s.SST, s.SD)
}

// initialNASBound is the source of the bound on the initial-NAS checks'
// outcomes inferred from the absence of an answer (initialNASWait).
const initialNASBound = "check bound; inside T3510, the UE's own 15 s wait for the answer to a REGISTRATION REQUEST (TS 24.501 Table 10.2.1)"

// erroredOutcome is the ERROR every check has, for a classifier that
// returns an outcome where nothing the check judges happened.
var erroredOutcome = Outcome{ID: OutcomeError, Verdict: Error}

// initialNASUnjudged returns r for a registration of the initial-NAS checks
// (what names it) that ended before anything the check judges, and true;
// false when it did not. The per-test timeout running out is ERROR; an
// attach that failed is the setup's to attribute (Result.SetupFailed); an
// AUTHENTICATION REJECT for the run's own subscriber is BLOCKED on the
// registration.
func initialNASUnjudged(ctx context.Context, r Result, o initialNASOutcome, what string, budget time.Duration) (Result, bool) {
	switch {
	case o.err != nil && ctx.Err() != nil:
		return r.Errored("timed out during "+what,
			fmt.Sprintf("the per-test timeout expired during %s (%v); raise --per-test-timeout to at least %s", what, o.err, budget)), true
	case o.err != nil:
		return r.SetupFailed("UE setup failed", fmt.Errorf("%s: %w", what, o.err)), true
	case o.authReject:
		return r.BlockedBy(ProcRegistration, "Authentication Reject", what+": the core refused to authenticate the run's own subscriber"), true
	}
	return r, false
}

// rejectObserved names o's Registration Reject, as name, for an Observed
// text: with its 5GMM cause, or as one that could not be read.
func rejectObserved(o initialNASOutcome, name string) string {
	if o.reject == nil {
		return name + " that could not be read"
	}
	return fmt.Sprintf("%s, 5GMM cause #%d", name, o.rejectCause())
}

// initialMsgContainerWinsFailure is the check's message when the answer is
// not the one the container's request gets.
const initialMsgContainerWinsFailure = "the AMF did not complete the registration on the REGISTRATION REQUEST in the SECURITY MODE COMPLETE's NAS message container. TS 24.501 5.4.2.4; TS 33.501 6.4.6 step 4. Likely cause: the AMF keeps IEs of the unprotected initial NAS message."

// nasInitialMsgContainerWins sends an initial REGISTRATION REQUEST whose
// Requested NSSAI, a non-cleartext IE sent in the clear, names an S-NSSAI
// the core does not serve, and a SECURITY MODE COMPLETE whose NAS message
// container carries the complete request with the served S-NSSAI. TS 24.501
// 5.4.2.4 has the AMF complete the registration "by considering the
// REGISTRATION REQUEST message contained in the NAS message container IE as
// the message that triggered the procedure" (TS 33.501 6.4.6 step 4: "The
// AMF shall use the complete initial NAS message that is in the NAS
// container as the message to respond to"), so the answer must be the one
// the container's request gets.
//
// What that answer is, the core says itself: two conformant registrations
// follow, one requesting the served S-NSSAI and one the unserved, each sent
// protected only. The stimulus must be answered as the first is (PASS).
// Any other answer, no answer included, is FAIL, and Observed says when it
// is the answer to the unprotected value. A core that answers both
// reference requests alike gives the two copies nothing to differ by
// (INFO). A core that ends the stimulus registration before any SECURITY
// MODE COMMAND never receives the container: a refusal or silence there is
// INFO, a registration on the unprotected initial message WARN. A
// reference registration the core does not complete is BLOCKED on the
// registration.
type nasInitialMsgContainerWins struct {
	initialNASWaits
}

// The keys of the shalls NAS-INITIAL-MSG-CONTAINER-WINS rests on.
const (
	containerWinsRef     = "TS 24.501 5.4.2.4"
	containerWinsRef3350 = "TS 33.501 6.4.6 step 4"
)

// The outcomes of NAS-INITIAL-MSG-CONTAINER-WINS.
var (
	containerWinsAnswered = Outcome{ID: "container-answered", Verdict: Pass, Refs: []string{containerWinsRef, containerWinsRef3350},
		Summary: "answered the registration as it answers the REGISTRATION REQUEST in the NAS message container sent alone"}
	containerWinsUnprotected = Outcome{ID: "unprotected-answered", Verdict: Fail, Refs: []string{containerWinsRef, containerWinsRef3350},
		Summary: "answered the registration as it answers the unprotected initial message's Requested NSSAI"}
	containerWinsOther = Outcome{ID: "other-answer", Verdict: Fail, Refs: []string{containerWinsRef, containerWinsRef3350},
		Summary: "answered the registration otherwise than it answers the REGISTRATION REQUEST in the NAS message container sent alone"}
	containerWinsUnanswered = Outcome{ID: "unanswered", Verdict: Fail, Refs: []string{containerWinsRef, containerWinsRef3350},
		Bound:   &Bound{Wait: initialNASWait, Source: initialNASBound},
		Summary: "left the registration without REGISTRATION ACCEPT or REJECT after the SECURITY MODE COMPLETE, where the request in the NAS message container sent alone is answered"}
	// INFO: the two copies cannot be told apart, so the requirement is not
	// exercised; answering two slices alike meets none of the criteria.
	containerWinsAlike = Outcome{ID: "references-alike", Verdict: Info,
		Summary: "answered the served and the unserved Requested NSSAI alike, so the two copies could not be told apart"}
	// INFO: TS 24.501 4.4.6 binds the UE, and refusing an initial message
	// that breaks it weakens no protection and drops no valid request.
	containerWinsRefused = Outcome{ID: "refused-before-smc", Verdict: Info,
		Summary: "rejected the initial message with non-cleartext IEs before any SECURITY MODE COMMAND, so the container was never sent"}
	// INFO: as for the refusal; the request it drops is not a valid one.
	containerWinsDropped = Outcome{ID: "dropped-before-smc", Verdict: Info, Bound: &Bound{Wait: initialNASWait, Source: initialNASBound},
		Summary: "released or left unanswered the initial message with non-cleartext IEs before any SECURITY MODE COMMAND"}
	// WARN for security: the core registered the UE on the unprotected
	// initial message, whose non-cleartext IEs had no protected copy.
	containerWinsUnprotectedAccept = Outcome{ID: "accepted-without-smc", Verdict: Warn, Criterion: CriterionSecurity,
		Summary: "registered the UE on the unprotected initial message, without any SECURITY MODE COMMAND"}
)

// Outcomes declares what the check can report.
func (nasInitialMsgContainerWins) Outcomes() []Outcome {
	return []Outcome{containerWinsAnswered, containerWinsUnprotected, containerWinsOther, containerWinsUnanswered,
		containerWinsAlike, containerWinsRefused, containerWinsDropped, containerWinsUnprotectedAccept}
}

func (nasInitialMsgContainerWins) ID() string         { return "NAS-INITIAL-MSG-CONTAINER-WINS" }
func (nasInitialMsgContainerWins) Category() Category { return Security }
func (nasInitialMsgContainerWins) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.4.2.4", Strength: Shall, Since: 15, Verified: ts24501Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 names the IE \"Replayed NAS message container\"; \"NAS message container\" by V15.7.0, the sentence otherwise unchanged through V19.8.0"},
			}},
		{Spec: "TS 33.501", Clause: "6.4.6 step 4", Strength: Shall, Since: 15, Verified: ts33501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 has the complete message resent only after a failed hash check (\"the AMF shall treat this as the initial NAS message to respond to\"); the NAS Container, and \"The AMF shall use the complete initial NAS message that is in the NAS container as the message to respond to\", by V15.20.0 and unchanged through V19.7.0"},
			}},
	}
}

// Budget covers the three registrations, a settle between them and the
// liveness probe.
func (c nasInitialMsgContainerWins) Budget() time.Duration { return c.budget(3) }

func (c nasInitialMsgContainerWins) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "the registration answered as the REGISTRATION REQUEST in the SECURITY MODE COMPLETE's NAS message container is when sent alone, although the unprotected initial message requested another S-NSSAI (TS 24.501 5.4.2.4; TS 33.501 6.4.6 step 4)"}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run security checks")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	wait := c.waitTimeout()
	served := h.pdu.Slice()
	unserved := stimulusSNSSAI(h.pdu)
	stimulus := fmt.Sprintf("stimulus: initial message with Requested NSSAI %s in the clear, NAS message container with Requested NSSAI %s", describeSNSSAI(unserved), describeSNSSAI(served))
	withStimulus := func(res Result) Result {
		res.Detail = joinDetail(res.Detail, stimulus)
		return res
	}

	stim := probeInitialNAS(ctx, h, initialNASStimulus{requested: served, plainComplete: true, plainRequested: &unserved}, wait)
	left := deregisterAfter(h, stim)
	if res, done := initialNASUnjudged(ctx, r, stim, "the stimulus registration", c.Budget()); done {
		return withStimulus(res)
	}
	if stim.smcs == 0 {
		o, observed, detail := classifyContainerWinsEarly(stim)
		r = r.As(o, observed, joinDetail(joinDetail(stimulus, detail), "state left: "+left))
		return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the initial NAS message")
	}

	// Each registration is its own subscriber where the check has three,
	// so none meets the context the core still holds of the one before.
	if err := nextSubscriber(ctx, h, 1, c.settleTime()); err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	refServed := probeInitialNAS(ctx, h, initialNASStimulus{requested: served}, wait)
	leftServed := deregisterAfter(h, refServed)
	if !refServed.registered || refServed.accept == nil {
		// The served S-NSSAI, sent conformantly, must register: without
		// that there is no reference to compare with.
		const what = "the reference registration requesting the served S-NSSAI"
		if res, done := initialNASUnjudged(ctx, r, refServed, what, c.Budget()); done {
			return withStimulus(res)
		}
		return withStimulus(r.BlockedBy(ProcRegistration, "reference registration not completed", what+", a conformant registration, ended with: "+refServed.answer()))
	}
	if err := nextSubscriber(ctx, h, 2, c.settleTime()); err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	refUnserved := probeInitialNAS(ctx, h, initialNASStimulus{requested: unserved}, wait)
	leftUnserved := deregisterAfter(h, refUnserved)
	if !refUnserved.answered() {
		const what = "the reference registration requesting the unserved S-NSSAI"
		if res, done := initialNASUnjudged(ctx, r, refUnserved, what, c.Budget()); done {
			return withStimulus(res)
		}
		if refUnserved.rejectErr != nil {
			return withStimulus(r.Errored("reference Registration Reject could not be read", fmt.Sprintf("%s: %v; the answers cannot be compared", what, refUnserved.rejectErr)))
		}
		return withStimulus(r.BlockedBy(ProcRegistration, "reference registration not answered", what+", a conformant registration, ended with: "+refUnserved.answer()))
	}

	o, observed, detail := judgeContainerWins(stim.answer(), refServed.answer(), refUnserved.answer(), stim.noAnswer)
	detail = joinDetail(detail, stimulus)
	detail = joinDetail(detail, fmt.Sprintf("state left: after the stimulus %s; after the first reference %s; after the second %s", left, leftServed, leftUnserved))
	r = r.As(o, observed, detail)
	if ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the registrations")
}

// classifyContainerWinsEarly judges a stimulus registration the core ended
// before any SECURITY MODE COMMAND, so before the container was sent: a
// Reject is the refusal, a registration the unprotected accept, anything
// else the drop.
func classifyContainerWinsEarly(o initialNASOutcome) (Outcome, string, string) {
	const notApplied = "no SECURITY MODE COMPLETE and no NAS message container were sent, so 5.4.2.4 did not apply"
	switch {
	case o.sawReject():
		return containerWinsRefused, fmt.Sprintf("core refuses non-cleartext IEs in the initial message (%s) before the container is consulted", rejectObserved(o, "Registration Reject")),
			"the Reject came before any SECURITY MODE COMMAND; " + notApplied + "; NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED reports what the core does with such an initial message"
	case o.registered:
		return containerWinsUnprotectedAccept, "registration accepted on the unprotected initial message, without a SECURITY MODE COMMAND",
			"the core registered the UE with only the unprotected initial message, its Requested NSSAI in the clear, to go on; answer: " + o.answer()
	}
	return containerWinsDropped, "initial message with non-cleartext IEs ended before a SECURITY MODE COMMAND: " + o.answer(),
		fmt.Sprintf("%s; no Registration Accept, Reject or SECURITY MODE COMMAND came within %s, or the core released the UE context first", notApplied, initialNASWait)
}

// judgeContainerWins compares the stimulus registration's answer with the
// answers the core gave the served and the unserved Requested NSSAI when
// each was sent protected only; unanswered says the stimulus drew neither
// a Registration Accept nor a Reject within the wait.
func judgeContainerWins(stim, served, unserved string, unanswered bool) (Outcome, string, string) {
	detail := fmt.Sprintf("stimulus answered: %s; the container's Requested NSSAI sent alone: %s; the initial message's Requested NSSAI sent alone: %s", stim, served, unserved)
	switch {
	case stim != served && stim == unserved:
		return containerWinsUnprotected, "registration answered on the unprotected Requested NSSAI, not the one in the NAS message container", initialMsgContainerWinsFailure + " " + detail
	case stim != served && unanswered:
		return containerWinsUnanswered, "registration left without an answer after the SECURITY MODE COMPLETE", initialMsgContainerWinsFailure + " " + detail
	case stim != served:
		return containerWinsOther, "registration not answered as the request in the NAS message container is: " + stim, initialMsgContainerWinsFailure + " " + detail
	case served == unserved:
		return containerWinsAlike, "core answers the served and the unserved Requested NSSAI alike; the two copies cannot be told apart",
			detail + "; the unserved S-NSSAI is the subject of NAS-REG-REJECTED-NSSAI"
	}
	return containerWinsAnswered, "registration answered on the request in the NAS message container", detail
}

// Observed texts of NAS-INITIAL-MSG-NO-CONTAINER.
const (
	noContainerRINMR      = "second SECURITY MODE COMMAND with RINMR set (the initial NAS message requested again)"
	noContainerSecondSMC  = "second SECURITY MODE COMMAND without RINMR"
	noContainerAccepted   = "registration accepted without the NAS message container"
	noContainerRejected   = "%s, after the SECURITY MODE COMPLETE without the NAS message container"
	noContainerEarly      = "initial message refused before the SECURITY MODE COMMAND (%s)"
	noContainerReleased   = "UE CONTEXT RELEASE COMMAND after the SECURITY MODE COMPLETE without the NAS message container"
	noContainerUnanswered = "no answer to the SECURITY MODE COMPLETE without the NAS message container"
	// noContainerAcceptedNoSMC and noContainerDropped are the registration
	// ended before any SECURITY MODE COMMAND.
	noContainerAcceptedNoSMC = "registration accepted on the unprotected initial message, without a SECURITY MODE COMMAND"
	noContainerDropped       = "initial message released or left unanswered before a SECURITY MODE COMMAND"
)

// nasInitialMsgNoContainer sends an initial REGISTRATION REQUEST with
// non-cleartext IEs in the clear, among them a Requested NSSAI the core
// does not serve, and then a SECURITY MODE COMPLETE without the NAS message
// container, and reports what the core does: asks for the initial NAS
// message again, goes on with what it has, or rejects.
//
// TS 24.501 4.4.6 requires the container of the UE. Of the AMF, 5.4.2.2
// requires a SECURITY MODE COMMAND with the RINMR bit only for a
// REGISTRATION REQUEST that failed its integrity check or whose container
// could not be deciphered, and no clause says what the AMF does with a
// SECURITY MODE COMPLETE that lacks the container. So the check is a
// characterisation: no outcome is PASS or FAIL. A core that goes on with
// the unprotected values, registering the UE or rejecting it with cause
// #62 for the Requested NSSAI only the initial message carried, acts on
// unprotected input: WARN, for security. Every other answer is INFO.
type nasInitialMsgNoContainer struct {
	initialNASWaits
}

// The key of the description the RINMR outcome follows.
const noContainerRINMRRef = "TS 24.501 5.4.2.2 RINMR"

// The outcomes of NAS-INITIAL-MSG-NO-CONTAINER.
var (
	noContainerOutRINMR = Outcome{ID: "asked-again-rinmr", Verdict: Info, Refs: []string{noContainerRINMRRef},
		Summary: "asked for the initial NAS message again: a second SECURITY MODE COMMAND with RINMR set, the request 5.4.2.2 describes"}
	noContainerOutSecondSMC = Outcome{ID: "second-smc", Verdict: Info,
		Summary: "sent a second SECURITY MODE COMMAND without RINMR"}
	// WARN for security: the core registered the UE with only the
	// unprotected initial message to go on.
	noContainerOutAccepted = Outcome{ID: "accepted", Verdict: Warn, Criterion: CriterionSecurity,
		Summary: "registered the UE without the NAS message container, on the unprotected initial message"}
	// WARN for security: cause #62 answers a Requested NSSAI, and only the
	// unprotected initial message carried one: the core acted on it.
	noContainerOutRejectedUnprotected = Outcome{ID: "rejected-on-unprotected-nssai", Verdict: Warn, Criterion: CriterionSecurity,
		Summary: "rejected the registration with cause #62, answering the Requested NSSAI only the unprotected initial message carried"}
	// INFO: a Reject with another cause acts on no unprotected value the
	// check can name, and the UE broke 4.4.6.
	noContainerOutRejected = Outcome{ID: "rejected", Verdict: Info,
		Summary: "rejected the registration after the SECURITY MODE COMPLETE without the NAS message container, with a cause other than #62"}
	// INFO: refusing an initial message that breaks 4.4.6 weakens no
	// protection and drops no valid request.
	noContainerOutRefused = Outcome{ID: "refused-before-smc", Verdict: Info,
		Summary: "rejected the initial message with non-cleartext IEs before any SECURITY MODE COMMAND"}
	// INFO: as for the refusal; the request dropped is not a valid one.
	noContainerOutDropped = Outcome{ID: "dropped-before-smc", Verdict: Info, Bound: &Bound{Wait: initialNASWait, Source: initialNASBound},
		Summary: "released or left unanswered the initial message with non-cleartext IEs before any SECURITY MODE COMMAND"}
	// INFO: releasing a UE that omitted the container drops no valid
	// request.
	noContainerOutReleased = Outcome{ID: "released", Verdict: Info,
		Summary: "released the UE context after the SECURITY MODE COMPLETE without the NAS message container"}
	// INFO: as for the release.
	noContainerOutUnanswered = Outcome{ID: "unanswered", Verdict: Info, Bound: &Bound{Wait: initialNASWait, Source: initialNASBound},
		Summary: "left the SECURITY MODE COMPLETE without the NAS message container unanswered"}
)

// Outcomes declares what the check can report.
func (nasInitialMsgNoContainer) Outcomes() []Outcome {
	return []Outcome{noContainerOutRINMR, noContainerOutSecondSMC, noContainerOutAccepted, noContainerOutRejectedUnprotected,
		noContainerOutRejected, noContainerOutRefused, noContainerOutDropped, noContainerOutReleased, noContainerOutUnanswered}
}

func (nasInitialMsgNoContainer) ID() string         { return "NAS-INITIAL-MSG-NO-CONTAINER" }
func (nasInitialMsgNoContainer) Category() Category { return Security }

// SpecRefs cites the UE's rule for the container (4.4.6 a)) and, as the
// primary reference, the request for the initial NAS message that 5.4.2.2
// describes for other conditions. Neither requires anything of an AMF
// whose SECURITY MODE COMPLETE arrives without the container.
func (nasInitialMsgNoContainer) SpecRefs() []SpecRef {
	rinmr := ref24501RINMR
	rinmr.Primary = true
	return []SpecRef{ref24501Clause446, rinmr}
}

// ref24501Clause446 is TS 24.501 4.4.6 a), the UE's rule for an initial
// REGISTRATION REQUEST without a NAS security context, cited as context.
var ref24501Clause446 = SpecRef{Spec: "TS 24.501", Clause: "4.4.6 a)", Strength: Descriptive, Since: 15, Verified: ts24501Verified,
	Changed: []SpecChange{
		{Release: 15, Note: "V15.0.0 has no clause 4.4.6 and protects the initial NAS message with a hash instead; the cleartext IEs and the NAS message container are in V15.7.0 and unchanged in substance through V19.8.0"},
	}}

// ref24501RINMR is the paragraph of TS 24.501 5.4.2.2 on the RINMR bit,
// cited as a description: its shall covers a REGISTRATION REQUEST that
// failed the integrity check or whose container could not be deciphered,
// which is not the case the checks create.
var ref24501RINMR = SpecRef{Spec: "TS 24.501", Clause: "5.4.2.2 RINMR", Strength: Descriptive, Since: 15, Verified: ts24501Verified,
	Changed: []SpecChange{
		{Release: 15, Note: "V15.0.0 has the AMF send a hash of the REGISTRATION REQUEST instead; the RINMR bit is in V15.7.0, for a REGISTRATION REQUEST that failed the integrity check"},
		{Release: 16, Note: "adds the case that the AMF cannot decipher the NAS message container IE of the REGISTRATION REQUEST"},
	}}

// Budget covers the registration and the liveness probe.
func (c nasInitialMsgNoContainer) Budget() time.Duration { return c.budget(1) }

func (c nasInitialMsgNoContainer) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "a second SECURITY MODE COMMAND with the RINMR bit set, after a SECURITY MODE COMPLETE without the NAS message container from a UE whose initial message had non-cleartext IEs in the clear (TS 24.501 5.4.2.2 describes that request for a message the AMF could not verify or decipher; the specification is silent on the missing container)"}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run security checks")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	wait := c.waitTimeout()
	unserved := stimulusSNSSAI(h.pdu)
	o := probeInitialNAS(ctx, h, initialNASStimulus{requested: unserved, plainComplete: true, omitContainer: true, stopAtSecondSMC: true}, wait)
	left := deregisterAfter(h, o)
	stimulus := fmt.Sprintf("stimulus: the complete REGISTRATION REQUEST as the initial message, Requested NSSAI %s (not served) and 5GMM capability in the clear, then a SECURITY MODE COMPLETE without the NAS message container", describeSNSSAI(unserved))
	if res, done := initialNASUnjudged(ctx, r, o, "the registration", c.Budget()); done {
		res.Detail = joinDetail(res.Detail, stimulus)
		return res
	}
	out, observed, detail := judgeNoContainer(o, wait)
	r = r.As(out, observed, joinDetail(joinDetail(detail, stimulus), "state left: "+left))
	if r.Verdict == Error || ctx.Err() != nil {
		return r
	}
	return observeAlive(ctx, env, r, "the SECURITY MODE COMPLETE without the NAS message container")
}

// judgeNoContainer maps what the core did with the containerless SECURITY
// MODE COMPLETE onto the check's outcomes. o is a registration the check
// judges (initialNASUnjudged returned false for it).
func judgeNoContainer(o initialNASOutcome, wait time.Duration) (Outcome, string, string) {
	switch {
	case o.sawReject() && o.rejectBeforeSMC:
		return noContainerOutRefused, fmt.Sprintf(noContainerEarly, rejectObserved(o, "Registration Reject")),
			"the core refused the initial message, so no SECURITY MODE COMPLETE was sent and the missing container was not exercised; answer: " + o.answer()
	case o.smcs == 0 && o.registered:
		return noContainerOutAccepted, noContainerAcceptedNoSMC, "the core registered the UE with only the unprotected initial message to go on; answer: " + o.answer()
	case o.smcs == 0:
		return noContainerOutDropped, noContainerDropped,
			fmt.Sprintf("no SECURITY MODE COMPLETE was sent and the missing container was not exercised; answer: %s (within %s)", o.answer(), wait)
	case o.smcs >= 2 && o.secondRINMR:
		return noContainerOutRINMR, noContainerRINMR, "the registration was stopped at the second SECURITY MODE COMMAND, which the UE did not answer"
	case o.smcs >= 2:
		return noContainerOutSecondSMC, noContainerSecondSMC, "the second SECURITY MODE COMMAND has no Additional 5G security information IE with RINMR set; the registration was stopped there, and the UE did not answer it"
	case o.reject != nil && o.rejectCause() == cause62:
		return noContainerOutRejectedUnprotected, fmt.Sprintf(noContainerRejected, rejectObserved(o, "REGISTRATION REJECT")),
			"answer: " + o.answer() + "; cause #62 \"No network slices available\" answers a Requested NSSAI, and only the initial message carried one, in the clear: the core went on with the unprotected values"
	case o.sawReject():
		return noContainerOutRejected, fmt.Sprintf(noContainerRejected, rejectObserved(o, "REGISTRATION REJECT")), "answer: " + o.answer()
	case o.registered && o.accept != nil:
		return noContainerOutAccepted, noContainerAccepted, "the core completed the registration with only the unprotected initial message to go on; answer: " + o.answer()
	case o.released:
		return noContainerOutReleased, noContainerReleased, "the core released the UE context without a Registration Accept or Reject"
	case o.noAnswer:
		return noContainerOutUnanswered, noContainerUnanswered, fmt.Sprintf("no Registration Accept or Reject, second SECURITY MODE COMMAND or release within %s (absence inferred within this bound)", wait)
	}
	return erroredOutcome, "no outcome", "the registration ended without a recorded answer"
}

// Observed texts of NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED.
const (
	nonCleartextAccepted   = "registration accepted"
	nonCleartextEarly      = "%s, before the SECURITY MODE COMMAND"
	nonCleartextRejected   = "%s, after the SECURITY MODE COMPLETE"
	nonCleartextReleased   = "UE CONTEXT RELEASE COMMAND, no Registration Accept or Reject"
	nonCleartextUnanswered = "no Registration Accept or Reject"
	nonCleartextNoSMC      = "registration accepted on the unprotected initial message, without a SECURITY MODE COMMAND"
)

// nasInitialMsgNonCleartextAccepted sends the complete REGISTRATION REQUEST,
// non-cleartext IEs included (5GMM capability and a Requested NSSAI the
// core serves), as the initial NAS message, and the same request in the
// SECURITY MODE COMPLETE's NAS message container, and reports whether the
// core registers the UE or refuses the message.
//
// TS 24.501 4.4.6 a) has a UE without a NAS security context send the
// cleartext IEs only; no clause says what an AMF does with an initial
// message that has more. So the check is a characterisation: no outcome is
// PASS or FAIL. A registration that goes through on the container, which
// 5.4.2.4 describes, is INFO, and so are a Registration Reject, before or
// after the SECURITY MODE COMMAND, a release and silence: the UE broke
// 4.4.6, and refusing it weakens no protection and drops no valid request.
// A registration with no SECURITY MODE COMMAND at all, on the unprotected
// initial message alone, is WARN, for security.
type nasInitialMsgNonCleartextAccepted struct {
	initialNASWaits
}

// The key of the description of the registration going through.
const nonCleartextRef = "TS 24.501 5.4.2.4"

// The outcomes of NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED.
var (
	nonCleartextOutAccepted = Outcome{ID: "accepted", Verdict: Info, Refs: []string{nonCleartextRef},
		Summary: "registered the UE on the REGISTRATION REQUEST in the NAS message container, as 5.4.2.4 describes"}
	// WARN for security: the core registered the UE with no protected copy
	// of a message whose non-cleartext IEs came in the clear.
	nonCleartextOutUnprotected = Outcome{ID: "accepted-without-smc", Verdict: Warn, Criterion: CriterionSecurity,
		Summary: "registered the UE on the unprotected initial message, without any SECURITY MODE COMMAND"}
	nonCleartextOutRefused = Outcome{ID: "refused-before-smc", Verdict: Info,
		Summary: "rejected the initial message with non-cleartext IEs before any SECURITY MODE COMMAND"}
	nonCleartextOutRejected = Outcome{ID: "rejected", Verdict: Info,
		Summary: "rejected the registration after the SECURITY MODE COMPLETE"}
	nonCleartextOutReleased = Outcome{ID: "released", Verdict: Info,
		Summary: "released the UE context without a Registration Accept or Reject"}
	nonCleartextOutUnanswered = Outcome{ID: "unanswered", Verdict: Info, Bound: &Bound{Wait: initialNASWait, Source: initialNASBound},
		Summary: "sent no Registration Accept or Reject"}
)

// Outcomes declares what the check can report.
func (nasInitialMsgNonCleartextAccepted) Outcomes() []Outcome {
	return []Outcome{nonCleartextOutAccepted, nonCleartextOutUnprotected, nonCleartextOutRefused, nonCleartextOutRejected,
		nonCleartextOutReleased, nonCleartextOutUnanswered}
}

func (nasInitialMsgNonCleartextAccepted) ID() string {
	return "NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED"
}
func (nasInitialMsgNonCleartextAccepted) Category() Category { return Security }

// SpecRefs cites the UE's rule the stimulus breaks (4.4.6 a)) and, as the
// primary reference and a description of the registration going through,
// 5.4.2.4: its shall is about which copy the AMF uses, which this check's
// identical copies do not test.
func (nasInitialMsgNonCleartextAccepted) SpecRefs() []SpecRef {
	return []SpecRef{ref24501Clause446,
		{Spec: "TS 24.501", Clause: "5.4.2.4", Strength: Descriptive, Since: 15, Verified: ts24501Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 names the IE \"Replayed NAS message container\"; \"NAS message container\" by V15.7.0"},
			}}}
}

// Budget covers the registration and the liveness probe.
func (c nasInitialMsgNonCleartextAccepted) Budget() time.Duration { return c.budget(1) }

func (c nasInitialMsgNonCleartextAccepted) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "the registration accepted on the REGISTRATION REQUEST in the NAS message container, from a UE whose initial message carried the same request with its non-cleartext IEs in the clear (TS 24.501 4.4.6 a) binds the UE; the specification is silent on what the AMF does with such a message)"}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run security checks")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	wait := c.waitTimeout()
	served := h.pdu.Slice()
	o := probeInitialNAS(ctx, h, initialNASStimulus{requested: served, plainComplete: true}, wait)
	left := deregisterAfter(h, o)
	stimulus := fmt.Sprintf("stimulus: the complete REGISTRATION REQUEST as the initial message, Requested NSSAI %s and 5GMM capability in the clear, and the same request in the SECURITY MODE COMPLETE's NAS message container", describeSNSSAI(served))
	if res, done := initialNASUnjudged(ctx, r, o, "the registration", c.Budget()); done {
		res.Detail = joinDetail(res.Detail, stimulus)
		return res
	}
	out, observed, detail := judgeNonCleartext(o, wait)
	r = r.As(out, observed, joinDetail(joinDetail(detail, stimulus), "state left: "+left))
	if r.Verdict == Error || ctx.Err() != nil {
		return r
	}
	return observeAlive(ctx, env, r, "the initial NAS message with non-cleartext IEs")
}

// judgeNonCleartext maps the registration's outcome onto the check's
// outcomes. o is a registration the check judges (initialNASUnjudged
// returned false for it).
func judgeNonCleartext(o initialNASOutcome, wait time.Duration) (Outcome, string, string) {
	switch {
	case o.sawReject() && o.rejectBeforeSMC:
		return nonCleartextOutRefused, fmt.Sprintf(nonCleartextEarly, rejectObserved(o, "Registration Reject")), "the core refused the initial message before authenticating the UE; answer: " + o.answer()
	case o.sawReject():
		return nonCleartextOutRejected, fmt.Sprintf(nonCleartextRejected, rejectObserved(o, "Registration Reject")), "answer: " + o.answer()
	case o.registered && o.accept != nil && o.smcs == 0:
		return nonCleartextOutUnprotected, nonCleartextNoSMC, "the core registered the UE with only the unprotected initial message to go on; answer: " + o.answer()
	case o.registered && o.accept != nil:
		return nonCleartextOutAccepted, nonCleartextAccepted, "answer: " + o.answer()
	case o.released:
		return nonCleartextOutReleased, nonCleartextReleased, "the core released the UE context during the registration"
	case o.noAnswer:
		return nonCleartextOutUnanswered, nonCleartextUnanswered, fmt.Sprintf("none arrived within %s (absence inferred within this bound)", wait)
	}
	return erroredOutcome, "no outcome", "the registration ended without a recorded answer"
}
