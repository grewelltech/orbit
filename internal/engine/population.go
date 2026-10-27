package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/ue"
)

// Strategy is how a population's UEs live (ADR-0008 §2, R2).
type Strategy int

const (
	// Storm attaches each UE and releases it at once, rate-paced, for Count
	// attaches or for Duration, cycling the SUPIs: today's load run, so
	// its numbers stay comparable with history.
	Storm Strategy = iota + 1
	// Persistent attaches each UE once and keeps it while the behaviours
	// run, then deregisters it: today's fleet run.
	Persistent
)

// PopulationSpec is a UE population, the one spec both strategies run.
type PopulationSpec struct {
	Strategy Strategy // 0 = Storm without behaviours, Persistent with
	AMFAddr  string
	// GNBs carry each gNB's config, N2 bind and (Persistent) N3 address.
	GNBs     []FleetGNB
	BaseIMSI string
	Count    int
	MCC, MNC string
	Ki, OPc  []byte
	// Rate is the attach-rate curve (nil = concurrency-bound).
	Rate        load.Rate
	Concurrency int
	// Duration is how long a Storm keeps attaching (a soak); 0 = Count.
	Duration    time.Duration
	SampleEvery time.Duration
	Observer    load.Observer
	GNBAssign   []int                // Persistent: UE index → gNB index
	PDUSession  *ue.PDUSessionParams // per-UE session
	GNBN3Addr   string               // Storm: the gNB N3 shared for sessions
}

// PopulationReport is a population's end-of-run report: Load for a Storm,
// Fleet for a Persistent run.
type PopulationReport struct {
	Load  *load.Report
	Fleet *FleetReport
}

// hasBehaviors reports whether any behaviour is configured.
func (b FleetBehaviors) hasBehaviors() bool {
	return b.Duration > 0 || b.MobileUEs > 0 || b.Traffic || b.Bulk.UEs > 0 || b.Bulk.Peer != "" ||
		b.Latency.Target != "" || len(b.Apps) > 0
}

// RunPopulation runs a UE population: the one executor every run kind uses
// (RunLoad and RunFleet are wrappers). Its attach phase is internal/load's
// controller either way; the strategy decides whether UEs are kept.
func RunPopulation(ctx context.Context, log *slog.Logger, spec PopulationSpec, beh FleetBehaviors, deps FleetDeps) (PopulationReport, error) {
	st := spec.Strategy
	if st == 0 {
		st = Storm
		if beh.hasBehaviors() {
			st = Persistent
		}
	}
	switch st {
	case Storm:
		if beh.hasBehaviors() {
			return PopulationReport{}, fmt.Errorf("a storm population has no behaviours: its UEs are released as they attach")
		}
		r, err := runStorm(ctx, log, spec.loadSpec())
		return PopulationReport{Load: &r}, err
	case Persistent:
		r, err := runPersistent(ctx, log, spec.fleetSpec(), beh, deps)
		return PopulationReport{Fleet: &r}, err
	}
	return PopulationReport{}, fmt.Errorf("unknown population strategy %d", st)
}

// PopulationFromLoad is a load spec as a Storm population.
func PopulationFromLoad(s LoadSpec) PopulationSpec {
	ps := PopulationSpec{Strategy: Storm, BaseIMSI: s.BaseIMSI, Count: s.Count, MCC: s.MCC, MNC: s.MNC,
		Ki: s.Ki, OPc: s.OPc, Rate: s.Rate, Concurrency: s.Concurrency, Duration: s.Duration,
		SampleEvery: s.SampleEvery, Observer: s.Observer, PDUSession: s.PDUSession, GNBN3Addr: s.GNBN3Addr}
	for _, g := range s.GNBs {
		if ps.AMFAddr == "" {
			ps.AMFAddr = g.AMFAddr
		}
		ps.GNBs = append(ps.GNBs, FleetGNB{Config: g.Config, BindAddr: g.BindAddr})
	}
	return ps
}

// PopulationFromFleet is a fleet spec as a population (Strategy unset:
// chosen by the behaviours).
func PopulationFromFleet(s FleetRunSpec) PopulationSpec {
	rate := s.Rate
	if rate == nil && s.RateRPS > 0 {
		rate = load.Constant{RPS: s.RateRPS}
	}
	return PopulationSpec{AMFAddr: s.AMFAddr, GNBs: s.GNBs, BaseIMSI: s.BaseIMSI, Count: s.Count, MCC: s.MCC, MNC: s.MNC,
		Ki: s.Ki, OPc: s.OPc, Rate: rate, Concurrency: s.Concurrency, SampleEvery: s.SampleEvery, Observer: s.Observer,
		GNBAssign: s.GNBAssign, PDUSession: s.PDUSession}
}

// loadSpec is the population as the storm engine's spec. Every gNB dials
// the population's AMF.
func (p PopulationSpec) loadSpec() LoadSpec {
	ls := LoadSpec{BaseIMSI: p.BaseIMSI, Count: p.Count, MCC: p.MCC, MNC: p.MNC, Ki: p.Ki, OPc: p.OPc,
		Concurrency: p.Concurrency, Rate: p.Rate, Duration: p.Duration, SampleEvery: p.SampleEvery,
		PDUSession: p.PDUSession, GNBN3Addr: p.GNBN3Addr, Observer: p.Observer}
	for _, g := range p.GNBs {
		ls.GNBs = append(ls.GNBs, GNBSpec{Config: g.Config, AMFAddr: p.AMFAddr, BindAddr: g.BindAddr})
	}
	return ls
}

// fleetSpec is the population as the persistent engine's spec.
func (p PopulationSpec) fleetSpec() FleetRunSpec {
	return FleetRunSpec{AMFAddr: p.AMFAddr, GNBs: p.GNBs, BaseIMSI: p.BaseIMSI, Count: p.Count, MCC: p.MCC, MNC: p.MNC,
		Ki: p.Ki, OPc: p.OPc, Rate: p.Rate, Concurrency: p.Concurrency, SampleEvery: p.SampleEvery, Observer: p.Observer,
		GNBAssign: p.GNBAssign, PDUSession: p.PDUSession}
}
