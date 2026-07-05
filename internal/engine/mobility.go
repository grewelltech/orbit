package engine

import (
	"context"
	"time"

	"github.com/bgrewell/orbit/internal/meas"
)

// HandoverExecutor performs the network-side handover for one measurement
// trigger. The live implementation drives the N2 handover FSM (internal/gnb:
// BuildHandoverRequired → … → HandoverNotify) between the gNBs that host the
// serving and target cells; tests use a fake. Keeping execution behind this
// interface lets the mobility decision layer be exercised without a core.
type HandoverExecutor interface {
	Handover(ctx context.Context, t meas.Trigger) error
}

// MobilityController closes the mobility loop: it runs a synthesized mobility
// scenario (meas) to decide when and where a UE hands over, and executes each
// handover via the executor. Decision (spec-grounded, offline) is separated
// from execution (core-bound) so each is independently testable.
type MobilityController struct {
	Scenario meas.Scenario
	Exec     HandoverExecutor
}

// Run evaluates the scenario from base time and executes each resulting
// handover in order, stopping at the first execution error. It returns the
// triggers that were produced (whether or not all executed).
func (m MobilityController) Run(ctx context.Context, base time.Time) ([]meas.Trigger, error) {
	triggers := m.Scenario.Run(base)
	for _, t := range triggers {
		if err := ctx.Err(); err != nil {
			return triggers, err
		}
		if err := m.Exec.Handover(ctx, t); err != nil {
			return triggers, err
		}
	}
	return triggers, nil
}
