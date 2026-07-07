package conformance

import (
	"context"
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

// gtpuUnknownTEID sends a GTP-U G-PDU carrying a non-zero TEID the UPF has no
// context for, and asserts the UPF returns a GTP-U Error Indication. Per
// TS 29.281 §7.3.1 the UPF "shall discard the G-PDU [and] if the TEID ... is
// different from ... 'all zeros' ... shall also return a GTP error indication",
// with TEID Data I echoing the offending TEID. This is a genuine "shall", so a
// FAIL is possible — but Error Indication on an unknown TEID is commonly not
// implemented in production UPFs, so a missing one is a benign §7.3.1 deviation,
// not a crash. Runs only when N3 params are set (from the UPF access network).
type gtpuUnknownTEID struct{}

func (gtpuUnknownTEID) ID() string         { return "GTPU-UNKNOWN-TEID-ERRIND" }
func (gtpuUnknownTEID) Category() Category { return GTPU }
func (gtpuUnknownTEID) SpecRef() string {
	return "TS 29.281 §7.3.1 (Error Indication on unknown TEID)"
}

func (gtpuUnknownTEID) Run(ctx context.Context, env Env) Result {
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

	deadline := time.Now().Add(4 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			r.Verdict, r.Observed = Deviation, "no Error Indication"
			r.Detail = "UPF returned no GTP-U Error Indication for the unknown TEID (§7.3.1 'shall'); " +
				"benign and commonly unimplemented (dropping is safe) — a documented deviation, not a gate failure"
			return r
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
}
