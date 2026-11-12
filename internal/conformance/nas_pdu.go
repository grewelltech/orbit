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
	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
)

func init() {
	builtins = append(builtins, nasPDUUnknownDNN{}, nasPDUReleaseUERequested{})
}

// stimulusDNN is the DNN the check requests: one no core provisions.
const stimulusDNN = "orbit-unknown-dnn"

const (
	// dnnRegisterWait bounds the registration, up to the PDU Session
	// Establishment Request.
	dnnRegisterWait = 10 * time.Second
	// dnnAnswerWait bounds the wait for the answer to the request.
	dnnAnswerWait = 10 * time.Second
	// dnnDeregisterWait bounds the deregistration that ends the check.
	dnnDeregisterWait = 5 * time.Second
)

// nasPDUUnknownDNN registers the UE and requests a PDU session for
// stimulusDNN on the run's slice. Either the SMF rejects it (TS 24.501
// 6.4.1.4.1: a PDU SESSION ESTABLISHMENT REJECT, whose 5GSM cause is
// "typically" #27, so any cause passes) or SMF selection fails at the AMF,
// which returns the un-forwarded request in a DL NAS TRANSPORT with 5GMM
// cause #90 or #91 (5.4.5.2.5 a) 1), 5.4.5.3.2 for case e) of 5.4.5.3.1),
// or, from V16.3.0, with a congestion cause (case f)). A #90/#91 answer
// whose payload is not the request, the request returned without a 5GMM
// cause, another decoded answer, a release, or nothing within
// dnnAnswerWait is FAIL. A core that accepts the DNN is INFO: no shall
// forbids it. A congestion send-back is SKIP, the alternative 5.4.5.2.5 a)
// 1) permits, and the request returned with any other 5GMM cause is INFO.
// An answer whose MAC fails is WARN for interoperability; a downlink or
// 5GSM message that does not decode is ERROR. A registration that does not
// reach the request is BLOCKED on it. The answer is read through the UE's
// NAS security context. Whenever the registration completed, the UE is
// deregistered at the end.
type nasPDUUnknownDNN struct {
	// registerWait and answerWait override dnnRegisterWait and
	// dnnAnswerWait (tests only).
	registerWait, answerWait time.Duration
}

// The keys of the references the outcomes of NAS-PDU-UNKNOWN-DNN rest on.
const (
	dnnSendBackRef = "TS 24.501 5.4.5.2.5 a) 1)"
	dnnCauseRef    = "TS 24.501 5.4.5.3.2 (case e) of 5.4.5.3.1)"
	dnnRejectRef   = "TS 24.501 6.4.1.4.1 (6.4.1.4 in V15.1.0 to V15.7.0)"
)

// dnnAnyAnswer are the shalls an answer to the request meets one of: the
// SMF's reject, or the AMF's send-back with #90 or #91.
var dnnAnyAnswer = []string{dnnSendBackRef, dnnCauseRef, dnnRejectRef}

// The outcomes of NAS-PDU-UNKNOWN-DNN.
var (
	dnnRejected = Outcome{ID: "rejected", Verdict: Pass, Refs: []string{dnnRejectRef},
		Summary: "rejected the unknown DNN with a PDU SESSION ESTABLISHMENT REJECT"}
	dnnReturned = Outcome{ID: "returned", Verdict: Pass, Refs: []string{dnnSendBackRef, dnnCauseRef},
		Summary: "returned the un-forwarded request with 5GMM cause #90 or #91"}
	dnnWrongPayload = Outcome{ID: "wrong-payload", Verdict: Fail, Refs: []string{dnnSendBackRef, dnnCauseRef},
		Summary: "sent 5GMM cause #90 or #91 with a payload that is not the request"}
	dnnReturnedNoCause = Outcome{ID: "returned-without-cause", Verdict: Fail, Refs: []string{dnnCauseRef},
		Summary: "returned the request without a 5GMM cause"}
	dnnUnexpected = Outcome{ID: "unexpected-answer", Verdict: Fail, Refs: dnnAnyAnswer,
		Summary: "answered with neither a reject nor the returned request"}
	dnnStatusOnly = Outcome{ID: "5gmm-status-only", Verdict: Fail, Refs: dnnAnyAnswer,
		Bound:   &Bound{Wait: dnnAnswerWait, Source: "check bound; T3580, which the UE would retransmit on, is longer"},
		Summary: "sent a 5GMM STATUS and neither a reject nor the returned request"}
	dnnNoAnswer = Outcome{ID: "no-answer", Verdict: Fail, Refs: dnnAnyAnswer,
		Bound:   &Bound{Wait: dnnAnswerWait, Source: "check bound; T3580, which the UE would retransmit on, is longer"},
		Summary: "sent neither a reject nor the returned request"}
	dnnReleased = Outcome{ID: "released", Verdict: Fail, Refs: dnnAnyAnswer,
		Summary: "released the UE instead of rejecting or returning the request"}
	dnnCongestion = Outcome{ID: "congestion", Verdict: Skip, Skip: SkipPermitted, Refs: []string{dnnSendBackRef},
		Summary: "returned the request with a congestion cause (case f) of 5.4.5.3.1), which 5.4.5.2.5 a) 1) permits from V16.3.0"}
	// Accepting the DNN is INFO: no shall forbids a wildcard DNN
	// subscription or a core without a DNN check, and none of the three
	// criteria applies.
	dnnAccepted = Outcome{ID: "accepted", Verdict: Info,
		Summary: "accepted a PDU session for the unknown DNN"}
	// A send-back with a 5GMM cause 5.4.5.3.2 does not assign to a routing
	// failure is INFO: the AMF may return the request for another case of
	// 5.4.5.3.1, so the DNN is not judged, and no criterion applies.
	dnnReturnedOtherCause = Outcome{ID: "returned-other-cause", Verdict: Info,
		Summary: "returned the request with a 5GMM cause that is neither a routing nor a congestion cause"}
	// An answer whose MAC fails is WARN for interoperability: the answer
	// cannot be judged, and a conformant UE discards it.
	dnnIntegrityFailed = Outcome{ID: "integrity-failed", Verdict: Warn, Criterion: CriterionInteroperability,
		Summary: "sent a downlink NAS message after the request that fails the integrity check"}
)

// Outcomes declares what the check can report.
func (nasPDUUnknownDNN) Outcomes() []Outcome {
	return []Outcome{dnnRejected, dnnReturned, dnnWrongPayload, dnnReturnedNoCause, dnnUnexpected, dnnStatusOnly, dnnNoAnswer,
		dnnReleased, dnnCongestion, dnnAccepted, dnnReturnedOtherCause, dnnIntegrityFailed}
}

func (nasPDUUnknownDNN) ID() string { return "NAS-PDU-UNKNOWN-DNN" }
func (nasPDUUnknownDNN) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.4.5.2.5 a) 1)", Strength: Shall, Since: 15, Verified: ts24501UnknownDNNVerified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 is a may (item a)); V15.1.0 to V15.2.1 shall only for DNN not supported in the slice; from V15.3.0 shall on any SMF selection failure"},
				{Release: 16, Note: "from V16.3.0 the send-back may be case e) or case f); from V16.15.0 the request type may also be \"MA PDU request\""},
				{Release: 18, Note: "\"subclause\" becomes \"clause\""},
			}},
		{Spec: "TS 24.501", Clause: "5.4.5.3.2 (case e) of 5.4.5.3.1)", Strength: Shall, Since: 15, Verified: ts24501UnknownDNNVerified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 has #90 only; V15.1.0 adds #91 as \"DNN not supported with no slice or DNN not supported in specific slice\"; V15.2.0 renames it \"DNN not supported or not subscribed in the slice\" and adds \"Otherwise ... #90\""},
				{Release: 17, Note: "V17.3.0 adds an optional Back-off timer value IE with #91"},
			}},
		{Spec: "TS 24.501", Clause: "6.4.1.4.1 (6.4.1.4 in V15.1.0 to V15.7.0)", Strength: Shall, Since: 15, Verified: ts24501UnknownDNNVerified, Primary: true,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.1.0 to V15.7.0 drop the sub-clause, so the text sits in 6.4.1.4; V15.x reads \"a SM PDU SESSION ESTABLISHMENT REJECT\"; V15.1.0 adds #46 and #82 and V15.2.0 adds #8, #32, #33, #38 and #95 to #111 to the typical causes"},
				{Release: 16, Note: "6.4.1.4.1 again from V16.0.0, which drops \"SM\"; V16.1.0 adds #39 and V16.3.0 adds #57, #58 and #61 to the typical causes; the two shalls and #27 in the list are unchanged in every version"},
				{Release: 17, Note: "V17.5.0 adds #86 to the typical causes"},
			}},
		{Spec: "TS 24.501", Clause: "Annex B.1 #27", Strength: Descriptive, Since: 15, Verified: ts24501UnknownDNNVerified, WordingUnchanged: true},
	}
}

func (c nasPDUUnknownDNN) registerTimeout() time.Duration {
	if c.registerWait > 0 {
		return c.registerWait
	}
	return dnnRegisterWait
}

func (c nasPDUUnknownDNN) answerTimeout() time.Duration {
	if c.answerWait > 0 {
		return c.answerWait
	}
	return dnnAnswerWait
}

// Budget covers the registration, the answer, the deregistration and the
// liveness probe.
func (c nasPDUUnknownDNN) Budget() time.Duration {
	return c.registerTimeout() + c.answerTimeout() + dnnDeregisterWait + 5*time.Second
}

func (c nasPDUUnknownDNN) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "a PDU Session Establishment Reject (any 5GSM cause; #27 is typical), or the un-forwarded request returned in a DL NAS Transport with 5GMM cause #90 or #91 (TS 24.501 6.4.1.4.1, 5.4.5.2.5 a) 1), 5.4.5.3.2 (case e) of 5.4.5.3.1)); the send-back is required from V15.1.0 (a may in V15.0.0)"}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run NAS checks")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()

	o := c.probe(ctx, h)
	if o.setupErr != nil {
		r = r.SetupFailed("UE registration failed", o.setupErr)
	} else {
		out, observed, detail := judgeDNN(o, c.answerTimeout())
		r = asClassified(r, out, observed, detail)
	}
	if o.registered {
		dctx, cancel := context.WithTimeout(context.Background(), dnnDeregisterWait)
		err := h.Deregister(dctx)
		cancel()
		if err != nil {
			r.Detail = joinDetail(r.Detail, fmt.Sprintf("deregistration failed (%v); the association was dropped", err))
		} else {
			r.Detail = joinDetail(r.Detail, "UE deregistered")
		}
	} else if o.sent != nil {
		r.Detail = joinDetail(r.Detail, "the attach failed after the request, so its association closed without a deregistration")
	}
	if r.Verdict == Error && ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the unknown-DNN request")
}

// dnnOutcome is what the probing registration saw.
type dnnOutcome struct {
	sent       []byte  // the 5GSM request as sent, nil if never sent
	answer     *dlN1SM // the first N1 SM DL NAS Transport after it
	accepted   bool    // the session was established
	noAnswer   bool    // nothing within the answer wait
	mmStatus   bool    // a 5GMM STATUS came after the request
	mmStatusC  uint8   // its 5GMM cause
	decodeErr  error   // a downlink after the request that did not decode
	decodeRaw  []byte  // its NAS PDU
	decodeType uint8   // its 5GMM message type (NASEvent.MessageType), 0 if unreadable
	registered bool    // the attach returned a REGISTERED UE
	// acceptedNAS is a PDU SESSION ESTABLISHMENT ACCEPT that did not stop
	// the attach (probeSession's haltOnAccept false); noUserPlane is such
	// an Accept with no session set up on N2 within the answer wait.
	acceptedNAS, noUserPlane bool
	// regTimedOut is a registration that did not reach the request within
	// its wait; setupErr an attach that failed before the request.
	regTimedOut bool
	setupErr    error
	released    bool  // the core released the UE after the request
	err         error // the harness failed, or the check's time ran out
}

// probe registers the UE with a PDU session for stimulusDNN on the
// harness's slice and stops the attach at the first N1 SM DL NAS Transport
// after the request, at a downlink that does not decode, at an established
// session, or when a wait runs out.
func (c nasPDUUnknownDNN) probe(ctx context.Context, h *UEHarness) dnnOutcome {
	pdu := h.pdu
	pdu.DNN = stimulusDNN
	return probeSession(ctx, h, pdu, c.registerTimeout(), c.answerTimeout(), true)
}

// probeSession registers h's UE with PDU session pdu and stops the attach
// at the first N1 SM DL NAS Transport after the request, at a downlink that
// does not decode, at an established session, or when a wait runs out.
// With haltOnAccept false a PDU SESSION ESTABLISHMENT ACCEPT does not stop
// it: the session's setup on N2 is awaited within answerWait of the
// request, and noUserPlane records one that did not come.
func probeSession(ctx context.Context, h *UEHarness, pdu ue.PDUSessionParams, registerWait, answerWait time.Duration, haltOnAccept bool) dnnOutcome {
	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	defer halt()
	var mu sync.Mutex
	var o dnnOutcome
	var regTimedOut bool
	var answerTimer *time.Timer
	regTimer := time.AfterFunc(registerWait, func() {
		mu.Lock()
		defer mu.Unlock()
		if o.sent == nil {
			regTimedOut = true
			halt()
		}
	})
	defer regTimer.Stop()
	decided := func() bool { return o.answer != nil || o.noAnswer || o.noUserPlane || o.decodeErr != nil }
	probe := &engine.AttachProbe{
		Stop: stop,
		OnSMRequest: func(sm []byte) {
			mu.Lock()
			defer mu.Unlock()
			if o.sent != nil || regTimedOut {
				return
			}
			o.sent = sm
			regTimer.Stop()
			answerTimer = time.AfterFunc(answerWait, func() {
				mu.Lock()
				defer mu.Unlock()
				if !decided() {
					o.noAnswer, o.noUserPlane = !o.acceptedNAS, o.acceptedNAS
					halt()
				}
			})
		},
		OnNAS: func(ev engine.NASEvent) {
			mu.Lock()
			defer mu.Unlock()
			if o.sent == nil && ev.Err != nil && o.err == nil {
				o.err = fmt.Errorf("a downlink NAS PDU before the PDU Session Establishment Request did not decode: %w", ev.Err)
				halt()
				return
			}
			if o.sent == nil || decided() {
				return
			}
			if ev.Err != nil {
				o.decodeErr, o.decodeRaw, o.decodeType = ev.Err, ev.Raw, ev.MessageType()
				halt()
				return
			}
			if d, ok := dlN1SMFrom(ev.Msg); ok {
				if !haltOnAccept && readSM(d.payload).kind == smAccept {
					o.acceptedNAS = true
					return
				}
				o.answer = &d
				halt()
			} else if ev.Msg != nil && ev.Msg.GmmMessage != nil && ev.Msg.Status5GMM != nil && !o.mmStatus {
				// Not an answer the check can judge: the wait goes on, and
				// the status is reported if nothing else comes.
				o.mmStatus, o.mmStatusC = true, ev.Msg.Status5GMM.GetCauseValue()
			}
		},
	}
	res, err := h.RegisterWithProbe(ctx, probe, func(cfg *engine.UEConfig) {
		cfg.PDUSession = &pdu
		cfg.GNBN3Addr = h.env.UE.GNBN3
	})
	mu.Lock()
	defer mu.Unlock()
	if answerTimer != nil {
		answerTimer.Stop()
	}
	o.registered = err == nil
	switch {
	case o.err != nil:
	case o.decodeErr != nil:
	case err == nil && res != nil && res.SessionActive:
		o.accepted = true
	case err == nil && (o.answer != nil || o.noAnswer || o.noUserPlane):
	case err == nil:
		o.err = errors.New("registration ended without the PDU session or an answer to it")
	case regTimedOut:
		o.regTimedOut = true
	case o.sent == nil && !errors.Is(err, engine.ErrAfterRegistration):
		o.setupErr = err
	case errors.Is(err, engine.ErrReleasedByCore):
		o.released = true
	case ctx.Err() != nil:
		o.err = fmt.Errorf("the per-test timeout expired before the answer (%w); raise --per-test-timeout", err)
	default:
		o.err = err
	}
	return o
}

// dlN1SM is the part of a DL NAS Transport the check judges.
type dlN1SM struct {
	payload []byte
	mmCause uint8 // 0 when the 5GMM cause IE is absent
	backoff bool  // a Back-off timer value IE is present
}

// dlN1SMFrom reads a decoded downlink 5GMM message: ok when it is a DL NAS
// Transport carrying N1 SM information.
func dlN1SMFrom(m *f5nas.Message) (dlN1SM, bool) {
	if m == nil || m.GmmMessage == nil || m.DLNASTransport == nil {
		return dlN1SM{}, false
	}
	t := m.DLNASTransport
	if t.GetPayloadContainerType() != nasMessage.PayloadContainerTypeN1SMInfo {
		return dlN1SM{}, false
	}
	n := int(t.PayloadContainer.GetLen())
	if n > len(t.PayloadContainer.Buffer) {
		n = len(t.PayloadContainer.Buffer)
	}
	d := dlN1SM{payload: append([]byte(nil), t.PayloadContainer.Buffer[:n]...)}
	if t.Cause5GMM != nil {
		d.mmCause = t.GetCauseValue()
	}
	d.backoff = t.BackoffTimerValue != nil
	return d, true
}

// smKind classifies the 5GSM message in a payload container.
type smKind int

const (
	smOther smKind = iota
	smReject
	smAccept
	smRequest
	smStatus
	smNot5GSM
	smUndecodable // a Reject or STATUS that did not decode
)

// smInfo is what the check reads from a payload container.
type smInfo struct {
	kind      smKind
	name      string
	cause     uint8 // the 5GSM cause of a Reject or STATUS
	decodeErr error
}

// readSM names the 5GSM message in a payload and reads the cause of a
// Reject or a STATUS.
func readSM(sm []byte) smInfo {
	if len(sm) < 4 || sm[0] != nasMessage.Epd5GSSessionManagementMessage {
		return smInfo{kind: smNot5GSM, name: "no 5GSM message"}
	}
	decode := func() (*f5nas.Message, error) {
		m := f5nas.NewMessage()
		b := append([]byte(nil), sm...)
		return m, m.PlainNasDecode(&b)
	}
	switch sm[3] {
	case f5nas.MsgTypePDUSessionEstablishmentReject:
		m, err := decode()
		if err == nil && m.PDUSessionEstablishmentReject == nil {
			err = errors.New("no Reject in the decoded message")
		}
		if err != nil {
			return smInfo{kind: smUndecodable, name: "PDU Session Establishment Reject", decodeErr: err}
		}
		c := m.PDUSessionEstablishmentReject.GetCauseValue()
		return smInfo{kind: smReject, name: "PDU Session Establishment Reject, 5GSM cause " + gsmCause(c), cause: c}
	case f5nas.MsgTypeStatus5GSM:
		m, err := decode()
		if err == nil && m.Status5GSM == nil {
			err = errors.New("no 5GSM STATUS in the decoded message")
		}
		if err != nil {
			return smInfo{kind: smUndecodable, name: "5GSM STATUS", decodeErr: err}
		}
		c := m.Status5GSM.GetCauseValue()
		return smInfo{kind: smStatus, name: "5GSM STATUS, 5GSM cause " + gsmCause(c), cause: c}
	case f5nas.MsgTypePDUSessionEstablishmentAccept:
		return smInfo{kind: smAccept, name: "PDU Session Establishment Accept"}
	case f5nas.MsgTypePDUSessionEstablishmentRequest:
		return smInfo{kind: smRequest, name: "PDU Session Establishment Request"}
	}
	return smInfo{kind: smOther, name: fmt.Sprintf("5GSM message type %#x", sm[3])}
}

// gsmCause renders a 5GSM cause, naming #27.
func gsmCause(c uint8) string {
	if c == nasMessage.Cause5GSMMissingOrUnknownDNN {
		return "#27 missing or unknown DNN"
	}
	return fmt.Sprintf("#%d", c)
}

// mmCause renders a 5GMM cause, naming #90 and #91.
func mmCause(c uint8) string {
	switch c {
	case nasMessage.Cause5GMMPayloadWasNotForwarded:
		return "#90 payload was not forwarded"
	case nasMessage.Cause5GMMDNNNotSupportedOrNotSubscribedInTheSlice:
		return "#91 DNN not supported or not subscribed in the slice"
	}
	return fmt.Sprintf("#%d", c)
}

// isRoutingCause reports whether c is 5GMM cause #90 or #91, the causes
// 5.4.5.3.2 sets for case e) of 5.4.5.3.1.
func isRoutingCause(c uint8) bool {
	return c == nasMessage.Cause5GMMPayloadWasNotForwarded || c == nasMessage.Cause5GMMDNNNotSupportedOrNotSubscribedInTheSlice
}

// isCongestionCause reports whether c is a 5GMM cause of case f) of 5.4.5.3.1:
// #22 congestion, #67 insufficient resources for specific slice and DNN,
// #69 insufficient resources for specific slice.
func isCongestionCause(c uint8) bool { return c == 22 || c == 67 || c == 69 }

// pointsAtStimulus reports whether a 5GSM cause says the request itself was
// faulty: #43 invalid PDU session identity, or a protocol error, #95 to
// #101 and #111.
func pointsAtStimulus(c uint8) bool { return c == 43 || (c >= 95 && c <= 101) || c == 111 }

// indistinguishable is the known limit every PASS (A) states.
const indistinguishable = "an AMF-built PDU Session Establishment Reject without a 5GMM cause looks the same on the wire as an SMF's, so this PASS does not tell which network function rejected"

// codecGap is the note on an answer Orbit could not decode.
const codecGap = "Orbit's NAS codec covers about Rel-15/16, so this may be a gap in Orbit, not the core"

// unknownDNNFail is the FAIL message; got is what came back.
func unknownDNNFail(got, likely string) string {
	return fmt.Sprintf("Unknown DNN: neither a PDU Session Establishment Reject nor the un-forwarded 5GSM message with 5GMM cause #90/#91 came back (got %s). TS 24.501 6.4.1.4.1, 5.4.5.2.5 a) 1), 5.4.5.3.2 (case e) of 5.4.5.3.1). Likely cause: %s.", got, likely)
}

// typeNote names a downlink's 5GMM message type for an Observed line, ""
// when the header was unreadable.
func typeNote(t uint8) string {
	switch t {
	case 0:
		return ""
	case f5nas.MsgTypeDLNASTransport:
		return " (DL NAS Transport)"
	}
	return fmt.Sprintf(" (5GMM message type %#x)", t)
}

const (
	acceptedObserved   = "core accepted the stimulus DNN; no rejection to check"
	congestionObserved = "AMF applied congestion control (case f)); the unknown-DNN path was not exercised"
)

// judgeDNN turns the outcome into an outcome, Observed and Detail. An
// attach that failed before the request (setupErr) is the caller's to
// report.
func judgeDNN(o dnnOutcome, wait time.Duration) (Outcome, string, string) {
	switch {
	case o.err != nil:
		return classifiedError, "registration or harness failure", o.err.Error()
	case o.regTimedOut:
		return classifiedBlocked(ProcRegistration), "registration did not reach the PDU Session Establishment Request",
			fmt.Sprintf("the UE did not register within %s, so the request was not sent", dnnRegisterWait)
	case o.released:
		return dnnReleased, "UE released after the request, with no answer",
			unknownDNNFail("a UE Context Release Command and nothing else", "the AMF releases the UE when no SMF is selected")
	case o.decodeErr != nil && errors.Is(o.decodeErr, nas.ErrIntegrity):
		return dnnIntegrityFailed, "downlink NAS after the request failed integrity check" + typeNote(o.decodeType),
			fmt.Sprintf("%v; NAS %x; a UE discards a message whose MAC fails, so the answer cannot be judged", o.decodeErr, o.decodeRaw)
	case o.decodeErr != nil:
		return classifiedError, "downlink NAS after the request did not decode" + typeNote(o.decodeType), fmt.Sprintf("%v; %s; NAS %x", o.decodeErr, codecGap, o.decodeRaw)
	case o.accepted:
		return dnnAccepted, acceptedObserved,
			fmt.Sprintf("the core established a PDU session for DNN %q (a wildcard DNN subscription, or no DNN check); no shall forbids that", stimulusDNN)
	case o.noAnswer && o.mmStatus:
		got := "a 5GMM STATUS with 5GMM cause " + mmCause(o.mmStatusC)
		return dnnStatusOnly, fmt.Sprintf("5GMM STATUS, 5GMM cause %s, and nothing else within %s", mmCause(o.mmStatusC), wait),
			unknownDNNFail(got+" and nothing else within "+wait.String(), "the AMF reports the failure in a 5GMM STATUS instead of sending back the 5GSM message")
	case o.noAnswer:
		return dnnNoAnswer, fmt.Sprintf("nothing back within %s", wait),
			unknownDNNFail("nothing within "+wait.String(), "the request is dropped when no SMF is selected, or the SMF does not answer")
	}
	a := o.answer
	sm := readSM(a.payload)
	cites := "5GMM cause absent"
	if a.mmCause != 0 {
		cites = "5GMM cause " + mmCause(a.mmCause)
	}
	returned := bytes.Equal(a.payload, o.sent)
	switch {
	case isRoutingCause(a.mmCause) && returned:
		return dnnReturned, fmt.Sprintf("un-forwarded request returned, 5GMM cause %s", mmCause(a.mmCause)),
			fmt.Sprintf("path (B): the AMF returned the PDU Session Establishment Request byte for byte in a DL NAS Transport with %s (which of #90 and #91 applies is recorded, not asserted)", cites)
	case isRoutingCause(a.mmCause):
		likely := "AMF returns a payload other than the un-forwarded 5GSM message"
		if sm.kind == smReject {
			likely = "AMF returns its own Reject in the payload container instead of the un-forwarded message"
		}
		return dnnWrongPayload, fmt.Sprintf("5GMM cause %s with a payload that is not the request", mmCause(a.mmCause)),
			unknownDNNFail(fmt.Sprintf("a DL NAS Transport with %s carrying %s; sent %x, returned %x", cites, sm.name, o.sent, a.payload), likely)
	case sm.kind == smUndecodable:
		return classifiedError, "answer did not decode", fmt.Sprintf("a DL NAS Transport with %s carrying a %s that did not decode: %v; %s; payload %x", cites, sm.name, sm.decodeErr, codecGap, a.payload)
	case sm.kind == smReject:
		detail := fmt.Sprintf("path (A): %s, %s", sm.name, cites)
		if sm.cause != nasMessage.Cause5GSMMissingOrUnknownDNN {
			detail += "; the cause is not #27, which 6.4.1.4.1 names as typical (any cause is conformant)"
		}
		return dnnRejected, sm.name, detail + "; " + indistinguishable
	case sm.kind == smAccept:
		return dnnAccepted, acceptedObserved,
			fmt.Sprintf("a PDU Session Establishment Accept for DNN %q came in a plain DL NAS Transport (%s) without a PDU Session Resource Setup Request; no shall forbids accepting the DNN", stimulusDNN, cites)
	case a.mmCause == 0 && returned:
		return dnnReturnedNoCause, "request returned without a 5GMM cause",
			unknownDNNFail(fmt.Sprintf("the PDU Session Establishment Request returned in a DL NAS Transport with %s", cites), "AMF returned the request without the 5GMM cause 5.4.5.3.2 requires")
	case isCongestionCause(a.mmCause) && returned:
		backoff := "no Back-off timer value IE"
		if a.backoff {
			backoff = "a Back-off timer value IE"
		}
		return dnnCongestion, congestionObserved,
			fmt.Sprintf("the AMF returned the request with %s and %s; 5.4.5.2.5 a) 1) lists this case f) of 5.4.5.3.1 send-back from V16.3.0, so it is conformant, but the DNN was not judged: re-run when the core is not congested", cites, backoff)
	case a.mmCause != 0 && returned:
		return dnnReturnedOtherCause, fmt.Sprintf("request returned with 5GMM cause %s", mmCause(a.mmCause)),
			fmt.Sprintf("the AMF returned the request with %s, not a cause 5.4.5.3.2 assigns to a routing failure; this check does not judge the DNN then", cites)
	}
	got := fmt.Sprintf("a DL NAS Transport with %s carrying %s", cites, sm.name)
	if sm.kind == smStatus && pointsAtStimulus(sm.cause) {
		got += "; that cause points at a malformed request: check the stimulus"
	}
	return dnnUnexpected, "unexpected answer: " + sm.name, unknownDNNFail(got, "an answer other than a reject or the returned request")
}

const (
	// pduReleaseRegisterWait bounds the registration and PDU session
	// establishment that precede the release.
	pduReleaseRegisterWait = 10 * time.Second
	// pduReleaseWait bounds the wait for the network's answer to the PDU
	// SESSION RELEASE REQUEST: 16 s, the default of T3582 (TS 24.501 Table
	// 10.3.1), on whose expiry a UE retransmits the request.
	pduReleaseWait = engine.DefaultPDUReleaseWait
	// pduReleaseSettle bounds the wait for the second of the NAS and NGAP
	// release commands after the first.
	pduReleaseSettle = engine.DefaultPDUReleaseSettle
	// pduReleaseGrace is how long after the release downlink already in
	// flight may still arrive on the released tunnel without counting.
	pduReleaseGrace = time.Second
	// pduReleaseWatch is how long past the grace the released tunnel is
	// watched for downlink.
	pduReleaseWatch = 3 * time.Second
	// pduReleaseTrafficLead lets the optional echo flow put downlink on the
	// tunnel before the release.
	pduReleaseTrafficLead = time.Second
	// pduReleaseDeregisterWait bounds the deregistration that ends the
	// check.
	pduReleaseDeregisterWait = 5 * time.Second
)

// nasPDUReleaseUERequested registers the UE with a PDU session, starts the
// optional echo flow, and sends a PDU SESSION RELEASE REQUEST. The SMF
// shall then perform the network-requested release (TS 24.501 6.4.3.3),
// which starts with a PDU SESSION RELEASE COMMAND (6.3.3.2), read through
// the UE's NAS security context whether it comes inside the NGAP command or
// in a DOWNLINK NAS TRANSPORT. With the user plane active, the SMF's answer
// shall include the N2 release (TS 23.502 4.3.4.2 step 3), seen as a PDU
// SESSION RESOURCE RELEASE COMMAND listing the session (TS 38.413 8.2.2.2),
// and the UPF shall drop the session's packets (TS 23.502 4.3.4.2 step 2a),
// so no downlink G-PDU may arrive on the released tunnel once
// pduReleaseGrace has passed. Both commands and a quiet tunnel are PASS; a
// missing command, or downlink after the grace, is FAIL. A PDU SESSION
// RELEASE REJECT is SKIP, the alternative 6.4.3.4 permits. A registration
// or PDU session that does not complete is BLOCKED. The echo flow keeps sending
// uplink on the released tunnel, which is what would draw downlink from a
// UPF that kept the session; without it the tunnel is only watched. The UE
// is deregistered at the end.
type nasPDUReleaseUERequested struct {
	// registerWait, wait, settle, grace and watch override the constants
	// above (tests only).
	registerWait, wait, settle, grace, watch time.Duration
}

// The keys of the references the outcomes of NAS-PDU-RELEASE-UE-REQUESTED
// rest on.
const (
	pduReleaseAcceptedRef = "TS 24.501 6.4.3.3"
	pduReleaseCommandRef  = "TS 24.501 6.3.3.2"
	pduReleaseUPFRef      = "TS 23.502 4.3.4.2 step 2a"
	pduReleaseN2Ref       = "TS 23.502 4.3.4.2 step 3"
	pduReleaseRejectRef   = "TS 24.501 6.4.3.4"
)

// The outcomes of NAS-PDU-RELEASE-UE-REQUESTED.
var (
	pduReleaseReleased = Outcome{ID: "released", Verdict: Pass, Refs: []string{pduReleaseAcceptedRef, pduReleaseCommandRef, pduReleaseN2Ref, pduReleaseUPFRef},
		Summary: "released the PDU session on N1 and N2, and the released tunnel stayed quiet"}
	pduReleaseNoAnswer = Outcome{ID: "no-answer", Verdict: Fail, Refs: []string{pduReleaseAcceptedRef, pduReleaseCommandRef},
		Bound:   &Bound{Wait: pduReleaseWait, Source: "T3582, TS 24.501 Table 10.3.1"},
		Summary: "sent neither a PDU SESSION RELEASE COMMAND nor a PDU SESSION RESOURCE RELEASE COMMAND"}
	pduReleaseNoNASCommand = Outcome{ID: "no-nas-command", Verdict: Fail, Refs: []string{pduReleaseAcceptedRef, pduReleaseCommandRef},
		Bound:   &Bound{Wait: pduReleaseSettle, Source: "check bound after the NGAP command"},
		Summary: "released the session on N2 without a PDU SESSION RELEASE COMMAND"}
	pduReleaseNoN2Release = Outcome{ID: "no-n2-release", Verdict: Fail, Refs: []string{pduReleaseN2Ref},
		Bound:   &Bound{Wait: pduReleaseSettle, Source: "check bound after the NAS command"},
		Summary: "sent the PDU SESSION RELEASE COMMAND without a PDU SESSION RESOURCE RELEASE COMMAND for the active user plane"}
	pduReleaseDownlinkAfter = Outcome{ID: "downlink-after-release", Verdict: Fail, Refs: []string{pduReleaseUPFRef},
		Summary: "kept sending downlink on the released tunnel after the release"}
	pduReleaseAssociationLost = Outcome{ID: "association-lost", Verdict: Fail, Refs: []string{pduReleaseAcceptedRef, pduReleaseCommandRef},
		Summary: "dropped the UE's association instead of completing the release"}
	pduReleaseRejected = Outcome{ID: "rejected", Verdict: Skip, Skip: SkipPermitted, Refs: []string{pduReleaseRejectRef},
		Summary: "rejected the release with a PDU SESSION RELEASE REJECT, which 6.4.3.4 permits"}
)

// Outcomes declares what the check can report.
func (nasPDUReleaseUERequested) Outcomes() []Outcome {
	return []Outcome{pduReleaseReleased, pduReleaseNoAnswer, pduReleaseNoNASCommand, pduReleaseNoN2Release, pduReleaseDownlinkAfter, pduReleaseAssociationLost, pduReleaseRejected}
}

func (nasPDUReleaseUERequested) ID() string { return "NAS-PDU-RELEASE-UE-REQUESTED" }
func (nasPDUReleaseUERequested) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "6.4.3.3", Strength: Shall, Since: 15, Verified: ts24501Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 18, Note: "\"subclause 6.3.3\" becomes \"clause 6.3.3\"; the sentence is otherwise unchanged from V15.0.0 through V19.8.0"},
			}},
		{Spec: "TS 24.501", Clause: "6.3.3.2", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 names the \"SM cause IE\", \"5GSM cause IE\" by V15.7.0; \"the SMF shall create a PDU SESSION RELEASE COMMAND message\" is unchanged from V15.0.0 through V19.8.0"},
				{Release: 17, Note: "the PTI sentence adds \"and shall not include the Access type IE in the PDU SESSION RELEASE COMMAND\""},
			}},
		{Spec: "TS 23.502", Clause: "4.3.4.2 step 2a", Strength: Shall, Since: 15, Verified: ts23502Verified, WordingUnchanged: true},
		{Spec: "TS 23.502", Clause: "4.3.4.2 step 3", Strength: Shall, Since: 15, Verified: ts23502ReleaseN2Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "the shall first appears in V15.5.0; V15.0.0 to V15.4.1 only list the N2 SM Resource Release request in step 3a's message"},
			}},
		{Spec: "TS 38.413", Clause: "8.2.2.2", Strength: Descriptive, Since: 15, Verified: ts38413Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 ends the second sentence \"to release at least one PDU session in the PDU Session Resource to Release List IE\"; reworded during Rel-15, with the NAS-PDU sentence a paragraph of its own from V15.1.0 and \"over Uu and over NG, if any\" from V15.4.0"},
				{Release: 18, Note: "a Downlink TL Container sentence is added; the quoted sentences are unchanged from Rel-16 through Rel-19"},
			}},
		{Spec: "TS 24.501", Clause: "Table 10.3.1 T3582", Strength: Descriptive, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "the value is TBD in V15.0.0 and 16s by V15.7.0"},
				{Release: 16, Note: "adds a WB-N1/CE mode value of 24s; the normal value stays 16s"},
				{Release: 17, Note: "adds 21s for access via a satellite NG-RAN cell; the normal value stays 16s"},
			}},
		{Spec: "TS 24.501", Clause: "6.4.3.4", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 reads \"an PDU SESSION RELEASE REJECT\"; the sentence is otherwise unchanged from V15.0.0 through V19.8.0"},
			}},
	}
}

func (c nasPDUReleaseUERequested) registerTimeout() time.Duration {
	return orDefault(c.registerWait, pduReleaseRegisterWait)
}
func (c nasPDUReleaseUERequested) waitTimeout() time.Duration {
	return orDefault(c.wait, pduReleaseWait)
}
func (c nasPDUReleaseUERequested) settleTimeout() time.Duration {
	return orDefault(c.settle, pduReleaseSettle)
}
func (c nasPDUReleaseUERequested) graceWindow() time.Duration {
	return orDefault(c.grace, pduReleaseGrace)
}
func (c nasPDUReleaseUERequested) watchWindow() time.Duration {
	return orDefault(c.watch, pduReleaseWatch)
}

// Budget covers the registration, the echo flow's lead, the answer wait and
// its settle, the grace and watch windows, the deregistration and the
// liveness probe.
func (c nasPDUReleaseUERequested) Budget() time.Duration {
	return c.registerTimeout() + pduReleaseTrafficLead + c.waitTimeout() + c.settleTimeout() +
		c.graceWindow() + c.watchWindow() + pduReleaseDeregisterWait + 5*time.Second
}

func (c nasPDUReleaseUERequested) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: fmt.Sprintf("after a PDU Session Release Request, a PDU Session Release Command (TS 24.501 6.4.3.3, 6.3.3.2) and a PDU SESSION RESOURCE RELEASE COMMAND listing the session (TS 23.502 4.3.4.2 step 3; TS 38.413 8.2.2.2), and no downlink G-PDU on the released tunnel from %s after them (TS 23.502 4.3.4.2 step 2a)", c.graceWindow())}
	if !env.UE.Configured() {
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc and --gnb-n3 to run checks that register a UE")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	rctx, cancel := context.WithTimeout(ctx, c.registerTimeout())
	_, err = h.Register(rctx)
	cancel()
	if err != nil {
		return r.SetupFailed("UE setup failed", err)
	}
	h.StartTraffic(ctx)
	if env.UE.Traffic != "" && !sleepCtx(ctx, pduReleaseTrafficLead) {
		return r.Errored("timed out before the release", ctx.Err().Error())
	}

	res, err := h.ReleasePDUSession(ctx, engine.WithPDUReleaseWait(c.waitTimeout()), engine.WithPDUReleaseSettle(c.settleTimeout()))
	cut := ctx.Err() != nil
	if err == nil && res.Released() && !cut {
		// Wait out the grace and the watch, plus a moment for packets
		// already read to reach the observers.
		cut = !sleepCtx(ctx, time.Until(pduReleasedAt(res).Add(c.graceWindow()+c.watchWindow()))+50*time.Millisecond)
	}
	out, observed, detail := judgePDURelease(res, err, h.Observe(), cut, c)
	r = asClassified(r, out, observed, detail)

	dctx, dcancel := context.WithTimeout(context.Background(), pduReleaseDeregisterWait)
	derr := h.Deregister(dctx)
	dcancel()
	if derr != nil {
		r.Detail = joinDetail(r.Detail, fmt.Sprintf("deregistration failed (%v); the association was dropped", derr))
	} else {
		r.Detail = joinDetail(r.Detail, "UE deregistered")
	}
	if r.Verdict == Error {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the PDU session release")
}

// pduReleasedAt is when the later of the two release commands arrived.
func pduReleasedAt(res *engine.PDUReleaseResult) time.Time {
	if res.ResourceReleaseAt.After(res.NASCommandAt) {
		return res.ResourceReleaseAt
	}
	return res.NASCommandAt
}

// pduReleaseFail is the FAIL message of NAS-PDU-RELEASE-UE-REQUESTED.
const pduReleaseFail = "PDU session not released on UE request. TS 24.501 6.4.3.3, 6.3.3.2; TS 23.502 4.3.4.2; TS 38.413 8.2.2.2. Likely cause: SMF release path."

// judgePDURelease turns a release's outcome and the downlink seen on the
// session's tunnel into an outcome, Observed and Detail. cut reports that
// the check's own time ran out before its waits ended. The association
// failing while the release is awaited, with the check's time left, is the
// core's (FAIL); a request or answer that could not be sent is ERROR.
func judgePDURelease(res *engine.PDUReleaseResult, err error, o PathObservation, cut bool, c nasPDUReleaseUERequested) (Outcome, string, string) {
	if errors.Is(err, engine.ErrPDUReleaseAssociationLost) && res != nil && !cut {
		var seen []string
		if res.NASCommand {
			seen = append(seen, "the PDU Session Release Command")
		}
		if res.ResourceRelease {
			seen = append(seen, "the PDU SESSION RESOURCE RELEASE COMMAND")
		}
		got := "nothing of the release"
		if len(seen) > 0 {
			got = "only " + strings.Join(seen, " and ")
		}
		return pduReleaseAssociationLost, "association lost during the PDU session release",
			joinDetail(pduReleaseFail, fmt.Sprintf("the UE's association ended %s after the PDU Session Release Request, with %s received: %v",
				time.Since(res.SentAt).Round(time.Millisecond), got, err))
	}
	if err != nil || res == nil {
		return classifiedError, "PDU session release procedure failed", fmt.Sprint(err)
	}
	sinceSent := func(t time.Time) time.Duration { return t.Sub(res.SentAt).Round(time.Millisecond) }
	switch {
	case res.Rejected:
		return pduReleaseRejected, fmt.Sprintf("core rejected the release (PDU Session Release Reject, 5GSM cause #%d)", res.RejectCause),
			"TS 24.501 6.4.3.4 lets the SMF refuse the request with a PDU SESSION RELEASE REJECT, so there is no release to judge; the session was left established"
	case !res.NASCommand && res.DecodeErr != nil:
		return classifiedError, "downlink NAS after the request did not decode", fmt.Sprintf("%v; %s", res.DecodeErr, codecGap)
	case cut:
		return classifiedError, "timed out before the release was judged", fmt.Sprintf("raise --per-test-timeout to at least %s", c.Budget())
	case !res.NASCommand && !res.ResourceRelease:
		return pduReleaseNoAnswer, fmt.Sprintf("no answer to the PDU Session Release Request within %s", c.waitTimeout()),
			joinDetail(pduReleaseFail, "neither a PDU Session Release Command nor a PDU SESSION RESOURCE RELEASE COMMAND came")
	case !res.NASCommand:
		return pduReleaseNoNASCommand, "PDU SESSION RESOURCE RELEASE COMMAND without a PDU Session Release Command",
			joinDetail(pduReleaseFail, fmt.Sprintf("the NGAP command came after %s; no PDU Session Release Command came with it or within %s of it (TS 24.501 6.3.3.2)", sinceSent(res.ResourceReleaseAt), c.settleTimeout()))
	case !res.ResourceRelease:
		return pduReleaseNoN2Release, "PDU Session Release Command without a PDU SESSION RESOURCE RELEASE COMMAND",
			joinDetail(pduReleaseFail, fmt.Sprintf("the NAS command came after %s; no PDU SESSION RESOURCE RELEASE COMMAND listing session %d came within %s of it, with the session's user plane active (TS 23.502 4.3.4.2 step 3)", sinceSent(res.NASCommandAt), res.PDUSessionID, c.settleTimeout()))
	}

	released := pduReleasedAt(res)
	cutoff := released.Add(c.graceWindow())
	var before, inGrace, after int
	var lastAfter time.Time
	for _, g := range o.GPDUs {
		if g.TEID != res.DLTEID || !sameSocket(o.SourceN3, g.GNB) {
			continue
		}
		switch {
		case g.Arrival.Before(res.SentAt):
			before++
		case !g.Arrival.After(cutoff):
			inGrace++
		default:
			after++
			lastAfter = g.Arrival
		}
	}
	carried := "inside the PDU SESSION RESOURCE RELEASE COMMAND"
	if !res.NASInResourceRelease {
		carried = "in a DOWNLINK NAS TRANSPORT"
	}
	pti := fmt.Sprintf("PTI %d echoed", res.PTI)
	if res.CommandPTI != res.PTI {
		pti = fmt.Sprintf("PTI %d, where the request's is %d (TS 24.501 6.3.3.2 has the SMF echo it; recorded, not scored)", res.CommandPTI, res.PTI)
	}
	detail := fmt.Sprintf("PDU Session Release Command after %s, %s, 5GSM cause #%d, %s; PDU SESSION RESOURCE RELEASE COMMAND after %s; PDU Session Release Complete and PDU SESSION RESOURCE RELEASE RESPONSE sent; downlink on TEID %#x: %d G-PDUs before the request, %d up to %s after the release, %d after that",
		sinceSent(res.NASCommandAt), carried, res.CommandCause, pti, sinceSent(res.ResourceReleaseAt), res.DLTEID, before, inGrace, c.graceWindow(), after)
	if o.Traffic == "" {
		detail = joinDetail(detail, fmt.Sprintf("no echo peer (--traffic): the released tunnel was watched for %s, with no uplink sent to draw downlink", c.graceWindow()+c.watchWindow()))
	} else {
		detail = joinDetail(detail, fmt.Sprintf("echo flow to %s kept sending uplink on the released tunnel", o.Traffic))
		if before == 0 {
			detail = joinDetail(detail, "no downlink arrived before the release either, so the quiet tunnel shows little: check the echo peer")
		}
	}
	if after > 0 {
		return pduReleaseDownlinkAfter, fmt.Sprintf("%d downlink G-PDUs on the released tunnel after the release", after),
			joinDetail(joinDetail(pduReleaseFail, fmt.Sprintf("downlink continues: the last G-PDU came %s after the release (TS 23.502 4.3.4.2 step 2a)", lastAfter.Sub(released).Round(time.Millisecond))), detail)
	}
	return pduReleaseReleased, "PDU session released on N1 and N2; the released tunnel stayed quiet", detail
}
