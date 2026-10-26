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
	builtins = append(builtins, nasSMCNoNIA0{})
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
//     too;
//   - a UE offering only NIA0 and NIA3 (non-conforming, since a UE shall
//     support NIA1 and NIA2; a robustness stimulus): the AMF may select
//     NIA3, reject or release, but shall not select NIA0 or skip the
//     command;
//   - last, a UE offering only NIA0 (doubly non-conforming, the same
//     robustness framing): no non-null algorithm is common. A reject or a
//     release is PASS, and so is a non-null algorithm the UE did not
//     offer (recorded, see below); NIA0 or no command is FAIL. After a Registration Accept
//     without NAS security the UE is cleaned up as far as it can be (a
//     plain deregistration, then a UE Context Release Request if needed),
//     and the cleanup is recorded.
//
// Which algorithm wins is operator policy (6.7.1.1), so it is recorded, as
// is one the UE did not offer: no shall requires the selected algorithm to
// be one the UE offered at initial registration (5.11.2 b) says only
// "dependent on"; 6.7.1.2, from V19.4.0, covers AMF change). The selected ciphering algorithm is recorded; NEA0 is legal.
type nasSMCNoNIA0 struct {
	// wait and cleanup override smcWait and cleanupWait (tests only).
	wait, cleanup time.Duration
}

func (nasSMCNoNIA0) ID() string         { return "NAS-SMC-NO-NIA0-REPLAYED-CAPS" }
func (nasSMCNoNIA0) Category() Category { return Security }
func (nasSMCNoNIA0) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 33.501", Clause: "5.5.2", Strength: Shall, Since: 15, Verified: ts33501NIA0Verified,
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
	r := Result{Expected: "Normal UE (5G-EA0/1/2, 5G-IA0/1/2): a Security Mode Command that selects no NIA0 and replays the UE security capabilities exactly. UE offering only NIA0 and NIA3: NIA3, a reject or a release. UE offering only NIA0: a reject or a release. For any UE, a Security Mode Command selecting NIA0, or no Security Mode Command, is FAIL (TS 33.501 5.5.2, 6.7.2 step 1b). A non-null algorithm the UE did not offer is PASS, recorded, not scored: no shall requires the selected algorithm to be one the UE offered at initial registration. 5.11.2 b) says only 'dependent on', and 6.7.1.2 (from V19.4.0) covers AMF change."}
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

	// The NIA0-only UE runs last: a core that lets it register without NAS
	// security is the one most likely to be left in a bad state.
	subs := []subResult{judgeSMC(c.probeSMC(ctx, h, normalUE.caps()), normalUE)}
	for _, u := range []smcStimulus{nia0nia3UE, nia0OnlyUE} {
		if subs[0].v == Error {
			subs = append(subs, subResult{name: u.String(), v: Skip, observed: "not run", detail: "not run: the normal UE's result was not a verdict"})
			continue
		}
		subs = append(subs, judgeSMC(c.probeSMC(ctx, h, u.caps()), u))
	}
	r.Verdict, r.Observed, r.Detail = combineSMC(subs...)
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
	noCleanup   error  // why a plain Accept got no cleanup: it did not decode
	reject      string // a reject that ended the registration
	released    bool   // a UE Context Release Command ended it
	noAnswer    bool   // nothing of the above within the wait
	err         error  // another failure
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
			// The engine ends the attach before the cleanup when the plain
			// Accept does not decode.
			o.noCleanup = err
		}
	case errors.Is(err, engine.ErrReleasedByCore):
		o.released = true
	case err == nil:
		o.err = errors.New("registration completed with the Security Mode Complete withheld")
	case timedOut && ctx.Err() == nil:
		o.noAnswer = true
	default:
		o.err = err
	}
	return o
}

// smcFields are the parts of a Security Mode Command the check judges.
type smcFields struct {
	integrity, ciphering uint8
	replayed             []byte
}

// parseSMC reads a Security Mode Command NAS PDU (integrity-protected, not
// ciphered: the inner plain message starts at offset 7) without the key.
func parseSMC(raw []byte) (smcFields, error) {
	if len(raw) > 7 && raw[1]&0x0f != nas.SecHdrPlain {
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
	}, nil
}

// subResult is one UE's verdict within the check.
type subResult struct {
	name             string
	v                Verdict
	observed, detail string
}

// judgeSMC turns one registration's outcome into a sub-result. Every UE
// FAILs on NIA0, on replayed capabilities that differ from those it sent,
// and on a Registration Accept with no Security Mode Command. A reject or a
// release is PASS for the two non-conforming UEs and ERROR for the normal
// one. A non-null algorithm the UE did not offer is PASS for every UE,
// including the two non-conforming ones, with the selection recorded, not
// scored.
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
	switch {
	case o.err != nil:
		s.v, s.observed, s.detail = Error, "UE setup failed", o.err.Error()
	case o.acceptFirst:
		s.v, s.observed, s.detail = Fail, "Security Mode Command skipped", skipMsg
	case o.reject != "" && odd:
		s.v, s.observed, s.detail = Pass, o.reject, fmt.Sprintf("rejected the UE offering %s", offered)
	case o.released && odd:
		s.v, s.observed, s.detail = Pass, "UE Context Release Command", fmt.Sprintf("released the UE offering %s", offered)
	case o.reject != "":
		s.v, s.observed = Error, o.reject+" before the Security Mode Command"
		s.detail = "this check needs a subscriber the core registers"
	case o.released:
		s.v, s.observed, s.detail = Error, "released before the Security Mode Command", "the AMF released a UE offering "+offered
	case o.noAnswer || o.smc == nil:
		s.v, s.observed = Error, "no Security Mode Command, reject or release"
		s.detail = "none arrived within the wait after the Authentication Response; check the subscriber and the core's logs"
	default:
		f, err := parseSMC(o.smc)
		if err != nil {
			s.v, s.observed, s.detail = Error, "Security Mode Command did not decode", err.Error()
			return s
		}
		var fails, notes []string
		observed := ""
		if f.integrity == nas.NIA0 {
			fails, observed = append(fails, nia0Msg), "NIA0 selected"
		}
		if !bytes.Equal(f.replayed, caps.Buffer) {
			fails = append(fails, fmt.Sprintf("%s Sent %x, replayed %x.", capsMsg, caps.Buffer, f.replayed))
			if observed == "" {
				observed = "replayed UE security capabilities differ"
			}
		}
		notes = append(notes, fmt.Sprintf("selected 128-NIA%d and NEA%d", f.integrity, f.ciphering))
		if !offers(caps, f.integrity, f.ciphering) {
			notes = append(notes, "an algorithm the UE did not offer (recorded, not scored)")
		}
		if len(fails) > 0 {
			s.v, s.observed, s.detail = Fail, observed, strings.Join(append(fails, notes...), " ")
			break
		}
		s.v, s.observed = Pass, fmt.Sprintf("NIA%d selected, capabilities replayed", f.integrity)
		s.detail = strings.Join(notes, "; ")
	}
	if u == nia0OnlyUE && s.v == Fail {
		s.detail += " " + nia0OnlyOrderNote
	}
	switch {
	case o.cleanup != "":
		s.detail += " Cleanup: " + o.cleanup + "."
	case o.plainAccept && o.noCleanup != nil:
		s.detail += fmt.Sprintf(" Cleanup: skipped, the plain Registration Accept did not decode (%v).", o.noCleanup)
	}
	return s
}

// noEmergencyNote follows every NIA0 or no-command FAIL: 5.5.2 excepts
// emergency calls, and the check's UEs always send an initial registration.
const noEmergencyNote = "The check never requests an emergency registration, so the emergency exception in 5.5.2 does not apply."

// nia0OnlyOrderNote follows a FAIL of the NIA0-only UE: which FAIL a core
// gives can depend on its configuration.
const nia0OnlyOrderNote = "Which of the two FAILs a core gives can depend on its configured NAS algorithm order (for example, a core may skip the Security Mode Command when only NEA0 is configured, and send one selecting NIA0 otherwise). Both are FAIL."

// combineSMC gives the check's verdict from the UEs' sub-results, in run
// order: the worst wins (FAIL, then ERROR, then PASS), and Observed names
// the first sub-result with that verdict (all of them on PASS). Detail gives
// every sub-result, and says so when the two non-conforming UEs failed the
// same way.
func combineSMC(subs ...subResult) (Verdict, string, string) {
	parts := make([]string, len(subs))
	for i, s := range subs {
		parts[i] = fmt.Sprintf("%s: %s %s, %s", s.name, s.v, s.observed, s.detail)
	}
	detail := strings.Join(parts, "; ")
	if len(subs) == 3 && subs[1].v == Fail && subs[2].v == Fail && subs[1].observed == subs[2].observed {
		detail += fmt.Sprintf(". The %s and the %s failed the same way (%s).", subs[1].name, subs[2].name, subs[1].observed)
	}
	for _, v := range []Verdict{Fail, Error} {
		for _, s := range subs {
			if s.v == v {
				return v, s.name + ": " + s.observed, detail
			}
		}
	}
	obs := make([]string, len(subs))
	for i, s := range subs {
		obs[i] = s.name + ": " + s.observed
	}
	return Pass, strings.Join(obs, "; "), detail
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
