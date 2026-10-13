package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/load"
)

// GetRun summarises a finished fleet run from its report (#104). While the
// run is in flight there is no report and the live snapshot counts the UEs
// attached; once it completes the report carries the attach totals, per gNB,
// while the torn-down snapshot truthfully reads none attached.
func TestGetRunFleetReportOnceComplete(t *testing.T) {
	c, reg := newRunRPCClient(t)
	attached := make(chan struct{})
	finish := make(chan struct{})
	info, err := reg.StartFleet(engine.RunOptions{Name: "summary"}, func(ctx context.Context, live *engine.FleetLiveStats, _ engine.RunEventFunc) (engine.FleetReport, error) {
		ues := []struct{ gnb, supi string }{
			{"gnb-1", "208930100007500"}, {"gnb-2", "208930100007501"},
			{"gnb-1", "208930100007502"}, {"gnb-2", "208930100007503"},
		}
		for _, u := range ues {
			live.AttachOK(u.gnb, u.supi, nil)
		}
		close(attached)
		<-finish
		for _, u := range ues {
			live.Detached(u.gnb, nil)
		}
		return engine.FleetReport{
			Attached: 4, AttachElapsed: 1200 * time.Millisecond, Deregistered: 4,
			PerGNB: map[string]load.GNBProgress{
				"gnb-2": {Attempted: 2, Succeeded: 2},
				"gnb-1": {Attempted: 2, Succeeded: 2},
			},
		}, nil
	})
	if err != nil {
		t.Fatalf("StartFleet: %v", err)
	}
	<-attached
	get := func() *orbitv1.GetRunResponse {
		t.Helper()
		res, err := c.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: info.ID}))
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		return res.Msg
	}

	running := get()
	if running.GetFleetReport() != nil {
		t.Errorf("running fleet run carries a report: %+v", running.GetFleetReport())
	}
	if got := running.GetFleetProgress().GetAttached(); got != 4 {
		t.Errorf("running: fleet_progress.attached = %d, want 4", got)
	}

	close(finish)
	waitRunState(t, c, info.ID, orbitv1.RunState_RUN_STATE_COMPLETE)
	done := get()
	rep := done.GetFleetReport()
	if rep == nil {
		t.Fatal("completed fleet run has no fleet_report")
	}
	if rep.GetAttached() != 4 || rep.GetAttachFailed() != 0 || rep.GetDeregistered() != 4 || rep.GetAttachElapsedMs() != 1200 {
		t.Errorf("fleet_report = %+v", rep)
	}
	var sum uint32
	var names []string
	for _, g := range rep.GetPerGnb() {
		sum += g.GetSucceeded()
		names = append(names, g.GetGnb())
	}
	if sum != 4 || len(names) != 2 || names[0] != "gnb-1" || names[1] != "gnb-2" {
		t.Errorf("per_gnb = %v (sum %d), want gnb-1 then gnb-2 summing to 4", rep.GetPerGnb(), sum)
	}
	if got := done.GetFleetProgress().GetAttached(); got != 0 {
		t.Errorf("completed: fleet_progress.attached = %d, want 0 (every UE detached)", got)
	}
}

// A fleet run that fails has no report: GetRun keeps to GetRunReport's
// predicate, so the run is shown from its snapshot alone.
func TestGetRunFleetFailedHasNoReport(t *testing.T) {
	c, reg := newRunRPCClient(t)
	info, err := reg.StartFleet(engine.RunOptions{Name: "fails"}, func(context.Context, *engine.FleetLiveStats, engine.RunEventFunc) (engine.FleetReport, error) {
		return engine.FleetReport{AttachFailed: 4}, context.DeadlineExceeded
	})
	if err != nil {
		t.Fatalf("StartFleet: %v", err)
	}
	waitRunState(t, c, info.ID, orbitv1.RunState_RUN_STATE_FAILED)
	res, err := c.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: info.ID}))
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if res.Msg.GetFleetReport() != nil {
		t.Errorf("failed fleet run carries a report: %+v", res.Msg.GetFleetReport())
	}
}

// A fleet run served from the archive carries its archived report, the same
// as one still in the registry; an archived run without one (it did not
// complete) is shown from its snapshot alone.
func TestGetRunArchivedFleetReport(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := newArchiveStore(log, dir, 10)
	if err != nil {
		t.Fatalf("newArchiveStore: %v", err)
	}
	report := &orbitv1.FleetReport{Attached: 4, Deregistered: 4,
		PerGnb: []*orbitv1.GnbProgress{{Gnb: "gnb-1", Attempted: 2, Succeeded: 2}, {Gnb: "gnb-2", Attempted: 2, Succeeded: 2}}}
	for _, a := range []*orbitv1.RunArchive{
		{Run: &orbitv1.Run{RunId: "fleet-done", Kind: orbitv1.RunKind_RUN_KIND_FLEET, State: orbitv1.RunState_RUN_STATE_COMPLETE},
			FleetProgress: &orbitv1.FleetProgress{}, Report: &orbitv1.RunArchive_FleetReport{FleetReport: report}},
		{Run: &orbitv1.Run{RunId: "fleet-failed", Kind: orbitv1.RunKind_RUN_KIND_FLEET, State: orbitv1.RunState_RUN_STATE_FAILED},
			FleetProgress: &orbitv1.FleetProgress{AttachFailed: 4}},
	} {
		if err := store.save(a); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewRunServiceHandler(&runService{log: log, reg: engine.NewRunRegistry(log, 0), archive: store}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL)

	for _, tc := range []struct {
		id         string
		wantReport bool
	}{{"fleet-done", true}, {"fleet-failed", false}} {
		res, err := c.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: tc.id}))
		if err != nil {
			t.Fatalf("%s: GetRun: %v", tc.id, err)
		}
		rep := res.Msg.GetFleetReport()
		if (rep != nil) != tc.wantReport {
			t.Errorf("%s: fleet_report = %+v, want present %v", tc.id, rep, tc.wantReport)
		}
		if rep != nil && (rep.GetAttached() != 4 || len(rep.GetPerGnb()) != 2) {
			t.Errorf("%s: archived report mismapped: %+v", tc.id, rep)
		}
		if res.Msg.GetFleetProgress() == nil {
			t.Errorf("%s: archived snapshot dropped", tc.id)
		}
	}
}
