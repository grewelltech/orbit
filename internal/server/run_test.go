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

// seedRun registers a run through the registry with a controllable launcher and
// returns its id. It lets the server tests exercise the COMPLETE/report and
// StopRun-success paths that a real load run (needing a core) cannot reach.
func seedRun(t *testing.T, reg *engine.RunRegistry, name string, fn engine.LoadRunFunc) string {
	t.Helper()
	info, err := reg.StartLoad(name, fn)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return info.ID
}

// The COMPLETE path: a run that finishes cleanly exposes its report over RPC,
// with the load fields mapped through. This is the happy path a fail-fast spec
// cannot reach.
func TestRunServiceReportOnComplete(t *testing.T) {
	c, reg := newRunRPCClient(t)
	report := load.Report{
		Attempted: 100, Succeeded: 98, Failed: 2,
		Duration: 5 * time.Second, AchievedRate: 19.6,
		Latencies: map[string]load.Stats{
			"attach": {Count: 98, P50: 12 * time.Millisecond, P99: 45 * time.Millisecond, Max: 90 * time.Millisecond},
		},
	}
	id := seedRun(t, reg, "clean", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
		return report, nil
	})
	waitRunState(t, c, id, orbitv1.RunState_RUN_STATE_COMPLETE)

	resp, err := c.GetRunReport(context.Background(), connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id}))
	if err != nil {
		t.Fatalf("GetRunReport: %v", err)
	}
	lr := resp.Msg.GetLoad()
	if lr == nil {
		t.Fatal("completed run returned no load report")
	}
	if lr.GetAttempted() != 100 || lr.GetSucceeded() != 98 || lr.GetFailed() != 2 {
		t.Errorf("report counts = %d/%d/%d, want 100/98/2", lr.GetAttempted(), lr.GetSucceeded(), lr.GetFailed())
	}
	var attach *orbitv1.ProcedureLatency
	for _, l := range lr.GetLatency() {
		if l.GetProcedure() == "attach" {
			attach = l
		}
	}
	if attach == nil {
		t.Fatal("report has no attach latency")
	}
	if attach.GetP50Ms() != 12 || attach.GetP99Ms() != 45 {
		t.Errorf("attach P50/P99 = %.0f/%.0f ms, want 12/45", attach.GetP50Ms(), attach.GetP99Ms())
	}
}

// GetRun returns a live progress snapshot while a run is in flight.
func TestRunServiceGetRunLiveProgress(t *testing.T) {
	c, reg := newRunRPCClient(t)
	proceed := make(chan struct{})
	t.Cleanup(func() { close(proceed) })
	seen := make(chan struct{})
	id := seedRun(t, reg, "live", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
		s.Observe(load.Sample{Metrics: map[string]time.Duration{"attach": 10 * time.Millisecond}})
		s.Observe(load.Sample{Err: errBoom})
		close(seen)
		<-proceed
		return load.Report{}, nil
	})
	<-seen

	resp, err := c.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	prog := resp.Msg.GetLoadProgress()
	if prog == nil {
		t.Fatal("running load run returned no live progress")
	}
	if prog.GetAttempted() != 2 || prog.GetSucceeded() != 1 || prog.GetFailed() != 1 {
		t.Errorf("progress = %d/%d/%d, want 2/1/1", prog.GetAttempted(), prog.GetSucceeded(), prog.GetFailed())
	}
}

var errBoom = errorString("attach rejected")

type errorString string

func (e errorString) Error() string { return string(e) }

// StopRun of a live run over RPC returns the run and drives it to CANCELLED.
func TestRunServiceStopRunSuccess(t *testing.T) {
	c, reg := newRunRPCClient(t)
	id := seedRun(t, reg, "stoppable", func(ctx context.Context, s *load.LiveStats) (load.Report, error) {
		<-ctx.Done()
		return load.Report{}, ctx.Err()
	})

	resp, err := c.StopRun(context.Background(), connect.NewRequest(&orbitv1.StopRunRequest{RunId: id}))
	if err != nil {
		t.Fatalf("StopRun: %v", err)
	}
	if got := resp.Msg.GetRun().GetRunId(); got != id {
		t.Errorf("StopRun returned run %q, want %q", got, id)
	}
	waitRunState(t, c, id, orbitv1.RunState_RUN_STATE_CANCELLED)
}

// gnbs disagreeing on PLMN are rejected rather than silently taking the last
// one's MCC/MNC for every UE's identity.
func TestRunServiceRejectsMixedPLMN(t *testing.T) {
	c, _ := newRunRPCClient(t)
	spec := failFastLoadSpec()
	spec.Gnbs = append(spec.Gnbs, &orbitv1.GnbConfig{
		Id: 2, IdBits: 24, Name: "orbit-gnb-2",
		Mcc: "310", Mnc: "410", Tac: 1, // different PLMN
		Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}},
	})
	_, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{
		Spec: &orbitv1.StartRunRequest_Load{Load: spec},
	}))
	if err == nil {
		t.Fatal("StartRun accepted gnbs with divergent PLMNs")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// loadRunFunc maps the ramp fields onto a load.LinearRamp, and a ramp with no
// duration is rejected rather than producing a degenerate curve.
func TestLoadRunFuncRampMapping(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	bad := failFastLoadSpec()
	bad.RampStart, bad.RampEnd = 5, 50 // ramp requested but no ramp_seconds
	if _, err := loadRunFunc(log, bad); err == nil {
		t.Error("a ramp with no ramp_seconds was accepted")
	}

	ok := failFastLoadSpec()
	ok.RampStart, ok.RampEnd, ok.RampSeconds = 5, 50, 30
	if _, err := loadRunFunc(log, ok); err != nil {
		t.Errorf("a well-formed ramp was rejected: %v", err)
	}
}

// loadRunFunc validates a PDU session's SST as one octet before a run starts.
func TestLoadRunFuncPDUValidation(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec := failFastLoadSpec()
	spec.PduSession = &orbitv1.PDUSession{PduSessionId: 1, Sst: 300, Dnn: "internet"} // > 0xFF
	if _, err := loadRunFunc(log, spec); err == nil {
		t.Error("an out-of-range SST was accepted")
	}
}

// A minimal fleet scenario that fails fast: the AMF is unroutable, so the fleet
// attach phase errors quickly and the run reaches a terminal state without a
// live core. Exercises StartRun → registry → RunFleet over the wire.
const failFastFleetYAML = `
kind: fleet
name: smoke
core:
  amf: 127.0.0.1:1
  plmn: { mcc: "208", mnc: "93" }
  tac: 1
  slice: { sst: 1, sd: "010203" }
  dnn: internet
credentials:
  ki: "00112233445566778899aabbccddeeff"
  opc: "000102030405060708090a0b0c0d0e0f"
topology:
  gnbs:
    count: 1
    id_base: 1
    layout: grid
    source_ips: ["127.0.0.1"]
fleet:
  count: 1
  supi_base: "208930100007500"
  distribution: even
  attach_rate: 10/s
`

func fleetSpec() *orbitv1.FleetRunSpec {
	return &orbitv1.FleetRunSpec{
		ScenarioYaml: failFastFleetYAML,
		Credentials: &orbitv1.Credentials{
			Ki:  "00112233445566778899aabbccddeeff",
			Opc: "000102030405060708090a0b0c0d0e0f",
		},
	}
}

// A fleet run starts server-side, is listed as a FLEET kind, and fails fast
// against the dead AMF.
func TestRunServiceStartFleet(t *testing.T) {
	c, _ := newRunRPCClient(t)
	resp, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{
		Name: "fleet-smoke",
		Spec: &orbitv1.StartRunRequest_Fleet{Fleet: fleetSpec()},
	}))
	if err != nil {
		t.Fatalf("StartRun(fleet): %v", err)
	}
	run := resp.Msg.GetRun()
	if run.GetKind() != orbitv1.RunKind_RUN_KIND_FLEET {
		t.Errorf("kind = %v, want FLEET", run.GetKind())
	}
	waitRunState(t, c, run.GetRunId(), orbitv1.RunState_RUN_STATE_FAILED)

	list, _ := c.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{
		Kind: orbitv1.RunKind_RUN_KIND_FLEET,
	}))
	if len(list.Msg.GetRuns()) != 1 {
		t.Errorf("fleet-filtered ListRuns returned %d, want 1", len(list.Msg.GetRuns()))
	}
}

// A malformed fleet YAML is rejected synchronously, before a run is registered.
func TestRunServiceFleetValidatesYAML(t *testing.T) {
	c, _ := newRunRPCClient(t)
	spec := fleetSpec()
	spec.ScenarioYaml = "kind: fleet\nthis is: not valid: yaml: ["
	_, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{
		Spec: &orbitv1.StartRunRequest_Fleet{Fleet: spec},
	}))
	if err == nil {
		t.Fatal("StartRun accepted a malformed fleet YAML")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
	list, _ := c.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{}))
	if len(list.Msg.GetRuns()) != 0 {
		t.Errorf("a rejected fleet StartRun registered %d runs, want 0", len(list.Msg.GetRuns()))
	}
}

// A completed fleet run exposes its FleetReport over RPC (seeded, since a real
// fleet needs a core).
func TestRunServiceFleetReport(t *testing.T) {
	c, reg := newRunRPCClient(t)
	want := engine.FleetReport{Attached: 50, AttachFailed: 2, Handovers: 8, Deregistered: 50}
	info, err := reg.StartFleet("seeded", func(ctx context.Context) (engine.FleetReport, error) {
		return want, nil
	})
	if err != nil {
		t.Fatalf("seed fleet: %v", err)
	}
	waitRunState(t, c, info.ID, orbitv1.RunState_RUN_STATE_COMPLETE)

	resp, err := c.GetRunReport(context.Background(), connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: info.ID}))
	if err != nil {
		t.Fatalf("GetRunReport: %v", err)
	}
	fr := resp.Msg.GetFleet()
	if fr == nil {
		t.Fatal("completed fleet run returned no fleet report")
	}
	if fr.GetAttached() != 50 || fr.GetHandovers() != 8 || fr.GetDeregistered() != 50 {
		t.Errorf("fleet report = %+v, want attached 50 / handovers 8 / dereg 50", fr)
	}
}
