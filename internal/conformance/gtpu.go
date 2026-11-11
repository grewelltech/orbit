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
	builtins = append(builtins, gtpuUnknownTEID{}, gtpuEchoResponse{})
}

// bogusN3TEID is a non-zero TEID (required by §7.3.1 to trigger an Error
// Indication) chosen high in the TEID space, so it is unlikely to hit a live
// tunnel on a shared core.
const bogusN3TEID = uint32(0x7FABCDEF)

// errIndWait is how long the check waits for the Error Indication; its
// absence is inferred within this bound.
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
// (7.2.1) on either side of the stimulus. No Echo Response before it is
// BLOCKED on the GTP-U Echo, which GTPU-ECHO-RESPONSE owns. No Echo Response
// after a silent stimulus is CRASH, so an entry for "no Error Indication"
// never hides a UPF that stopped answering. The Echo Response itself
// (7.2.2) is not asserted. Runs only when N3 params are set (from the UPF
// access network).
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

// unknownTEIDRef is the key of the shall the outcomes of
// GTPU-UNKNOWN-TEID-ERRIND rest on.
const unknownTEIDRef = "TS 29.281 7.3.1"

// The outcomes of GTPU-UNKNOWN-TEID-ERRIND.
var (
	unknownTEIDErrorInd = Outcome{ID: "error-indication", Verdict: Pass, Refs: []string{unknownTEIDRef},
		Summary: "returned a GTP-U Error Indication for the G-PDU with an unknown TEID"}
	unknownTEIDNoErrorInd = Outcome{ID: "no-error-indication", Verdict: Fail, Refs: []string{unknownTEIDRef},
		Bound:   &Bound{Wait: errIndWait, Source: "check bound; TS 29.281 sets no timer for the Error Indication"},
		Summary: "returned no GTP-U Error Indication for the G-PDU with an unknown TEID, and still answered GTP-U Echo"}
)

// Outcomes declares what the check can report.
func (gtpuUnknownTEID) Outcomes() []Outcome {
	return []Outcome{unknownTEIDErrorInd, unknownTEIDNoErrorInd}
}

func (gtpuUnknownTEID) ID() string         { return "GTPU-UNKNOWN-TEID-ERRIND" }
func (gtpuUnknownTEID) Category() Category { return GTPU }
func (gtpuUnknownTEID) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 29.281", Clause: "7.3.1", Strength: Shall, Since: 8, Verified: ts29281Verified, Primary: true,
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
	conn, raddr, r, ok := openN3(env, r)
	if !ok {
		return r
	}
	defer func() { _ = conn.Close() }()
	errWait, echo := c.waits()

	// Liveness before the stimulus: without it, silence afterwards could be
	// a UPF that was never reachable.
	alive, err := gtpuEcho(ctx, conn, raddr, 1, echo)
	if err != nil {
		return r.Errored("GTP-U Echo Request could not be sent", err.Error())
	}
	if !alive && cutShort(ctx) {
		r.Observed = ObservedUPFNoEcho
		return cutShortResult(r, gtpuNeeds(errWait, echo))
	}
	if !alive {
		return r.BlockedBy(ProcGTPUEcho, ObservedUPFNoEcho,
			fmt.Sprintf("no GTP-U Echo Response from %s within %s before the test, so the UPF's liveness cannot be confirmed and the G-PDU was not sent", env.UPFN3, echo))
	}

	// A minimal inner packet; the UPF rejects on the TEID before inspecting it.
	inner, err := datapath.BuildUDPPacket(net.IPv4(192, 168, 100, 200), net.IPv4(8, 8, 8, 8),
		40000, 9999, []byte("orbit-conformance-probe"))
	if err != nil {
		return r.Errored("G-PDU could not be built", err.Error())
	}
	if _, err := conn.WriteToUDP(gtpu.EncodeGPDU(bogusN3TEID, 1, inner), raddr); err != nil {
		return r.Errored("G-PDU could not be sent", err.Error())
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
			return r.As(unknownTEIDErrorInd, "Error Indication (TEID-I echoes the probe)", "")
		}
		return r.As(unknownTEIDErrorInd, "Error Indication", "Error Indication received; TEID-I not confirmed")
	}

	// Silence: confirm the UPF still answers before calling it a discard.
	alive, err = gtpuEcho(ctx, conn, raddr, 2, echo)
	if err != nil {
		return r.Errored("GTP-U Echo Request could not be sent", err.Error())
	}
	if !alive && cutShort(ctx) {
		r.Observed = "no Error Indication"
		return cutShortResult(r, gtpuNeeds(errWait, echo))
	}
	if !alive {
		return r.Crashed(ObservedUPFUnresponsive,
			fmt.Sprintf("the UPF answered a GTP-U Echo Request before the unknown-TEID G-PDU but not within %s after it, "+
				"and sent no Error Indication: it may not have survived the G-PDU", echo))
	}
	return r.As(unknownTEIDNoErrorInd, "no Error Indication",
		fmt.Sprintf("no GTP-U Error Indication for the unknown TEID 0x%08X within %s (absence inferred within this bound; "+
			"the UPF answered GTP-U Echo before and after); "+
			"TS 29.281 7.3.1: the UPF shall discard the G-PDU and, for a non-zero TEID, shall also return an Error Indication",
			bogusN3TEID, errWait))
}

// openN3 resolves the run's N3 addresses and binds the local N3 socket for
// a GTP-U check. Without N3 params r is the SKIP for the missing UPF N3
// endpoint; an address that does not resolve or bind is ERROR. ok is false
// with r then final.
func openN3(env Env, r Result) (conn *net.UDPConn, raddr *net.UDPAddr, _ Result, ok bool) {
	if env.UPFN3 == "" || env.N3Bind == "" {
		return nil, nil, r.Skipped(InputUPFN3, "no N3 params (run from the UPF access network with --upf-n3 / --n3-bind)", ""), false
	}
	laddr, err := net.ResolveUDPAddr("udp", env.N3Bind)
	if err != nil {
		return nil, nil, r.Errored("bad N3 bind", err.Error()), false
	}
	raddr, err = net.ResolveUDPAddr("udp", env.UPFN3)
	if err != nil {
		return nil, nil, r.Errored("bad UPF N3", err.Error()), false
	}
	conn, err = net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, nil, r.Errored("bind N3 socket", err.Error()), false
	}
	return conn, raddr, r, true
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

// echoAttempts is how many Echo Requests GTPU-ECHO-RESPONSE sends, each
// with its own sequence number, before it reports no response: one lost
// datagram is not a missing Echo Response.
const echoAttempts = 3

// echoSeqBase is the first sequence number GTPU-ECHO-RESPONSE sends; each
// later request adds one. It is far from 0 and 1, so a UPF that answers
// with a constant or its own counter does not match by accident.
const echoSeqBase = uint16(0x5A01)

// Observed texts of GTPU-ECHO-RESPONSE's two FAILs.
const (
	observedNoEchoResponse = "no Echo Response"
	observedEchoSeqDiffers = "Echo Response with a sequence number that was not sent"
)

// echoFailText is the detail both FAILs of GTPU-ECHO-RESPONSE end with.
const echoFailText = "no GTP-U Echo Response / sequence number not copied. TS 29.281 7.2.1, 7.2.2, 4.3.1 (Rel-8 onward). " +
	"Likely cause: UPF echo handling."

// gtpuEchoResponse sends a GTP-U Echo Request to the UPF's N3 address and
// asserts an Echo Response that carries the request's sequence number. TS
// 29.281 7.2.1: a GTP-U peer "shall be prepared to receive an Echo Request
// at any time and it shall reply with an Echo Response"; 4.3.1: the
// sequence number of a response "shall be copied from" its request. No
// session or tunnel is needed.
//
// It sends up to echoAttempts requests, each with its own sequence number
// and its own wait. An Echo Response carrying any sequence number sent so
// far is PASS, so a late answer to an earlier request counts. An Echo
// Response carrying any other sequence number is FAIL (4.3.1), and so is no
// Echo Response to any request (7.2.1). Silence cannot tell a UPF without
// Echo handling from one that --upf-n3 does not reach, and the detail says
// so. The response's S flag and Recovery IE are not asserted. Runs only
// when N3 params are set (from the UPF access network).
//
// The check owns the GTP-U Echo: a check that needs the UPF to answer one
// reports BLOCKED, naming this check, when it does not.
type gtpuEchoResponse struct {
	// wait overrides echoWait; tests shorten it.
	wait time.Duration
}

func (c gtpuEchoResponse) echoWait() time.Duration {
	if c.wait > 0 {
		return c.wait
	}
	return echoWait
}

// The keys of the shalls the outcomes of GTPU-ECHO-RESPONSE rest on.
const (
	echoReplyRef = "TS 29.281 7.2.1"
	echoSeqRef   = "TS 29.281 4.3.1"
)

// The outcomes of GTPU-ECHO-RESPONSE. A response whose sequence number was
// not sent answers no request the sender can match, so it too reports the
// GTP-U Echo as not completed.
var (
	echoAnswered = Outcome{ID: "echo-response", Verdict: Pass, Refs: []string{echoReplyRef, echoSeqRef},
		Summary: "answered the GTP-U Echo Request with an Echo Response carrying its sequence number"}
	echoSeqDiffers = Outcome{ID: "sequence-not-copied", Verdict: Fail, Refs: []string{echoSeqRef}, NotCompleted: ProcGTPUEcho,
		Summary: "answered with an Echo Response whose sequence number no request carried"}
	echoNoResponse = Outcome{ID: "no-echo-response", Verdict: Fail, Refs: []string{echoReplyRef}, NotCompleted: ProcGTPUEcho,
		Bound:   &Bound{Wait: echoAttempts * echoWait, Source: "check bound: 3 requests, each waited 2s; TS 29.281 sets no timer for the Echo Response"},
		Summary: "sent no Echo Response to any of the GTP-U Echo Requests"}
)

// Outcomes declares what the check can report.
func (gtpuEchoResponse) Outcomes() []Outcome {
	return []Outcome{echoAnswered, echoSeqDiffers, echoNoResponse}
}

func (gtpuEchoResponse) ID() string         { return "GTPU-ECHO-RESPONSE" }
func (gtpuEchoResponse) Category() Category { return GTPU }
func (gtpuEchoResponse) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 29.281", Clause: "7.2.1", Strength: Shall, Since: 8, Verified: ts29281EchoVerified, Primary: true,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 adds the prefix \"Even if there is no path in use,\" (absent in V8.0.0 and V14.1.0); unchanged through Rel-19"},
			}},
		{Spec: "TS 29.281", Clause: "7.2.2", Strength: Shall, Since: 8, Verified: ts29281EchoVerified, WordingUnchanged: true},
		{Spec: "TS 29.281", Clause: "4.3.1", Strength: Shall, Since: 8, Verified: ts29281EchoVerified,
			Changed: []SpecChange{
				{Release: 11, Note: "\"the GSN or RNC\" (V8.0.0, V9.0.0) reads \"the GTP-U entity\" by V11.0.0 (Rel-10 not read); unchanged through Rel-19"},
			}},
	}
}

// MinRelease is Rel-15: the three requirements date from Rel-8, but the
// check addresses a UPF's N3 interface, which exists from Rel-15.
func (gtpuEchoResponse) MinRelease() Release { return 15 }

func (c gtpuEchoResponse) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "GTP-U Echo Response (type 2) carrying the Echo Request's sequence number"}
	conn, raddr, r, ok := openN3(env, r)
	if !ok {
		return r
	}
	defer func() { _ = conn.Close() }()
	wait := c.echoWait()

	buf := make([]byte, 2048)
	for i := 0; i < echoAttempts; i++ {
		seq := echoSeqBase + uint16(i)
		if _, err := conn.WriteToUDP(gtpu.EncodeEchoRequest(seq), raddr); err != nil {
			return r.Errored("GTP-U Echo Request could not be sent", err.Error())
		}
		deadline := time.Now().Add(wait)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = conn.SetReadDeadline(deadline)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				break
			}
			got, ok := gtpu.EchoResponseSeq(buf[:n])
			if !ok {
				continue // not an Echo Response; keep waiting for one
			}
			if got >= echoSeqBase && got <= seq {
				return r.As(echoAnswered, "Echo Response (sequence number copied)",
					fmt.Sprintf("sequence number 0x%04X, answering request %d of %d sent", got, int(got-echoSeqBase)+1, i+1))
			}
			return r.As(echoSeqDiffers, observedEchoSeqDiffers,
				joinDetail(fmt.Sprintf("Echo Response from %s carries sequence number 0x%04X; the %d request(s) sent carried 0x%04X to 0x%04X",
					env.UPFN3, got, i+1, echoSeqBase, seq), echoFailText))
		}
		if cutShort(ctx) {
			r.Observed = observedNoEchoResponse
			return cutShortResult(r, (echoAttempts * wait).String())
		}
	}
	return r.As(echoNoResponse, observedNoEchoResponse,
		joinDetail(fmt.Sprintf("no GTP-U Echo Response from %s to %d Echo Requests, each waited %s (absence inferred within this bound); "+
			"silence cannot tell a UPF without Echo handling from one this address does not reach: --upf-n3 must be the UPF's primary N3 address",
			env.UPFN3, echoAttempts, wait), echoFailText))
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
// it is an ERROR: no G-PDU was sampled, and silence cannot tell an echo
// peer that the testbed does not reach from a user plane that drops.
type n3DLQFIContainer struct {
	// target and window override qfiSampleTarget and qfiSampleWindow (tests
	// only).
	target int
	window time.Duration
}

// qfiContainerRef is the key of the shall the outcomes of
// N3-DL-QFI-CONTAINER rest on.
const qfiContainerRef = "TS 29.281 5.2.2.7"

// The outcomes of N3-DL-QFI-CONTAINER.
var (
	qfiContainerPresent = Outcome{ID: "container-present", Verdict: Pass, Refs: []string{qfiContainerRef},
		Summary: "sent every sampled downlink G-PDU on N3 with the PDU Session Container"}
	qfiContainerAbsent = Outcome{ID: "container-absent", Verdict: Fail, Refs: []string{qfiContainerRef},
		Summary: "sent a downlink G-PDU on N3 without the PDU Session Container"}
)

// Outcomes declares what the check can report.
func (n3DLQFIContainer) Outcomes() []Outcome {
	return []Outcome{qfiContainerPresent, qfiContainerAbsent}
}

func (n3DLQFIContainer) ID() string         { return "N3-DL-QFI-CONTAINER" }
func (n3DLQFIContainer) Category() Category { return GTPU }
func (n3DLQFIContainer) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 29.281", Clause: "5.2.2.7", Strength: Shall, Since: 15, Verified: ts29281QFIContainerVerified, Primary: true,
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
		return r.Skipped(InputUE, "no UE configured", "set --supi, --ki/--opc, --gnb-n3 and --traffic to run this check")
	}
	if env.UE.Traffic == "" {
		return r.Skipped(InputTraffic, "no downlink traffic source configured",
			"set --traffic host:port (a UDP echo peer on the data network): its echoes are the downlink G-PDUs the check samples")
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		return r.Errored("harness setup failed", err.Error())
	}
	defer h.Close()
	if _, err := h.Register(ctx); err != nil {
		return r.SetupFailed("UE setup failed", err)
	}
	h.StartTraffic(ctx)
	deadline := time.Now().Add(window)
	for len(h.Observe().GPDUs) < target && time.Now().Before(deadline) {
		if !sleepCtx(ctx, 20*time.Millisecond) {
			return r.Errored("timed out while sampling downlink", "raise --per-test-timeout above the registration and "+window.String())
		}
	}
	gpdus := h.Observe().GPDUs
	if len(gpdus) == 0 {
		return r.Errored("no downlink G-PDU arrived",
			fmt.Sprintf("the echo peer %s returned nothing within %s, so no downlink G-PDU was sampled; check that it answers UDP from the UE's data network", env.UE.Traffic, window))
	}
	return r.As(classifyQFIContainer(gpdus, env.UE.Traffic))
}

// classifyQFIContainer judges the sampled downlink G-PDUs, of which there
// is at least one: FAIL when any lacks the PDU Session Container, else
// PASS. The detail records the counts and the QFI values seen.
func classifyQFIContainer(gpdus []datapath.GPDUArrival, peer string) (Outcome, string, string) {
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
		return qfiContainerAbsent, fmt.Sprintf("%d of %d downlink G-PDUs without a PDU Session Container", without, len(gpdus)),
			qfiContainerFailure + " Recorded: " + detail
	}
	return qfiContainerPresent, fmt.Sprintf("%d downlink G-PDUs, each with a PDU Session Container", len(gpdus)), detail
}
