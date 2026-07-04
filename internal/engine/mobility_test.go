package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/meas"
)

// fakeExecutor records the handovers it is asked to perform.
type fakeExecutor struct {
	targets []int64
	err     error
}

func (f *fakeExecutor) Handover(_ context.Context, t meas.Trigger) error {
	if f.err != nil {
		return f.err
	}
	f.targets = append(f.targets, t.TargetCellID)
	return nil
}

// A UE crossing three cells drives two handovers (1→2→3) through the executor.
func TestMobilityControllerDrivesHandovers(t *testing.T) {
	exec := &fakeExecutor{}
	mc := engine.MobilityController{
		Scenario: meas.Scenario{
			Cells:   []meas.Cell{{ID: 1, X: 0}, {ID: 2, X: 1000}, {ID: 3, X: 2000}},
			Serving: 1,
			Track:   meas.Track{StartX: 0, EndX: 2000, Duration: 20 * time.Second, Step: 100 * time.Millisecond},
			Event:   meas.EventA3{Offset: 2, Hysteresis: 1, TTT: 320 * time.Millisecond},
		},
		Exec: exec,
	}
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	triggers, err := mc.Run(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(triggers) != 2 {
		t.Fatalf("expected 2 triggers, got %d", len(triggers))
	}
	if len(exec.targets) != 2 || exec.targets[0] != 2 || exec.targets[1] != 3 {
		t.Fatalf("executor drove %v, want [2 3]", exec.targets)
	}
}

func TestMobilityControllerStopsOnExecError(t *testing.T) {
	exec := &fakeExecutor{err: context.DeadlineExceeded}
	mc := engine.MobilityController{
		Scenario: meas.Scenario{
			Cells:   []meas.Cell{{ID: 1, X: 0}, {ID: 2, X: 1000}},
			Serving: 1,
			Track:   meas.Track{StartX: 50, EndX: 950, Duration: 10 * time.Second, Step: 100 * time.Millisecond},
			Event:   meas.EventA3{Offset: 2, Hysteresis: 1, TTT: 320 * time.Millisecond},
		},
		Exec: exec,
	}
	if _, err := mc.Run(context.Background(), time.Now()); err == nil {
		t.Fatal("expected the executor error to propagate")
	}
}
