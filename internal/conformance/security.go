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

func (nasSMCNoNIA0) ID() string { return "NAS-SMC-NO-NIA0-REPLAYED-CAPS" }
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

// discardOutcomes are the declared outcomes of a discard check.
type discardOutcomes struct {
	discarded, answered, status, actedOn, stateChanged, released Outcome
}

// The keys of the shalls the discard checks' outcomes rest on.
const (
	integrityDiscardRef = "TS 24.501 4.4.4.3"
	integrityFailureRef = "TS 33.501 6.4.3.3"
	replayActivationRef = "TS 33.501 6.4.3.2"
	replayProtectionRef = "TS 24.501 4.4.3.2"
)

// discardQuietBound is the bound a discard is inferred within, and
// discardAnswerBound the one an unanswered valid request is.
var (
	discardQuietBound  = &Bound{Wait: uplinkQuietWait, Source: "check bound; a discarded message draws no answer, and TS 24.501 sets no timer for one"}
	discardAnswerBound = &Bound{Wait: uplinkAnswerWait, Source: "check bound; the request before the stimulus was answered within it"}
)

// actedWindow names the window an action on the UE's state is attributed to
// the stimulus within: the quiet wait the discard is judged in. The core's
// messages for the earlier requests have settled before the stimulus, and
// the UE sends nothing else within it.
var actedWindow = fmt.Sprintf("within the %s quiet wait after it", uplinkQuietWait)

// An action on the UE's existing state within the quiet window after the
// stimulus (a PDU session released or modified, or the UE deregistered) is
// FAIL under the discard shall: the UE sent nothing else, so only the
// message can have caused it, and a discarded message changes nothing.
//
// A release of the UE context after the stimulus stays WARN for security:
// no shall forbids the AMF to release the UE, and the release is not an
// observed answer to the message. A UE Context Release moves the UE to
// 5GMM-IDLE and leaves its registration, its security context and its PDU
// sessions in place, and an AMF may release a UE's N2 connection for local
// reasons of its own, so the release does not show that the message's
// content was processed. A core that drops a UE's connection after a forged
// or replayed message still acts on input it had the means to reject.

// The outcomes of NAS-INTEGRITY-FAIL-DISCARD.
var integrityDiscardOutcomes = discardOutcomes{
	discarded: Outcome{ID: "discarded", Verdict: Pass, Refs: []string{integrityDiscardRef, integrityFailureRef}, Bound: discardQuietBound,
		Summary: "discarded the bad-MAC request and answered a valid request after it"},
	answered: Outcome{ID: "answered", Verdict: Fail, Refs: []string{integrityDiscardRef, integrityFailureRef},
		Summary: "answered the PDU Session Establishment Request whose MAC failed the integrity check"},
	status: Outcome{ID: "5gmm-status", Verdict: Fail, Refs: []string{integrityDiscardRef, integrityFailureRef},
		Summary: "answered the bad-MAC request with a 5GMM STATUS"},
	actedOn: Outcome{ID: "acted-on", Verdict: Fail, Refs: []string{integrityDiscardRef, integrityFailureRef},
		Summary: "sent no answer to the bad-MAC request but, " + actedWindow + ", released or modified a PDU session of the UE or deregistered it"},
	stateChanged: Outcome{ID: "state-changed", Verdict: Fail, Refs: []string{integrityDiscardRef, integrityFailureRef}, Bound: discardAnswerBound,
		Summary: "left a valid request after the bad-MAC request unanswered: the discarded message changed its state"},
	released: Outcome{ID: "released", Verdict: Warn, Criterion: CriterionSecurity,
		Summary: "released the UE after the bad-MAC request instead of discarding it"},
}

// The outcomes of NAS-REPLAY-DISCARD.
var replayDiscardOutcomes = discardOutcomes{
	discarded: Outcome{ID: "discarded", Verdict: Pass, Refs: []string{replayActivationRef, replayProtectionRef}, Bound: discardQuietBound,
		Summary: "discarded the replayed request and answered a valid request after it"},
	answered: Outcome{ID: "answered", Verdict: Fail, Refs: []string{replayActivationRef, replayProtectionRef},
		Summary: "answered the replayed PDU Session Establishment Request again"},
	status: Outcome{ID: "5gmm-status", Verdict: Fail, Refs: []string{replayActivationRef, replayProtectionRef},
		Summary: "answered the replayed request with a 5GMM STATUS"},
	actedOn: Outcome{ID: "acted-on", Verdict: Fail, Refs: []string{replayActivationRef, replayProtectionRef},
		Summary: "sent no answer to the replayed request but, " + actedWindow + ", released or modified a PDU session of the UE (the one the request names, say) or deregistered it"},
	stateChanged: Outcome{ID: "state-changed", Verdict: Fail, Refs: []string{replayActivationRef, replayProtectionRef}, Bound: discardAnswerBound,
		Summary: "left a valid request after the replayed request unanswered: the discarded message changed its state"},
	released: Outcome{ID: "released", Verdict: Warn, Criterion: CriterionSecurity,
		Summary: "released the UE after the replayed request instead of discarding it"},
}

// list is the outcomes in declaration order.
func (d discardOutcomes) list() []Outcome {
	return []Outcome{d.discarded, d.answered, d.status, d.actedOn, d.stateChanged, d.released}
}

// outcomes are the declared outcomes of the discard check for k.
func (k discardStimulus) outcomes() discardOutcomes {
	if k == replayStimulus {
		return replayDiscardOutcomes
	}
	return integrityDiscardOutcomes
}

// nasIntegrityFailDiscard registers the UE and, on the secured NAS
// signalling connection, sends a valid PDU Session Establishment Request
// (which must be answered), then a second one with a corrupted MAC, then a
// third, valid one. The AMF shall discard the second (TS 24.501 4.4.4.3,
// TS 33.501 6.4.3.3): an answer to it, or a 5GMM STATUS after it, is FAIL;
// so is a PDU session of the UE released or modified, or the UE
// deregistered, within the quiet wait after it, since the UE sent nothing
// else; and so is a third request that goes unanswered, which shows the
// discarded message changed the AMF's state. A UE the AMF releases after the bad MAC
// is WARN for security: no answer violates the clause, but the release
// acts on the message, and the third request cannot be judged. A first
// request the core does not answer, or a UE it releases before the
// stimulus, is BLOCKED on the PDU session establishment. Each request names
// a fresh PDU session identity and procedure transaction identity, so an
// answer names its request. The UE is deregistered at the end.
type nasIntegrityFailDiscard struct {
	// waits override the default bounds (tests only).
	waits uplinkWaits
}

// Outcomes declares what the check can report.
func (nasIntegrityFailDiscard) Outcomes() []Outcome { return integrityDiscardOutcomes.list() }

func (nasIntegrityFailDiscard) ID() string { return "NAS-INTEGRITY-FAIL-DISCARD" }
func (nasIntegrityFailDiscard) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "4.4.4.3", Strength: Shall, Since: 15, Verified: ts24501Verified, Primary: true,
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
// answer to the first request, or a 5GMM STATUS after the repeat, is FAIL;
// so is the session the first request set up (or any other of the UE's)
// released or modified, or the UE deregistered, within the quiet wait after
// the repeat; and so is a second request that goes unanswered. A UE the AMF releases
// after the repeat is WARN for security, as for NAS-INTEGRITY-FAIL-DISCARD.
// The UE is deregistered at the end.
type nasReplayDiscard struct {
	// waits override the default bounds (tests only).
	waits uplinkWaits
}

// Outcomes declares what the check can report.
func (nasReplayDiscard) Outcomes() []Outcome { return replayDiscardOutcomes.list() }

func (nasReplayDiscard) ID() string { return "NAS-REPLAY-DISCARD" }
func (nasReplayDiscard) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 33.501", Clause: "6.4.3.2", Strength: Shall, Since: 15, Verified: ts33501Verified, WordingUnchanged: true, Primary: true},
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
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run the UE security checks")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	d, err := newUplinkDriver(ctx, h, waits)
	if err != nil {
		return uplinkSetupFailed(r, err)
	}
	o := probeDiscard(ctx, d, k)
	out, observed, detail := judgeDiscard(o, k, d.waits)
	r = asClassified(r, out, observed, joinDetail(detail, d.cleanup()))
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
	// sent is when the stimulus went; acted are the core's actions on the
	// UE's state within the quiet wait after it, and later the ones after
	// the follow-up request went, which are recorded and not judged.
	sent         time.Time
	acted, later []coreAction
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
	o.sent = time.Now()
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
	o.acted = l.actions
	if note(l) || len(o.acted) > 0 {
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
	o.later = l.actions
	note(l)
	return o
}

// judgeDiscard turns a discard check's outcome into an outcome, Observed
// and Detail. A first request the core does not answer, or a UE released
// before the stimulus, is BLOCKED on the PDU session establishment.
func judgeDiscard(o discardOutcome, k discardStimulus, w uplinkWaits) (Outcome, string, string) {
	name := k.name()
	outs := k.outcomes()
	notes := ""
	if len(o.undecoded) > 0 {
		notes = fmt.Sprintf("; %d downlink NAS PDU(s) after the %s did not decode: %s", len(o.undecoded), name, strings.Join(o.undecoded, ", "))
	}
	if len(o.later) > 0 {
		notes += fmt.Sprintf("; after the valid request that followed it was sent, the AMF also sent (not judged): %s", describeActions(o.later, o.sent, name))
	}
	switch {
	case o.err != nil:
		return classifiedError, "harness failure", o.err.Error()
	case o.goneFirst != "":
		return classifiedBlocked(ProcPDUSession), "UE released before the " + name,
			o.goneFirst + " while the first, valid PDU Session Establishment Request was being answered"
	case o.first == "":
		return classifiedBlocked(ProcPDUSession), "a valid PDU Session Establishment Request got no answer",
			fmt.Sprintf("no 5GSM answer and no PDU Session Resource Setup Request within %s; the check needs the core to answer the request (DNN and slice from the UE configuration) before the %s can be judged", w.answer, name)
	case o.reply != nil:
		return outs.answered, fmt.Sprintf("%s answered: %s", name, o.reply.name), k.failure() + notes
	case o.status:
		return outs.status, "5GMM STATUS after the " + name, k.failure() + " A 5GMM STATUS is an answer: the message was not discarded." + notes
	case len(o.acted) > 0:
		released := ""
		if o.gone != "" {
			released = "; then " + o.gone
		}
		return outs.actedOn, fmt.Sprintf("%s not answered, but acted on: %s", name, describeActions(o.acted, o.sent, name)),
			fmt.Sprintf("%s The %s got no answer, but within %s of it the AMF acted on the UE's registration or PDU sessions (%s%s). The UE sent nothing else in that window and the core's messages for the earlier request had settled, so only the %s can have caused it: the message was processed, not discarded.",
				k.failure(), name, w.quiet, describeActions(o.acted, o.sent, name), released, name) + notes
	case o.gone != "":
		return outs.released, fmt.Sprintf("UE released after the %s; discard not confirmed", name),
			fmt.Sprintf("the %s got no answer, but %s, so the valid request after it could not be judged; a release is neither the discard the clause requires nor an answer to the message", name, o.gone) + notes
	case o.followUp == "":
		return outs.stateChanged, "valid request not answered after the " + name,
			fmt.Sprintf("%s The %s itself got no answer, but a valid PDU Session Establishment Request sent after it got none within %s, while the one before it was answered (%s): the message changed the AMF's state.", k.failure(), name, w.answer, o.first) + notes
	}
	return outs.discarded, fmt.Sprintf("%s discarded; a valid request after it was answered", name),
		fmt.Sprintf("the request before it was answered (%s); nothing answered the %s within %s; the request after it was answered (%s)", o.first, name, w.quiet, o.followUp) + notes
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
// COUNT accepted again), and C is then not judged; so is a PDU session of
// the UE released or modified, or the UE deregistered, within the quiet
// wait after the re-sent A, which only the re-sent A can have caused. A UE
// released after the re-sent A, before C is answered, is WARN for security,
// as for the discard checks. A or B unanswered, or a UE released before A is sent again, is
// BLOCKED on the PDU session establishment. The UE's security context may
// be unusable afterwards, so the cleanup drops the association whatever the
// deregistration brings.
type nasCountNotAdvanced struct {
	// waits override the default bounds (tests only).
	waits uplinkWaits
}

// The keys of the shalls the outcomes of NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL
// rest on.
const (
	countStoredRef = "TS 24.501 4.4.3.3"
	countReplayRef = "TS 24.501 4.4.3.2"
)

// The outcomes of NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL. An action on the UE's
// state after the re-sent request is FAIL under 4.4.3.2, as an answer to
// it is: the request's MAC verifies only at a NAS COUNT the AMF had already
// accepted. A release of the UE context after it is WARN for security, as
// for the discard checks: no shall forbids it, but the core acts on a
// message whose MAC failed.
var (
	countKept = Outcome{ID: "count-kept", Verdict: Pass, Refs: []string{countStoredRef},
		Summary: "answered a valid request sent after a MAC-failed one with a lower sequence number: the stored uplink NAS COUNT did not move"}
	countAdvanced = Outcome{ID: "count-advanced", Verdict: Fail, Refs: []string{countStoredRef},
		Bound:   &Bound{Wait: uplinkAnswerWait, Source: "check bound; requests A and B were answered within it"},
		Summary: "left a valid request sent after a MAC-failed one unanswered: the stored uplink NAS COUNT moved before the integrity check"}
	countReplayAccepted = Outcome{ID: "replay-accepted", Verdict: Fail, Refs: []string{countReplayRef},
		Summary: "answered a request re-sent with a lower sequence number, accepting a NAS COUNT it had accepted"}
	countActedOn = Outcome{ID: "acted-on", Verdict: Fail, Refs: []string{countReplayRef},
		Summary: "sent no answer to a request re-sent with a lower sequence number but, " + actedWindow + ", released or modified a PDU session of the UE (the one the request names, say) or deregistered it"}
	countReleased = Outcome{ID: "released", Verdict: Warn, Criterion: CriterionSecurity,
		Summary: "released the UE after the request re-sent with a lower sequence number"}
)

// Outcomes declares what the check can report.
func (nasCountNotAdvanced) Outcomes() []Outcome {
	return []Outcome{countKept, countAdvanced, countReplayAccepted, countActedOn, countReleased}
}

func (nasCountNotAdvanced) ID() string { return "NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL" }
func (nasCountNotAdvanced) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "4.4.3.3", Strength: Shall, Since: 15, Verified: ts24501Verified, Primary: true,
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
	countActedFailure  = "AMF acted on a PDU Session Establishment Request re-sent with a sequence number lower than the last accepted one, whose MAC verifies only at a NAS COUNT the AMF had already accepted. TS 24.501 4.4.3.2. Likely cause: the AMF checks an old sequence number at its original NAS COUNT and has no replay check, or acts on a message that failed the integrity check. The request sent after it was not judged."
)

func (c nasCountNotAdvanced) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "after two answered PDU Session Establishment Requests, the first one's bytes sent again (a lower sequence number, so a failed MAC at the estimated NAS COUNT) leave the stored uplink NAS COUNT unchanged: a third, valid request is answered (TS 24.501 4.4.3.3); the re-sent request is not answered or acted on (4.4.3.2)"}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run the UE security checks")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	d, err := newUplinkDriver(ctx, h, c.waits)
	if err != nil {
		return uplinkSetupFailed(r, err)
	}
	o := probeCount(ctx, d)
	out, observed, detail := judgeCount(o, d.waits)
	r = asClassified(r, out, observed, joinDetail(detail, d.cleanup()))
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
	// sent is when A went again; acted are the core's actions on the UE's
	// state within the quiet wait after it, and later the ones after C
	// went, which are recorded and not judged.
	sent         time.Time
	acted, later []coreAction
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
	o.sent = time.Now()
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
	o.acted = l.actions
	if note(l) || len(o.acted) > 0 {
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
	o.later = l.actions
	note(l)
	return o
}

// judgeCount turns the NAS COUNT check's outcome into an outcome, Observed
// and Detail.
func judgeCount(o countOutcome, w uplinkWaits) (Outcome, string, string) {
	const resent = "re-sent request"
	notes := ""
	if len(o.later) > 0 {
		notes = fmt.Sprintf("; after C was sent, the AMF also sent (not judged): %s", describeActions(o.later, o.sent, resent))
	}
	switch {
	case o.err != nil:
		return classifiedError, "harness failure", o.err.Error()
	case o.goneEarly != "":
		return classifiedBlocked(ProcPDUSession), "UE released before the re-sent request", o.goneEarly + " while requests A and B were being answered"
	case o.answers[0] == "" || o.answers[1] == "":
		which := "A"
		if o.answers[0] != "" {
			which = "B"
		}
		return classifiedBlocked(ProcPDUSession), fmt.Sprintf("PDU Session Establishment Request %s got no answer", which),
			fmt.Sprintf("no 5GSM answer and no PDU Session Resource Setup Request within %s; the check needs the core to answer two requests in a row (DNN and slice from the UE configuration) before the stored NAS COUNT can be judged", w.answer)
	case o.replayed != nil:
		return countReplayAccepted, fmt.Sprintf("re-sent request answered (%s): an already accepted NAS COUNT was accepted again", o.replayed.name), countReplayFailure
	case len(o.acted) > 0:
		released := ""
		if o.gone != "" {
			released = "; then " + o.gone
		}
		return countActedOn, "re-sent request not answered, but acted on: " + describeActions(o.acted, o.sent, resent),
			fmt.Sprintf("%s A and B were answered (%s; %s); A sent again got no answer, but within %s of it the AMF acted on the UE's registration or PDU sessions (%s%s). The UE sent nothing else in that window and the core's messages for A and B had settled, so only the re-sent A can have caused it.",
				countActedFailure, o.answers[0], o.answers[1], w.quiet, describeActions(o.acted, o.sent, resent), released)
	case o.third != "":
		return countKept, "valid request answered after the re-sent one; the stored NAS COUNT was not advanced",
			fmt.Sprintf("A and B were answered (%s; %s); A sent again got no answer within %s; C was answered (%s)", o.answers[0], o.answers[1], w.quiet, o.third) + notes
	case o.gone != "":
		return countReleased, "UE released after the re-sent request; the stored NAS COUNT could not be judged",
			fmt.Sprintf("%s before request C was answered, so whether the stored uplink NAS COUNT was advanced is not known; the release acts on a message whose MAC failed", o.gone) + notes
	}
	return countAdvanced, countFailObserved,
		fmt.Sprintf("%s A and B were answered (%s; %s); A sent again got no answer; C, protected at the UE's next uplink NAS COUNT, got none within %s with the association up.", countFailure, o.answers[0], o.answers[1], w.answer) + notes
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
// readings forbid it, and SKIP when one allows it: silence is PASS; an
// AUTHENTICATION REQUEST (and an IDENTITY REQUEST, which may precede one)
// is SKIP, the alternative the authentication rule permits; a REGISTRATION
// ACCEPT, REGISTRATION REJECT, SECURITY MODE COMMAND, 5GMM STATUS,
// AUTHENTICATION REJECT or network DEREGISTRATION REQUEST with no
// authentication first is FAIL, and so is the UE's PDU session released or
// modified with no authentication first: a discard changes nothing, and the
// authentication rule has the AMF authenticate before it processes the
// registration any further. A release of the UE context with no answer is
// WARN for security: neither reading describes it, and it acts on the
// message. A UE without its PDU session is BLOCKED on the PDU session
// establishment.
type nasBadMACRegistration struct {
	// waits override the default bounds (tests only).
	waits uplinkWaits
}

// badMACRegRef is the key of the shall the check's outcomes rest on.
const badMACRegRef = "TS 24.501 4.4.4.3"

// The outcomes of NAS-BADMAC-REGISTRATION-AFTER-SMC. A release with no
// answer is WARN for security: neither reading of 4.4.4.3 describes it, no
// shall forbids it, and the core acts on a message that failed the
// integrity check.
var (
	badMACRegDiscarded = Outcome{ID: "discarded", Verdict: Pass, Refs: []string{badMACRegRef},
		Bound:   &Bound{Wait: uplinkQuietWait, Source: "check bound; a discarded message draws no answer, and TS 24.501 sets no timer for one"},
		Summary: "sent no answer to the bad-MAC REGISTRATION REQUEST"}
	badMACRegProcessed = Outcome{ID: "processed", Verdict: Fail, Refs: []string{badMACRegRef},
		Summary: "answered the bad-MAC REGISTRATION REQUEST with a message neither reading of 4.4.4.3 allows, with no authentication first"}
	badMACRegActedOn = Outcome{ID: "acted-on", Verdict: Fail, Refs: []string{badMACRegRef},
		Summary: fmt.Sprintf("released or modified the UE's PDU session within the %s quiet wait after the bad-MAC REGISTRATION REQUEST, with no authentication first", uplinkQuietWait)}
	badMACRegReauthenticated = Outcome{ID: "reauthenticated", Verdict: Skip, Skip: SkipPermitted, Refs: []string{badMACRegRef},
		Summary: "answered the bad-MAC REGISTRATION REQUEST with an AUTHENTICATION REQUEST, as the authentication rule of 4.4.4.3 has it"}
	badMACRegIdentityRequested = Outcome{ID: "identity-requested", Verdict: Skip, Skip: SkipPermitted, Refs: []string{badMACRegRef},
		Summary: "answered the bad-MAC REGISTRATION REQUEST with an IDENTITY REQUEST, which may start the authentication 4.4.4.3 has"}
	badMACRegReleased = Outcome{ID: "released", Verdict: Warn, Criterion: CriterionSecurity,
		Summary: "released the UE after the bad-MAC REGISTRATION REQUEST, with no answer"}
)

// Outcomes declares what the check can report.
func (nasBadMACRegistration) Outcomes() []Outcome {
	return []Outcome{badMACRegDiscarded, badMACRegProcessed, badMACRegActedOn, badMACRegReauthenticated, badMACRegIdentityRequested, badMACRegReleased}
}

func (nasBadMACRegistration) ID() string { return "NAS-BADMAC-REGISTRATION-AFTER-SMC" }
func (nasBadMACRegistration) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "4.4.4.3", Strength: Shall, Since: 15, Verified: ts24501BadMACRegistrationVerified, Primary: true,
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
	// allowedByOneReading is an answer reading B allows: a permitted
	// alternative, SKIP.
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
	r := Result{Expected: "after the security mode control procedure, with one PDU session established, a REGISTRATION REQUEST for mobility registration updating sent with a corrupted MAC is not acted on without authentication: no answer (PASS); an AUTHENTICATION REQUEST is SKIP, since TS 24.501 4.4.4.3 can be read to require it; a REGISTRATION ACCEPT or any other answer without authentication first is FAIL"}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run the UE security checks")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	d, err := newUplinkDriver(ctx, h, c.waits)
	if err != nil {
		return uplinkSetupFailed(r, err)
	}
	o := probeBadMACRegistration(ctx, d)
	out, observed, detail := judgeBadMACRegistration(o, d.waits)
	r = asClassified(r, out, observed, joinDetail(detail, d.cleanup()))
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
	// sent is when the stimulus went, and acted are the core's actions on
	// the UE's PDU sessions after it, up to the first answer the grading
	// rule classifies.
	sent  time.Time
	acted []coreAction
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
	o.sent = time.Now()
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
	o.answers, o.undecoded, o.acted = l.mm, l.undecoded, sessionActions(l.actions)
	if l.gone() {
		o.gone = l.goneNote()
	}
	return o
}

// judgeBadMACRegActedOn is the acted-on result of the bad-MAC registration check:
// o's actions on the UE's PDU session came with no authentication first;
// answer, when set, names the answer the rule allows that followed them.
func judgeBadMACRegActedOn(o badMACRegOutcome, answer string) (Outcome, string, string) {
	const stimulus = "bad-MAC REGISTRATION REQUEST"
	acted := describeActions(o.acted, o.sent, stimulus)
	then := ""
	if answer != "" {
		then = fmt.Sprintf(" The %s came only after it.", answer)
	}
	released := ""
	if o.gone != "" {
		released = "; then " + o.gone
	}
	return badMACRegActedOn, fmt.Sprintf("PDU session acted on after the bad-MAC REGISTRATION REQUEST, with no AUTHENTICATION REQUEST before it: %s", acted),
		fmt.Sprintf("%s The AMF acted on the UE's PDU session (%s%s) with no authentication first. The UE sent nothing else in that window and the core's messages for the PDU session had settled, so only the bad-MAC REGISTRATION REQUEST can have caused it: a discard changes nothing, and the authentication rule has the AMF authenticate before it processes the registration any further, so both readings of the clause forbid it.%s",
			badMACRegFailure, acted, released, then)
}

// judgeBadMACRegistration turns the outcome into an outcome, Observed and
// Detail by the grading rule: the first answer the rule classifies decides,
// except that an action on the UE's PDU session before an answer a reading
// allows is FAIL (acted-on).
func judgeBadMACRegistration(o badMACRegOutcome, w uplinkWaits) (Outcome, string, string) {
	const ambiguity = "TS 24.501 4.4.4.3 has the AMF discard a message that fails the integrity check once secure exchange is established, and also has it authenticate when a REGISTRATION REQUEST for mobility registration update fails the check and the UE has only a non-emergency PDU session; it does not say whether the second rule still applies after secure exchange"
	blocked := classifiedBlocked(ProcPDUSession)
	switch {
	case o.err != nil:
		return classifiedError, "harness failure", o.err.Error()
	case o.goneEarly != "":
		return blocked, "the UE was released before the stimulus", o.goneEarly + " while the PDU session was being established"
	case o.sessionAnswer != "":
		return blocked, "no PDU session established",
			fmt.Sprintf("the PDU Session Establishment Request was answered with %s; the clause's rule depends on the UE having one non-emergency PDU session (DNN and slice from the UE configuration)", o.sessionAnswer)
	case o.session == "":
		return blocked, "no PDU session established",
			fmt.Sprintf("the PDU Session Establishment Request got no answer within %s; the clause's rule depends on the UE having one non-emergency PDU session", w.answer)
	}
	var unrelated []string
	for _, mt := range o.answers {
		class, name := classifyBadMACRegAnswer(mt)
		switch class {
		case unrelatedAnswer:
			unrelated = append(unrelated, name)
		case allowedByOneReading:
			if len(o.acted) > 0 {
				return judgeBadMACRegActedOn(o, name)
			}
			out, observed := badMACRegReauthenticated, badMACRegReauth
			if mt == f5nas.MsgTypeIdentityRequest {
				out, observed = badMACRegIdentityRequested, badMACRegIdentity
			}
			return out, observed,
				fmt.Sprintf("the AMF answered with %s. %s. Reading it as a discard forbids this answer; reading the authentication rule as still in force requires it, so it is not graded. The UE did not answer", name, ambiguity)
		default:
			return badMACRegProcessed, fmt.Sprintf("%s after the bad-MAC REGISTRATION REQUEST, with no AUTHENTICATION REQUEST before it", name),
				fmt.Sprintf("%s The AMF answered with %s: a discard forbids any answer, and the authentication rule requires an authentication first, so both readings of the clause forbid it.", badMACRegFailure, name)
		}
	}
	notes := ""
	if len(unrelated) > 0 {
		notes += fmt.Sprintf("; other downlink 5GMM messages in the window, not answers to a registration: %s", strings.Join(unrelated, ", "))
	}
	if len(o.acted) > 0 {
		out, observed, detail := judgeBadMACRegActedOn(o, "")
		return out, observed, detail + notes
	}
	if len(o.undecoded) > 0 {
		return classifiedError, "a downlink NAS PDU after the stimulus did not decode",
			fmt.Sprintf("%d downlink NAS PDU(s) after the bad-MAC REGISTRATION REQUEST did not verify or decode, so the AMF's answer cannot be classified: %s", len(o.undecoded), strings.Join(o.undecoded, ", ")) + notes
	}
	if o.gone != "" {
		return badMACRegReleased, "UE released after the bad-MAC REGISTRATION REQUEST, with no answer",
			fmt.Sprintf("%s, with no NAS answer before it: neither the discard nor the authentication either reading of the clause has, and the release acts on the message", o.gone) + notes
	}
	return badMACRegDiscarded, "no answer to the bad-MAC REGISTRATION REQUEST",
		fmt.Sprintf("nothing answered it within %s (discarded); the PDU session was set up by %s", w.quiet, o.session) + notes
}
