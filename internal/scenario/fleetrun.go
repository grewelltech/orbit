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
	assign, err := GNBAssignment(f.Fleet.Count, len(fgnbs), f.Fleet.Distribution, f.Fleet.DistributionSeed)
	if err != nil {
		return engine.FleetRunSpec{}, engine.FleetBehaviors{}, err
	}
	spec.GNBAssign = assign
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
	if m := f.Behaviors.Mobility; m != nil {
		// The handover kind was parsed and then ignored — fleet mobility runs
		// an Xn PathSwitch regardless — so a scenario asking for N2 silently
		// got Xn and its results were mislabelled. Refuse rather than
		// pretend; single-UE N2 handover is available via `orbit ue handover`.
		switch strings.ToLower(strings.TrimSpace(m.Handover)) {
		case "", "xn":
		case "n2":
			return engine.FleetRunSpec{}, engine.FleetBehaviors{},
				fmt.Errorf("behaviors.mobility.handover: fleet mobility performs Xn path switches only; " +
					"n2 is not implemented for fleet runs (use `orbit ue handover` for a single UE)")
		default:
			return engine.FleetRunSpec{}, engine.FleetBehaviors{},
				fmt.Errorf("behaviors.mobility.handover %q: want \"xn\" (or omit)", m.Handover)
		}
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
			// The entry's own target, when it names one. The previous
			// hardcoded default silently ignored `target:`, so a scenario
			// aimed at its own N6 responder was really addressing 8.8.8.8
			// and only looked right because uplink counts either way.
			beh.TrafficTarget = m.Target
			if beh.TrafficTarget == "" {
				beh.TrafficTarget = defaultTrafficTarget
			}
			break
		}
	}
	if l := f.Behaviors.Latency; l != nil && l.Target != "" && f.Fleet.PDUSession {
		beh.Latency = engine.FleetLatencyProbe{Target: l.Target, UEs: l.UEs}
		if l.Interval != "" {
			d, err := time.ParseDuration(l.Interval)
			if err != nil {
				return engine.FleetRunSpec{}, engine.FleetBehaviors{},
					fmt.Errorf("behaviors.latency.interval %q: %w", l.Interval, err)
			}
			beh.Latency.Interval = d
		}
		if l.Timeout != "" {
			d, err := time.ParseDuration(l.Timeout)
			if err != nil {
				return engine.FleetRunSpec{}, engine.FleetBehaviors{},
					fmt.Errorf("behaviors.latency.timeout %q: %w", l.Timeout, err)
			}
			beh.Latency.Timeout = d
		}
	}
	// App cohorts (design §8): mix entries with app: become real-application
	// cohorts, sized by the same share allocation as the profiles.
	for _, c := range f.AppCohorts() {
		cohort := engine.FleetAppCohort{
			Name: c.Name, App: c.App,
			Peer: c.Peer, Token: c.Token, PeerDataIP: c.PeerDataIP,
			Params: c.Params, Count: c.Count,
		}
		if c.StartAfter != "" {
			d, err := time.ParseDuration(c.StartAfter)
			if err != nil {
				return engine.FleetRunSpec{}, engine.FleetBehaviors{},
					fmt.Errorf("traffic cohort %q start_after %q: %w", c.Name, c.StartAfter, err)
			}
			if d < 0 {
				return engine.FleetRunSpec{}, engine.FleetBehaviors{},
					fmt.Errorf("traffic cohort %q start_after %q must not be negative", c.Name, c.StartAfter)
			}
			if beh.Duration > 0 && d >= beh.Duration {
				return engine.FleetRunSpec{}, engine.FleetBehaviors{},
					fmt.Errorf("traffic cohort %q starts after %s but the run is only %s, so it would never run",
						c.Name, d, beh.Duration)
			}
			cohort.StartAfter = d
		}
		beh.Apps = append(beh.Apps, cohort)
	}

	return spec, beh, nil
}

// defaultTrafficTarget is where synthetic flows aim when a mix entry names no
// target: an off-testbed address, so the traffic traverses the UPF and its N6
// egress rather than looping back inside the lab.
const defaultTrafficTarget = "8.8.8.8:9999"

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
