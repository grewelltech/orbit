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

// nasSMCNoNIA0 registers twice, withholding the Security Mode Complete (and
// the Registration Complete), and judges the Security Mode Command the AMF
// sends:
//
//   - a normal UE offering 5G-EA0/1/2 and 5G-IA0/1/2: the AMF shall not
//     select NIA0 (TS 33.501 5.5.2) and shall replay the UE security
//     capabilities exactly (6.7.2 step 1b); skipping the command is FAIL
//     too;
//   - a UE offering only NIA0 and NIA3 (non-conforming, since a UE shall
//     support NIA1 and NIA2; a robustness stimulus): the AMF may select
//     NIA3, reject or release, but shall not select NIA0 or skip the
//     command.
//
// Which algorithm wins is operator policy (6.7.1.1), so it is recorded, as
// is one the UE did not offer: no shall before Rel-19 forbids that for this
// case. The selected ciphering algorithm is recorded; NEA0 is legal.
type nasSMCNoNIA0 struct {
	// wait overrides smcWait (tests only).
	wait time.Duration
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

// Budget covers two registrations' waits and the liveness probe.
func (c nasSMCNoNIA0) Budget() time.Duration { return 2*c.smcWait() + 5*time.Second }

// The two UEs' security capabilities.
var (
	normalUECaps = mustCaps([]uint8{nas.NEA0, nas.NEA1, nas.NEA2}, []uint8{nas.NIA0, nas.NIA1, nas.NIA2})
	nia0nia3Caps = mustCaps([]uint8{nas.NEA0, nas.NEA3}, []uint8{nas.NIA0, nas.NIA3})
)

func mustCaps(ea, ia []uint8) nasType.UESecurityCapability {
	c, err := ue.NewSecurityCapability(ea, ia)
	if err != nil {
		panic(err)
	}
	return c
}

func (c nasSMCNoNIA0) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "no NIA0 and the UE security capabilities replayed exactly in the Security Mode Command of a normal registration; for a UE offering only NIA0 and NIA3, NIA3, a reject or a release, never NIA0 or no Security Mode Command (TS 33.501 5.5.2, 6.7.2 step 1b)"}
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

	normal := judgeSMC(c.probeSMC(ctx, h, normalUECaps), normalUECaps, false)
	odd := subResult{name: "NIA0/NIA3 UE", v: Skip, observed: "not run", detail: "not run: the normal UE's result was not a verdict"}
	if normal.v != Error {
		odd = judgeSMC(c.probeSMC(ctx, h, nia0nia3Caps), nia0nia3Caps, true)
	}
	r.Verdict, r.Observed, r.Detail = combineSMC(normal, odd)
	if r.Verdict == Error && ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the Security Mode Command checks")
}

// smcOutcome is what one probing registration saw.
type smcOutcome struct {
	smc         []byte // the Security Mode Command's NAS PDU, nil if none
	acceptFirst bool   // a Registration Accept came with no Security Mode Command
	reject      string // a reject that ended the registration
	released    bool   // a UE Context Release Command ended it
	noAnswer    bool   // nothing of the above within the wait
	err         error  // another failure
}

// probeSMC registers the UE with caps, withholding the Security Mode
// Complete and the Registration Complete, until the command, a reject, a
// release, an Accept without the command, or the wait.
func (c nasSMCNoNIA0) probeSMC(ctx context.Context, h *UEHarness, caps nasType.UESecurityCapability) smcOutcome {
	actx, cancel := context.WithTimeout(ctx, c.smcWait())
	defer cancel()
	var mu sync.Mutex
	var o smcOutcome
	_, err := h.RegisterWithProbe(actx, &engine.AttachProbe{
		Withhold: map[uint8]bool{f5nas.MsgTypeSecurityModeComplete: true, f5nas.MsgTypeRegistrationComplete: true},
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
				if o.smc == nil {
					o.acceptFirst = true
					cancel()
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
	switch {
	case o.smc != nil, o.acceptFirst, o.reject != "":
	case errors.Is(err, engine.ErrReleasedByCore):
		o.released = true
	case err == nil:
		o.err = errors.New("registration completed with the Security Mode Complete withheld")
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
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

// judgeSMC turns one registration's outcome into a sub-result. Both UEs
// FAIL on NIA0, on replayed capabilities that differ from caps, and on a
// Registration Accept with no Security Mode Command. A reject or a release
// is PASS for the NIA0/NIA3 UE (odd) and ERROR for the normal one.
func judgeSMC(o smcOutcome, caps nasType.UESecurityCapability, odd bool) subResult {
	s := subResult{name: "normal UE"}
	if odd {
		s.name = "NIA0/NIA3 UE"
	}
	offered := capsString(caps)
	nia0Msg := fmt.Sprintf("AMF selected NIA0 / skipped the Security Mode Command for a normal registration (UE offered %s). TS 33.501 5.5.2. Likely cause: AMF algorithm selection falls back to NIA0.", offered)
	const capsMsg = "Replayed UE security capabilities differ from those sent. TS 33.501 6.7.2 step 1b. Likely cause: AMF capability echo."
	switch {
	case o.err != nil:
		s.v, s.observed, s.detail = Error, "UE setup failed", o.err.Error()
	case o.acceptFirst:
		s.v, s.observed, s.detail = Fail, "Security Mode Command skipped", nia0Msg
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
			return s
		}
		s.v, s.observed = Pass, fmt.Sprintf("NIA%d selected, capabilities replayed", f.integrity)
		s.detail = strings.Join(notes, "; ")
	}
	return s
}

// combineSMC gives the check's verdict: FAIL if either sub-result failed,
// else ERROR if either errored, else PASS. Observed names the first
// sub-result that decided it (both on PASS); Detail gives both.
func combineSMC(normal, odd subResult) (Verdict, string, string) {
	detail := fmt.Sprintf("%s: %s %s, %s; %s: %s %s, %s", normal.name, normal.v, normal.observed, normal.detail, odd.name, odd.v, odd.observed, odd.detail)
	for _, v := range []Verdict{Fail, Error} {
		for _, s := range []subResult{normal, odd} {
			if s.v == v {
				return v, s.name + ": " + s.observed, detail
			}
		}
	}
	return Pass, fmt.Sprintf("%s: %s; %s: %s", normal.name, normal.observed, odd.name, odd.observed), detail
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
