package conformance

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/free5gc/nas/nasMessage"
)

func init() {
	builtins = append(builtins, nasSMCIMEISVRequest{})
}

// Observed texts of NAS-SMC-IMEISV-REQUEST. The first half says whether the
// SECURITY MODE COMMAND requested the IMEISV; the second, after "; ", what
// the core did when the UE left it out.
const (
	imeisvNotRequested = "SECURITY MODE COMMAND does not request the IMEISV; the omission was not exercised"
	imeisvRequested    = "IMEISV requested in the SECURITY MODE COMMAND; left out, "
	imeisvIdentified   = imeisvRequested + "the core asks for it with an IDENTITY REQUEST"
	imeisvAccepted     = imeisvRequested + "the registration is accepted without it"
	imeisvRejected     = imeisvRequested + "REGISTRATION REJECT, 5GMM cause #%d"
	imeisvOtherID      = imeisvRequested + "IDENTITY REQUEST for another identity (%s)"
	imeisvSecondSMC    = imeisvRequested + "a second SECURITY MODE COMMAND"
	imeisvReleased     = imeisvRequested + "UE CONTEXT RELEASE COMMAND"
	imeisvUnanswered   = imeisvRequested + "no answer"
)

// nasSMCIMEISVRequest registers the UE and reports whether the SECURITY
// MODE COMMAND requests the IMEISV and, the UE having left a requested
// IMEISV out of its SECURITY MODE COMPLETE, what the core does: asks for it
// with an IDENTITY REQUEST, registers the UE without it, or rejects.
//
// TS 24.501 5.4.2.2 says the AMF "may request the UE to include its IMEISV
// in the SECURITY MODE COMPLETE message"; the shall is the UE's (5.4.2.3;
// TS 33.501 6.7.2: "shall include PEI in case AMF requested it"). No clause
// read says what the AMF does when a requested IMEISV is missing. So the
// check is informational. INFO is a request followed, on the omission, by
// the identification procedure for the IMEISV; a core that does not request
// it, one that goes on without it and one that rejects are INFO-DIFF,
// Observed saying which. An IDENTITY REQUEST is answered, so the
// registration can complete, and the UE is then deregistered.
type nasSMCIMEISVRequest struct {
	initialNASWaits
}

func (nasSMCIMEISVRequest) ID() string         { return "NAS-SMC-IMEISV-REQUEST" }
func (nasSMCIMEISVRequest) Category() Category { return Security }

// Informational declares the check informational: it cites descriptive
// text only.
func (nasSMCIMEISVRequest) Informational() bool { return true }

// SpecRefs cites the AMF's option to request the IMEISV (TS 24.501 5.4.2.2,
// a may) and the UE's obligation to send a requested one (TS 33.501 6.7.2),
// as descriptions: neither requires anything of the AMF.
func (nasSMCIMEISVRequest) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.4.2.2 IMEISV request", Strength: Descriptive, Since: 15, Verified: ts24501Verified, WordingUnchanged: true},
		{Spec: "TS 33.501", Clause: "6.7.2 step 2b", Strength: Descriptive, Since: 15, Verified: ts33501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "an unnumbered paragraph in V15.0.0, step 2b by V15.20.0; the sentence on the PEI is unchanged through V19.7.0"},
			}},
	}
}

// Budget covers the registration and the liveness probe.
func (c nasSMCIMEISVRequest) Budget() time.Duration { return c.budget(1) }

func (c nasSMCIMEISVRequest) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "the IMEISV requested in the SECURITY MODE COMMAND and, when the UE leaves it out of the SECURITY MODE COMPLETE, an IDENTITY REQUEST for it (TS 24.501 5.4.2.2: the AMF may request it; the specification is silent on a requested IMEISV that is missing)"}
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
	o := probeInitialNAS(ctx, h, initialNASStimulus{requested: h.pdu.Slice(), omitIMEISV: true, stopAtSecondSMC: true}, wait)
	left := deregisterAfter(ctx, h, o, c.settleTime())
	r.Verdict, r.Observed, r.Detail = judgeIMEISV(o, wait)
	r.Detail = joinDetail(r.Detail, "stimulus: a conformant registration whose SECURITY MODE COMPLETE carries no IMEISV, requested or not; an IDENTITY REQUEST is answered")
	r.Detail = joinDetail(r.Detail, "state left: "+left)
	if r.Verdict == Error || ctx.Err() != nil {
		return r
	}
	return observeAlive(ctx, env, r, "the SECURITY MODE COMPLETE without the IMEISV")
}

// judgeIMEISV maps the registration's outcome onto the informational
// verdicts.
func judgeIMEISV(o initialNASOutcome, wait time.Duration) (Verdict, string, string) {
	if o.err != nil || o.authReject || o.rejectErr != nil || o.smcs == 0 {
		return initialNASError(o, "the registration", wait)
	}
	ended := "answer: " + o.answer()
	if !o.imeisvRequested {
		return InfoDiff, imeisvNotRequested, "the SECURITY MODE COMMAND has no IMEISV request IE set to \"IMEISV requested\"; " + ended
	}
	switch {
	case slices.Contains(o.identities, nasMessage.MobileIdentity5GSTypeImeisv):
		return Info, imeisvIdentified, "the UE answered the IDENTITY REQUEST with its IMEISV; " + ended
	case len(o.identities) > 0:
		names := make([]string, len(o.identities))
		for i, id := range o.identities {
			names[i] = identityTypeName(id)
		}
		return InfoDiff, fmt.Sprintf(imeisvOtherID, names[0]),
			fmt.Sprintf("identities requested after the SECURITY MODE COMPLETE: %s; the UE answered; %s", strings.Join(names, ", "), ended)
	case o.smcs >= 2:
		return InfoDiff, imeisvSecondSMC, "the registration was stopped at the second SECURITY MODE COMMAND, which the UE did not answer"
	case o.reject != nil:
		return InfoDiff, fmt.Sprintf(imeisvRejected, o.rejectCause()), ended
	case o.registered && o.accept != nil:
		return InfoDiff, imeisvAccepted, ended
	case o.released:
		return InfoDiff, imeisvReleased, "the core released the UE context without a Registration Accept or Reject"
	case o.noAnswer:
		return InfoDiff, imeisvUnanswered, fmt.Sprintf("no Registration Accept or Reject, IDENTITY REQUEST or release within %s (absence inferred within this bound)", wait)
	}
	return Error, "no outcome", "the registration ended without a recorded answer"
}

// identityTypeName names a type of identity of an IDENTITY REQUEST (TS
// 24.501 9.11.3.3).
func identityTypeName(t uint8) string {
	switch t {
	case nasMessage.MobileIdentity5GSTypeSuci:
		return "SUCI"
	case nasMessage.MobileIdentity5GSType5gGuti:
		return "5G-GUTI"
	case nasMessage.MobileIdentity5GSTypeImei:
		return "IMEI"
	case nasMessage.MobileIdentity5GSType5gSTmsi:
		return "5G-S-TMSI"
	case nasMessage.MobileIdentity5GSTypeImeisv:
		return "IMEISV"
	}
	return fmt.Sprintf("type %d", t)
}
