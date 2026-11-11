package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
)

func init() {
	builtins = append(builtins, nasSMCNoNIA0{}, nasIntegrityFailDiscard{}, nasReplayDiscard{}, nasCountNotAdvanced{}, nasBadMACRegistration{})
}

// smcWait bounds each registration's wait for the Security Mode Command, a
// reject or a release.
const smcWait = 10 * time.Second

// cleanupWait bounds each step of the cleanup after a Registration Accept
// sent without NAS security: the wait for a UE Context Release Command after
// the Deregistration Request, and again after a UE Context Release Request
// (engine.AttachProbe.CleanUpPlainAccept).
const cleanupWait = 2 * time.Second

// nasSMCNoNIA0 registers three times, withholding the Security Mode
// Complete (and the Registration Complete), and judges the Security Mode
// Command the AMF sends:
//
//   - a normal UE offering 5G-EA0/1/2 and 5G-IA0/1/2: the AMF shall not
//     select NIA0 (TS 33.501 5.5.2) and shall replay the UE security
//     capabilities exactly (6.7.2 step 1b); skipping the command is FAIL
//     too. A reject, a release or silence before the command means the
//     core did not take the UE as far as the command: BLOCKED on the
//     registration;
//   - a UE offering only NIA0 and NIA3 (non-conforming, since a UE shall
//     support NIA1 and NIA2; a robustness stimulus): the AMF may select
//     NIA3, reject, release or not answer, but shall not select NIA0 or skip
//     the command;
//   - last, a UE offering only NIA0 (doubly non-conforming, the same
//     robustness framing): no non-null algorithm is common. A reject, a
//     release or no answer is PASS, and so is a non-null algorithm the UE
//     did not offer (recorded, see below); NIA0 or no command is FAIL.
//     After a Registration Accept without NAS security the UE is cleaned
//     up as far as it can be (a plain deregistration, then a UE Context
//     Release Request if needed), and the cleanup is recorded.
//
// Which algorithm wins is operator policy (6.7.1.1), so it is recorded, as
// is one the UE did not offer: no shall requires the selected algorithm to
// be one the UE offered at initial registration (5.11.2 b) says only
// "dependent on"; 6.7.1.2, from V19.4.0, covers AMF change). The selected
// ciphering algorithm is recorded; NEA0 is legal.
type nasSMCNoNIA0 struct {
	// wait and cleanup override smcWait and cleanupWait (tests only).
	wait, cleanup time.Duration
}

// The keys of the shalls the outcomes of NAS-SMC-NO-NIA0-REPLAYED-CAPS rest
// on.
const (
	smcNIA0Ref   = "TS 33.501 5.5.2"
	smcReplayRef = "TS 33.501 6.7.2 step 1b"
)

// The outcomes of NAS-SMC-NO-NIA0-REPLAYED-CAPS.
var (
	smcSecured = Outcome{ID: "secured", Verdict: Pass, Refs: []string{smcNIA0Ref, smcReplayRef},
		Summary: "sent the normal UE a Security Mode Command with a non-null integrity algorithm and its capabilities replayed, and neither selected NIA0 for nor skipped the command with the non-conforming UEs"}
	smcNIA0Selected = Outcome{ID: "nia0-selected", Verdict: Fail, Refs: []string{smcNIA0Ref},
		Summary: "selected the null integrity algorithm NIA0 in a Security Mode Command"}
	smcCapsDiffer = Outcome{ID: "capabilities-not-replayed", Verdict: Fail, Refs: []string{smcReplayRef},
		Summary: "replayed UE security capabilities that differ from those the UE sent"}
	smcSkipped = Outcome{ID: "command-skipped", Verdict: Fail, Refs: []string{smcNIA0Ref},
		Summary: "sent a Registration Accept with no Security Mode Command before it"}
)

// Outcomes declares what the check can report.
func (nasSMCNoNIA0) Outcomes() []Outcome {
	return []Outcome{smcSecured, smcNIA0Selected, smcCapsDiffer, smcSkipped}
}

func (nasSMCNoNIA0) ID() string         { return "NAS-SMC-NO-NIA0-REPLAYED-CAPS" }
func (nasSMCNoNIA0) Category() Category { return Security }
func (nasSMCNoNIA0) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 33.501", Clause: "5.5.2", Strength: Shall, Since: 15, Verified: ts33501NIA0Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 reads only \"shall be integrity-protected\"; the NIA-0 exclusion appears in V15.1.0 and is unchanged through V19.7.0"},
			}},
		{Spec: "TS 33.501", Clause: "6.7.2 step 1b", Strength: Shall, Since: 15, Verified: ts33501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0: unnumbered, \"selected NAS algorithm\", and adds the N2-handover input container; step 1b from V15.1.0; current wording from V15.2.0; the replayed UE security capabilities are required in every version"},
			}},
		{Spec: "TS 33.501", Clause: "6.7.1.1", Strength: Descriptive, Since: 15, Verified: ts33501Verified, WordingUnchanged: true},
		{Spec: "TS 33.501", Clause: "5.2.3", Strength: Descriptive, Since: 15, Verified: ts33501Verified, WordingUnchanged: true},
	}
}

func (c nasSMCNoNIA0) smcWait() time.Duration {
	if c.wait > 0 {
		return c.wait
	}
	return smcWait
}

func (c nasSMCNoNIA0) cleanupWait() time.Duration {
	if c.cleanup > 0 {
		return c.cleanup
	}
	return cleanupWait
}

// probeBound is the most one registration can take: the wait for an answer,
// then, after a Registration Accept without NAS security, both cleanup
// steps and a second of slack.
func (c nasSMCNoNIA0) probeBound() time.Duration {
	return c.smcWait() + 2*c.cleanupWait() + time.Second
}

// Budget covers three registrations and the liveness probe.
func (c nasSMCNoNIA0) Budget() time.Duration { return 3*c.probeBound() + 5*time.Second }

// The three UEs' security capabilities. The NIA0-only UE offers the normal
// UE's ciphering algorithms, so what the core does with it follows its own
// ciphering order.
var (
	normalUECaps = mustCaps([]uint8{nas.NEA0, nas.NEA1, nas.NEA2}, []uint8{nas.NIA0, nas.NIA1, nas.NIA2})
	nia0nia3Caps = mustCaps([]uint8{nas.NEA0, nas.NEA3}, []uint8{nas.NIA0, nas.NIA3})
	nia0OnlyCaps = mustCaps([]uint8{nas.NEA0, nas.NEA1, nas.NEA2}, []uint8{nas.NIA0})
)

// smcStimulus is one of the check's three UEs.
type smcStimulus int

const (
	normalUE smcStimulus = iota
	nia0nia3UE
	nia0OnlyUE
)

func (u smcStimulus) String() string {
	switch u {
	case nia0nia3UE:
		return "NIA0/NIA3 UE"
	case nia0OnlyUE:
		return "NIA0-only UE"
	}
	return "normal UE"
}

// caps is the UE's security capability.
func (u smcStimulus) caps() nasType.UESecurityCapability {
	switch u {
	case nia0nia3UE:
		return nia0nia3Caps
	case nia0OnlyUE:
		return nia0OnlyCaps
	}
	return normalUECaps
}

func mustCaps(ea, ia []uint8) nasType.UESecurityCapability {
	c, err := ue.NewSecurityCapability(ea, ia)
	if err != nil {
		panic(err)
	}
	return c
}

func (c nasSMCNoNIA0) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "Normal UE (5G-EA0/1/2, 5G-IA0/1/2): a Security Mode Command that selects no NIA0 and replays the UE security capabilities exactly. UE offering only NIA0 and NIA3: NIA3, a reject, a release or no answer. UE offering only NIA0: a reject, a release or no answer. For any UE, a Security Mode Command selecting NIA0, or a Registration Accept with no Security Mode Command, is FAIL (TS 33.501 5.5.2, 6.7.2 step 1b). A non-null algorithm the UE did not offer is PASS, recorded, not scored: no shall requires the selected algorithm to be one the UE offered at initial registration. 5.11.2 b) says only 'dependent on', and 6.7.1.2 (from V19.4.0) covers AMF change."}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run the UE security checks")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()

	// The NIA0-only UE runs last: a core that lets it register without NAS
	// security is the one most likely to be left in a bad state.
	// Each UE is its own subscriber where the check has three: every
	// registration is abandoned, and the next must not meet what the core
	// still holds of the one before. The non-conforming UEs run only when
	// the normal one reached a verdict on the command.
	subs := []subResult{judgeSMC(c.probeSMC(ctx, h, normalUE.caps()), normalUE)}
	for i, u := range []smcStimulus{nia0nia3UE, nia0OnlyUE} {
		if v := subs[0].verdict(); v != Pass && v != Fail {
			subs = append(subs, subResult{name: u.String(), notRun: true, observed: "not run", detail: "not run: the normal UE's result was not a verdict on the command"})
			continue
		}
		if err := h.UseSubscriber(i + 1); err != nil {
			subs = append(subs, subResult{name: u.String(), o: classifiedError, observed: "harness setup failed", detail: err.Error()})
			continue
		}
		subs = append(subs, judgeSMC(c.probeSMC(ctx, h, u.caps()), u))
	}
	s, observed, detail := combineSMC(subs...)
	if s.setupErr != nil {
		r = r.SetupFailed(observed, s.setupErr)
		r.Detail = joinDetail(r.Detail, detail)
	} else {
		r = asClassified(r, s.o, observed, detail)
	}
	if r.Verdict == Error && ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the Security Mode Command checks")
}

// smcOutcome is what one probing registration saw.
type smcOutcome struct {
	smc         []byte // the Security Mode Command's NAS PDU, nil if none
	acceptFirst bool   // a Registration Accept came with no Security Mode Command
	plainAccept bool   // that Accept came without NAS security (security header type 0)
	cleanup     string // what the cleanup after a plain Accept did
	noCleanup   error  // why a plain Accept got no cleanup: the attach ended first
	reject      string // a reject that ended the registration
	released    bool   // a UE Context Release Command ended it
	noAnswer    bool   // nothing of the above within the wait
	setupErr    error  // the attach failed before any of the above
	err         error  // the harness failed, or the check's time ran out
}

// probeSMC registers the UE with caps, withholding the Security Mode
// Complete and the Registration Complete, until the command, a reject, a
// release, an Accept without the command, or the wait. An Accept without NAS
// security is left to the engine's cleanup, which ends the attach.
func (c nasSMCNoNIA0) probeSMC(ctx context.Context, h *UEHarness, caps nasType.UESecurityCapability) smcOutcome {
	actx, cancel := context.WithTimeout(ctx, c.probeBound())
	defer cancel()
	var mu sync.Mutex
	var o smcOutcome
	timedOut := false
	answerWait := time.AfterFunc(c.smcWait(), func() {
		mu.Lock()
		defer mu.Unlock()
		if !o.plainAccept {
			timedOut = true
			cancel()
		}
	})
	defer answerWait.Stop()
	_, err := h.RegisterWithProbe(actx, &engine.AttachProbe{
		Withhold:           map[uint8]bool{f5nas.MsgTypeSecurityModeComplete: true, f5nas.MsgTypeRegistrationComplete: true},
		CleanUpPlainAccept: c.cleanupWait(),
		OnDownlink: func(ev engine.DownlinkEvent) {
			mu.Lock()
			defer mu.Unlock()
			switch ev.NASType {
			case f5nas.MsgTypeSecurityModeCommand:
				if o.smc == nil && !o.acceptFirst {
					o.smc = append([]byte(nil), ev.NAS...)
					cancel()
				}
			case f5nas.MsgTypeRegistrationAccept:
				if o.smc == nil && !o.acceptFirst {
					o.acceptFirst = true
					// A plain Accept is the engine's to clean up.
					if o.plainAccept = len(ev.NAS) > 1 && ev.NAS[1]&0x0f == nas.SecHdrPlain; !o.plainAccept {
						cancel()
					}
				}
			case f5nas.MsgTypeRegistrationReject:
				o.reject = "Registration Reject"
			case f5nas.MsgTypeAuthenticationReject:
				o.reject = "Authentication Reject"
			}
		},
	}, func(cfg *engine.UEConfig) { cfg.SecurityCapabilities = &caps })
	mu.Lock()
	defer mu.Unlock()
	var pa *engine.PlainAcceptError
	switch {
	case errors.As(err, &pa):
		o.cleanup = strings.Join(pa.Steps, "; ")
	case o.smc != nil, o.acceptFirst, o.reject != "":
		if o.plainAccept && err != nil {
			// The attach ended before the cleanup: the plain Accept did not
			// decode, or the transport or a context ended it.
			o.noCleanup = err
		}
	case errors.Is(err, engine.ErrReleasedByCore):
		o.released = true
	case err == nil:
		o.err = errors.New("registration completed with the Security Mode Complete withheld")
	case timedOut && ctx.Err() == nil:
		o.noAnswer = true
	case ctx.Err() != nil:
		o.err = fmt.Errorf("the per-test timeout expired before the answer (%w); raise --per-test-timeout to at least %s", err, c.Budget())
	default:
		o.setupErr = err
	}
	return o
}

// smcFields are the parts of a Security Mode Command the check judges.
type smcFields struct {
	integrity, ciphering uint8
	replayed             []byte
	ngKSI                uint8 // the NAS key set identifier
}

// parseSMC reads a Security Mode Command NAS PDU (integrity-protected, not
// ciphered: the inner plain message starts at offset 7) without the key. A
// ciphered security header is refused rather than read: past it would be
// ciphertext (TS 24.501 4.4.6: the SMC is never ciphered).
func parseSMC(raw []byte) (smcFields, error) {
	if len(raw) > 7 && raw[1]&0x0f != nas.SecHdrPlain {
		switch raw[1] & 0x0f {
		case nas.SecHdrIntegrity, nas.SecHdrIntegrityNewContext:
		default:
			return smcFields{}, fmt.Errorf("security mode command with ciphered security header type %d", raw[1]&0x0f)
		}
		raw = raw[7:]
	}
	m, err := nas.DecodePlain(raw)
	if err != nil {
		return smcFields{}, err
	}
	if m.GmmMessage == nil || m.SecurityModeCommand == nil {
		return smcFields{}, errors.New("not a Security Mode Command")
	}
	smc := m.SecurityModeCommand
	rep := smc.ReplayedUESecurityCapabilities
	n := int(rep.Len)
	if n > len(rep.Buffer) {
		n = len(rep.Buffer)
	}
	return smcFields{
		integrity: smc.SelectedNASSecurityAlgorithms.GetTypeOfIntegrityProtectionAlgorithm(),
		ciphering: smc.SelectedNASSecurityAlgorithms.GetTypeOfCipheringAlgorithm(),
		replayed:  append([]byte(nil), rep.Buffer[:n]...),
		ngKSI:     smc.GetNasKeySetIdentifiler(),
	}, nil
}

// subResult is one UE's outcome within the check: a declared outcome,
// classifiedError or a classifiedBlocked; or, with setupErr set, an attach
// that failed before the command (Result.SetupFailed decides); or not run.
type subResult struct {
	name             string
	o                Outcome
	setupErr         error
	notRun           bool
	observed, detail string
}

// verdict is the sub-result's verdict; "" when it was not run.
func (s subResult) verdict() Verdict {
	switch {
	case s.notRun:
		return ""
	case s.setupErr != nil:
		if localFault(s.setupErr) {
			return Error
		}
		return Blocked
	}
	return s.o.Verdict
}

// judgeSMC turns one registration's outcome into a sub-result. Every UE
// FAILs on NIA0, on replayed capabilities that differ from those it sent,
// and on a Registration Accept with no Security Mode Command. A reject, a
// release or no answer is PASS for the two non-conforming UEs, and for the
// normal one BLOCKED on the registration: the core did not take a UE it
// registered in the run's preflight as far as the command. A non-null
// algorithm the UE did not offer is PASS for every UE, including the two
// non-conforming ones, with the selection recorded, not scored.
func judgeSMC(o smcOutcome, u smcStimulus) subResult {
	s := subResult{name: u.String()}
	caps := u.caps()
	odd := u != normalUE
	offered := capsString(caps)
	nia0Msg := fmt.Sprintf("AMF selected NIA0 for a normal registration (UE offered %s). TS 33.501 5.5.2. %s Likely cause: AMF algorithm selection falls back to NIA0.", offered, noEmergencyNote)
	skipMsg := fmt.Sprintf("no Security Mode Command before the Registration Accept (UE offered %s). TS 33.501 5.5.2. %s", offered, noEmergencyNote)
	if o.plainAccept {
		skipMsg = fmt.Sprintf("no Security Mode Command; registration proceeded without NAS security (UE offered %s). TS 33.501 5.5.2. %s", offered, noEmergencyNote)
	}
	const capsMsg = "Replayed UE security capabilities differ from those sent. TS 33.501 6.7.2 step 1b. Likely cause: AMF capability echo."
	blocked := classifiedBlocked(ProcRegistration)
	switch {
	case o.err != nil:
		s.o, s.observed, s.detail = classifiedError, "UE setup failed", o.err.Error()
	case o.setupErr != nil:
		s.setupErr, s.observed, s.detail = o.setupErr, "registration failed before the Security Mode Command", o.setupErr.Error()
	case o.acceptFirst:
		s.o, s.observed, s.detail = smcSkipped, "Security Mode Command skipped", skipMsg
	case o.reject != "" && odd:
		s.o, s.observed, s.detail = smcSecured, o.reject, fmt.Sprintf("rejected the UE offering %s", offered)
	case o.released && odd:
		s.o, s.observed, s.detail = smcSecured, "UE Context Release Command", fmt.Sprintf("released the UE offering %s", offered)
	case o.noAnswer && odd:
		s.o, s.observed = smcSecured, "no Security Mode Command, reject or release"
		s.detail = fmt.Sprintf("the core neither secured, rejected nor released the UE offering %s within the wait: no NIA0 was selected and no command skipped", offered)
	case o.reject != "":
		s.o, s.observed = blocked, o.reject+" before the Security Mode Command"
		s.detail = "the core did not take the normal UE's registration as far as the Security Mode Command"
	case o.released:
		s.o, s.observed, s.detail = blocked, "released before the Security Mode Command", "the AMF released a UE offering "+offered
	case o.noAnswer || o.smc == nil:
		s.o, s.observed = blocked, "no Security Mode Command, reject or release"
		s.detail = "none arrived within the wait after the Registration Request"
	default:
		f, err := parseSMC(o.smc)
		if err != nil {
			s.o, s.observed, s.detail = classifiedError, "Security Mode Command did not decode", err.Error()
			return s
		}
		var fails, notes []string
		observed := ""
		failed := Outcome{}
		if f.integrity == nas.NIA0 {
			fails, observed, failed = append(fails, nia0Msg), "NIA0 selected", smcNIA0Selected
		}
		if !bytes.Equal(f.replayed, caps.Buffer) {
			fails = append(fails, fmt.Sprintf("%s Sent %x, replayed %x.", capsMsg, caps.Buffer, f.replayed))
			if observed == "" {
				observed, failed = "replayed UE security capabilities differ", smcCapsDiffer
			}
		}
		notes = append(notes, fmt.Sprintf("selected 128-NIA%d and NEA%d", f.integrity, f.ciphering))
		if !offers(caps, f.integrity, f.ciphering) {
			notes = append(notes, "an algorithm the UE did not offer (recorded, not scored)")
		}
		if len(fails) > 0 {
			s.o, s.observed, s.detail = failed, observed, strings.Join(append(fails, notes...), " ")
			break
		}
		s.o, s.observed = smcSecured, fmt.Sprintf("NIA%d selected, capabilities replayed", f.integrity)
		s.detail = strings.Join(notes, "; ")
	}
	if u == nia0OnlyUE && s.verdict() == Fail {
		s.detail += " " + nia0OnlyOrderNote
	}
	switch {
	case o.cleanup != "":
		s.detail += " Cleanup: " + o.cleanup + "."
	case o.plainAccept && errors.Is(o.noCleanup, engine.ErrDownlinkNASDecode):
		s.detail += fmt.Sprintf(" Cleanup: skipped, the plain Registration Accept did not decode: %v.", o.noCleanup)
	case o.plainAccept && o.noCleanup != nil:
		s.detail += fmt.Sprintf(" Cleanup: skipped, the attach ended before cleanup: %v.", o.noCleanup)
	}
	return s
}

// noEmergencyNote follows every NIA0 or no-command FAIL: 5.5.2 excepts
// emergency calls, and the check's UEs always send an initial registration.
const noEmergencyNote = "The check never requests an emergency registration, so the emergency exception in 5.5.2 does not apply."

// nia0OnlyOrderNote follows a FAIL of the NIA0-only UE: which FAIL a core
// gives can depend on its configuration.
const nia0OnlyOrderNote = "Which of the two FAILs a core gives can depend on its configured NAS algorithm order (for example, a core may skip the Security Mode Command when only NEA0 is configured, and send one selecting NIA0 otherwise). Both are FAIL."

// combineSMC picks the sub-result that decides the check, in run order:
// the first FAIL, else the first that is not PASS (BLOCKED or ERROR), else
// the PASS of all. Observed names the deciding sub-result (all of them on
// PASS). Detail gives every sub-result, and says so when the two
// non-conforming UEs failed the same way.
func combineSMC(subs ...subResult) (subResult, string, string) {
	parts := make([]string, len(subs))
	for i, s := range subs {
		label := string(s.verdict())
		if s.notRun {
			label = "NOT RUN"
		}
		parts[i] = fmt.Sprintf("%s: %s %s, %s", s.name, label, s.observed, strings.TrimSuffix(s.detail, "."))
	}
	detail := strings.Join(parts, "; ")
	if len(subs) == 3 && subs[1].verdict() == Fail && subs[2].verdict() == Fail && subs[1].observed == subs[2].observed {
		detail += fmt.Sprintf(". The %s and the %s failed the same way (%s)", subs[1].name, subs[2].name, subs[1].observed)
	}
	for _, s := range subs {
		if s.verdict() == Fail {
			return s, s.name + ": " + s.observed, detail
		}
	}
	for _, s := range subs {
		if !s.notRun && s.verdict() != Pass {
			return s, s.name + ": " + s.observed, detail
		}
	}
	obs := make([]string, len(subs))
	for i, s := range subs {
		obs[i] = s.name + ": " + s.observed
	}
	return subResult{o: smcSecured}, strings.Join(obs, "; "), detail
}

// capsString names the algorithms a capability offers, e.g.
// "NEA0/NEA1/NEA2, NIA0/NIA1/NIA2".
func capsString(c nasType.UESecurityCapability) string {
	list := func(octet int, prefix string) string {
		var out []string
		for a := 0; a < 8 && octet < len(c.Buffer); a++ {
			if c.Buffer[octet]&(0x80>>a) != 0 {
				out = append(out, fmt.Sprintf("%s%d", prefix, a))
			}
		}
		return strings.Join(out, "/")
	}
	return list(0, "NEA") + ", " + list(1, "NIA")
}

// offers reports whether c offers integrity algorithm ia and ciphering
// algorithm ea.
func offers(c nasType.UESecurityCapability, ia, ea uint8) bool {
	return len(c.Buffer) > 1 && ia < 8 && ea < 8 && c.Buffer[1]&(0x80>>ia) != 0 && c.Buffer[0]&(0x80>>ea) != 0
}

// discardStimulus is the uplink NAS message an AMF shall discard.
type discardStimulus int

const (
	// badMACStimulus is a fresh PDU Session Establishment Request whose
	// MAC is corrupted.
	badMACStimulus discardStimulus = iota
	// replayStimulus is the first, already answered PDU Session
	// Establishment Request sent again byte for byte, so at the NAS COUNT
	// the AMF accepted last.
	replayStimulus
)

// name is how Observed and Detail call the stimulus.
func (k discardStimulus) name() string {
	if k == replayStimulus {
		return "replayed request"
	}
	return "bad-MAC request"
}

// expected is the check's Expected line.
func (k discardStimulus) expected() string {
	if k == replayStimulus {
		return "after the security mode control procedure, an answered PDU Session Establishment Request sent again byte for byte (the same NAS COUNT) gets no answer and changes nothing: a valid request sent after it is answered (TS 33.501 6.4.3.2, TS 24.501 4.4.3.2)"
	}
	return "after the security mode control procedure, a PDU Session Establishment Request sent with a corrupted MAC gets no answer and changes nothing: a valid request sent after it is answered like the one before it (TS 24.501 4.4.4.3, TS 33.501 6.4.3.3)"
}

// failure is the check's failure message.
func (k discardStimulus) failure() string {
	if k == replayStimulus {
		return "AMF accepted a replayed NAS message (repeated NAS COUNT). TS 33.501 6.4.3.2. Likely cause: NAS COUNT replay check missing."
	}
	return "AMF processed an uplink NAS message that failed integrity check. TS 24.501 4.4.4.3. Likely cause: AMF integrity check not enforced on this message type."
}

// nasIntegrityFailDiscard registers the UE and, on the secured NAS
// signalling connection, sends a valid PDU Session Establishment Request
// (which must be answered), then a second one with a corrupted MAC, then a
// third, valid one. The AMF shall discard the second (TS 24.501 4.4.4.3,
// TS 33.501 6.4.3.3): an answer to it, or a 5GMM STATUS after it, is FAIL,
// and so is a third request that goes unanswered, which shows the discarded
// message changed the AMF's state. A UE the AMF releases after the bad MAC
// is ERROR: the message was not answered, but a release is not a discard,
// and the third request cannot be judged. Each request names a fresh PDU
// session identity and procedure transaction identity, so an answer names
// its request. The UE is deregistered at the end.
type nasIntegrityFailDiscard struct {
	// waits override the default bounds (tests only).
	waits uplinkWaits
}

func (nasIntegrityFailDiscard) ID() string         { return "NAS-INTEGRITY-FAIL-DISCARD" }
func (nasIntegrityFailDiscard) Category() Category { return Security }
func (nasIntegrityFailDiscard) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "4.4.4.3", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 16, Note: "V16.3.0 splits the REGISTRATION REQUEST rule into initial and mobility registration; the discard paragraph is unchanged from V15.0.0 through V19.8.0"},
				{Release: 18, Note: "V18.0.0 adds the unsuccessful-authentication sentence to the REGISTRATION REQUEST rules; the discard paragraph is unchanged"},
			}},
		{Spec: "TS 33.501", Clause: "6.4.3.3", Strength: Shall, Since: 15, Verified: ts33501Verified, WordingUnchanged: true},
	}
}

// Budget covers the registration, three requests, the quiet watch, the
// cleanup and the liveness probe.
func (c nasIntegrityFailDiscard) Budget() time.Duration { return c.waits.total(2) }

func (c nasIntegrityFailDiscard) Run(ctx context.Context, env Env) Result {
	return runDiscard(ctx, env, badMACStimulus, c.waits)
}

// nasReplayDiscard registers the UE and, on the secured NAS signalling
// connection, sends a valid PDU Session Establishment Request (which must
// be answered), then the same NAS PDU again, then a second, valid request.
// The repeat carries the NAS COUNT the AMF accepted last, which it shall
// accept at most once (TS 33.501 6.4.3.2, TS 24.501 4.4.3.2): a further
// answer to the first request, or a 5GMM STATUS after the repeat, is FAIL,
// and so is a second request that goes unanswered. A UE the AMF releases
// after the repeat is ERROR. The UE is deregistered at the end.
type nasReplayDiscard struct {
	// waits override the default bounds (tests only).
	waits uplinkWaits
}

func (nasReplayDiscard) ID() string         { return "NAS-REPLAY-DISCARD" }
func (nasReplayDiscard) Category() Category { return Security }
func (nasReplayDiscard) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 33.501", Clause: "6.4.3.2", Strength: Shall, Since: 15, Verified: ts33501Verified, WordingUnchanged: true},
		{Spec: "TS 24.501", Clause: "4.4.3.2", Strength: Shall, Since: 15, Verified: ts24501Verified, WordingUnchanged: true},
	}
}

// Budget covers the registration, two requests, the quiet watch, the
// cleanup and the liveness probe.
func (c nasReplayDiscard) Budget() time.Duration { return c.waits.total(2) }

func (c nasReplayDiscard) Run(ctx context.Context, env Env) Result {
	return runDiscard(ctx, env, replayStimulus, c.waits)
}

// runDiscard runs a discard check for stimulus k.
func runDiscard(ctx context.Context, env Env, k discardStimulus, waits uplinkWaits) Result {
	r := Result{Expected: k.expected()}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run the UE security checks"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	d, err := newUplinkDriver(ctx, h, waits)
	switch {
	case errors.Is(err, errNIA0Selected):
		return nia0Skip(r)
	case err != nil:
		r.Verdict, r.Observed, r.Detail = Error, "UE setup failed", err.Error()
		return r
	}
	o := probeDiscard(ctx, d, k)
	r.Verdict, r.Observed, r.Detail = judgeDiscard(o, k, d.waits)
	r.Detail = joinDetail(r.Detail, d.cleanup())
	if r.Verdict == Error && ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the "+k.name())
}

// discardOutcome is what a discard check saw.
type discardOutcome struct {
	err error // the harness could not send or read
	// first is the answer to the first, valid request, "" when none came;
	// goneFirst says how the UE went while it was awaited.
	first, goneFirst string
	// reply is an answer to the stimulus, and status a 5GMM STATUS after
	// it; gone says how the UE went after it.
	reply  *smReply
	status bool
	gone   string
	// followUp is the answer to the valid request sent after the stimulus,
	// "" when none came.
	followUp string
	// undecoded are downlink NAS PDUs after the stimulus that did not
	// verify or decode.
	undecoded []string
}

// probeDiscard sends the valid request, the stimulus and the follow-up
// request, and records the answers.
func probeDiscard(ctx context.Context, d *uplinkDriver, k discardStimulus) discardOutcome {
	var o discardOutcome
	first, err := d.sendRequest()
	if err != nil {
		o.err = fmt.Errorf("send the first PDU Session Establishment Request: %w", err)
		return o
	}
	l, err := d.awaitAnswer(ctx, first)
	if err == nil && l.answer(first) != nil {
		o.first = l.answer(first).name
		// Anything more the core sends for the first request arrives
		// before the stimulus.
		l, err = d.watch(ctx, d.waits.settle, nil)
	}
	switch {
	case err != nil:
		o.err = err
		return o
	case l.gone():
		o.goneFirst = l.goneNote()
		return o
	case o.first == "":
		return o
	}

	// A replay repeats the first request, whose answers so far were read
	// above, so any further answer to it is the replay's.
	stimulus := first
	if k == replayStimulus {
		err = d.p.Send(first.pdu)
	} else if stimulus, err = d.request(); err == nil {
		err = d.p.Send(corruptMAC(stimulus.pdu))
	}
	if err != nil {
		o.err = fmt.Errorf("send the %s: %w", k.name(), err)
		return o
	}
	processed := func(l downlinkLog) bool { return l.answer(stimulus) != nil || l.has(f5nas.MsgTypeStatus5GMM) }
	note := func(l downlinkLog) bool {
		o.reply, o.status = l.answer(stimulus), l.has(f5nas.MsgTypeStatus5GMM)
		o.undecoded = append(o.undecoded, l.undecoded...)
		if l.gone() {
			o.gone = l.goneNote()
		}
		return processed(l) || l.gone()
	}
	if l, err = d.watch(ctx, d.waits.quiet, processed); err != nil {
		o.err = err
		return o
	}
	if note(l) {
		return o
	}

	followUp, err := d.sendRequest()
	if err != nil {
		o.err = fmt.Errorf("send the PDU Session Establishment Request after the %s: %w", k.name(), err)
		return o
	}
	l, err = d.watch(ctx, d.waits.answer, func(l downlinkLog) bool { return l.answer(followUp) != nil || processed(l) })
	if err != nil {
		o.err = err
		return o
	}
	if a := l.answer(followUp); a != nil {
		o.followUp = a.name
	}
	note(l)
	return o
}

// judgeDiscard turns a discard check's outcome into a verdict, Observed and
// Detail.
func judgeDiscard(o discardOutcome, k discardStimulus, w uplinkWaits) (Verdict, string, string) {
	name := k.name()
	undecoded := ""
	if len(o.undecoded) > 0 {
		undecoded = fmt.Sprintf("; %d downlink NAS PDU(s) after the %s did not decode: %s", len(o.undecoded), name, strings.Join(o.undecoded, ", "))
	}
	switch {
	case o.err != nil:
		return Error, "harness failure", o.err.Error()
	case o.goneFirst != "":
		return Error, "UE released before the " + name, o.goneFirst + " while the first, valid PDU Session Establishment Request was being answered"
	case o.first == "":
		return Error, "precondition failed: a valid PDU Session Establishment Request got no answer",
			fmt.Sprintf("no 5GSM answer and no PDU Session Resource Setup Request within %s; the check needs a core that answers the request (DNN and slice from the UE configuration)", w.answer)
	case o.reply != nil:
		return Fail, fmt.Sprintf("%s answered: %s", name, o.reply.name), k.failure() + undecoded
	case o.status:
		return Fail, "5GMM STATUS after the " + name, k.failure() + " A 5GMM STATUS is an answer: the message was not discarded." + undecoded
	case o.gone != "":
		return Error, fmt.Sprintf("UE released after the %s; discard not confirmed", name),
			fmt.Sprintf("the %s got no answer, but %s, so the valid request after it could not be judged; a release is neither the discard the clause requires nor an answer", name, o.gone) + undecoded
	case o.followUp == "":
		return Fail, "valid request not answered after the " + name,
			fmt.Sprintf("%s The %s itself got no answer, but a valid PDU Session Establishment Request sent after it got none within %s, while the one before it was answered (%s): the message changed the AMF's state.", k.failure(), name, w.answer, o.first) + undecoded
	}
	return Pass, fmt.Sprintf("%s discarded; a valid request after it was answered", name),
		fmt.Sprintf("the request before it was answered (%s); nothing answered the %s within %s; the request after it was answered (%s)", o.first, name, w.quiet, o.followUp) + undecoded
}

// nasCountNotAdvanced registers the UE and, on the secured NAS signalling
// connection, sends two valid PDU Session Establishment Requests, A and B
// (both must be answered), then A's exact bytes again, then a third valid
// request, C. The re-sent A carries a sequence number lower than the last
// accepted one, so at the NAS COUNT the AMF estimates for it (TS 24.501
// 4.4.3.1, from V16.5.1) its MAC fails, and the stored uplink NAS COUNT
// shall stay where it was (4.4.3.3: it is updated "after successful
// integrity protection validation"). C answered is PASS. C unanswered with
// the association up is FAIL: every later uplink is checked at the wrong
// COUNT. An answer to the re-sent A is FAIL under 4.4.3.2 (an accepted
// COUNT accepted again), and C is then not judged. The UE's security
// context may be unusable afterwards, so the cleanup drops the association
// whatever the deregistration brings.
type nasCountNotAdvanced struct {
	// waits override the default bounds (tests only).
	waits uplinkWaits
}

func (nasCountNotAdvanced) ID() string         { return "NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL" }
func (nasCountNotAdvanced) Category() Category { return Security }
func (nasCountNotAdvanced) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "4.4.3.3", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 16, Note: "the paragraph on the integrity algorithm's input is split into items a) and b) with a NOTE; the sentence on updating the stored NAS COUNT is unchanged from V15.0.0 through V19.8.0"},
				{Release: 18, Note: "\"subclause\" becomes \"clause\""},
			}},
		{Spec: "TS 24.501", Clause: "4.4.3.2", Strength: Shall, Since: 15, Verified: ts24501Verified, WordingUnchanged: true},
		{Spec: "TS 24.501", Clause: "4.4.3.1", Strength: Descriptive, Since: 16, Verified: ts24501CountEstimateVerified,
			Changed: []SpecChange{
				{Release: 16, Note: "the stored-COUNT sentence and the estimation rules appear in V16.5.1; V15.0.0 to V16.4.0 give no estimation rules"},
				{Release: 18, Note: "\"subclause\" becomes \"clause\""},
			}},
	}
}

// Budget covers the registration, three answered requests, the quiet watch
// after the re-sent one, the cleanup and the liveness probe.
func (c nasCountNotAdvanced) Budget() time.Duration { return c.waits.total(3) }

const (
	countFailure       = "AMF did not process a valid uplink NAS message sent after a MAC-failed message with a lower sequence number. TS 24.501 4.4.3.3. Likely cause: the stored uplink NAS COUNT is advanced before the integrity check passes."
	countFailObserved  = "valid uplink not processed after a MAC-failed PDU with a lower sequence number; consistent with the stored COUNT being advanced before the integrity check"
	countReplayFailure = "AMF answered a PDU Session Establishment Request re-sent with a sequence number lower than the last accepted one, so it accepted a NAS COUNT it had already accepted. TS 24.501 4.4.3.2. Likely cause: the AMF checks an old sequence number at its original NAS COUNT and has no replay check. The request sent after it was not judged."
)

func (c nasCountNotAdvanced) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "after two answered PDU Session Establishment Requests, the first one's bytes sent again (a lower sequence number, so a failed MAC at the estimated NAS COUNT) leave the stored uplink NAS COUNT unchanged: a third, valid request is answered (TS 24.501 4.4.3.3); the re-sent request is not answered (4.4.3.2)"}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run the UE security checks"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	d, err := newUplinkDriver(ctx, h, c.waits)
	switch {
	case errors.Is(err, errNIA0Selected):
		return nia0Skip(r)
	case err != nil:
		r.Verdict, r.Observed, r.Detail = Error, "UE setup failed", err.Error()
		return r
	}
	o := probeCount(ctx, d)
	r.Verdict, r.Observed, r.Detail = judgeCount(o, d.waits)
	r.Detail = joinDetail(r.Detail, d.cleanup())
	if r.Verdict == Error && ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the re-sent request")
}

// countOutcome is what the NAS COUNT check saw.
type countOutcome struct {
	err error // the harness could not send or read
	// answers are the answers to requests A and B, "" for none; goneEarly
	// says how the UE went before the re-sent request.
	answers   [2]string
	goneEarly string
	// replayed is an answer to the re-sent A; gone says how the UE went
	// after it.
	replayed *smReply
	gone     string
	// third is the answer to request C, "" when none came.
	third string
}

// probeCount sends A and B, A again, and C, and records the answers.
func probeCount(ctx context.Context, d *uplinkDriver) countOutcome {
	var o countOutcome
	var a sessionRequest
	for i := range o.answers {
		req, err := d.sendRequest()
		if err != nil {
			o.err = fmt.Errorf("send PDU Session Establishment Request %c: %w", 'A'+i, err)
			return o
		}
		if i == 0 {
			a = req
		}
		l, err := d.awaitAnswer(ctx, req)
		if err == nil && l.answer(req) != nil {
			o.answers[i] = l.answer(req).name
			// Anything more the core sends for A or B arrives before A
			// is sent again.
			l, err = d.watch(ctx, d.waits.settle, nil)
		}
		switch {
		case err != nil:
			o.err = err
			return o
		case l.gone():
			o.goneEarly = l.goneNote()
			return o
		case o.answers[i] == "":
			return o
		}
	}

	if err := d.p.Send(a.pdu); err != nil {
		o.err = fmt.Errorf("re-send request A: %w", err)
		return o
	}
	again := func(l downlinkLog) bool { return l.answer(a) != nil }
	note := func(l downlinkLog) bool {
		o.replayed = l.answer(a)
		if l.gone() {
			o.gone = l.goneNote()
		}
		return o.replayed != nil || l.gone()
	}
	l, err := d.watch(ctx, d.waits.quiet, again)
	if err != nil {
		o.err = err
		return o
	}
	if note(l) {
		return o
	}

	third, err := d.sendRequest()
	if err != nil {
		o.err = fmt.Errorf("send PDU Session Establishment Request C: %w", err)
		return o
	}
	l, err = d.watch(ctx, d.waits.answer, func(l downlinkLog) bool { return l.answer(third) != nil || again(l) })
	if err != nil {
		o.err = err
		return o
	}
	if ans := l.answer(third); ans != nil {
		o.third = ans.name
	}
	note(l)
	return o
}

// judgeCount turns the NAS COUNT check's outcome into a verdict, Observed
// and Detail.
func judgeCount(o countOutcome, w uplinkWaits) (Verdict, string, string) {
	switch {
	case o.err != nil:
		return Error, "harness failure", o.err.Error()
	case o.goneEarly != "":
		return Error, "UE released before the re-sent request", o.goneEarly + " while requests A and B were being answered"
	case o.answers[0] == "" || o.answers[1] == "":
		which := "A"
		if o.answers[0] != "" {
			which = "B"
		}
		return Error, fmt.Sprintf("precondition failed: PDU Session Establishment Request %s got no answer", which),
			fmt.Sprintf("no 5GSM answer and no PDU Session Resource Setup Request within %s; the check needs a core that answers two requests in a row (DNN and slice from the UE configuration)", w.answer)
	case o.replayed != nil:
		return Fail, fmt.Sprintf("re-sent request answered (%s): an already accepted NAS COUNT was accepted again", o.replayed.name), countReplayFailure
	case o.third != "":
		return Pass, "valid request answered after the re-sent one; the stored NAS COUNT was not advanced",
			fmt.Sprintf("A and B were answered (%s; %s); A sent again got no answer within %s; C was answered (%s)", o.answers[0], o.answers[1], w.quiet, o.third)
	case o.gone != "":
		return Error, "UE released after the re-sent request; the stored NAS COUNT could not be judged",
			fmt.Sprintf("%s before request C was answered, so whether the stored uplink NAS COUNT was advanced is not known", o.gone)
	}
	return Fail, countFailObserved,
		fmt.Sprintf("%s A and B were answered (%s; %s); A sent again got no answer; C, protected at the UE's next uplink NAS COUNT, got none within %s with the association up.", countFailure, o.answers[0], o.answers[1], w.answer)
}

// nasBadMACRegistration registers the UE, establishes one PDU session and
// then, on the secured NAS signalling connection, sends a REGISTRATION
// REQUEST for mobility registration updating (5G-GUTI, the current ngKSI,
// no EPS NAS message container) protected under the current context with
// its MAC corrupted.
//
// TS 24.501 4.4.4.3 can be read two ways here. Its last paragraph has the
// AMF discard any message that fails the integrity check once secure
// exchange is established (reading A). Its message-specific rule for a
// REGISTRATION REQUEST that fails the check has the AMF, for a UE with only
// a non-emergency PDU session, "initiate a primary authentication and key
// agreement procedure", and the text does not say that rule ends when
// secure exchange begins (reading B). An answer is FAIL only when both
// readings forbid it, and SKIP when one allows it: silence and a release
// are PASS; an AUTHENTICATION REQUEST (and an IDENTITY REQUEST, which may
// precede one) is SKIP; a REGISTRATION ACCEPT, REGISTRATION REJECT,
// SECURITY MODE COMMAND, 5GMM STATUS, AUTHENTICATION REJECT or network
// DEREGISTRATION REQUEST with no authentication first is FAIL.
type nasBadMACRegistration struct {
	// waits override the default bounds (tests only).
	waits uplinkWaits
}

func (nasBadMACRegistration) ID() string         { return "NAS-BADMAC-REGISTRATION-AFTER-SMC" }
func (nasBadMACRegistration) Category() Category { return Security }
func (nasBadMACRegistration) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "4.4.4.3", Strength: Shall, Since: 15, Verified: ts24501BadMACRegistrationVerified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 to V16.2.0 have one REGISTRATION REQUEST rule (\"only a PDU session for non-emergency services\" and a HASHAMF IE in V15.0.0) and \"the AMF shall authenticate the subscriber before processing the registration request any further\""},
				{Release: 16, Note: "V16.3.0 splits the rule into initial registration and mobility and periodic registration update, with the RINMR bit"},
				{Release: 18, Note: "V18.0.0 adds \"If authentication procedure is not successful the AMF shall maintain, if any, the 5GMM-context and 5G NAS security context unchanged\"; \"subclause\" becomes \"clause\""},
			}},
	}
}

// Budget covers the registration, the PDU session, the watch after the
// stimulus, the cleanup and the liveness probe.
func (c nasBadMACRegistration) Budget() time.Duration { return c.waits.total(1) }

const (
	badMACRegFailure  = "AMF processed a REGISTRATION REQUEST that failed the integrity check after secure exchange was established, without re-authenticating. TS 24.501 4.4.4.3. Likely cause: the AMF applies the integrity-failure exemption list after secure exchange."
	badMACRegReauth   = "AMF re-authenticated after a bad-MAC REGISTRATION REQUEST after secure exchange; TS 24.501 4.4.4.3 does not settle whether its message-specific rules apply after secure exchange"
	badMACRegIdentity = "AMF sent an IDENTITY REQUEST after a bad-MAC REGISTRATION REQUEST after secure exchange; TS 24.501 4.4.4.3 does not settle whether its message-specific rules apply after secure exchange"
)

// badMACRegAnswer is how the grading rule classifies a 5GMM message the AMF
// sends after the stimulus.
type badMACRegAnswer int

const (
	// unrelatedAnswer is a message that does not answer a registration
	// request (a CONFIGURATION UPDATE COMMAND, say): recorded, not judged.
	unrelatedAnswer badMACRegAnswer = iota
	// allowedByOneReading is an answer reading B allows: SKIP.
	allowedByOneReading
	// forbiddenByBoth is an answer neither reading allows: FAIL.
	forbiddenByBoth
)

// classifyBadMACRegAnswer applies the grading rule to 5GMM message type mt
// and names the message. Reading A forbids every answer. Reading B requires
// the authentication first, which an AUTHENTICATION REQUEST starts and an
// IDENTITY REQUEST may precede (the AMF may ask for the SUCI it
// authenticates); any other answer to the registration is neither.
func classifyBadMACRegAnswer(mt uint8) (badMACRegAnswer, string) {
	switch mt {
	case f5nas.MsgTypeAuthenticationRequest:
		return allowedByOneReading, "AUTHENTICATION REQUEST"
	case f5nas.MsgTypeIdentityRequest:
		return allowedByOneReading, "IDENTITY REQUEST"
	case f5nas.MsgTypeRegistrationAccept:
		return forbiddenByBoth, "REGISTRATION ACCEPT"
	case f5nas.MsgTypeRegistrationReject:
		return forbiddenByBoth, "REGISTRATION REJECT"
	case f5nas.MsgTypeSecurityModeCommand:
		return forbiddenByBoth, "SECURITY MODE COMMAND"
	case f5nas.MsgTypeStatus5GMM:
		return forbiddenByBoth, "5GMM STATUS"
	case f5nas.MsgTypeAuthenticationReject:
		return forbiddenByBoth, "AUTHENTICATION REJECT"
	case f5nas.MsgTypeDeregistrationRequestUETerminatedDeregistration:
		return forbiddenByBoth, "DEREGISTRATION REQUEST"
	}
	return unrelatedAnswer, fmt.Sprintf("5GMM message type %#x", mt)
}

func (c nasBadMACRegistration) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "after the security mode control procedure, with one PDU session established, a REGISTRATION REQUEST for mobility registration updating sent with a corrupted MAC is not acted on without authentication: no answer or a release (PASS); an AUTHENTICATION REQUEST is SKIP, since TS 24.501 4.4.4.3 can be read to require it; a REGISTRATION ACCEPT or any other answer without authentication first is FAIL"}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run the UE security checks"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	d, err := newUplinkDriver(ctx, h, c.waits)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "precondition failed: the UE did not register", err.Error()
		return r
	}
	o := probeBadMACRegistration(ctx, d)
	r.Verdict, r.Observed, r.Detail = judgeBadMACRegistration(o, d.waits)
	r.Detail = joinDetail(r.Detail, d.cleanup())
	if r.Verdict == Error && ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the bad-MAC REGISTRATION REQUEST")
}

// badMACRegOutcome is what the bad-MAC registration check saw.
type badMACRegOutcome struct {
	err error // the harness could not build or send the stimulus, or read
	// session is the answer that set up the PDU session, "" when the core
	// set none up; sessionAnswer is what it answered instead, and
	// goneEarly how the UE went before the stimulus.
	session, sessionAnswer, goneEarly string
	// answers are the 5GMM message types sent after the stimulus, in
	// order; gone says how the UE went after it.
	answers []uint8
	gone    string
	// undecoded are downlink NAS PDUs after the stimulus that did not
	// verify or decode.
	undecoded []string
}

// probeBadMACRegistration establishes the PDU session, sends the stimulus
// and records what the AMF sends until an answer the grading rule
// classifies, a release, or the quiet wait.
func probeBadMACRegistration(ctx context.Context, d *uplinkDriver) badMACRegOutcome {
	var o badMACRegOutcome
	req, err := d.sendRequest()
	if err != nil {
		o.err = fmt.Errorf("send the PDU Session Establishment Request: %w", err)
		return o
	}
	l, err := d.awaitAnswer(ctx, req)
	if a := l.answer(req); err == nil && a != nil {
		if len(d.established) > 0 {
			o.session = a.name
			l, err = d.watch(ctx, d.waits.settle, nil)
		} else {
			o.sessionAnswer = a.name
		}
	}
	switch {
	case err != nil:
		o.err = err
		return o
	case l.gone():
		o.goneEarly = l.goneNote()
		return o
	case o.session == "":
		return o
	}

	stimulus, err := d.mobilityRegistration()
	if err == nil {
		err = d.p.Send(corruptMAC(stimulus))
	}
	if err != nil {
		o.err = fmt.Errorf("send the bad-MAC REGISTRATION REQUEST: %w", err)
		return o
	}
	l, err = d.watch(ctx, d.waits.quiet, func(l downlinkLog) bool {
		for _, mt := range l.mm {
			if class, _ := classifyBadMACRegAnswer(mt); class != unrelatedAnswer {
				return true
			}
		}
		return false
	})
	if err != nil {
		o.err = err
		return o
	}
	o.answers, o.undecoded = l.mm, l.undecoded
	if l.gone() {
		o.gone = l.goneNote()
	}
	return o
}

// judgeBadMACRegistration turns the outcome into a verdict, Observed and
// Detail by the grading rule: the first answer the rule classifies decides.
func judgeBadMACRegistration(o badMACRegOutcome, w uplinkWaits) (Verdict, string, string) {
	const ambiguity = "TS 24.501 4.4.4.3 has the AMF discard a message that fails the integrity check once secure exchange is established, and also has it authenticate when a REGISTRATION REQUEST for mobility registration update fails the check and the UE has only a non-emergency PDU session; it does not say whether the second rule still applies after secure exchange"
	switch {
	case o.err != nil:
		return Error, "harness failure", o.err.Error()
	case o.goneEarly != "":
		return Error, "precondition failed: the UE was released before the stimulus", o.goneEarly + " while the PDU session was being established"
	case o.sessionAnswer != "":
		return Error, "precondition failed: no PDU session established",
			fmt.Sprintf("the PDU Session Establishment Request was answered with %s; the clause's rule depends on the UE having one non-emergency PDU session (DNN and slice from the UE configuration)", o.sessionAnswer)
	case o.session == "":
		return Error, "precondition failed: no PDU session established",
			fmt.Sprintf("the PDU Session Establishment Request got no answer within %s; the clause's rule depends on the UE having one non-emergency PDU session", w.answer)
	}
	var unrelated []string
	for _, mt := range o.answers {
		class, name := classifyBadMACRegAnswer(mt)
		switch class {
		case unrelatedAnswer:
			unrelated = append(unrelated, name)
		case allowedByOneReading:
			observed := badMACRegReauth
			if mt == f5nas.MsgTypeIdentityRequest {
				observed = badMACRegIdentity
			}
			return Skip, observed,
				fmt.Sprintf("the AMF answered with %s. %s. Reading it as a discard forbids this answer; reading the authentication rule as still in force requires it, so it is not graded. The UE did not answer", name, ambiguity)
		default:
			return Fail, fmt.Sprintf("%s after the bad-MAC REGISTRATION REQUEST, with no AUTHENTICATION REQUEST before it", name),
				fmt.Sprintf("%s The AMF answered with %s: a discard forbids any answer, and the authentication rule requires an authentication first, so both readings of the clause forbid it.", badMACRegFailure, name)
		}
	}
	notes := ""
	if len(unrelated) > 0 {
		notes += fmt.Sprintf("; other downlink 5GMM messages in the window, not answers to a registration: %s", strings.Join(unrelated, ", "))
	}
	if len(o.undecoded) > 0 {
		return Error, "a downlink NAS PDU after the stimulus did not decode",
			fmt.Sprintf("%d downlink NAS PDU(s) after the bad-MAC REGISTRATION REQUEST did not verify or decode, so the AMF's answer cannot be classified: %s", len(o.undecoded), strings.Join(o.undecoded, ", ")) + notes
	}
	if o.gone != "" {
		return Pass, "UE released after the bad-MAC REGISTRATION REQUEST; the message was not processed",
			fmt.Sprintf("%s, with no NAS answer before it; both readings are satisfied", o.gone) + notes
	}
	return Pass, "no answer to the bad-MAC REGISTRATION REQUEST",
		fmt.Sprintf("nothing answered it within %s (discarded); the PDU session was set up by %s", w.quiet, o.session) + notes
}
