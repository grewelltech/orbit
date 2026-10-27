package server

import (
	"fmt"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/armplan"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/scenario"
)

// The old run arms (load, fleet) run as one-workload plans (R2), so every
// run takes the plan's path to RunPopulation. An arm a plan cannot express
// exactly runs from its own spec instead, as before: today that is a load
// arm whose gNBs differ in more than their IDs and addresses (a plan's
// gNBs share the target's PLMN, TAC, slices and name), or a fleet whose
// text the typed behaviours would rewrite.

// armRun is an old arm, built: through its plan, or from its own spec.
type armRun struct {
	viaPlan bool
	load    engine.LoadRunFunc
	fleet   engine.FleetRunFunc
	needs   engine.Needs
	storm   engine.LoadSpec       // the storm path's engine spec, as run
	built   engine.FleetRunSpec   // the persistent path's engine spec, as run
	beh     engine.FleetBehaviors // and its behaviours
}

// loadArm builds a load arm: as its plan when a plan expresses it, else
// from its own spec. A plan that will not build falls back too, so the
// arm's own errors are the ones its client sees.
func (s *runService) loadArm(p *orbitv1.LoadRunSpec, verbosity engine.EventVerbosity) (armRun, error) { //nolint:staticcheck // the deprecated arm, kept for API clients
	if plan, err := armplan.Load(p); err == nil {
		pr, err := s.buildPlan(plan, verbosity)
		if err == nil {
			return armRun{viaPlan: true, load: pr.load, needs: pr.needs, storm: pr.storm}, nil
		}
		s.log.Debug("load arm runs from its own spec", "err", err)
	} else {
		s.log.Debug("load arm runs from its own spec", "err", err)
	}
	spec, err := loadSpecFrom(p)
	if err != nil {
		return armRun{}, err
	}
	fn, needs, err := loadRunFrom(s.log, spec)
	if err != nil {
		return armRun{}, err
	}
	return armRun{load: fn, needs: needs, storm: spec}, nil
}

// fleetArm builds a fleet arm: as its plan when a plan expresses it, else
// from its scenario. It is persistent with or without behaviours, as a
// fleet always was.
func (s *runService) fleetArm(p *orbitv1.FleetRunSpec, verbosity engine.EventVerbosity) (armRun, error) { //nolint:staticcheck // the deprecated arm, kept for API clients
	if p.GetScenarioYaml() == "" {
		return armRun{}, fmt.Errorf("scenario_yaml is required")
	}
	// Parse WITHOUT ${ENV} expansion: the YAML is client-supplied, and
	// expanding it against the server's environment would leak the
	// server's variables back to the client. Credentials come from the
	// request.
	f, err := scenario.ParseFleetNoEnv([]byte(p.GetScenarioYaml()))
	if err != nil {
		return armRun{}, err
	}
	if plan, err := armplan.Fleet(f, p.GetCredentials().GetKi(), p.GetCredentials().GetOpc()); err == nil {
		pr, err := s.buildPlanAs(plan, verbosity, true)
		if err == nil {
			return armRun{viaPlan: true, fleet: pr.fleet, needs: pr.needs, built: pr.built, beh: pr.beh}, nil
		}
		s.log.Debug("fleet arm runs from its scenario", "err", err)
	} else {
		s.log.Debug("fleet arm runs from its scenario", "err", err)
	}
	spec, beh, err := fleetSpecFrom(f, p.GetCredentials())
	if err != nil {
		return armRun{}, err
	}
	fn, needs, err := fleetRunFrom(s.log, s.mgr, spec, beh, verbosity)
	if err != nil {
		return armRun{}, err
	}
	return armRun{fleet: fn, needs: needs, built: spec, beh: beh}, nil
}
