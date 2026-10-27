package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/nas"
)

func init() {
	builtins = append(builtins, nasPDUUnknownDNN{})
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
// cause, another decoded answer, or nothing within dnnAnswerWait is FAIL.
// A core that accepts the DNN leaves nothing to judge: SKIP, as does a
// congestion send-back. The request returned with any other 5GMM cause, and
// a downlink or 5GSM message that does not decode, are ERROR. The answer is
// read through the UE's NAS security context. Whenever the registration
// completed, the UE is deregistered at the end.
type nasPDUUnknownDNN struct {
	// registerWait and answerWait override dnnRegisterWait and
	// dnnAnswerWait (tests only).
	registerWait, answerWait time.Duration
}

func (nasPDUUnknownDNN) ID() string         { return "NAS-PDU-UNKNOWN-DNN" }
func (nasPDUUnknownDNN) Category() Category { return NAS }
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
		{Spec: "TS 24.501", Clause: "6.4.1.4.1 (6.4.1.4 in V15.1.0 to V15.7.0)", Strength: Shall, Since: 15, Verified: ts24501UnknownDNNVerified,
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

	o := c.probe(ctx, h)
	r.Verdict, r.Observed, r.Detail = judgeDNN(o, c.answerTimeout())
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
	decodeErr  error   // a downlink after the request that did not decode
	decodeRaw  []byte  // its NAS PDU
	decodeType uint8   // its 5GMM message type (NASEvent.MessageType), 0 if unreadable
	registered bool    // the attach returned a REGISTERED UE
	err        error   // registration or harness failure
}

// probe registers the UE with a PDU session for stimulusDNN and stops the
// attach at the first N1 SM DL NAS Transport after the request, at a
// downlink that does not decode, at an established session, or when a wait
// runs out.
func (c nasPDUUnknownDNN) probe(ctx context.Context, h *UEHarness) dnnOutcome {
	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	defer halt()
	var mu sync.Mutex
	var o dnnOutcome
	var regTimedOut bool
	var answerTimer *time.Timer
	regTimer := time.AfterFunc(c.registerTimeout(), func() {
		mu.Lock()
		defer mu.Unlock()
		if o.sent == nil {
			regTimedOut = true
			halt()
		}
	})
	defer regTimer.Stop()
	decided := func() bool { return o.answer != nil || o.noAnswer || o.decodeErr != nil }
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
			answerTimer = time.AfterFunc(c.answerTimeout(), func() {
				mu.Lock()
				defer mu.Unlock()
				if !decided() {
					o.noAnswer = true
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
				o.answer = &d
				halt()
			}
		},
	}
	pdu := h.pdu
	pdu.DNN = stimulusDNN
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
	case err == nil && (o.answer != nil || o.noAnswer):
	case err == nil && res != nil && res.SessionActive:
		o.accepted = true
	case err == nil:
		o.err = errors.New("registration ended without the PDU session or an answer to it")
	case regTimedOut:
		o.err = fmt.Errorf("registration did not reach the PDU Session Establishment Request within %s", c.registerTimeout())
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

// judgeDNN turns the outcome into a verdict, Observed and Detail.
func judgeDNN(o dnnOutcome, wait time.Duration) (Verdict, string, string) {
	switch {
	case o.err != nil:
		return Error, "registration or harness failure", o.err.Error()
	case o.decodeErr != nil && errors.Is(o.decodeErr, nas.ErrIntegrity):
		return Error, "downlink NAS after the request failed integrity check" + typeNote(o.decodeType), fmt.Sprintf("%v; NAS %x", o.decodeErr, o.decodeRaw)
	case o.decodeErr != nil:
		return Error, "downlink NAS after the request did not decode" + typeNote(o.decodeType), fmt.Sprintf("%v; %s; NAS %x", o.decodeErr, codecGap, o.decodeRaw)
	case o.accepted:
		return Skip, acceptedObserved,
			fmt.Sprintf("the core established a PDU session for DNN %q (a wildcard DNN subscription, or no DNN check); no shall forbids that", stimulusDNN)
	case o.noAnswer:
		return Fail, fmt.Sprintf("nothing back within %s", wait),
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
		return Pass, fmt.Sprintf("un-forwarded request returned, 5GMM cause %s", mmCause(a.mmCause)),
			fmt.Sprintf("path (B): the AMF returned the PDU Session Establishment Request byte for byte in a DL NAS Transport with %s (which of #90 and #91 applies is recorded, not asserted)", cites)
	case isRoutingCause(a.mmCause):
		likely := "AMF returns a payload other than the un-forwarded 5GSM message"
		if sm.kind == smReject {
			likely = "AMF returns its own Reject in the payload container instead of the un-forwarded message"
		}
		return Fail, fmt.Sprintf("5GMM cause %s with a payload that is not the request", mmCause(a.mmCause)),
			unknownDNNFail(fmt.Sprintf("a DL NAS Transport with %s carrying %s; sent %x, returned %x", cites, sm.name, o.sent, a.payload), likely)
	case sm.kind == smUndecodable:
		return Error, "answer did not decode", fmt.Sprintf("a DL NAS Transport with %s carrying a %s that did not decode: %v; %s; payload %x", cites, sm.name, sm.decodeErr, codecGap, a.payload)
	case sm.kind == smReject:
		detail := fmt.Sprintf("path (A): %s, %s", sm.name, cites)
		if sm.cause != nasMessage.Cause5GSMMissingOrUnknownDNN {
			detail += "; the cause is not #27, which 6.4.1.4.1 names as typical (any cause is conformant)"
		}
		return Pass, sm.name, detail + "; " + indistinguishable
	case sm.kind == smAccept:
		return Skip, acceptedObserved,
			fmt.Sprintf("a PDU Session Establishment Accept for DNN %q came in a plain DL NAS Transport (%s) without a PDU Session Resource Setup Request; no shall forbids accepting the DNN", stimulusDNN, cites)
	case a.mmCause == 0 && returned:
		return Fail, "request returned without a 5GMM cause",
			unknownDNNFail(fmt.Sprintf("the PDU Session Establishment Request returned in a DL NAS Transport with %s", cites), "AMF returned the request without the 5GMM cause 5.4.5.3.2 requires")
	case isCongestionCause(a.mmCause) && returned:
		backoff := "no Back-off timer value IE"
		if a.backoff {
			backoff = "a Back-off timer value IE"
		}
		return Skip, congestionObserved,
			fmt.Sprintf("the AMF returned the request with %s and %s; 5.4.5.2.5 a) 1) lists this case f) of 5.4.5.3.1 send-back from V16.3.0, so it is conformant, but the DNN was not judged: re-run when the core is not congested", cites, backoff)
	case a.mmCause != 0 && returned:
		return Error, fmt.Sprintf("request returned with 5GMM cause %s", mmCause(a.mmCause)),
			fmt.Sprintf("the AMF returned the request with %s, not a cause 5.4.5.3.2 assigns to a routing failure; this check does not judge the DNN then", cites)
	}
	got := fmt.Sprintf("a DL NAS Transport with %s carrying %s", cites, sm.name)
	if sm.kind == smStatus && pointsAtStimulus(sm.cause) {
		got += "; that cause points at a malformed request: check the stimulus"
	}
	return Fail, "unexpected answer: " + sm.name, unknownDNNFail(got, "an answer other than a reject or the returned request")
}
