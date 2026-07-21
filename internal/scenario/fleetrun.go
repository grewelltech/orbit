package scenario

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/ue"
)

// BuildFleetRun turns a parsed fleet scenario plus subscriber keys into the
// engine's run spec and behaviours. It is the single place that mapping lives,
// so `orbit run <fleet>` (CLI) and the RunService (server) build identical runs
// from the same scenario — credentials are passed in rather than read from the
// scenario so each caller controls where secrets come from (the CLI expands
// ${ENV}; the server takes them from the request).
func BuildFleetRun(f *FleetScenario, ki, opc []byte) (engine.FleetRunSpec, engine.FleetBehaviors, error) {
	gnbs := f.GenGNBs()
	if len(gnbs) == 0 {
		return engine.FleetRunSpec{}, engine.FleetBehaviors{}, fmt.Errorf("fleet generated no gNBs")
	}

	rateRPS, err := ParseAttachRate(f.Fleet.AttachRate)
	if err != nil {
		return engine.FleetRunSpec{}, engine.FleetBehaviors{}, err
	}

	fgnbs := make([]engine.FleetGNB, len(gnbs))
	for i, pg := range gnbs {
		fgnbs[i] = engine.FleetGNB{
			Config: gnb.Config{
				ID: pg.GNB.ID, Name: pg.GNB.Name,
				MCC: f.Core.PLMN.MCC, MNC: f.Core.PLMN.MNC, TAC: f.Core.TAC,
				Slices: []gnb.SNSSAI{{SST: uint8(f.Core.Slice.SST), SD: f.Core.Slice.SD}},
			},
			BindAddr: pg.GNB.Bind,
			N3Addr:   pg.GNB.N3,
			X:        pg.X,
			Y:        pg.Y,
		}
	}

	spec := engine.FleetRunSpec{
		AMFAddr: f.Core.AMF, GNBs: fgnbs,
		BaseIMSI: f.Fleet.SUPIBase, Count: f.Fleet.Count,
		MCC: f.Core.PLMN.MCC, MNC: f.Core.PLMN.MNC,
		Ki: ki, OPc: opc, RateRPS: rateRPS, Concurrency: 64,
	}
	if f.Fleet.PDUSession {
		spec.PDUSession = &ue.PDUSessionParams{
			PDUSessionID: 1, SST: uint8(f.Core.Slice.SST), SD: f.Core.Slice.SD, DNN: f.Core.DNN,
		}
	}

	var beh engine.FleetBehaviors
	if f.Run.Duration != "" {
		d, err := time.ParseDuration(f.Run.Duration)
		if err != nil {
			return engine.FleetRunSpec{}, engine.FleetBehaviors{}, fmt.Errorf("run.duration %q: %w", f.Run.Duration, err)
		}
		beh.Duration = d
	}
	if f.Behaviors.Mobility != nil {
		beh.MobileUEs = f.Fleet.Count / 2
		if beh.MobileUEs < 1 {
			beh.MobileUEs = 1
		}
	}
	if t := f.Behaviors.Traffic; t != nil && f.Fleet.PDUSession {
		// Synthetic profiles → loom constant-rate flows (first synthetic entry's
		// rate applies, the preview behaviour).
		for _, m := range t.Mix {
			if m.Profile == "" {
				continue
			}
			beh.Traffic = true
			beh.TrafficRate = m.Rate
			if beh.TrafficRate == "" {
				beh.TrafficRate = "10Mbps"
			}
			beh.TrafficTarget = "8.8.8.8:9999"
			break
		}
	}
	// App cohorts (design §8): mix entries with app: become real-application
	// cohorts, sized by the same share allocation as the profiles.
	for _, c := range f.AppCohorts() {
		beh.Apps = append(beh.Apps, engine.FleetAppCohort{
			Name: c.Name, App: c.App,
			Peer: c.Peer, Token: c.Token, PeerDataIP: c.PeerDataIP,
			Params: c.Params, Count: c.Count,
		})
	}

	return spec, beh, nil
}

// ParseAttachRate turns "10/s" (or "10") into an attaches/sec rate; empty = 0.
func ParseAttachRate(s string) (float64, error) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "/s")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	r, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("attach_rate %q: %w", s, err)
	}
	if r < 0 {
		return 0, nil
	}
	return r, nil
}
