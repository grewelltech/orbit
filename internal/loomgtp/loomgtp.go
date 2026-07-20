// Package loomgtp bridges loom to ORBIT's userspace GTP-U tunnel. It supplies
// loom datapaths (implementing loom's datapath.Tx/RxDatapath frame contract)
// that, instead of opening kernel sockets, ride the session's N3 tunnel — so
// one process drives many UEs with no TUN device, no NET_ADMIN, and no per-UE
// netns. loom is embedded unmodified via its injectable components; nothing
// here forks loom. See docs/DESIGN.md (loom addendum) and
// docs/design/real-app-traffic.md §6.
//
// Two transmit variants exist because loom hands them different bytes:
//
//   - "orbit-gtp" (rawTx + rxDatapath, Capabilities.RawL3): frames are
//     complete inner IP packets. loom's dgram network (core/netpath/dgram)
//     encodes real IPv4+UDP headers — checksums included — into each frame,
//     so TxCommit forwards the frame verbatim to Tunnel.SendUplink and never
//     double-builds headers. Downlink is the session Demux's wildcard UDP
//     lane (datapath.UERx.SubscribeUDPAll), arrival timestamps preserved into
//     loom Frame.Meta. NetworkFor assembles the pair into a netpath.Network.
//
//   - "orbit-gtp-payload" (payloadTx, no RawL3): frames are raw application
//     payloads from loom's legacy stream generator (RunFlow). TxCommit wraps
//     each payload in an inner IPv4+UDP packet (datapath.BuildUDPPacket)
//     before sending it uplink. Kept solely for the RunFlow throughput path;
//     new consumers should go through NetworkFor.
package loomgtp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/bgrewell/loom/core/components"
	loomdp "github.com/bgrewell/loom/core/datapath"
	"github.com/bgrewell/loom/core/flow"
	"github.com/bgrewell/loom/core/registry"

	"github.com/bgrewell/orbit/internal/datapath"
)

// Uplink is the GTP-U send side this bridge needs; *engine.Session,
// *datapath.UETunnel and *datapath.UEFlow all satisfy it (each stamping the
// UE's own UL TEID/QFI on the shared per-gNB socket). Kept minimal so tests
// can inject a loopback.
type Uplink interface {
	SendUplink(innerIP []byte) error
}

const poolDepth = 64

// rawTx implements loom's datapath.TxDatapath with Capabilities.RawL3: every
// committed frame is already a complete inner IP packet (loom's dgram network
// builds the IPv4+UDP headers, checksums included), so TxCommit sends
// Data[:Len] up the GTP-U tunnel unmodified. The committer reserves → fills →
// commits synchronously (dgram serializes pairs under its tx mutex), so
// reusing one frame pool is safe.
type rawTx struct {
	up   Uplink
	pool []loomdp.Frame
}

func newRawTx(up Uplink, frameSize int) *rawTx {
	if frameSize <= 0 {
		frameSize = DefaultInnerMTU
	}
	t := &rawTx{up: up, pool: make([]loomdp.Frame, poolDepth)}
	for i := range t.pool {
		t.pool[i].Data = make([]byte, frameSize)
	}
	return t
}

func (t *rawTx) Name() string { return "orbit-gtp" }
func (t *rawTx) Caps() loomdp.Capabilities {
	return loomdp.Capabilities{RawL3: true} // frames are complete IP packets
}
func (t *rawTx) Close() error { return nil }

func (t *rawTx) TxReserve(n int) []loomdp.Frame {
	if n > len(t.pool) {
		n = len(t.pool)
	}
	for i := 0; i < n; i++ {
		t.pool[i].Len = 0
	}
	return t.pool[:n]
}

func (t *rawTx) TxCommit(frames []loomdp.Frame) (int, error) {
	sent := 0
	for i := range frames {
		if frames[i].Len == 0 {
			continue
		}
		if err := t.up.SendUplink(frames[i].Data[:frames[i].Len]); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

// payloadTx is the legacy transmit variant for RunFlow: loom's stream
// generator fills frames with raw application payloads (not IP packets), so
// TxCommit wraps each one in an inner IPv4+UDP packet before sending it
// uplink. It does NOT advertise RawL3 — dgram.New rightly refuses it.
type payloadTx struct {
	up               Uplink
	src, dst         net.IP
	srcPort, dstPort uint16
	pool             []loomdp.Frame
}

func newPayloadTx(up Uplink, src, dst net.IP, srcPort, dstPort uint16, frameSize int) *payloadTx {
	if frameSize <= 0 {
		frameSize = 1400
	}
	t := &payloadTx{up: up, src: src, dst: dst, srcPort: srcPort, dstPort: dstPort}
	t.pool = make([]loomdp.Frame, poolDepth)
	for i := range t.pool {
		t.pool[i].Data = make([]byte, frameSize)
	}
	return t
}

func (t *payloadTx) Name() string              { return "orbit-gtp-payload" }
func (t *payloadTx) Caps() loomdp.Capabilities { return loomdp.Capabilities{} }
func (t *payloadTx) Close() error              { return nil }

func (t *payloadTx) TxReserve(n int) []loomdp.Frame {
	if n > len(t.pool) {
		n = len(t.pool)
	}
	for i := 0; i < n; i++ {
		t.pool[i].Len = 0
	}
	return t.pool[:n]
}

func (t *payloadTx) TxCommit(frames []loomdp.Frame) (int, error) {
	sent := 0
	for i := range frames {
		if frames[i].Len == 0 {
			continue
		}
		pkt, err := datapath.BuildUDPPacket(t.src, t.dst, t.srcPort, t.dstPort, frames[i].Data[:frames[i].Len])
		if err != nil {
			return sent, err
		}
		if err := t.up.SendUplink(pkt); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

// Config parameterises one UDP flow over the GTP-U tunnel.
type Config struct {
	Uplink     Uplink
	UEIP       net.IP // source (PDU-session) address
	SrcPort    uint16 // UE source port (default 5001)
	Target     string // "host:port" destination
	PacketSize int    // inner UDP payload size (default 1200)
	Rate       string // loom rate string, e.g. "10Mbps"; "" = unlimited
	Duration   time.Duration
}

// Result is the throughput of one flow.
type Result struct {
	Bytes, Packets uint64
	Duration       time.Duration
	Mbps           float64 // application-layer megabits/sec (payload only)
}

// RunFlow drives one loom UDP flow over the GTP-U tunnel and returns its
// throughput. It embeds loom via an injected component set with ORBIT's
// legacy payload datapath registered as "orbit-gtp-payload" (the stream
// generator hands raw payloads, so the datapath builds the inner headers).
func RunFlow(ctx context.Context, cfg Config) (Result, error) {
	host, portStr, err := net.SplitHostPort(cfg.Target)
	if err != nil {
		return Result{}, fmt.Errorf("target %q: %w", cfg.Target, err)
	}
	dstIP := net.ParseIP(host)
	dstPort, err := strconv.Atoi(portStr)
	if err != nil || dstIP == nil {
		return Result{}, fmt.Errorf("invalid target %q", cfg.Target)
	}
	if cfg.UEIP == nil {
		return Result{}, errors.New("UEIP is required")
	}
	srcPort := cfg.SrcPort
	if srcPort == 0 {
		srcPort = 5001
	}
	psize := cfg.PacketSize
	if psize <= 0 {
		psize = 1200
	}

	comps := components.Default()
	tx := registry.New[loomdp.TxDatapath, loomdp.Options]()
	tx.Register("orbit-gtp-payload", func(o loomdp.Options) (loomdp.TxDatapath, error) {
		return newPayloadTx(cfg.Uplink, cfg.UEIP, dstIP, srcPort, uint16(dstPort), o.FrameSize), nil
	})
	comps.TxDatapaths = tx

	f, err := flow.Build(flow.Spec{
		Datapath:   "orbit-gtp-payload",
		Target:     cfg.Target,
		PacketSize: psize,
		Rate:       cfg.Rate,
		Duration:   cfg.Duration,
	}, comps)
	if err != nil {
		return Result{}, err
	}

	start := time.Now()
	if err := f.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return Result{}, err
	}
	dur := time.Since(start)
	c := f.Counters()
	b, p := c.Bytes(), c.Packets()
	mbps := 0.0
	if dur > 0 {
		mbps = float64(b*8) / dur.Seconds() / 1e6
	}
	return Result{Bytes: b, Packets: p, Duration: dur, Mbps: mbps}, nil
}
