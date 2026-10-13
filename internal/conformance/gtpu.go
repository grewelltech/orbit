package conformance

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gtpu"
)

func init() {
	builtins = append(builtins, gtpuUnknownTEID{})
}

// bogusN3TEID is a non-zero TEID (required by §7.3.1 to trigger an Error
// Indication) chosen well above the range SD-Core allocates, so it is very
// unlikely to hit a live tunnel on a shared core.
const bogusN3TEID = uint32(0x7FABCDEF)

// errIndWait is how long the check waits for the Error Indication; its
// absence within this bound is what the FAIL detail reports.
const errIndWait = 4 * time.Second

// echoWait is how long each GTP-U Echo Request waits for its response.
const echoWait = 2 * time.Second

// gtpuUnknownTEID sends a GTP-U G-PDU carrying a non-zero TEID the UPF has no
// context for, and asserts the UPF returns a GTP-U Error Indication. Per
// TS 29.281 §7.3.1 the UPF "shall discard the G-PDU [and] if the TEID ... is
// different from ... 'all zeros' ... shall also return a GTP error indication",
// with TEID Data I echoing the offending TEID. A missing Error Indication
// misses that shall and is FAIL; a core that is known not to send one lists
// the failure on its expected-failures list.
//
// Silence alone cannot tell a UPF that discards from one that is down or
// unreachable, so the check confirms liveness with a GTP-U Echo Request
// (7.2.1) on either side of the stimulus. No Echo Response before it is an
// ERROR (liveness cannot be confirmed: a wrong --upf-n3, or a UPF without
// Echo support). No Echo Response after a silent stimulus is FAIL with its
// own Observed text, so an entry for "no Error Indication" never hides a UPF
// that stopped answering. The Echo Response itself (7.2.2) is not asserted.
// Runs only when N3 params are set (from the UPF access network).
type gtpuUnknownTEID struct {
	// errWait and echoWait override errIndWait and echoWait; tests shorten
	// them.
	errWait, echoWait time.Duration
}

func (c gtpuUnknownTEID) waits() (errWait, echo time.Duration) {
	errWait, echo = errIndWait, echoWait
	if c.errWait > 0 {
		errWait = c.errWait
	}
	if c.echoWait > 0 {
		echo = c.echoWait
	}
	return errWait, echo
}

func (gtpuUnknownTEID) ID() string         { return "GTPU-UNKNOWN-TEID-ERRIND" }
func (gtpuUnknownTEID) Category() Category { return GTPU }
func (gtpuUnknownTEID) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 29.281", Clause: "7.3.1", Strength: Shall, Since: 8, Verified: ts29281Verified,
			Changed: []SpecChange{
				{Release: 8, Note: "V8.0.0 and V8.1.0 have a single sentence without the discard rule, added by V8.3.0"},
				{Release: 11, Note: "the 'all zeros' TEID exception is present by V11.6.0"},
				{Release: 12, Note: "current wording from V12.0.0, without \"PDU Session\""},
				{Release: 15, Note: "\"PDU Session\" added in V15.4.0"},
			}},
	}
}

// MinRelease is Rel-15: the requirement dates from Rel-8, but this check
// asserts it for a PDU Session, wording added in V15.4.0.
func (gtpuUnknownTEID) MinRelease() Release { return 15 }

func (c gtpuUnknownTEID) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "GTP-U Error Indication (type 26) echoing the unknown TEID"}
	if env.UPFN3 == "" || env.N3Bind == "" {
		r.Verdict, r.Observed = Skip,
			"no N3 params (run from the UPF access network with --upf-n3 / --n3-bind)"
		return r
	}
	laddr, err := net.ResolveUDPAddr("udp", env.N3Bind)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "bad N3 bind", err.Error()
		return r
	}
	raddr, err := net.ResolveUDPAddr("udp", env.UPFN3)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "bad UPF N3", err.Error()
		return r
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "bind N3 socket", err.Error()
		return r
	}
	defer conn.Close()
	errWait, echo := c.waits()

	// Liveness before the stimulus: without it, silence afterwards could be
	// a UPF that was never reachable.
	alive, err := gtpuEcho(ctx, conn, raddr, 1, echo)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "GTP-U Echo Request", err.Error()
		return r
	}
	if !alive {
		r.Verdict, r.Observed = Error, ObservedUPFNoEcho
		r.Detail = fmt.Sprintf("no GTP-U Echo Response from %s within %s before the test, so the UPF's liveness cannot be confirmed: "+
			"--upf-n3 must be the UPF's primary N3 address (a UPF may answer Echo only there), or the UPF may not support Echo (TS 29.281 7.2)", env.UPFN3, echo)
		return r
	}

	// A minimal inner packet; the UPF rejects on the TEID before inspecting it.
	inner, err := datapath.BuildUDPPacket(net.IPv4(192, 168, 100, 200), net.IPv4(8, 8, 8, 8),
		40000, 9999, []byte("orbit-conformance-probe"))
	if err != nil {
		r.Verdict, r.Detail = Error, err.Error()
		return r
	}
	if _, err := conn.WriteToUDP(gtpu.EncodeGPDU(bogusN3TEID, 1, inner), raddr); err != nil {
		r.Verdict, r.Detail = Error, err.Error()
		return r
	}

	deadline := time.Now().Add(errWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		mt, ok := gtpu.MessageType(buf[:n])
		if !ok || mt != gtpu.MsgTypeErrorInd {
			continue // stray G-PDU or short frame — keep waiting for an Error Indication
		}
		if teid, echoed := gtpu.ErrorIndicationTEID(buf[:n]); echoed && teid == bogusN3TEID {
			r.Verdict, r.Observed = Pass, "Error Indication (TEID-I echoes the probe)"
		} else {
			r.Verdict, r.Observed = Pass, "Error Indication"
			r.Detail = "Error Indication received; TEID-I not confirmed"
		}
		return r
	}

	// Silence: confirm the UPF still answers before calling it a discard.
	alive, err = gtpuEcho(ctx, conn, raddr, 2, echo)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "GTP-U Echo Request", err.Error()
		return r
	}
	if !alive {
		r.Verdict, r.Observed = Fail, ObservedUPFUnresponsive
		r.Detail = fmt.Sprintf("the UPF answered a GTP-U Echo Request before the unknown-TEID G-PDU but not within %s after it, "+
			"and sent no Error Indication: it may not have survived the G-PDU", echo)
		return r
	}
	r.Verdict, r.Observed = Fail, "no Error Indication"
	r.Detail = fmt.Sprintf("no GTP-U Error Indication for the unknown TEID 0x%08X within %s (absence inferred within this bound; "+
		"the UPF answered GTP-U Echo before and after); "+
		"TS 29.281 7.3.1: the UPF shall discard the G-PDU and, for a non-zero TEID, shall also return an Error Indication",
		bogusN3TEID, errWait)
	return r
}

// gtpuEcho sends a GTP-U Echo Request with sequence number seq to raddr and
// reports whether a matching Echo Response arrives within wait (or before
// ctx's deadline). Other packets are ignored. An error means the request
// could not be sent.
func gtpuEcho(ctx context.Context, conn *net.UDPConn, raddr *net.UDPAddr, seq uint16, wait time.Duration) (bool, error) {
	if _, err := conn.WriteToUDP(gtpu.EncodeEchoRequest(seq), raddr); err != nil {
		return false, err
	}
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return false, nil
		}
		if got, ok := gtpu.EchoResponseSeq(buf[:n]); ok && got == seq {
			return true, nil
		}
	}
}
