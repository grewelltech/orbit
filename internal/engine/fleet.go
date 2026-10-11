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
// gNBs and returns them in sctp.NormalizeLocal's host:port form. The rule is
// the same on every surface (CLI --bind, LoadRunSpec bind_addresses):
//
//   - none  → every gNB lets the kernel choose (today's behaviour)
//   - one   → every gNB shares that address (gNBs then differ only by ID, so
//     callers should say so)
//   - n     → one address per gNB, in order
//   - other → an error naming both counts
//
// Port rule: an explicit non-zero port is legal only when exactly one
// association will use that ip:port. So a shared address for more than one
// gNB must have port 0 (each association then gets its own ephemeral port),
// and two gNBs in the one-per-gNB list may not name the same non-zero
// ip:port; the same ip:0 twice is just sharing and is allowed. Each address
// is normalised here so a malformed one fails before anything is dialled;
// host resolution and ownership are still the dial's to report.
func ExpandBindAddrs(binds []string, n int) ([]string, error) {
	if n < 1 {
		return nil, fmt.Errorf("bind addresses for %d gNBs: need at least one gNB", n)
	}
	norm := make([]string, len(binds))
	for i, b := range binds {
		nb, err := sctp.NormalizeLocal(b)
		if err != nil {
			return nil, err
		}
		if nb == "" {
			return nil, fmt.Errorf("bind address %d is empty; omit --bind entirely to let the kernel choose", i+1)
		}
		norm[i] = nb
	}
	switch {
	case len(norm) == 0:
		return make([]string, n), nil
	case len(norm) == 1:
		if n > 1 && !ephemeralPort(norm[0]) {
			return nil, fmt.Errorf("shared bind address %q has a fixed port, which only one of the %d gNBs could bind: give the IP alone (or ip:0), or one address per gNB",
				norm[0], n)
		}
		out := make([]string, n)
		for i := range out {
			out[i] = norm[0]
		}
		return out, nil
	case len(norm) == n:
		seen := make(map[string]int, n)
		for i, b := range norm {
			if ephemeralPort(b) {
				continue
			}
			if j, dup := seen[b]; dup {
				return nil, fmt.Errorf("bind addresses %d and %d are both %q with a fixed port, which only one gNB could bind: use port 0 or distinct ports",
					j+1, i+1, b)
			}
			seen[b] = i
		}
		return norm, nil
	default:
		return nil, fmt.Errorf("%d bind addresses for %d gNBs: pass none (route-selected source), one (shared by every gNB), or exactly %d (one per gNB)",
			len(norm), n, n)
	}
}

// ephemeralPort reports whether a normalised host:port asks for port 0.
func ephemeralPort(hostport string) bool {
	_, port, err := net.SplitHostPort(hostport)
	return err == nil && port == "0"
}

// n2SourceFor returns the N2 source bound for the gNB serving UE index i.
func (f *Fleet) n2SourceFor(i int) string {
	return f.sources[i%len(f.sources)]
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
// the route-selected address when the spec left BindAddr empty.
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
