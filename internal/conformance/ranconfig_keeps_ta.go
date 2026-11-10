package conformance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
)

func init() {
	builtins = append(builtins, ranConfigUpdateKeepsTA{})
}

const (
	// keepsTAUpdateWait bounds the wait for the RAN CONFIGURATION UPDATE's
	// outcome; its absence within this bound is what the ERROR reports.
	keepsTAUpdateWait = 5 * time.Second
	// keepsTARegisterWait bounds the registration that follows the outcome.
	keepsTARegisterWait = 10 * time.Second
	// keepsTADeregisterWait bounds the deregistration after a registration
	// that succeeded.
	keepsTADeregisterWait = 5 * time.Second
	// keepsTADefaultName is the RAN Node Name the update carries when the
	// run's gNB has none.
	keepsTADefaultName = "orbit-conf"
)

// keepsTAFailText is the FAIL detail.
const keepsTAFailText = "AMF stopped serving an NG Setup tracking area after a RAN CONFIGURATION UPDATE that did not include the Supported TA List. " +
	"TS 38.413 8.7.2.2. Likely cause: the AMF treats an absent Supported TA List as an empty one."

// taCauses are the 5GMM causes TS 24.501 Annex A ties to a tracking area: a
// Registration Reject with one of them after the ACKNOWLEDGE is the FAIL.
var taCauses = map[uint8]string{
	12: "Tracking area not allowed",
	13: "Roaming not allowed in this tracking area",
	15: "No suitable cells in tracking area",
}

// otherCauseNames names causes a Registration Reject may carry that are not
// tied to a tracking area, for the ERROR's Observed text.
var otherCauseNames = map[uint8]string{
	11: "PLMN not allowed",
	62: "No network slices available",
}

// ranConfigUpdateKeepsTA sends, on an association that has completed NG
// Setup, a RAN CONFIGURATION UPDATE that carries only a RAN Node Name, and
// then registers the UE over the same association in the tracking area the
// NG Setup presented. TS 38.413 8.7.2.2: "If an information element is not
// included in the RAN CONFIGURATION UPDATE message, the AMF shall interpret
// that the corresponding configuration data is not changed and shall
// continue to operate the NG-C interface with the existing related
// configuration data." The Supported TA List is not included, so the AMF
// must still serve the tracking area.
//
// The shall sits in the successful-operation clause, so the state is judged
// only after a RAN CONFIGURATION UPDATE ACKNOWLEDGE: a registration that
// succeeds is PASS; a Registration Reject with a tracking-area cause (#12,
// #13, #15) is FAIL; any other registration failure is ERROR, a Reject with
// another cause included, since that does not show the tracking area was
// dropped. After a RAN CONFIGURATION UPDATE FAILURE, which 8.7.2.3 allows,
// the UE still registers and the check SKIPs with both results in Observed.
// No outcome within keepsTAUpdateWait is ERROR and no registration is
// attempted. A registered UE is deregistered, and a fresh NG Setup confirms
// the core afterwards.
type ranConfigUpdateKeepsTA struct {
	// updateWait and registerWait override keepsTAUpdateWait and
	// keepsTARegisterWait; tests shorten them.
	updateWait, registerWait time.Duration
}

func (c ranConfigUpdateKeepsTA) waits() (update, register time.Duration) {
	update, register = keepsTAUpdateWait, keepsTARegisterWait
	if c.updateWait > 0 {
		update = c.updateWait
	}
	if c.registerWait > 0 {
		register = c.registerWait
	}
	return update, register
}

func (ranConfigUpdateKeepsTA) ID() string         { return "NGAP-RAN-CONFIG-UPDATE-KEEPS-TA" }
func (ranConfigUpdateKeepsTA) Category() Category { return Procedural }

// SpecRefs cites the shall in 8.7.2.2. The 5GMM causes that decide between
// FAIL and ERROR (TS 24.501 Annex A) are in the provenance file only: the
// versions their wording was read in are not listed.
func (ranConfigUpdateKeepsTA) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.7.2.2", Strength: Shall, Since: 15, Verified: ts38413RANConfigUnchangedVerified,
			Changed: []SpecChange{
				{Release: 15, Note: "the sentence is added in V15.3.0 (absent in V15.0.0 to V15.2.0); unchanged to V18.11.0"},
				{Release: 19, Note: "in V19.4.0 the same words are a bullet under \"If the RAN Configuration Update procedure is executed between the NG-RAN node and the AMF\""},
			}},
	}
}

// Budget covers the update, the registration, a deregistration and the
// liveness probe.
func (c ranConfigUpdateKeepsTA) Budget() time.Duration {
	update, register := c.waits()
	return update + register + keepsTADeregisterWait + 5*time.Second
}

func (c ranConfigUpdateKeepsTA) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "after a RAN CONFIGURATION UPDATE ACKNOWLEDGE for an update without a Supported TA List, a UE still registers in the tracking area the NG Setup presented (TS 38.413 8.7.2.2)"}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run checks that register a UE"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()

	name := env.GNB.Name
	if name == "" {
		name = keepsTADefaultName
	}
	update, register := c.waits()
	o := c.probe(ctx, h, name)
	r.Verdict, r.Observed, r.Detail = judgeKeepsTA(o, env.GNB, update, register)
	if o.update != nil {
		r.Detail = joinDetail(r.Detail, fmt.Sprintf("stimulus: RAN CONFIGURATION UPDATE carrying only RAN Node Name %q; registration in TAC %d of PLMN %s/%s over the same association",
			name, env.GNB.TAC, env.GNB.MCC, env.GNB.MNC))
	}
	if o.registered {
		dctx, cancel := context.WithTimeout(context.Background(), keepsTADeregisterWait)
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
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the RAN CONFIGURATION UPDATE")
}

// keepsTAOutcome is what the update and the registration after it showed.
type keepsTAOutcome struct {
	// update is the RAN Configuration Update's outcome, nil without one;
	// updateErr is then why, and updateTimedOut whether the wait ran out
	// (as opposed to the association ending). Both nil: the association
	// never got as far as the update.
	update         *gnb.RANConfigUpdateResult
	updateErr      error
	updateTimedOut bool
	updateAfter    time.Duration // from the update to its outcome
	updateCutShort bool          // the per-test timeout expired during the wait

	registered  bool  // the attach returned a REGISTERED UE
	sawReject   bool  // a Registration Reject arrived
	rejectCause uint8 // its 5GMM cause, when rejectErr is nil
	rejectErr   error // why the Reject could not be read
	authReject  bool  // an Authentication Reject ended the registration
	released    bool  // a UE Context Release Command ended it
	noAnswer    bool  // nothing of the above within the wait
	err         error // another failure
}

// probe opens the UE's association, runs the RAN Configuration Update on it
// before the Initial UE Message, and registers the UE (no PDU session) until
// it is registered, rejected, released, or the registration wait runs out.
func (c ranConfigUpdateKeepsTA) probe(ctx context.Context, h *UEHarness, name string) keepsTAOutcome {
	updateWait, registerWait := c.waits()
	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	defer halt()
	var mu sync.Mutex
	var o keepsTAOutcome
	timedOut := false
	var timer *time.Timer
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
	}()

	_, err := h.RegisterWithProbe(ctx, &engine.AttachProbe{
		Stop: stop,
		BeforeRegistration: func(actx context.Context, conn gnb.Transport) error {
			pdu, err := gnb.BuildRANConfigurationUpdate(gnb.WithRANNodeName(name))
			if err != nil {
				return err
			}
			uctx, cancel := context.WithTimeout(actx, updateWait)
			defer cancel()
			sent := time.Now()
			res, err := gnb.RANConfigurationUpdate(uctx, conn, pdu)
			mu.Lock()
			defer mu.Unlock()
			o.updateAfter = time.Since(sent)
			if err != nil {
				o.updateErr = err
				o.updateCutShort = actx.Err() != nil
				o.updateTimedOut = errors.Is(err, context.DeadlineExceeded) && !o.updateCutShort
				return err
			}
			o.update = res
			timer = time.AfterFunc(registerWait, func() {
				mu.Lock()
				timedOut = true
				mu.Unlock()
				halt()
			})
			return nil
		},
		OnNAS: func(ev engine.NASEvent) {
			mu.Lock()
			defer mu.Unlock()
			switch ev.MessageType() {
			case f5nas.MsgTypeRegistrationReject:
				if !o.sawReject {
					o.sawReject = true
					plain := readableNAS(ev)
					if plain == nil {
						o.rejectErr = fmt.Errorf("the Registration Reject could not be read: %v", ev.Err)
					} else {
						o.rejectCause, _, o.rejectErr = rejectIEs(plain)
					}
				}
				// A decoded Reject ends the attach by itself; one the codec
				// refused is skipped, so stop here.
				halt()
			case f5nas.MsgTypeAuthenticationReject:
				o.authReject = true
				halt()
			}
		},
	})
	mu.Lock()
	defer mu.Unlock()
	switch {
	case err == nil:
		o.registered = true
	case o.updateErr != nil, o.sawReject, o.authReject:
	case errors.Is(err, engine.ErrReleasedByCore):
		o.released = true
	case o.update != nil && timedOut && ctx.Err() == nil:
		o.noAnswer = true
	default:
		o.err = err
	}
	return o
}

// judgeKeepsTA maps an outcome onto a verdict for a UE registering in g's
// tracking area; updateWait and registerWait are the bounds the absences
// were inferred within.
func judgeKeepsTA(o keepsTAOutcome, g gnb.Config, updateWait, registerWait time.Duration) (v Verdict, observed, detail string) {
	switch {
	case o.update == nil && o.updateTimedOut:
		return Error, "no response to the RAN CONFIGURATION UPDATE",
			fmt.Sprintf("no RAN CONFIGURATION UPDATE ACKNOWLEDGE or FAILURE within %s of the update (absence inferred within this bound), so the state after it was not judged and no registration was attempted; "+
				"whether a class 1 NGAP procedure may go unanswered is not settled, so the missing response itself is not scored", updateWait)
	case o.update == nil && o.updateCutShort:
		return Error, "per-test timeout expired during the RAN CONFIGURATION UPDATE",
			fmt.Sprintf("the check's time ran out %s after the update, before its %s wait for an outcome ended; raise --per-test-timeout", o.updateAfter.Round(time.Millisecond), updateWait)
	case o.update == nil && o.updateErr != nil:
		return Error, "association lost during the RAN CONFIGURATION UPDATE",
			fmt.Sprintf("the association ended %s after the update without an outcome (%v); no registration was attempted", o.updateAfter.Round(time.Millisecond), o.updateErr)
	case o.update == nil:
		return Error, "setup failed", fmt.Sprintf("the association did not reach the RAN CONFIGURATION UPDATE: %v", o.err)
	}

	ta := fmt.Sprintf("TAC %d of PLMN %s/%s", g.TAC, g.MCC, g.MNC)
	reg, regDetail := keepsTARegistration(o, registerWait)
	if !o.update.Acknowledged {
		return Skip, fmt.Sprintf("RAN CONFIGURATION UPDATE FAILURE (cause %s); %s", o.update.Cause, reg),
			joinDetail("the AMF did not accept the update, which TS 38.413 8.7.2.3 allows; 8.7.2.2 states the unchanged-configuration rule for an acknowledged update, so the state was not judged", regDetail)
	}
	answered := fmt.Sprintf("RAN CONFIGURATION UPDATE ACKNOWLEDGE after %s", o.updateAfter.Round(time.Millisecond))
	if o.update.Skipped > 0 {
		answered += fmt.Sprintf(" (%d PDU(s) of other procedures before it)", o.update.Skipped)
	}
	switch {
	case o.registered:
		return Pass, "registration succeeded in " + ta + " after the ACKNOWLEDGE", answered
	case o.sawReject && o.rejectErr == nil && taCauses[o.rejectCause] != "":
		return Fail, fmt.Sprintf("%s in %s after the ACKNOWLEDGE", reg, ta), joinDetail(answered, keepsTAFailText)
	case o.sawReject && o.rejectErr == nil:
		return Error, reg + " after the ACKNOWLEDGE",
			joinDetail(answered, fmt.Sprintf("5GMM cause #%d is not tied to a tracking area (TS 24.501 Annex A; only #12, #13 and #15 are), so the Reject does not show the AMF stopped serving %s", o.rejectCause, ta))
	default:
		return Error, reg + " after the ACKNOWLEDGE", joinDetail(answered, regDetail)
	}
}

// keepsTARegistration renders the registration's result for Observed, with
// a detail for the results that have one.
func keepsTARegistration(o keepsTAOutcome, registerWait time.Duration) (result, detail string) {
	switch {
	case o.registered:
		return "registration succeeded", ""
	case o.sawReject && o.rejectErr != nil:
		return "Registration Reject that could not be read", o.rejectErr.Error()
	case o.sawReject:
		return "Registration Reject with 5GMM cause " + causeLabel(o.rejectCause), ""
	case o.authReject:
		return "Authentication Reject", "the registration ended at an Authentication Reject, before the tracking area could matter"
	case o.released:
		return "UE context released during the registration", "the core sent a UE CONTEXT RELEASE COMMAND before a Registration Accept or Reject"
	case o.noAnswer:
		return "registration unanswered", fmt.Sprintf("no Registration Accept or Reject within %s (absence inferred within this bound)", registerWait)
	default:
		return "registration failed", fmt.Sprint(o.err)
	}
}

// causeLabel renders a 5GMM cause as "#12 (Tracking area not allowed)",
// with the name when it is one the check knows.
func causeLabel(cause uint8) string {
	name := taCauses[cause]
	if name == "" {
		name = otherCauseNames[cause]
	}
	if name == "" {
		return fmt.Sprintf("#%d", cause)
	}
	return fmt.Sprintf("#%d (%s)", cause, name)
}
