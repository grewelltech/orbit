package engine

import (
	"context"
	"fmt"
	"log/slog"
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

// Fleet holds one gNB.Session (one N2 association) per gNB and registers UEs
// across them. A real gNB muxes its whole UE population over a single
// association, so this is one association per gNB, not per UE.
type Fleet struct {
	sessions []*gnb.Session
	log      *slog.Logger
}

// NewFleet dials one association per gNB (distinct bind address where given)
// and performs NG Setup on each. All associations are torn down by Close.
func NewFleet(ctx context.Context, gnbs []GNBSpec, log *slog.Logger) (*Fleet, error) {
	f := &Fleet{log: log}
	for _, g := range gnbs {
		conn, err := sctp.Dial(g.BindAddr, g.AMFAddr)
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
	}
	return f, nil
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
