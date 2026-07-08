package engine

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// FleetGNB is one generated gNB in a fleet run: its NGAP config, the local
// source address to bind (distinct per gNB, for handover), and its N3 address
// for the data path (usually the same IP).
type FleetGNB struct {
	Config   gnb.Config
	BindAddr string
	N3Addr   string
}

// FleetRunSpec parameterises a fleet attach (ADR-0004). It is the direct-drive
// analogue of LoadSpec, with a distinct gNB (source IP + N3) per entry rather
// than one shared N3. UEs are spread round-robin across the gNBs (even
// distribution), so UE i attaches through GNBs[i % len(GNBs)] using that gNB's
// N3 for its session.
type FleetRunSpec struct {
	AMFAddr     string
	GNBs        []FleetGNB
	BaseIMSI    string
	Count       int
	MCC, MNC    string
	Ki, OPc     []byte
	Rate        load.Rate // offered attach rate (nil = concurrency-bound)
	Concurrency int
	PDUSession  *ue.PDUSessionParams // per-UE session using the serving gNB's N3
}

// RunFleet brings up one association per gNB, generates Count UEs from BaseIMSI,
// and attaches them across the gNBs at the offered rate — the fleet-mode attach
// phase. It returns the per-procedure KPI report (integration-capacity numbers,
// bounded by the core under test). Mobility and traffic behaviours run on top of
// the attached fleet (added by later chunks).
func RunFleet(ctx context.Context, log *slog.Logger, spec FleetRunSpec) (load.Report, error) {
	if len(spec.GNBs) == 0 {
		return load.Report{}, fmt.Errorf("fleet run needs at least one gNB")
	}
	specs := make([]GNBSpec, len(spec.GNBs))
	for i, g := range spec.GNBs {
		specs[i] = GNBSpec{AMFAddr: spec.AMFAddr, Config: g.Config, BindAddr: g.BindAddr}
	}
	f, err := NewFleet(ctx, specs, log)
	if err != nil {
		return load.Report{}, err
	}
	defer f.Close()

	makeUE := func(i int) (UEConfig, error) {
		supi, err := incIMSI(spec.BaseIMSI, i)
		if err != nil {
			return UEConfig{}, err
		}
		id, err := ue.ParseIdentity(supi, spec.MCC, spec.MNC, "0")
		if err != nil {
			return UEConfig{}, err
		}
		cfg := UEConfig{Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: spec.Ki, OPc: spec.OPc}}
		if spec.PDUSession != nil {
			p := *spec.PDUSession
			cfg.PDUSession = &p
			cfg.GNBN3Addr = spec.GNBs[i%len(spec.GNBs)].N3Addr
		}
		return cfg, nil
	}

	rep := load.Run(ctx, load.Config{
		Total: spec.Count, Concurrency: spec.Concurrency, Rate: spec.Rate,
	}, f.LoadFunc(makeUE))
	return rep, nil
}
