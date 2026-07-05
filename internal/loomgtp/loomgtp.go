// Package loomgtp bridges the loom traffic generator to ORBIT's userspace
// GTP-U tunnel. It registers a loom TxDatapath (implementing loom's
// datapath.TxDatapath frame contract) that, instead of opening a kernel
// socket, wraps each generated payload in a native inner IPv4+UDP packet
// sourced from the UE's PDU-session IP and sends it up the N3 tunnel.
//
// This is the design's Mode B: loom is the traffic engine and the source
// binding lives in ORBIT's tunnel, so one process drives many UEs with no TUN
// device, no NET_ADMIN, and no per-UE netns. loom is embedded unmodified via
// its injectable components.Components (flow.Build(spec, c)); nothing here
// forks loom. See docs/DESIGN.md (loom addendum).
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

// Uplink is the GTP-U send side this bridge needs; *datapath.Tunnel satisfies
// it. Kept minimal so tests can inject a loopback.
type Uplink interface {
	SendUplink(innerIP []byte) error
}

const poolDepth = 64

// txDatapath implements loom's datapath.TxDatapath over a GTP-U Uplink. The
// pump reserves frames, the generator fills them, and TxCommit wraps each
// filled payload in an inner UDP packet and sends it. The pump reserves →
// fills → commits synchronously, so reusing one frame pool is safe.
type txDatapath struct {
	up               Uplink
	src, dst         net.IP
	srcPort, dstPort uint16
	pool             []loomdp.Frame
}

func newTx(up Uplink, src, dst net.IP, srcPort, dstPort uint16, frameSize int) *txDatapath {
	if frameSize <= 0 {
		frameSize = 1400
	}
	t := &txDatapath{up: up, src: src, dst: dst, srcPort: srcPort, dstPort: dstPort}
	t.pool = make([]loomdp.Frame, poolDepth)
	for i := range t.pool {
		t.pool[i].Data = make([]byte, frameSize)
	}
	return t
}

func (t *txDatapath) Name() string              { return "orbit-gtp" }
func (t *txDatapath) Caps() loomdp.Capabilities { return loomdp.Capabilities{} }
func (t *txDatapath) Close() error              { return nil }

func (t *txDatapath) TxReserve(n int) []loomdp.Frame {
	if n > len(t.pool) {
		n = len(t.pool)
	}
	for i := 0; i < n; i++ {
		t.pool[i].Len = 0
	}
	return t.pool[:n]
}

func (t *txDatapath) TxCommit(frames []loomdp.Frame) (int, error) {
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
// datapath registered as "orbit-gtp".
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
	tx.Register("orbit-gtp", func(o loomdp.Options) (loomdp.TxDatapath, error) {
		return newTx(cfg.Uplink, cfg.UEIP, dstIP, srcPort, uint16(dstPort), o.FrameSize), nil
	})
	comps.TxDatapaths = tx

	f, err := flow.Build(flow.Spec{
		Datapath:   "orbit-gtp",
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
