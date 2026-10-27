package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Two fleets that share nothing run at the same time through the registry,
// against one AMF, and each attaches its own population: with the
// one-per-kind rule gone, nothing they share may couple them (run under
// -race, this is the check that no package-level state does).
func TestTwoFleetsRunConcurrently(t *testing.T) {
	amf, cfg, ki, opc := newBindTestAMF(t)
	addr := sctpFront(t, amf)
	r := quietRegistry(0)

	// Both launchers wait at a barrier once started, so the runs overlap.
	var started sync.WaitGroup
	started.Add(2)
	release := make(chan struct{})
	go func() { started.Wait(); close(release) }()

	fleet := func(id uint32, base string) (FleetRunSpec, Needs) {
		c := cfg
		c.ID, c.Name = id, ""
		spec := FleetRunSpec{
			AMFAddr: addr, GNBs: []FleetGNB{{Config: c}},
			BaseIMSI: base, Count: 3, MCC: "208", MNC: "93", Ki: ki, OPc: opc,
		}
		needs, err := FleetNeeds(spec)
		if err != nil {
			t.Fatal(err)
		}
		return spec, needs
	}
	var ids []string
	reports := make(map[string]*FleetReport)
	var mu sync.Mutex
	for i, base := range []string{"208930100007500", "208930100007600"} {
		spec, needs := fleet(uint32(10+i), base)
		info, err := r.StartFleet(RunOptions{Name: base, Needs: needs}, func(ctx context.Context, live *FleetLiveStats, _ RunEventFunc) (FleetReport, error) {
			started.Done()
			select {
			case <-release:
			case <-ctx.Done():
				return FleetReport{}, ctx.Err()
			}
			rep, err := RunFleet(ctx, testLogger(), spec, FleetBehaviors{}, FleetDeps{Stats: live})
			mu.Lock()
			reports[base] = &rep
			mu.Unlock()
			return rep, err
		})
		if err != nil {
			t.Fatalf("fleet %s refused: %v", base, err)
		}
		ids = append(ids, info.ID)
	}
	for _, id := range ids {
		waitStateWithin(t, r, id, RunComplete, 30*time.Second)
	}
	mu.Lock()
	defer mu.Unlock()
	for base, rep := range reports {
		if rep.Attached != 3 || rep.AttachFailed != 0 {
			t.Errorf("fleet %s: %d attached, %d failed; want 3 and 0", base, rep.Attached, rep.AttachFailed)
		}
	}
}

// waitStateWithin is waitState with a caller-chosen deadline, for runs that
// do real attaches.
func waitStateWithin(t *testing.T, r *RunRegistry, id string, want RunState, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		info, err := r.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if info.State == want {
			return
		}
		if info.State.Terminal() {
			t.Fatalf("run %s ended %s (%s), want %s", id, info.State, info.Err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach %s within %s", id, want, d)
}
