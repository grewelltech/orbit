package conformance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
)

func init() {
	builtins = append(builtins, ranConfigUpdateTransferSyntax{})
}

// ranConfigSyntaxWait is how long the check waits for the ERROR INDICATION.
// A silent drop is inferred from its absence within this bound, and the
// detail says so.
const ranConfigSyntaxWait = 3 * time.Second

// ranConfigSyntaxText is the detail of every outcome without an ERROR
// INDICATION.
const ranConfigSyntaxText = "no ERROR INDICATION for a transfer-syntax error in RAN CONFIGURATION UPDATE. TS 38.413 10.2 (should). " +
	"Likely cause: AMF silently drops malformed PDUs."

// ranConfigUpdateTransferSyntax sends, on an association that has completed
// NG Setup, a RAN CONFIGURATION UPDATE whose Supported TA List holds one
// Supported TA Item cut off after its TAC: the item's mandatory Broadcast
// PLMN List is missing, a transfer syntax error ("Missing mandatory elements
// in ASN.1 SEQUENCE definitions", TS 38.413 10.2). For one, 10.2 says the
// receiver "should initiate Error Indication procedure".
//
// An ERROR INDICATION is PASS, its cause recorded and not asserted. Anything
// else misses the should and is DEVIATION, with its own outcome: no answer
// within ranConfigSyntaxWait, another PDU (a RAN CONFIGURATION UPDATE
// FAILURE or ACKNOWLEDGE included), or a lost association. After every
// outcome, a lost association included, a fresh NG Setup confirms the core
// (confirmAlive): one that no longer completes it is CRASH, which tells a
// core that only dropped the association from one that is gone.
type ranConfigUpdateTransferSyntax struct {
	// wait overrides ranConfigSyntaxWait; tests shorten it.
	wait time.Duration
}

// ranConfigSyntaxRef is the key of the should every outcome of the check
// rests on.
const ranConfigSyntaxRef = "TS 38.413 10.2"

// The outcomes of NGAP-RAN-CONFIG-UPDATE-TRANSFER-SYNTAX.
var (
	ranConfigSyntaxIndicated = Outcome{ID: "error-indication", Verdict: Pass, Refs: []string{ranConfigSyntaxRef},
		Summary: "answered the RAN CONFIGURATION UPDATE with a transfer syntax error with an ERROR INDICATION"}
	ranConfigSyntaxOtherPDU = Outcome{ID: "other-pdu", Verdict: Deviation, Refs: []string{ranConfigSyntaxRef},
		Summary: "answered with another PDU instead of an ERROR INDICATION; the association survived"}
	ranConfigSyntaxSilent = Outcome{ID: "silent-drop", Verdict: Deviation, Refs: []string{ranConfigSyntaxRef},
		Bound:   &Bound{Wait: ranConfigSyntaxWait, Source: "check bound; TS 38.413 sets no timer for the ERROR INDICATION"},
		Summary: "sent no ERROR INDICATION; the association survived"}
	ranConfigSyntaxLost = Outcome{ID: "association-lost", Verdict: Deviation, Refs: []string{ranConfigSyntaxRef},
		Summary: "dropped the association instead of sending an ERROR INDICATION, and kept running"}
)

// Outcomes declares what the check can report.
func (ranConfigUpdateTransferSyntax) Outcomes() []Outcome {
	return []Outcome{ranConfigSyntaxIndicated, ranConfigSyntaxOtherPDU, ranConfigSyntaxSilent, ranConfigSyntaxLost}
}

func (c ranConfigUpdateTransferSyntax) errIndWait() time.Duration {
	if c.wait > 0 {
		return c.wait
	}
	return ranConfigSyntaxWait
}

func (ranConfigUpdateTransferSyntax) ID() string         { return "NGAP-RAN-CONFIG-UPDATE-TRANSFER-SYNTAX" }
func (ranConfigUpdateTransferSyntax) Category() Category { return NegativeIE }

// SpecRefs cites the should in 10.2. The clauses that make the stimulus a
// transfer syntax error and not a missing IE or a rejected update (9.2.6.4,
// 8.7.2.3) are in the provenance file only: the check asserts neither.
func (ranConfigUpdateTransferSyntax) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "10.2", Strength: Should, Since: 15, Verified: ts38413Verified, WordingUnchanged: true, Primary: true},
	}
}

func (c ranConfigUpdateTransferSyntax) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "ERROR INDICATION for a RAN CONFIGURATION UPDATE with a transfer syntax error, and the NG association keeps working (TS 38.413 10.2)"}
	stim, err := truncatedTAListUpdate(env.GNB)
	if err != nil {
		return r.Errored("stimulus could not be built", err.Error())
	}
	conn, err := env.DialSetup(ctx)
	if err != nil {
		return r.SetupFailed("setup failed", err)
	}
	defer func() { _ = conn.Close() }()

	// RAN CONFIGURATION UPDATE is non-UE-associated: stream 0.
	if err := conn.WriteNGAP(sctp.StreamNonUEAssociated, stim); err != nil {
		return r.Errored("stimulus could not be sent", err.Error())
	}
	sent := time.Now()
	wait := c.errIndWait()
	rctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	resp, err := gnb.ReadPDU(rctx, conn)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
		return r.Errored("timed out waiting for the ERROR INDICATION",
			fmt.Sprintf("the per-test timeout expired before the %s the check waits; raise --per-test-timeout", wait))
	}
	o, observed, detail, survived := classifyRANConfigSyntax(resp, err, time.Since(sent), wait)
	r = r.As(o, observed, detail)

	// The fresh NG Setup runs after a lost association too: there it is
	// what tells a dropped association from a core that is gone.
	r = confirmAlive(ctx, env, r, true, "fresh NG Setup failed — the core did not survive the malformed RAN CONFIGURATION UPDATE")
	if !survived && r.Outcome == ranConfigSyntaxLost.ID {
		r.Detail = joinDetail("a fresh NG Setup still completes, so the AMF dropped the association and kept running", r.Detail)
	}
	return r
}

// classifyRANConfigSyntax maps the AMF's reaction to the malformed RAN
// CONFIGURATION UPDATE onto an outcome; after is how long after the
// stimulus the reaction came and wait the bound a silent drop is inferred
// within. survived reports whether the association is still up.
func classifyRANConfigSyntax(resp *ngapType.NGAPPDU, err error, after, wait time.Duration) (o Outcome, observed, detail string, survived bool) {
	switch {
	case err == nil && isErrorIndication(resp):
		return ranConfigSyntaxIndicated, "ERROR INDICATION", "cause " + errorIndicationCause(resp) + " (recorded, not asserted)", true
	case err == nil:
		name := ranConfigOutcomeName(resp)
		return ranConfigSyntaxOtherPDU, name, joinDetail(fmt.Sprintf("answered with %s %s after the malformed RAN CONFIGURATION UPDATE; the association survived",
			name, after.Round(time.Millisecond)), ranConfigSyntaxText), true
	case errors.Is(err, context.DeadlineExceeded):
		return ranConfigSyntaxSilent, "silently dropped; association survived",
			joinDetail(fmt.Sprintf("no ERROR INDICATION within %s of the malformed RAN CONFIGURATION UPDATE (absence inferred within this bound); the association survived", wait),
				ranConfigSyntaxText), true
	default:
		return ranConfigSyntaxLost, "association reset",
			joinDetail(fmt.Sprintf("the association was lost %s after the malformed RAN CONFIGURATION UPDATE (%v)", after.Round(time.Millisecond), err),
				ranConfigSyntaxText), false
	}
}

// ranConfigOutcomeName names a PDU that answered the stimulus: the two RAN
// Configuration Update outcomes by name, anything else as ngapName does.
func ranConfigOutcomeName(pdu *ngapType.NGAPPDU) string {
	switch {
	case gnb.IsRANConfigurationUpdateFailure(pdu):
		return "RAN CONFIGURATION UPDATE FAILURE"
	case gnb.IsRANConfigurationUpdateAcknowledge(pdu):
		return "RAN CONFIGURATION UPDATE ACKNOWLEDGE (accepted!)"
	}
	return ngapName(pdu)
}

// errorIndicationCause renders an ERROR INDICATION's Cause IE, naming the
// Protocol cause 10.2 is about ("Transfer Syntax Error", 9.3.1.2).
func errorIndicationCause(pdu *ngapType.NGAPPDU) string {
	ei := pdu.InitiatingMessage.Value.ErrorIndication
	if ei == nil {
		return "absent"
	}
	for _, ie := range ei.ProtocolIEs.List {
		if ie.Id.Value == ngapType.ProtocolIEIDCause && ie.Value.Cause != nil {
			return causeText(ie.Value.Cause)
		}
	}
	return "absent"
}

// causeText renders a Cause IE (TS 38.413 9.3.1.2) as its group and value,
// naming the Protocol cause "Transfer Syntax Error".
func causeText(c *ngapType.Cause) string {
	switch c.Present {
	case ngapType.CausePresentProtocol:
		if c.Protocol.Value == ngapType.CauseProtocolPresentTransferSyntaxError {
			return "Protocol transfer-syntax-error"
		}
		return fmt.Sprintf("Protocol %d", c.Protocol.Value)
	case ngapType.CausePresentRadioNetwork:
		return fmt.Sprintf("RadioNetwork %d", c.RadioNetwork.Value)
	case ngapType.CausePresentTransport:
		return fmt.Sprintf("Transport %d", c.Transport.Value)
	case ngapType.CausePresentNas:
		return fmt.Sprintf("NAS %d", c.Nas.Value)
	case ngapType.CausePresentMisc:
		return fmt.Sprintf("Misc %d", c.Misc.Value)
	}
	return "absent"
}

// Layout of an encoded RAN CONFIGURATION UPDATE whose only IE is a short
// Supported TA List (TS 38.413 9.2.6.4, aligned PER): the PDU choice, the
// procedure code, the criticality and the message's length; the message's
// extension octet and IE count; the IE's ID, criticality and length; then
// the IE's value.
const (
	rcuMessageLen = 3  // offset of the message's length octet
	rcuIELen      = 10 // offset of the IE's length octet
	rcuIEValue    = 11 // offset of the IE's value
	// rcuTAItemPrefix is how much of the IE's value is kept: the list's
	// item count, the item's extension and optional bits, and its TAC.
	rcuTAItemPrefix = 5
)

// truncatedTAListUpdate encodes the stimulus: a RAN CONFIGURATION UPDATE
// carrying cfg's Supported TA List with its one Supported TA Item cut off
// after the TAC, the lengths around it stating the shortened sizes. The
// item's mandatory Broadcast PLMN List is then missing, so the IE's value
// ends where a decoder still needs it. Everything up to the cut is what the
// encoder produces for the well-formed message.
func truncatedTAListUpdate(cfg gnb.Config) ([]byte, error) {
	// One slice keeps every length in the message in a single octet; the
	// slices sit after the cut and never reach the wire.
	if len(cfg.Slices) > 1 {
		cfg.Slices = cfg.Slices[:1]
	}
	pdu, err := gnb.BuildRANConfigurationUpdate(gnb.WithSupportedTAList(cfg))
	if err != nil {
		return nil, err
	}
	valid, err := ngap.Encode(pdu)
	if err != nil {
		return nil, err
	}
	if len(valid) < rcuIEValue+rcuTAItemPrefix+1 || len(valid) > 127 ||
		valid[0] != 0x00 || int64(valid[1]) != ngapType.ProcedureCodeRANConfigurationUpdate ||
		int(valid[rcuMessageLen]) != len(valid)-rcuMessageLen-1 || int(valid[rcuIELen]) != len(valid)-rcuIEValue {
		return nil, fmt.Errorf("the well-formed RAN CONFIGURATION UPDATE (%x) does not have the layout the stimulus is cut from", valid)
	}
	out := append([]byte(nil), valid[:rcuIEValue+rcuTAItemPrefix]...)
	out[rcuMessageLen] = byte(len(out) - rcuMessageLen - 1)
	out[rcuIELen] = rcuTAItemPrefix
	return out, nil
}
