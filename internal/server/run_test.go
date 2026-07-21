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

// newRunRPCClient serves a runService over a real registry and returns a
// Connect client against it.
func newRunRPCClient(t *testing.T) (orbitv1connect.RunServiceClient, *engine.RunRegistry) {
	t.Helper()
	reg := engine.NewRunRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewRunServiceHandler(&runService{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		reg: reg,
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL), reg
}

// A well-formed load spec that fails fast: the AMF address is unroutable, so
// RunLoad returns an error quickly and the run reaches a terminal state without
// needing a live core. This exercises the full StartRun → registry → RunLoad
// path over the wire.
func failFastLoadSpec() *orbitv1.LoadRunSpec {
	return &orbitv1.LoadRunSpec{
		AmfAddress: "127.0.0.1:1", // nothing listens here
		BaseImsi:   "208930100007500",
		Count:      1,
		Credentials: &orbitv1.Credentials{
			Ki:  "00112233445566778899aabbccddeeff",
			Opc: "000102030405060708090a0b0c0d0e0f",
		},
		Gnbs: []*orbitv1.GnbConfig{{
			Id: 1, IdBits: 24, Name: "orbit-gnb",
			Mcc: "208", Mnc: "93", Tac: 1,
			Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}},
		}},
	}
}

func startLoad(t *testing.T, c orbitv1connect.RunServiceClient, name string) *orbitv1.Run {
	t.Helper()
	resp, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{
		Name: name,
		Spec: &orbitv1.StartRunRequest_Load{Load: failFastLoadSpec()},
	}))
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return resp.Msg.GetRun()
}

func waitRunState(t *testing.T, c orbitv1connect.RunServiceClient, id string, want orbitv1.RunState) *orbitv1.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := c.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if resp.Msg.GetRun().GetState() == want {
			return resp.Msg.GetRun()
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s", id, want)
	return nil
}

func TestRunServiceStartAndList(t *testing.T) {
	c, _ := newRunRPCClient(t)
	run := startLoad(t, c, "soak-1")

	if run.GetRunId() == "" {
		t.Fatal("StartRun returned no run id")
	}
	if run.GetKind() != orbitv1.RunKind_RUN_KIND_LOAD {
		t.Errorf("kind = %v, want LOAD", run.GetKind())
	}
	if run.GetName() != "soak-1" {
		t.Errorf("name = %q, want soak-1", run.GetName())
	}

	// It fails fast against the dead AMF.
	waitRunState(t, c, run.GetRunId(), orbitv1.RunState_RUN_STATE_FAILED)

	list, err := c.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{}))
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(list.Msg.GetRuns()) != 1 || list.Msg.GetRuns()[0].GetRunId() != run.GetRunId() {
		t.Errorf("ListRuns did not return the started run")
	}
}

// A second load run while the first is active is rejected as FailedPrecondition.
func TestRunServiceOneActiveRejectsSecond(t *testing.T) {
	c, reg := newRunRPCClient(t)

	// Hold the first run active with a launcher that blocks until the test ends.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	if _, err := reg.StartLoad("blocking", func(ctx context.Context, _ *load.LiveStats) (load.Report, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return load.Report{}, nil
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	_, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{
		Spec: &orbitv1.StartRunRequest_Load{Load: failFastLoadSpec()},
	}))
	if err == nil {
		t.Fatal("second StartRun succeeded while a run was active")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("second StartRun code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

func TestRunServiceGetUnknownIsNotFound(t *testing.T) {
	c, _ := newRunRPCClient(t)
	_, err := c.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: "run-nope"}))
	if err == nil {
		t.Fatal("GetRun of unknown id succeeded")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}

// GetRunReport before completion is FailedPrecondition; after a clean run it
// returns the report. Here the run fails, so the report stays unavailable and
// the failure is surfaced as FailedPrecondition.
func TestRunServiceReportUnavailableUntilComplete(t *testing.T) {
	c, _ := newRunRPCClient(t)
	run := startLoad(t, c, "r")
	waitRunState(t, c, run.GetRunId(), orbitv1.RunState_RUN_STATE_FAILED)

	_, err := c.GetRunReport(context.Background(), connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: run.GetRunId()}))
	if err == nil {
		t.Fatal("GetRunReport returned a report for a failed run")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

// StartRun with no spec is InvalidArgument.
func TestRunServiceStartRequiresSpec(t *testing.T) {
	c, _ := newRunRPCClient(t)
	_, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Name: "x"}))
	if err == nil {
		t.Fatal("StartRun with no spec succeeded")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// A load spec missing required fields is rejected synchronously, before a run
// is registered.
func TestRunServiceStartValidatesSpec(t *testing.T) {
	c, _ := newRunRPCClient(t)
	spec := failFastLoadSpec()
	spec.AmfAddress = "" // required
	_, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{
		Spec: &orbitv1.StartRunRequest_Load{Load: spec},
	}))
	if err == nil {
		t.Fatal("StartRun with an empty amf_address succeeded")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	// Nothing should have been registered.
	list, _ := c.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{}))
	if len(list.Msg.GetRuns()) != 0 {
		t.Errorf("a rejected StartRun registered %d runs, want 0", len(list.Msg.GetRuns()))
	}
}

// StopRun of an unknown id is NotFound.
func TestRunServiceStopUnknown(t *testing.T) {
	c, _ := newRunRPCClient(t)
	_, err := c.StopRun(context.Background(), connect.NewRequest(&orbitv1.StopRunRequest{RunId: "run-nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}
