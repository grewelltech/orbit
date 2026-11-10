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
// else with the association still up misses the should and is DEVIATION,
// with Observed saying which: a silent drop within ranConfigSyntaxWait, or
// another PDU (a RAN CONFIGURATION UPDATE FAILURE or ACKNOWLEDGE included).
// A lost association is returned as FAIL; a fresh NG Setup then tells a core
// that only dropped the association from one that is gone. The check cites
// no shall, so the registry scores the first DEVIATION (runOne) and keeps
// the second, the crash, FAIL. The same fresh NG Setup confirms the core
// after every other outcome.
type ranConfigUpdateTransferSyntax struct {
	// wait overrides ranConfigSyntaxWait; tests shorten it.
	wait time.Duration
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
		{Spec: "TS 38.413", Clause: "10.2", Strength: Should, Since: 15, Verified: ts38413Verified, WordingUnchanged: true},
	}
}

func (c ranConfigUpdateTransferSyntax) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "ERROR INDICATION for a RAN CONFIGURATION UPDATE with a transfer syntax error, and the NG association keeps working (TS 38.413 10.2)"}
	stim, err := truncatedTAListUpdate(env.GNB)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "stimulus could not be built", err.Error()
		return r
	}
	conn, err := env.DialSetup(ctx)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "setup failed", err.Error()
		return r
	}
	defer func() { _ = conn.Close() }()

	// RAN CONFIGURATION UPDATE is non-UE-associated: stream 0.
	if err := conn.WriteNGAP(sctp.StreamNonUEAssociated, stim); err != nil {
		r.Verdict, r.Detail = Error, err.Error()
		return r
	}
	sent := time.Now()
	wait := c.errIndWait()
	rctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	resp, err := gnb.ReadPDU(rctx, conn)
	var survived bool
	r.Verdict, r.Observed, r.Detail, survived = classifyRANConfigSyntax(resp, err, time.Since(sent), wait)

	// The fresh NG Setup runs after a lost association too: there it is
	// what tells a dropped association from a core that is gone.
	lost := r
	r = confirmAlive(ctx, env, r, true, "fresh NG Setup failed — the core did not survive the malformed RAN CONFIGURATION UPDATE")
	if !survived && r.Observed == lost.Observed {
		r.Detail = joinDetail("a fresh NG Setup still completes, so the AMF dropped the association and kept running", r.Detail)
	}
	return r
}

// classifyRANConfigSyntax maps the AMF's reaction to the malformed RAN
// CONFIGURATION UPDATE onto a verdict; after is how long after the stimulus
// the reaction came and wait the bound a silent drop is inferred within.
// survived reports whether the association is still up.
func classifyRANConfigSyntax(resp *ngapType.NGAPPDU, err error, after, wait time.Duration) (v Verdict, observed, detail string, survived bool) {
	switch {
	case err == nil && isErrorIndication(resp):
		return Pass, "ERROR INDICATION", "cause " + errorIndicationCause(resp) + " (recorded, not asserted)", true
	case err == nil:
		name := ranConfigOutcomeName(resp)
		return Deviation, name, joinDetail(fmt.Sprintf("answered with %s %s after the malformed RAN CONFIGURATION UPDATE; the association survived",
			name, after.Round(time.Millisecond)), ranConfigSyntaxText), true
	case errors.Is(err, context.DeadlineExceeded):
		return Deviation, "silently dropped; association survived",
			joinDetail(fmt.Sprintf("no ERROR INDICATION within %s of the malformed RAN CONFIGURATION UPDATE (absence inferred within this bound); the association survived", wait),
				ranConfigSyntaxText), true
	default:
		return Fail, "association reset",
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
