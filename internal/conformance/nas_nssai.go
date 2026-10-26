package conformance

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
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
	builtins = append(builtins, nasRegRejectedNSSAI{})
}

const (
	// nssaiWait bounds the registration's wait for a Registration Reject or
	// Accept.
	nssaiWait = 10 * time.Second
	// nssaiDeregisterWait bounds the deregistration after an Accept.
	nssaiDeregisterWait = 5 * time.Second
	// cause62 is 5GMM cause #62 "No network slices available".
	cause62 = 62
)

// stimulusCapability5GMM is the stimulus UE's 5GMM capability value,
// octets 3 to 5 (TS 24.501 9.11.3.1), all zero: NSSAA (octet 4 bit 7) and
// ER-NSSAI (octet 5 bit 5) sent explicitly as "not supported", since the
// spec gives an omitted octet no meaning.
var stimulusCapability5GMM = []byte{0, 0, 0}

// nasRegRejectedNSSAI registers the UE requesting only a stimulus S-NSSAI
// the core does not serve, with the NSSAA and ER-NSSAI bits sent as 0.
// TS 24.501 5.5.1.2.5 then requires, under condition b) 2) (the one present
// in every release), a REGISTRATION REJECT with 5GMM cause #62 (from
// V16.2.0) whose Rejected NSSAI IE lists the rejected S-NSSAI (from
// V16.6.0; with ER-NSSAI not set, that IE and not the Extended rejected
// NSSAI IE, from V17.1.0). A Reject with another cause, a #62 Reject
// without the stimulus in the Rejected NSSAI IE, and the stimulus only in a
// decoded Extended rejected NSSAI IE are FAIL; a #62 Reject without it but
// with an IE Orbit cannot decode is ERROR. A core that allows a default
// S-NSSAI instead accepts: the #62 rule did not apply, so SKIP (the
// Accept's rejected NSSAI is a may, recorded only). An Accept allowing the
// stimulus is a harness collision, ERROR. The Reject is read after the
// Security Mode Command, through the UE's NAS security context.
type nasRegRejectedNSSAI struct {
	// wait overrides nssaiWait (tests only).
	wait time.Duration
}

func (nasRegRejectedNSSAI) ID() string         { return "NAS-REG-REJECTED-NSSAI" }
func (nasRegRejectedNSSAI) Category() Category { return NAS }
func (nasRegRejectedNSSAI) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.5.1.2.5 #62", Strength: Shall, Since: 16, Verified: ts24501NSSAI62Verified,
			Changed: []SpecChange{
				{Release: 16, Note: "V16.2.0 triggers on \"the absence of allowed S-NSSAI(s)\"; by V16.5.1 the a)/b) conditions (all requested S-NSSAIs rejected, and per the UE's NSSAA bit no allowed default S-NSSAI); V16.15.0 adds b) 1) iii)"},
				{Release: 17, Note: "\"default S-NSSAIs\" replaces \"subscribed S-NSSAIs marked as default\"; a) reads \"all the S-NSSAI(s) included in the requested NSSAI are rejected\""},
				{Release: 18, Note: "V18.0.0 voids b) 1) i) and b) 2) i); V18.1.0 adds \"of the REGISTRATION REJECT message\""},
			}},
		{Spec: "TS 24.501", Clause: "5.5.1.2.5 rejected S-NSSAI in REGISTRATION REJECT", Strength: Shall, Since: 16, Verified: ts24501RejectedNSSAIVerified,
			Changed: []SpecChange{
				{Release: 16, Note: "a shall from V16.6.0; V16.2.0 to V16.3.0 say nothing, V16.4.0 to V16.5.1 \"may include the rejected NSSAI\""},
				{Release: 17, Note: "from V17.1.0 the Extended rejected NSSAI IE when the UE set ER-NSSAI, else the Rejected NSSAI IE; by V17.18.1 the \"if the UE had included requested NSSAI\" condition is dropped"},
				{Release: 18, Note: "from V18.1.0 the rejected NSSAI lists all the S-NSSAI(s) included in the requested NSSAI"},
			}},
		{Spec: "TS 24.501", Clause: "5.5.1.2.4 rejected NSSAI in REGISTRATION ACCEPT", Strength: Descriptive, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 16, Note: "adds \"shall not include\" restrictions"},
				{Release: 17, Note: "the IE follows the UE's ER-NSSAI bit; SNPN onboarding excluded"},
				{Release: 19, Note: "adds a Partially rejected NSSAI IE"},
			}},
	}
}

func (c nasRegRejectedNSSAI) waitTimeout() time.Duration {
	if c.wait > 0 {
		return c.wait
	}
	return nssaiWait
}

// Budget covers the registration, a deregistration after an Accept and the
// liveness probe.
func (c nasRegRejectedNSSAI) Budget() time.Duration {
	return c.waitTimeout() + nssaiDeregisterWait + 5*time.Second
}

// stimulusSNSSAI is the S-NSSAI the check requests: the run's SST with SD
// EEEEEE, or DDDDDD when the run's SD is EEEEEE.
func stimulusSNSSAI(run ue.PDUSessionParams) ue.SNSSAI {
	if strings.EqualFold(run.SD, "eeeeee") {
		return ue.SNSSAI{SST: run.SST, SD: "dddddd"}
	}
	return ue.SNSSAI{SST: run.SST, SD: "eeeeee"}
}

func (c nasRegRejectedNSSAI) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "a Registration Reject with 5GMM cause #62 whose Rejected NSSAI IE lists the only requested S-NSSAI, one the core does not serve, from a UE sending NSSAA and ER-NSSAI as not supported (TS 24.501 5.5.1.2.5, condition b) 2)); the Rejected NSSAI IE is required from V16.6.0. An Accept with a default S-NSSAI is SKIP"}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run NAS checks"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()

	stim := stimulusSNSSAI(h.pdu)
	o := c.probe(ctx, h, stim)
	r.Verdict, r.Observed, r.Detail = judgeNSSAI(o, stim, c.waitTimeout())
	r.Detail = joinDetail(r.Detail, fmt.Sprintf("stimulus: requested NSSAI is only SST %d SD %s; NSSAA and ER-NSSAI bits sent as 0 (not supported); condition exercised: 5.5.1.2.5 #62 b) 2)", stim.SST, stim.SD))
	if o.registered {
		dctx, cancel := context.WithTimeout(context.Background(), nssaiDeregisterWait)
		err := h.Deregister(dctx)
		cancel()
		if err != nil {
			r.Detail = joinDetail(r.Detail, fmt.Sprintf("deregistration failed (%v); the association was dropped", err))
		} else {
			r.Detail = joinDetail(r.Detail, "UE deregistered")
		}
	}
	if r.Verdict == Error && ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the rejected-NSSAI registration")
}

// nssaiOutcome is what the probing registration saw.
type nssaiOutcome struct {
	reject     []byte // the Registration Reject, plain (security header removed)
	rejectErr  error  // the security context's error for it, nil when it decoded
	accept     *nasMessage.RegistrationAccept
	acceptErr  error  // a Registration Accept that did not decode
	acceptRaw  []byte // its NAS PDU
	authReject bool   // an Authentication Reject ended the registration
	released   bool   // a UE Context Release Command ended it
	noAnswer   bool   // nothing of the above within the wait
	registered bool   // the attach returned a REGISTERED UE
	err        error  // another failure
}

// plainNAS strips the security header from a downlink NAS PDU: a protected
// PDU's inner message starts at offset 7. The inner message is readable
// only when ciphered with NEA0, which is all the engine selects today.
func plainNAS(raw []byte) []byte {
	if len(raw) > 7 && raw[1]&0x0f != nas.SecHdrPlain {
		return append([]byte(nil), raw[7:]...)
	}
	return append([]byte(nil), raw...)
}

// probe registers the UE requesting only stim, with the stimulus 5GMM
// capability, until a Registration Reject or Accept, an Authentication
// Reject, a release, or the wait.
func (c nasRegRejectedNSSAI) probe(ctx context.Context, h *UEHarness, stim ue.SNSSAI) nssaiOutcome {
	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	defer halt()
	var mu sync.Mutex
	var o nssaiOutcome
	timedOut := false
	timer := time.AfterFunc(c.waitTimeout(), func() {
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
			switch ev.Type {
			case f5nas.MsgTypeRegistrationReject:
				if o.reject == nil {
					o.reject, o.rejectErr = plainNAS(ev.Raw), ev.Err
				}
				// A decoded Reject ends the attach by itself; one the codec
				// refused is skipped, so stop here.
				halt()
			case f5nas.MsgTypeRegistrationAccept:
				if ev.Err != nil {
					o.acceptErr, o.acceptRaw = ev.Err, append([]byte(nil), ev.Raw...)
					halt()
				} else if ev.Msg != nil && ev.Msg.GmmMessage != nil {
					o.accept = ev.Msg.RegistrationAccept
				}
			case f5nas.MsgTypeAuthenticationReject:
				o.authReject = true
				halt()
			}
		},
	}, func(cfg *engine.UEConfig) {
		cfg.RequestedNSSAI = []ue.SNSSAI{stim}
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
	case o.reject != nil, o.acceptErr != nil, o.authReject:
	case errors.Is(err, engine.ErrReleasedByCore):
		o.released = true
	case timedOut && ctx.Err() == nil:
		o.noAnswer = true
	default:
		o.err = err
	}
	return o
}

// nasIE is one optional information element of a 5GMM message.
type nasIE struct {
	iei   uint8
	value []byte
}

// rejectIEs reads a plain Registration Reject: its 5GMM cause and its
// optional IEs. Following TS 24.007 11.2.4, an IEI of 0x80 or more is a
// one-octet type 1 IE, 0x70 to 0x7F a TLV-E (two-octet length), and the
// rest TLV (one-octet length).
func rejectIEs(plain []byte) (uint8, []nasIE, error) {
	if len(plain) < 4 || plain[0] != nasMessage.Epd5GSMobilityManagementMessage || plain[2] != f5nas.MsgTypeRegistrationReject {
		return 0, nil, fmt.Errorf("not a plain Registration Reject: %x", plain)
	}
	cause, rest := plain[3], plain[4:]
	var ies []nasIE
	for len(rest) > 0 {
		iei := rest[0]
		switch {
		case iei >= 0x80:
			ies, rest = append(ies, nasIE{iei: iei}), rest[1:]
		case iei&0xf0 == 0x70:
			if len(rest) < 3 || 3+int(rest[1])<<8|int(rest[2]) > len(rest) {
				return cause, ies, fmt.Errorf("IEI %#02x truncated: %x", iei, rest)
			}
			n := int(rest[1])<<8 | int(rest[2])
			ies, rest = append(ies, nasIE{iei: iei, value: rest[3 : 3+n]}), rest[3+n:]
		default:
			if len(rest) < 2 || 2+int(rest[1]) > len(rest) {
				return cause, ies, fmt.Errorf("IEI %#02x truncated: %x", iei, rest)
			}
			n := int(rest[1])
			ies, rest = append(ies, nasIE{iei: iei, value: rest[2 : 2+n]}), rest[2+n:]
		}
	}
	return cause, ies, nil
}

// The two slice IEs of the REGISTRATION REJECT (TS 24.501 8.2.9.1) the
// check decodes.
const (
	ieiRejectedNSSAI         = 0x69
	ieiExtendedRejectedNSSAI = 0x68
)

// rejectNonNSSAIIEIs are the other REGISTRATION REJECT IEs of TS 24.501
// 8.2.9.1 (V20.1.0), none of which can carry an S-NSSAI.
var rejectNonNSSAIIEIs = map[uint8]string{
	0x5f: "T3346 value",
	0x16: "T3502 value",
	0x78: "EAP message",
	0x75: "CAG information list",
	0x2c: "Disaster return wait range",
	0x71: "Extended CAG information list",
	0x3a: "Lower bound timer value",
	0x1d: "Forbidden TAI(s) for the list of \"5GS forbidden tracking areas for roaming\"",
	0x1e: "Forbidden TAI(s) for the list of \"5GS forbidden tracking areas for regional provision of service\"",
	0x3b: "Forbidden TAI(s), the IEI an AMF compliant with V17.7.0 or V17.8.0 may use",
	0x3c: "Forbidden TAI(s), the IEI an AMF compliant with V17.7.0 or V17.8.0 may use",
	0x3e: "N3IWF identifier",
	0x4d: "TNAN information",
	0x62: "Extended 5GMM cause",
	0x63: "Access technology utilization control",
}

// rejectedSNSSAI is one rejected S-NSSAI: its SST and SD (when sent), and
// the cause value.
type rejectedSNSSAI struct {
	snssai []byte
	cause  uint8
}

func (r rejectedSNSSAI) String() string { return fmt.Sprintf("%x (cause %d)", r.snssai, r.cause) }

// decodeRejectedNSSAI reads a Rejected NSSAI value (TS 24.501 9.11.3.46):
// each element a [length | cause] octet, then the SST and an optional SD,
// so a length of 1 or 4.
func decodeRejectedNSSAI(v []byte) ([]rejectedSNSSAI, error) {
	var out []rejectedSNSSAI
	for len(v) > 0 {
		n := int(v[0] >> 4)
		if (n != 1 && n != 4) || 1+n > len(v) {
			return out, fmt.Errorf("rejected S-NSSAI of length %d in %x", n, v)
		}
		out = append(out, rejectedSNSSAI{snssai: append([]byte(nil), v[1:1+n]...), cause: v[0] & 0x0f})
		v = v[1+n:]
	}
	return out, nil
}

// decodeExtendedRejectedNSSAI reads an Extended rejected NSSAI value (TS
// 24.501 9.11.3.75): partial lists, each a [spare | type of list (bits 7-5)
// | number of elements minus one (bits 4-1)] octet, a back-off timer octet
// for type 001, then the elements: a [length | cause] octet, the SST, an
// optional SD, an optional mapped HPLMN SST and an optional mapped HPLMN SD
// (lengths 1, 2, 4, 5 or 8). The S-NSSAI kept is the SST and SD.
func decodeExtendedRejectedNSSAI(v []byte) ([]rejectedSNSSAI, error) {
	var out []rejectedSNSSAI
	for len(v) > 0 {
		h := v[0]
		v = v[1:]
		switch (h >> 4) & 0x07 {
		case 0:
		case 1:
			if len(v) < 1 {
				return out, errors.New("partial list of type 001 without its back-off timer")
			}
			v = v[1:]
		default:
			return out, fmt.Errorf("partial list of reserved type %03b", (h>>4)&0x07)
		}
		count := int(h&0x0f) + 1
		if count > 8 {
			count = 8 // unused values are read as 8
		}
		for i := 0; i < count; i++ {
			if len(v) < 1 {
				return out, fmt.Errorf("partial list ends after %d of %d elements", i, count)
			}
			n := int(v[0] >> 4)
			switch n {
			case 1, 2, 4, 5, 8:
			default:
				return out, fmt.Errorf("rejected S-NSSAI of length %d", n)
			}
			if 1+n > len(v) {
				return out, fmt.Errorf("rejected S-NSSAI of length %d truncated", n)
			}
			s := v[1 : 1+n]
			if n >= 4 {
				s = s[:4]
			} else {
				s = s[:1]
			}
			out = append(out, rejectedSNSSAI{snssai: append([]byte(nil), s...), cause: v[0] & 0x0f})
			v = v[1+n:]
		}
	}
	return out, nil
}

// snssaiBytes is s as SST and SD octets.
func snssaiBytes(s ue.SNSSAI) []byte {
	out := []byte{s.SST}
	if sd, err := hex.DecodeString(s.SD); err == nil && len(sd) == 3 {
		out = append(out, sd...)
	}
	return out
}

// listsSNSSAI reports whether list holds stim.
func listsSNSSAI(list []rejectedSNSSAI, stim []byte) bool {
	for _, r := range list {
		if bytes.Equal(r.snssai, stim) {
			return true
		}
	}
	return false
}

// allowedSNSSAIs reads an Allowed NSSAI value (TS 24.501 9.11.3.37): S-NSSAI
// values, each a length octet and SST, SD and mapped S-NSSAI octets. Each is
// returned as its SST and SD.
func allowedSNSSAIs(v []byte) [][]byte {
	var out [][]byte
	for len(v) > 0 {
		n := int(v[0])
		if n == 0 || 1+n > len(v) {
			break
		}
		s := v[1 : 1+n]
		if n >= 4 {
			s = s[:4]
		} else {
			s = s[:1]
		}
		out = append(out, append([]byte(nil), s...))
		v = v[1+n:]
	}
	return out
}

// judgeNSSAI turns the outcome into a verdict.
func judgeNSSAI(o nssaiOutcome, stim ue.SNSSAI, wait time.Duration) (Verdict, string, string) {
	want := snssaiBytes(stim)
	switch {
	case o.err != nil:
		return Error, "UE setup failed", o.err.Error()
	case o.authReject:
		return Error, "Authentication Reject", "this check needs a subscriber the core authenticates"
	case o.reject != nil && errors.Is(o.rejectErr, nas.ErrIntegrity):
		return Error, "Registration Reject failed integrity check", fmt.Sprintf("NAS PDU %x", o.reject)
	case o.reject != nil:
		return judgeNSSAIReject(o.reject, want)
	case o.acceptErr != nil:
		return Error, "Registration Accept did not decode", fmt.Sprintf("%v; NAS PDU %x", o.acceptErr, o.acceptRaw)
	case o.accept != nil:
		var allowed [][]byte
		if o.accept.AllowedNSSAI != nil {
			allowed = allowedSNSSAIs(o.accept.AllowedNSSAI.Buffer[:o.accept.AllowedNSSAI.GetLen()])
		}
		detail := fmt.Sprintf("allowed NSSAI %x", allowed)
		if r := o.accept.RejectedNSSAI; r != nil {
			list, err := decodeRejectedNSSAI(r.Buffer[:r.GetLen()])
			detail += fmt.Sprintf("; rejected NSSAI in the Accept (a may, recorded only): %v", list)
			if err != nil {
				detail += fmt.Sprintf(" (%v)", err)
			}
		}
		for _, a := range allowed {
			if bytes.Equal(a, want) {
				return Error, "stimulus S-NSSAI is allowed for this subscriber", detail + "; the stimulus must be an S-NSSAI the core does not serve"
			}
		}
		return Skip, "core allowed default S-NSSAI(s); the #62 rule did not apply", detail
	case o.released:
		return Error, "released without a Registration Reject or Accept", "the core sent a UE Context Release Command; check the subscriber and the core's logs"
	case o.noAnswer:
		return Error, "no Registration Reject or Accept", fmt.Sprintf("none arrived within %s; check the subscriber and the core's logs", wait)
	}
	return Error, "no outcome", "the registration ended without a recorded answer"
}

// judgeNSSAIReject judges a decoded Registration Reject against the
// stimulus S-NSSAI want.
func judgeNSSAIReject(plain, want []byte) (Verdict, string, string) {
	cause, ies, err := rejectIEs(plain)
	if err != nil {
		return Error, "Registration Reject did not decode", err.Error()
	}
	if cause != cause62 {
		return Fail, fmt.Sprintf("Registration Reject, 5GMM cause #%d", cause),
			fmt.Sprintf("the only requested S-NSSAI was rejected with 5GMM cause #%d, not #62 \"No network slices available\". TS 24.501 5.5.1.2.5 (from V16.2.0). Likely cause: AMF NSSAI handling.", cause)
	}
	var rejected, extended []rejectedSNSSAI
	var undecodable []string
	for _, ie := range ies {
		switch ie.iei {
		case ieiRejectedNSSAI:
			list, err := decodeRejectedNSSAI(ie.value)
			rejected = append(rejected, list...)
			if err != nil {
				undecodable = append(undecodable, fmt.Sprintf("IEI %#02x (%v): %x", ie.iei, err, ie.value))
			}
		case ieiExtendedRejectedNSSAI:
			list, err := decodeExtendedRejectedNSSAI(ie.value)
			extended = append(extended, list...)
			if err != nil {
				undecodable = append(undecodable, fmt.Sprintf("IEI %#02x (%v): %x", ie.iei, err, ie.value))
			}
		default:
			if _, ok := rejectNonNSSAIIEIs[ie.iei]; ok {
				continue
			}
			undecodable = append(undecodable, fmt.Sprintf("IEI %#02x: %x", ie.iei, ie.value))
		}
	}
	const v181 = "from V18.1.0 the Reject must list every requested S-NSSAI; the check requests one, so that rule and this check coincide"
	recorded := fmt.Sprintf("Rejected NSSAI %v (causes recorded, not asserted)", rejected)
	if len(extended) > 0 {
		recorded += fmt.Sprintf("; Extended rejected NSSAI %v", extended)
	}
	switch {
	case listsSNSSAI(rejected, want):
		return Pass, "Registration Reject #62, stimulus S-NSSAI in the Rejected NSSAI IE", recorded + "; " + v181
	case listsSNSSAI(extended, want):
		return Fail, "Registration Reject #62, stimulus S-NSSAI only in the Extended rejected NSSAI IE",
			"the UE did not set ER-NSSAI, so the rejected S-NSSAI shall be in the Rejected NSSAI IE. TS 24.501 5.5.1.2.5 (from V17.1.0). Likely cause: AMF NSSAI handling. " + recorded
	case len(undecodable) > 0:
		return Error, "Registration Reject #62 with an IE Orbit cannot decode",
			"the stimulus S-NSSAI is not in the Rejected NSSAI IE, and " + strings.Join(undecodable, "; ") + ". Orbit's NAS codec covers about Rel-15/16, so this may be Orbit's gap. " + recorded
	}
	return Fail, "Registration Reject #62 without the stimulus S-NSSAI in the Rejected NSSAI IE",
		"requested S-NSSAI not allowed but not reported in rejected NSSAI. TS 24.501 5.5.1.2.5; required from V16.6.0 (a core built on V16.2.0 to V16.5.1 may omit it). Likely cause: AMF NSSAI handling. " + recorded + "; " + v181
}
