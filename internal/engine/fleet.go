package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/sctp"
)

// GNBSpec configures one gNB in a fleet: its identity plus where its N2
// association goes and (optionally) which local source address it binds.
type GNBSpec struct {
	Config  gnb.Config
	AMFAddr string
	// BindAddr is the local source address for this gNB's association. A
	// distinct address per gNB avoids SCTP source collisions when several
	// gNBs target one AMF (PacketRusher #138); empty lets the OS choose.
	BindAddr string
}

// n2Conn is an N2 association as the engine needs it: a gNB transport that
// can also report the local address the kernel bound, so callers can show
// the N2 source actually in use.
type n2Conn interface {
	gnb.Transport
	LocalAddr() net.Addr
}

// n2Dialer opens one N2 association from laddr (see sctp.NormalizeLocal for
// the accepted forms; "" lets the kernel choose) to the AMF at raddr. It is
// the seam that lets the bind plumbing be tested against an in-process AMF
// without kernel SCTP; production code uses dialN2.
type n2Dialer func(laddr, raddr string) (n2Conn, error)

// dialN2 is the production n2Dialer: a real SCTP association.
func dialN2(laddr, raddr string) (n2Conn, error) {
	return sctp.Dial(laddr, raddr)
}

// localAddrString renders a connection's bound local address, or "" when the
// transport cannot report one.
func localAddrString(c n2Conn) string {
	if a := c.LocalAddr(); a != nil {
		return a.String()
	}
	return ""
}

// ExpandBindAddrs maps the N2 source addresses an operator supplied onto n
// gNBs. The rule is the same on every surface (CLI --bind, LoadRunSpec
// bind_addresses):
//
//   - none  → every gNB lets the kernel choose (today's behaviour)
//   - one   → every gNB shares that address (gNBs then differ only by ID, so
//     callers should say so)
//   - n     → one address per gNB, in order
//   - other → an error naming both counts
//
// Addresses are not validated here; sctp.Dial normalises and rejects them at
// dial time, in one place.
func ExpandBindAddrs(binds []string, n int) ([]string, error) {
	switch {
	case len(binds) == 0:
		return make([]string, n), nil
	case len(binds) == 1:
		out := make([]string, n)
		for i := range out {
			out[i] = binds[0]
		}
		return out, nil
	case len(binds) == n:
		return append([]string(nil), binds...), nil
	default:
		return nil, fmt.Errorf("%d bind addresses for %d gNBs: pass none (kernel chooses), one (shared by every gNB), or exactly %d (one per gNB)",
			len(binds), n, n)
	}
}

// Fleet holds one gNB.Session (one N2 association) per gNB and registers UEs
// across them. A real gNB muxes its whole UE population over a single
// association, so this is one association per gNB, not per UE.
type Fleet struct {
	sessions []*gnb.Session
	sources  []string // N2 source address per session, as actually bound
	log      *slog.Logger
}

// NewFleet dials one association per gNB (distinct bind address where given)
// and performs NG Setup on each. All associations are torn down by Close.
func NewFleet(ctx context.Context, gnbs []GNBSpec, log *slog.Logger) (*Fleet, error) {
	return newFleet(ctx, gnbs, log, dialN2)
}

// newFleet is NewFleet with the N2 dialer injected.
func newFleet(ctx context.Context, gnbs []GNBSpec, log *slog.Logger, dial n2Dialer) (*Fleet, error) {
	f := &Fleet{log: log}
	for _, g := range gnbs {
		conn, err := dial(g.BindAddr, g.AMFAddr)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("gNB %d dial %s: %w", g.Config.ID, g.AMFAddr, err)
		}
		sess, err := gnb.Dial(ctx, conn, g.Config)
		if err != nil {
			conn.Close()
			f.Close()
			return nil, fmt.Errorf("gNB %d NG Setup: %w", g.Config.ID, err)
		}
		f.sessions = append(f.sessions, sess)
		f.sources = append(f.sources, localAddrString(conn))
	}
	return f, nil
}

// N2Sources returns the local N2 address each gNB's association actually
// bound, in gNB order — the real port even when the spec asked for :0, and
// the kernel's choice when the spec left BindAddr empty.
func (f *Fleet) N2Sources() []string {
	return append([]string(nil), f.sources...)
}

// FleetResult summarises a fleet registration.
type FleetResult struct {
	Registered int
	Failed     int
	FirstError error
}

// Register attaches the UEs across the fleet's gNBs (round-robin by index)
// with at most `workers` concurrent attaches — the D-6-informed bound that
// keeps an attach storm from becoming a scheduler pile-up. Each UE is muxed
// onto its gNB's shared association via a per-UE transport.
func (f *Fleet) Register(ctx context.Context, ues []UEConfig, workers int) *FleetResult {
	if workers <= 0 {
		workers = 64
	}
	var registered, failed atomic.Int64
	var firstErr atomic.Pointer[error]

	jobs := make(chan int, len(ues))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				sess := f.sessions[i%len(f.sessions)]
				uet, ranID := sess.NewUE()
				cfg := ues[i]
				cfg.RANUENGAPID = ranID
				gnbCfg := f.gnbConfigFor(i)
				res, err := Attach(ctx, uet, gnbCfg, cfg, f.log, nil)
				uet.Close()
				if err != nil || !res.Result.Registered {
					failed.Add(1)
					if err != nil && firstErr.Load() == nil {
						firstErr.CompareAndSwap(nil, &err)
					}
					continue
				}
				registered.Add(1)
			}
		}()
	}
	for i := range ues {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	out := &FleetResult{Registered: int(registered.Load()), Failed: int(failed.Load())}
	if p := firstErr.Load(); p != nil {
		out.FirstError = *p
	}
	return out
}

// gnbConfigFor returns the gNB config that UE i attaches through — the
// session's own config, kept alongside it.
func (f *Fleet) gnbConfigFor(i int) gnb.Config {
	return f.sessions[i%len(f.sessions)].Config()
}

// Close tears down all associations.
func (f *Fleet) Close() {
	for _, s := range f.sessions {
		if s != nil {
			s.Close()
		}
	}
}
