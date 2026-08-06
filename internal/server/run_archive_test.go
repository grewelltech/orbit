package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/engine"
)

// newArchivingRunService serves a runService backed by a fresh registry and an
// archive store at dir. Calling it twice with the same dir is what a server
// restart looks like: new registry, new frame logs, same directory.
func newArchivingRunService(t *testing.T, dir string) orbitv1connect.RunServiceClient {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := newArchiveStore(log, dir, 10)
	if err != nil {
		t.Fatalf("newArchiveStore: %v", err)
	}
	svc := &runService{log: log, reg: engine.NewRunRegistry(log, 0), archive: store}
	svc.restoreFrames()
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewRunServiceHandler(svc))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL)
}

// runUntilArchived starts a fail-fast load run and waits for its archive FILE
// to appear, which is the point a restart can be simulated.
//
// Waiting on the run's state is not enough: the run goes terminal in
// milliseconds, but the archive is written by the sampler on its next tick, up
// to a full sampleInterval later.
func runUntilArchived(t *testing.T, c orbitv1connect.RunServiceClient, dir, name string) string {
	t.Helper()
	resp, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{
		Name: name,
		Spec: &orbitv1.StartRunRequest_Load{Load: failFastLoadSpec()},
	}))
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	id := resp.Msg.GetRun().GetRunId()

	path := filepath.Join(dir, id+archiveExt)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return id
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("run %s was never archived to %s", id, path)
	return ""
}

func TestRunSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	first := newArchivingRunService(t, dir)
	id := runUntilArchived(t, first, dir, "survivor")

	// The restart: everything in memory is gone.
	second := newArchivingRunService(t, dir)

	list, err := second.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{}))
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var found *orbitv1.Run
	for _, r := range list.Msg.GetRuns() {
		if r.GetRunId() == id {
			found = r
		}
	}
	if found == nil {
		t.Fatal("run is missing from ListRuns after a restart — the picker would be empty")
	}
	if found.GetName() != "survivor" {
		t.Errorf("name = %q, want %q", found.GetName(), "survivor")
	}
	if found.GetEndedUnixNano() == 0 {
		t.Error("restored run has no end time; it would read as still running")
	}

	got, err := second.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
	if err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}
	if got.Msg.GetRun().GetRunId() != id {
		t.Errorf("GetRun returned %q, want %q", got.Msg.GetRun().GetRunId(), id)
	}
}

func TestRestoredRunReplaysTelemetry(t *testing.T) {
	dir := t.TempDir()
	first := newArchivingRunService(t, dir)
	id := runUntilArchived(t, first, dir, "replay-me")

	second := newArchivingRunService(t, dir)
	stream, err := second.RunTelemetry(context.Background(),
		connect.NewRequest(&orbitv1.RunTelemetryRequest{RunId: id}))
	if err != nil {
		t.Fatalf("RunTelemetry: %v", err)
	}
	var frames int
	for stream.Receive() {
		frames++
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if frames == 0 {
		t.Fatal("restored run replayed no frames; the chart would be empty")
	}
	// It must also END. A restored run is finished, so a stream that followed
	// it live would hang forever waiting for frames nothing will produce.
}

func TestRestoredRunServesItsReport(t *testing.T) {
	dir := t.TempDir()
	first := newArchivingRunService(t, dir)
	id := runUntilArchived(t, first, dir, "report-me")

	// Whether this run COMPLETEs or FAILs depends on the unroutable AMF, so
	// assert on consistency with the state rather than on one outcome.
	before, err := first.GetRunReport(context.Background(),
		connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id}))
	beforeErr := err

	second := newArchivingRunService(t, dir)
	after, err := second.GetRunReport(context.Background(),
		connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id}))

	switch {
	case beforeErr != nil:
		// Refused before the restart; it must be refused the same way after,
		// not reported as an unknown run.
		if err == nil {
			t.Fatal("report was refused before the restart but served after it")
		}
		var ce *connect.Error
		if errors.As(err, &ce) && ce.Code() == connect.CodeNotFound {
			t.Error("restored run reported as NOT FOUND; it should refuse on state, as before")
		}
	case err != nil:
		t.Fatalf("report was served before the restart but failed after: %v", err)
	default:
		if before.Msg.GetRun().GetRunId() != after.Msg.GetRun().GetRunId() {
			t.Error("restored report is for a different run")
		}
	}
}

func TestRestoredRunReplaysEvents(t *testing.T) {
	dir := t.TempDir()
	first := newArchivingRunService(t, dir)
	id := runUntilArchived(t, first, dir, "events-me")

	second := newArchivingRunService(t, dir)
	stream, err := second.RunEvents(context.Background(),
		connect.NewRequest(&orbitv1.RunEventsRequest{RunId: id}))
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}
	var events int
	for stream.Receive() {
		events++
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if events == 0 {
		t.Error("restored run replayed no events; the event panel would be empty")
	}
}

func TestUnknownRunStillNotFoundWithArchive(t *testing.T) {
	// The archive fallback must not turn a genuinely unknown run into a
	// success — an empty archive is not an answer.
	c := newArchivingRunService(t, t.TempDir())
	_, err := c.GetRun(context.Background(),
		connect.NewRequest(&orbitv1.GetRunRequest{RunId: "run-nope"}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeNotFound {
		t.Fatalf("want NotFound for an unknown run, got %v", err)
	}
}

func TestLiveRunNotDuplicatedByArchive(t *testing.T) {
	// A run is archived while the registry still holds it, so both sources
	// briefly have it. It must appear once.
	dir := t.TempDir()
	c := newArchivingRunService(t, dir)
	id := runUntilArchived(t, c, dir, "once-only")

	list, err := c.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{}))
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var seen int
	for _, r := range list.Msg.GetRuns() {
		if r.GetRunId() == id {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("run appears %d times in ListRuns, want exactly 1", seen)
	}
}
