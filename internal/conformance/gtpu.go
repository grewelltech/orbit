package conformance

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
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
	if !alive && cutShort(ctx) {
		r.Observed = ObservedUPFNoEcho
		return cutShortResult(r, gtpuNeeds(errWait, echo))
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
	if !alive && cutShort(ctx) {
		r.Observed = "no Error Indication"
		return cutShortResult(r, gtpuNeeds(errWait, echo))
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

// gtpuNeeds renders how long the check needs: an Echo wait, the Error
// Indication wait and a second Echo wait ("8s" at the defaults).
func gtpuNeeds(errWait, echo time.Duration) string {
	return (errWait + 2*echo).String()
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

func init() {
	builtins = append(builtins, n3DLQFIContainer{})
}

const (
	// qfiSampleTarget is how many downlink G-PDUs the QFI check samples
	// before it judges, and qfiSampleWindow how long it waits for them.
	qfiSampleTarget = 50
	qfiSampleWindow = 3 * time.Second
)

// qfiContainerFailure is the QFI check's failure message.
const qfiContainerFailure = "downlink G-PDU on N3 without a PDU Session Container. TS 29.281 5.2.2.7 (Rel-15 V15.5.0 onward; V15.2.0-V15.4.0 said \"may\"). Likely cause: UPF not adding the extension header."

// n3DLQFIContainer registers a UE with a PDU session, has it send to the
// echo peer on the data network (UESpec.Traffic) so the UPF has downlink to
// send, and asserts TS 29.281 5.2.2.7: every downlink G-PDU on N3 carries
// the PDU Session Container extension header, whose DL PDU SESSION
// INFORMATION frame has the QFI (TS 38.415 5.4.1.1, 5.5.2.1).
//
// The G-PDUs are those the gNB's N3 demux delivers to the UE's downlink
// lane: every one that arrives until qfiSampleTarget have, or
// qfiSampleWindow has passed. One without the container is FAIL. The QFI
// values are recorded, not asserted. The downlink comes only from the echo
// peer, so without one the check SKIPs, and with one that returns nothing
// it is an ERROR: no G-PDU was sampled.
type n3DLQFIContainer struct {
	// target and window override qfiSampleTarget and qfiSampleWindow (tests
	// only).
	target int
	window time.Duration
}

func (n3DLQFIContainer) ID() string         { return "N3-DL-QFI-CONTAINER" }
func (n3DLQFIContainer) Category() Category { return GTPU }
func (n3DLQFIContainer) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 29.281", Clause: "5.2.2.7", Strength: Shall, Since: 15, Verified: ts29281QFIContainerVerified,
			Changed: []SpecChange{
				{Release: 15, Note: "the clause is introduced in V15.2.0 as \"may be transmitted\"; \"shall be transmitted\" from V15.5.0"},
				{Release: 17, Note: "from V17.3.0 a bulleted list, adding N3mb/N19mb and End Marker bullets; the shall for G-PDUs over N3 and N9 is the same"},
				{Release: 19, Note: "from V19.1.0 \"between NG-RAN and UPF\" reads \"between 5G-AN and UPF\""},
			}},
		{Spec: "TS 38.415", Clause: "5.4.1.1", Strength: Descriptive, Since: 15, Verified: ts38415Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 writes \"DL PDU Session Information frame\"; \"DL PDU SESSION INFORMATION\" by V15.3.0 (editorial); the wording is otherwise unchanged Rel-15 through Rel-19"},
			}},
		{Spec: "TS 38.415", Clause: "5.5.2.1", Strength: Descriptive, Since: 15, Verified: ts38415Verified, WordingUnchanged: true},
	}
}

func (c n3DLQFIContainer) sampling() (target int, window time.Duration) {
	target, window = qfiSampleTarget, qfiSampleWindow
	if c.target > 0 {
		target = c.target
	}
	if c.window > 0 {
		window = c.window
	}
	return target, window
}

func (c n3DLQFIContainer) Run(ctx context.Context, env Env) Result {
	target, window := c.sampling()
	r := Result{Expected: "every downlink G-PDU on N3 carries the PDU Session Container extension header with a QFI (TS 29.281 5.2.2.7; TS 38.415 5.4.1.1)"}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc, --gnb-n3 and --traffic to run this check"
		return r
	}
	if env.UE.Traffic == "" {
		r.Verdict, r.Observed = Skip, "no downlink traffic source configured"
		r.Detail = "set --traffic host:port (a UDP echo peer on the data network): its echoes are the downlink G-PDUs the check samples"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	if _, err := h.Register(ctx); err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "UE setup failed", err.Error()
		return r
	}
	h.StartTraffic(ctx)
	deadline := time.Now().Add(window)
	for len(h.Observe().GPDUs) < target && time.Now().Before(deadline) {
		if !sleepCtx(ctx, 20*time.Millisecond) {
			r.Verdict, r.Observed = Error, "timed out while sampling downlink"
			r.Detail = "raise --per-test-timeout above the registration and " + window.String()
			return r
		}
	}
	r.Verdict, r.Observed, r.Detail = classifyQFIContainer(h.Observe().GPDUs, env.UE.Traffic, window)
	return r
}

// classifyQFIContainer judges the sampled downlink G-PDUs: ERROR when there
// are none, FAIL when any lacks the PDU Session Container, else PASS. The
// detail records the counts and the QFI values seen.
func classifyQFIContainer(gpdus []datapath.GPDUArrival, peer string, window time.Duration) (Verdict, string, string) {
	if len(gpdus) == 0 {
		return Error, "no downlink G-PDU arrived",
			fmt.Sprintf("the echo peer %s returned nothing within %s, so no downlink G-PDU was sampled; check that it answers UDP from the UE's data network", peer, window)
	}
	without := 0
	seen := map[int]bool{}
	for _, g := range gpdus {
		if !g.HasQFI {
			without++
			continue
		}
		seen[int(g.QFI)] = true
	}
	values := make([]int, 0, len(seen))
	for q := range seen {
		values = append(values, q)
	}
	sort.Ints(values)
	qfis := make([]string, len(values))
	for i, q := range values {
		qfis[i] = fmt.Sprint(q)
	}
	qfiText := "QFI values seen: none"
	if len(qfis) > 0 {
		qfiText = "QFI values seen: " + strings.Join(qfis, ", ") + " (recorded only)"
	}
	detail := fmt.Sprintf("%d downlink G-PDUs sampled from the echoes of %s, %d with a PDU Session Container and %d without; %s",
		len(gpdus), peer, len(gpdus)-without, without, qfiText)
	if without > 0 {
		return Fail, fmt.Sprintf("%d of %d downlink G-PDUs without a PDU Session Container", without, len(gpdus)),
			qfiContainerFailure + " Recorded: " + detail
	}
	return Pass, fmt.Sprintf("%d downlink G-PDUs, each with a PDU Session Container", len(gpdus)), detail
}
