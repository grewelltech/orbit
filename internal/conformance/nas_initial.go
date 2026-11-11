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

// initialNASError is the ERROR for a registration that gave the check
// nothing to report; what names the registration.
func initialNASError(o initialNASOutcome, what string, wait time.Duration) (Verdict, string, string) {
	switch {
	case o.err != nil:
		return Error, "UE setup failed", what + ": " + o.err.Error()
	case o.authReject:
		return Error, "Authentication Reject", what + ": this check needs a subscriber the core authenticates"
	case o.rejectErr != nil:
		return Error, "Registration Reject could not be read", fmt.Sprintf("%s: %v", what, o.rejectErr)
	case o.released:
		return Error, "released without a Registration Reject or Accept", what + ": the core sent a UE Context Release Command; check the subscriber and the core's logs"
	case o.noAnswer:
		return Error, "no Registration Reject or Accept", fmt.Sprintf("%s: none arrived within %s; check the subscriber and the core's logs", what, wait)
	}
	return Error, "no outcome", what + ": the registration ended without a recorded answer"
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
// Any other answer is FAIL, and Observed says when it is the answer to the
// unprotected value. A core that answers both reference requests alike
// gives the two copies nothing to differ by (SKIP), and so does one that
// refuses the initial message before a SECURITY MODE COMMAND: the container
// then never reaches it.
type nasInitialMsgContainerWins struct {
	initialNASWaits
}

func (nasInitialMsgContainerWins) ID() string         { return "NAS-INITIAL-MSG-CONTAINER-WINS" }
func (nasInitialMsgContainerWins) Category() Category { return Security }
func (nasInitialMsgContainerWins) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.4.2.4", Strength: Shall, Since: 15, Verified: ts24501Verified,
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
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run security checks"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	wait := c.waitTimeout()
	served := h.pdu.Slice()
	unserved := stimulusSNSSAI(h.pdu)
	stimulus := fmt.Sprintf("stimulus: initial message with Requested NSSAI %s in the clear, NAS message container with Requested NSSAI %s", describeSNSSAI(unserved), describeSNSSAI(served))

	stim := probeInitialNAS(ctx, h, initialNASStimulus{requested: served, plainComplete: true, plainRequested: &unserved}, wait)
	left := deregisterAfter(h, stim)
	switch {
	case stim.reject != nil && stim.rejectBeforeSMC:
		r.Verdict = Skip
		r.Observed = fmt.Sprintf("core refuses non-cleartext IEs in the initial message (Registration Reject, 5GMM cause #%d) before the container is consulted", stim.rejectCause())
		r.Detail = joinDetail(stimulus, "the Reject came before any SECURITY MODE COMMAND, so no SECURITY MODE COMPLETE and no NAS message container were sent and 5.4.2.4 did not apply; "+left)
		return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the initial NAS message")
	case stim.smcs == 0 || stim.err != nil || stim.authReject || stim.rejectErr != nil:
		r.Verdict, r.Observed, r.Detail = initialNASError(stim, "the stimulus registration", wait)
		r.Detail = joinDetail(r.Detail, stimulus)
		return r
	}

	// Each registration is its own subscriber where the check has three,
	// so none meets the context the core still holds of the one before.
	if err := nextSubscriber(ctx, h, 1, c.settleTime()); err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	refServed := probeInitialNAS(ctx, h, initialNASStimulus{requested: served}, wait)
	leftServed := deregisterAfter(h, refServed)
	if !refServed.registered || refServed.accept == nil {
		// The served S-NSSAI, sent conformantly, must register: without
		// that there is no reference to compare with.
		r.Verdict, r.Observed, r.Detail = initialNASError(refServed, "the reference registration requesting the served S-NSSAI", wait)
		if refServed.reject != nil {
			r.Observed, r.Detail = "reference registration rejected", "the reference registration requesting the served S-NSSAI was answered with "+refServed.answer()+"; check the subscriber and the slice flags"
		}
		r.Detail = joinDetail(r.Detail, stimulus)
		return r
	}
	if err := nextSubscriber(ctx, h, 2, c.settleTime()); err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	refUnserved := probeInitialNAS(ctx, h, initialNASStimulus{requested: unserved}, wait)
	leftUnserved := deregisterAfter(h, refUnserved)
	if !refUnserved.answered() {
		r.Verdict, r.Observed, r.Detail = initialNASError(refUnserved, "the reference registration requesting the unserved S-NSSAI", wait)
		r.Detail = joinDetail(r.Detail, stimulus)
		return r
	}

	r.Verdict, r.Observed, r.Detail = judgeContainerWins(stim.answer(), refServed.answer(), refUnserved.answer())
	r.Detail = joinDetail(r.Detail, stimulus)
	r.Detail = joinDetail(r.Detail, fmt.Sprintf("state left: after the stimulus %s; after the first reference %s; after the second %s", left, leftServed, leftUnserved))
	if ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the registrations")
}

// judgeContainerWins compares the stimulus registration's answer with the
// answers the core gave the served and the unserved Requested NSSAI when
// each was sent protected only.
func judgeContainerWins(stim, served, unserved string) (Verdict, string, string) {
	detail := fmt.Sprintf("stimulus answered: %s; the container's Requested NSSAI sent alone: %s; the initial message's Requested NSSAI sent alone: %s", stim, served, unserved)
	switch {
	case stim != served && stim == unserved:
		return Fail, "registration answered on the unprotected Requested NSSAI, not the one in the NAS message container", initialMsgContainerWinsFailure + " " + detail
	case stim != served:
		return Fail, "registration not answered as the request in the NAS message container is: " + stim, initialMsgContainerWinsFailure + " " + detail
	case served == unserved:
		return Skip, "core answers the served and the unserved Requested NSSAI alike; the two copies cannot be told apart",
			detail + "; the unserved S-NSSAI is the subject of NAS-REG-REJECTED-NSSAI"
	}
	return Pass, "registration answered on the request in the NAS message container", detail
}

// Observed texts of NAS-INITIAL-MSG-NO-CONTAINER.
const (
	noContainerRINMR      = "second SECURITY MODE COMMAND with RINMR set (the initial NAS message requested again)"
	noContainerSecondSMC  = "second SECURITY MODE COMMAND without RINMR"
	noContainerAccepted   = "registration accepted without the NAS message container"
	noContainerRejected   = "REGISTRATION REJECT, 5GMM cause #%d, after the SECURITY MODE COMPLETE without the NAS message container"
	noContainerEarly      = "initial message refused before the SECURITY MODE COMMAND (Registration Reject, 5GMM cause #%d)"
	noContainerReleased   = "UE CONTEXT RELEASE COMMAND after the SECURITY MODE COMPLETE without the NAS message container"
	noContainerUnanswered = "no answer to the SECURITY MODE COMPLETE without the NAS message container"
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
// SECURITY MODE COMPLETE that lacks the container. So the check is
// informational. INFO is the request 5.4.2.2 describes for getting the
// complete message, a second SECURITY MODE COMMAND with RINMR set; every
// other outcome is INFO-DIFF, Observed saying which. An Accept, or a Reject
// with cause #62, shows a core that went on with the unprotected values.
type nasInitialMsgNoContainer struct {
	initialNASWaits
}

func (nasInitialMsgNoContainer) ID() string         { return "NAS-INITIAL-MSG-NO-CONTAINER" }
func (nasInitialMsgNoContainer) Category() Category { return Security }

// Informational declares the check informational: it cites descriptive
// text only.
func (nasInitialMsgNoContainer) Informational() bool { return true }

// SpecRefs cites the UE's rule for the container (4.4.6 a)) and the
// request for the initial NAS message that 5.4.2.2 describes for other
// conditions. Neither requires anything of an AMF whose SECURITY MODE
// COMPLETE arrives without the container.
func (nasInitialMsgNoContainer) SpecRefs() []SpecRef {
	return []SpecRef{ref24501Clause446, ref24501RINMR}
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
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run security checks"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	wait := c.waitTimeout()
	unserved := stimulusSNSSAI(h.pdu)
	o := probeInitialNAS(ctx, h, initialNASStimulus{requested: unserved, plainComplete: true, omitContainer: true, stopAtSecondSMC: true}, wait)
	left := deregisterAfter(h, o)
	r.Verdict, r.Observed, r.Detail = judgeNoContainer(o, wait)
	r.Detail = joinDetail(r.Detail, fmt.Sprintf("stimulus: the complete REGISTRATION REQUEST as the initial message, Requested NSSAI %s (not served) and 5GMM capability in the clear, then a SECURITY MODE COMPLETE without the NAS message container", describeSNSSAI(unserved)))
	r.Detail = joinDetail(r.Detail, "state left: "+left)
	if r.Verdict == Error || ctx.Err() != nil {
		return r
	}
	return observeAlive(ctx, env, r, "the SECURITY MODE COMPLETE without the NAS message container")
}

// judgeNoContainer maps what the core did with the containerless SECURITY
// MODE COMPLETE onto the informational verdicts.
func judgeNoContainer(o initialNASOutcome, wait time.Duration) (Verdict, string, string) {
	switch {
	case o.err != nil || o.authReject || o.rejectErr != nil:
		return initialNASError(o, "the registration", wait)
	case o.reject != nil && o.rejectBeforeSMC:
		return InfoDiff, fmt.Sprintf(noContainerEarly, o.rejectCause()),
			"the core refused the initial message, so no SECURITY MODE COMPLETE was sent and the missing container was not exercised; answer: " + o.answer()
	case o.smcs == 0:
		return initialNASError(o, "the registration (no SECURITY MODE COMMAND arrived)", wait)
	case o.smcs >= 2 && o.secondRINMR:
		return Info, noContainerRINMR, "the registration was stopped at the second SECURITY MODE COMMAND, which the UE did not answer"
	case o.smcs >= 2:
		return InfoDiff, noContainerSecondSMC, "the second SECURITY MODE COMMAND has no Additional 5G security information IE with RINMR set; the registration was stopped there, and the UE did not answer it"
	case o.reject != nil:
		detail := "answer: " + o.answer()
		if o.rejectCause() == cause62 {
			detail += "; cause #62 \"No network slices available\" answers a Requested NSSAI, and only the initial message carried one, in the clear: the core went on with the unprotected values"
		}
		return InfoDiff, fmt.Sprintf(noContainerRejected, o.rejectCause()), detail
	case o.registered && o.accept != nil:
		return InfoDiff, noContainerAccepted, "the core completed the registration with only the unprotected initial message to go on; answer: " + o.answer()
	case o.released:
		return InfoDiff, noContainerReleased, "the core released the UE context without a Registration Accept or Reject"
	case o.noAnswer:
		return InfoDiff, noContainerUnanswered, fmt.Sprintf("no Registration Accept or Reject, second SECURITY MODE COMMAND or release within %s (absence inferred within this bound)", wait)
	}
	return Error, "no outcome", "the registration ended without a recorded answer"
}

// Observed texts of NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED.
const (
	nonCleartextAccepted   = "registration accepted"
	nonCleartextEarly      = "Registration Reject, 5GMM cause #%d, before the SECURITY MODE COMMAND"
	nonCleartextRejected   = "Registration Reject, 5GMM cause #%d, after the SECURITY MODE COMPLETE"
	nonCleartextReleased   = "UE CONTEXT RELEASE COMMAND, no Registration Accept or Reject"
	nonCleartextUnanswered = "no Registration Accept or Reject"
)

// nasInitialMsgNonCleartextAccepted sends the complete REGISTRATION REQUEST,
// non-cleartext IEs included (5GMM capability and a Requested NSSAI the
// core serves), as the initial NAS message, and the same request in the
// SECURITY MODE COMPLETE's NAS message container, and reports whether the
// core registers the UE or refuses the message.
//
// TS 24.501 4.4.6 a) has a UE without a NAS security context send the
// cleartext IEs only; no clause says what an AMF does with an initial
// message that has more. So the check is informational. INFO is a
// registration that goes through, which 5.4.2.4 describes for the request
// in the container; a Registration Reject, with its cause and whether it
// came before the SECURITY MODE COMMAND, and any other outcome are
// INFO-DIFF.
type nasInitialMsgNonCleartextAccepted struct {
	initialNASWaits
}

func (nasInitialMsgNonCleartextAccepted) ID() string {
	return "NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED"
}
func (nasInitialMsgNonCleartextAccepted) Category() Category { return Security }

// Informational declares the check informational: it cites descriptive
// text only.
func (nasInitialMsgNonCleartextAccepted) Informational() bool { return true }

// SpecRefs cites the UE's rule the stimulus breaks (4.4.6 a)) and, as a
// description of the registration going through, 5.4.2.4: its shall is
// about which copy the AMF uses, which this check's identical copies do not
// test.
func (nasInitialMsgNonCleartextAccepted) SpecRefs() []SpecRef {
	return []SpecRef{ref24501Clause446,
		{Spec: "TS 24.501", Clause: "5.4.2.4", Strength: Descriptive, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 names the IE \"Replayed NAS message container\"; \"NAS message container\" by V15.7.0"},
			}}}
}

// Budget covers the registration and the liveness probe.
func (c nasInitialMsgNonCleartextAccepted) Budget() time.Duration { return c.budget(1) }

func (c nasInitialMsgNonCleartextAccepted) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "the registration accepted on the REGISTRATION REQUEST in the NAS message container, from a UE whose initial message carried the same request with its non-cleartext IEs in the clear (TS 24.501 4.4.6 a) binds the UE; the specification is silent on what the AMF does with such a message)"}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run security checks"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	wait := c.waitTimeout()
	served := h.pdu.Slice()
	o := probeInitialNAS(ctx, h, initialNASStimulus{requested: served, plainComplete: true}, wait)
	left := deregisterAfter(h, o)
	r.Verdict, r.Observed, r.Detail = judgeNonCleartext(o, wait)
	r.Detail = joinDetail(r.Detail, fmt.Sprintf("stimulus: the complete REGISTRATION REQUEST as the initial message, Requested NSSAI %s and 5GMM capability in the clear, and the same request in the SECURITY MODE COMPLETE's NAS message container", describeSNSSAI(served)))
	r.Detail = joinDetail(r.Detail, "state left: "+left)
	if r.Verdict == Error || ctx.Err() != nil {
		return r
	}
	return observeAlive(ctx, env, r, "the initial NAS message with non-cleartext IEs")
}

// judgeNonCleartext maps the registration's outcome onto the informational
// verdicts.
func judgeNonCleartext(o initialNASOutcome, wait time.Duration) (Verdict, string, string) {
	switch {
	case o.err != nil || o.authReject || o.rejectErr != nil:
		return initialNASError(o, "the registration", wait)
	case o.reject != nil && o.rejectBeforeSMC:
		return InfoDiff, fmt.Sprintf(nonCleartextEarly, o.rejectCause()), "the core refused the initial message before authenticating the UE; answer: " + o.answer()
	case o.reject != nil:
		return InfoDiff, fmt.Sprintf(nonCleartextRejected, o.rejectCause()), "answer: " + o.answer()
	case o.registered && o.accept != nil:
		return Info, nonCleartextAccepted, "answer: " + o.answer()
	case o.released:
		return InfoDiff, nonCleartextReleased, "the core released the UE context during the registration"
	case o.noAnswer:
		return InfoDiff, nonCleartextUnanswered, fmt.Sprintf("none arrived within %s (absence inferred within this bound)", wait)
	}
	return Error, "no outcome", "the registration ended without a recorded answer"
}
