package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	if _, err := reg.StartLoad("blocking", func(ctx context.Context, _ *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
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
	id := seedRun(t, reg, "clean", func(ctx context.Context, s *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
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
	id := seedRun(t, reg, "live", func(ctx context.Context, s *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
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

// A running load run's live progress carries the per-gNB distribution end to
// end: the run attributes samples to gNBs, and GetRun surfaces them sorted by
// name so a client renders a stable population map.
func TestRunServiceGetRunPerGNB(t *testing.T) {
	c, reg := newRunRPCClient(t)
	proceed := make(chan struct{})
	t.Cleanup(func() { close(proceed) })
	seen := make(chan struct{})
	id := seedRun(t, reg, "pergnb", func(ctx context.Context, s *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
		s.Observe(load.Sample{GNB: "gnb-1", Metrics: map[string]time.Duration{"attach": 10 * time.Millisecond}})
		s.Observe(load.Sample{GNB: "gnb-0", Metrics: map[string]time.Duration{"attach": 10 * time.Millisecond}})
		s.Observe(load.Sample{GNB: "gnb-0", Err: errBoom})
		close(seen)
		<-proceed
		return load.Report{}, nil
	})
	<-seen

	resp, err := c.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	perGnb := resp.Msg.GetLoadProgress().GetPerGnb()
	if len(perGnb) != 2 {
		t.Fatalf("per-gNB entries = %d, want 2", len(perGnb))
	}
	if perGnb[0].GetGnb() != "gnb-0" || perGnb[1].GetGnb() != "gnb-1" {
		t.Errorf("per-gNB order = %q,%q, want gnb-0,gnb-1", perGnb[0].GetGnb(), perGnb[1].GetGnb())
	}
	if perGnb[0].GetAttempted() != 2 || perGnb[0].GetSucceeded() != 1 || perGnb[0].GetFailed() != 1 {
		t.Errorf("gnb-0 = %d/%d/%d, want 2/1/1", perGnb[0].GetAttempted(), perGnb[0].GetSucceeded(), perGnb[0].GetFailed())
	}
}

var errBoom = errorString("attach rejected")

type errorString string

func (e errorString) Error() string { return string(e) }

// StopRun of a live run over RPC returns the run and drives it to CANCELLED.
func TestRunServiceStopRunSuccess(t *testing.T) {
	c, reg := newRunRPCClient(t)
	id := seedRun(t, reg, "stoppable", func(ctx context.Context, s *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
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
	final := waitRunState(t, c, run.GetRunId(), orbitv1.RunState_RUN_STATE_FAILED)

	// The failure must come from RunFleet's dial against the dead AMF, proving
	// the real StartRun → fleetRunFunc → RunFleet path ran — not a parse error
	// or a stubbed launcher, which "FAILED, any cause" would also accept.
	if !strings.Contains(final.GetError(), "127.0.0.1:1") {
		t.Errorf("fleet run error = %q, want the AMF dial failure — it did not reach RunFleet", final.GetError())
	}

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
	// Distinct values in every field, so a swapped or dropped mapping is caught.
	want := engine.FleetReport{
		Attached: 50, AttachFailed: 2, AttachElapsed: 3 * time.Second,
		Handovers: 8, HandoverErr: 1, TrafficFlows: 40, TrafficBytes: 123456, Deregistered: 49,
	}
	info, err := reg.StartFleet("seeded", func(ctx context.Context, _ engine.RunEventFunc) (engine.FleetReport, error) {
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
	// Every field, so no wire mapping can silently swap or drop one.
	if fr.GetAttached() != 50 || fr.GetAttachFailed() != 2 || fr.GetAttachElapsedMs() != 3000 ||
		fr.GetHandovers() != 8 || fr.GetHandoverErrors() != 1 ||
		fr.GetTrafficFlows() != 40 || fr.GetTrafficBytes() != 123456 || fr.GetDeregistered() != 49 {
		t.Errorf("fleet report mismapped: %+v", fr)
	}
}

// A ${VAR} in a client-submitted fleet YAML must NOT be expanded against the
// server's environment — that would leak the server's variables back to the
// client via the run error. The reference is left literal.
func TestRunServiceFleetDoesNotExpandServerEnv(t *testing.T) {
	t.Setenv("ORBIT_TEST_SECRET", "super-secret-value")
	c, _ := newRunRPCClient(t)

	spec := fleetSpec()
	// Point the AMF at the secret via an env ref. If the server expanded it,
	// the dial error would contain the secret.
	spec.ScenarioYaml = strings.Replace(spec.ScenarioYaml, "amf: 127.0.0.1:1", "amf: ${ORBIT_TEST_SECRET}", 1)

	resp, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{
		Spec: &orbitv1.StartRunRequest_Fleet{Fleet: spec},
	}))
	if err != nil {
		// A literal "${ORBIT_TEST_SECRET}" is an invalid AMF address, so it may
		// fail validation synchronously — also acceptable, and secret-free.
		if strings.Contains(err.Error(), "super-secret-value") {
			t.Fatal("server env secret leaked into the StartRun error")
		}
		return
	}

	final := waitRunState(t, c, resp.Msg.GetRun().GetRunId(), orbitv1.RunState_RUN_STATE_FAILED)
	if strings.Contains(final.GetError(), "super-secret-value") {
		t.Errorf("server env secret leaked into the run error: %q", final.GetError())
	}
	if !strings.Contains(final.GetError(), "ORBIT_TEST_SECRET") {
		t.Errorf("expected the literal ${ORBIT_TEST_SECRET} in the error, got %q", final.GetError())
	}
}

// A telemetry stream on an unknown run is NotFound.
func TestRunTelemetryUnknownRunIsNotFound(t *testing.T) {
	c, _ := newRunRPCClient(t)
	st, err := c.RunTelemetry(context.Background(), connect.NewRequest(&orbitv1.RunTelemetryRequest{RunId: "run-nope"}))
	if err == nil {
		// Some transports surface the error on first Receive rather than at open.
		st.Receive()
		err = st.Err()
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}

// The server clamps a below-range requested interval and reports the applied
// one in every frame.
func TestRunTelemetryClampsInterval(t *testing.T) {
	c, reg := newRunRPCClient(t)
	id := seedRun(t, reg, "done", func(ctx context.Context, s *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
		return load.Report{}, nil
	})
	waitRunState(t, c, id, orbitv1.RunState_RUN_STATE_COMPLETE)

	st, err := c.RunTelemetry(context.Background(), connect.NewRequest(&orbitv1.RunTelemetryRequest{
		RunId: id, IntervalMs: 1, // below the 100ms floor
	}))
	if err != nil {
		t.Fatalf("RunTelemetry: %v", err)
	}
	if !st.Receive() {
		t.Fatalf("no frame: %v", st.Err())
	}
	if got := st.Msg().GetIntervalMs(); got != 100 {
		t.Errorf("applied interval = %d ms, want the clamped 100", got)
	}
}

// A live run streams complete frames, sequenced, until it ends with a terminal
// frame carrying the final state.
func TestRunTelemetryStreamsUntilTerminal(t *testing.T) {
	c, reg := newRunRPCClient(t)
	proceed := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(proceed) }) }
	t.Cleanup(release) // never wedge the launcher if a RUNNING frame never arrives
	info, err := reg.StartLoad("live", func(ctx context.Context, s *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
		s.Observe(load.Sample{Metrics: map[string]time.Duration{"attach": 10 * time.Millisecond}})
		s.Observe(load.Sample{Err: errBoom})
		<-proceed
		return load.Report{}, nil
	})
	if err != nil {
		t.Fatalf("StartLoad: %v", err)
	}

	st, err := c.RunTelemetry(context.Background(), connect.NewRequest(&orbitv1.RunTelemetryRequest{
		RunId: info.ID, IntervalMs: 20,
	}))
	if err != nil {
		t.Fatalf("RunTelemetry: %v", err)
	}

	// Deterministic, not wall-clock-raced: the run is blocked in its launcher
	// and cannot finish until we release it — and we release it only after
	// seeing a live RUNNING frame. So a RUNNING frame is guaranteed to precede
	// the terminal one on every machine.
	var frames []*orbitv1.TelemetryFrame
	var sawRunningWithProgress bool
	for st.Receive() {
		f := st.Msg()
		frames = append(frames, f)
		if f.GetState() == orbitv1.RunState_RUN_STATE_RUNNING {
			// A live (non-terminal) frame must carry the aggregates — otherwise
			// "live telemetry" is only ever the final snapshot.
			if lp := f.GetLoad(); lp != nil && lp.GetAttempted() == 2 {
				if lp.GetSucceeded() != 1 || lp.GetFailed() != 1 {
					t.Errorf("running progress = %d ok / %d failed, want 1/1", lp.GetSucceeded(), lp.GetFailed())
				}
				sawRunningWithProgress = true
			}
			release() // release the run now that a live frame is in hand
		}
	}
	if err := st.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if len(frames) < 2 {
		t.Fatalf("got %d frames, want at least a live one and a terminal one", len(frames))
	}
	if !sawRunningWithProgress {
		t.Error("no RUNNING frame carried the live load aggregates")
	}
	// frame_seq is monotonic from 0.
	for i, f := range frames {
		if f.GetFrameSeq() != uint64(i) {
			t.Errorf("frame %d has seq %d, want %d", i, f.GetFrameSeq(), i)
		}
	}
	// The stream ends on a terminal frame.
	if last := frames[len(frames)-1]; last.GetState() != orbitv1.RunState_RUN_STATE_COMPLETE {
		t.Errorf("final frame state = %v, want COMPLETE", last.GetState())
	}
}

// A run that is already terminal yields exactly one final frame, then ends.
func TestRunTelemetryTerminalRunEndsAfterOneFrame(t *testing.T) {
	c, reg := newRunRPCClient(t)
	id := seedRun(t, reg, "done", func(ctx context.Context, s *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
		return load.Report{}, nil
	})
	waitRunState(t, c, id, orbitv1.RunState_RUN_STATE_COMPLETE)

	st, err := c.RunTelemetry(context.Background(), connect.NewRequest(&orbitv1.RunTelemetryRequest{RunId: id}))
	if err != nil {
		t.Fatalf("RunTelemetry: %v", err)
	}
	var n int
	for st.Receive() {
		n++
		if st.Msg().GetState() != orbitv1.RunState_RUN_STATE_COMPLETE {
			t.Errorf("frame state = %v, want COMPLETE", st.Msg().GetState())
		}
	}
	if err := st.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if n != 1 {
		t.Errorf("terminal run yielded %d frames, want exactly 1", n)
	}
}

// A client that cancels its context mid-stream stops the server-side loop
// (guarding against a leaked ticker goroutine). The run keeps running; only
// the stream ends. The frame count also proves the ticker fires at the
// requested cadence rather than the 1s default — several frames arrive in a
// sub-second window.
func TestRunTelemetryStopsOnClientCancel(t *testing.T) {
	c, reg := newRunRPCClient(t)
	proceed := make(chan struct{})
	t.Cleanup(func() { close(proceed) })
	info, err := reg.StartLoad("blocked", func(ctx context.Context, s *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
		s.Observe(load.Sample{Metrics: map[string]time.Duration{"attach": time.Millisecond}})
		<-proceed // never completes on its own
		return load.Report{}, nil
	})
	if err != nil {
		t.Fatalf("StartLoad: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	st, err := c.RunTelemetry(ctx, connect.NewRequest(&orbitv1.RunTelemetryRequest{
		RunId: info.ID, IntervalMs: 20,
	}))
	if err != nil {
		t.Fatalf("RunTelemetry: %v", err)
	}

	var mu sync.Mutex
	var n int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for st.Receive() {
			mu.Lock()
			n++
			mu.Unlock()
		}
	}()

	// Let frames flow at the 20ms cadence, then hang up. The window is well
	// under the 1s default interval, so the ticker's actual period is
	// observable: >1 frame here means it fired at the requested cadence, since
	// the default would emit only the immediate frame in this window.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end after the client cancelled — the server loop leaked")
	}

	mu.Lock()
	got := n
	mu.Unlock()
	if got < 2 {
		t.Errorf("received %d frames in 300ms; the ticker did not fire at the requested 20ms cadence (the 1s default would give 1)", got)
	}
}

// With a long interval, a cancelled client must end the stream promptly via the
// ctx.Done path — not linger until the next (far-off) tick, when Send would
// finally fail on the closed connection. Pins the ctx.Done branch.
func TestRunTelemetryCancelIsPromptAtLongInterval(t *testing.T) {
	c, reg := newRunRPCClient(t)
	proceed := make(chan struct{})
	t.Cleanup(func() { close(proceed) })
	info, err := reg.StartLoad("blocked", func(ctx context.Context, s *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
		<-proceed // stays RUNNING
		return load.Report{}, nil
	})
	if err != nil {
		t.Fatalf("StartLoad: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Requested 60s, clamped to the 10s max — far longer than the prompt window.
	st, err := c.RunTelemetry(ctx, connect.NewRequest(&orbitv1.RunTelemetryRequest{
		RunId: info.ID, IntervalMs: 60000,
	}))
	if err != nil {
		t.Fatalf("RunTelemetry: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for st.Receive() {
		}
	}()

	time.Sleep(50 * time.Millisecond) // let the immediate first frame land
	cancel()

	select {
	case <-done:
		// Ended promptly, well within one 10s tick — the ctx.Done path fired.
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled stream did not end promptly; it waited for the next tick instead of ctx.Done")
	}
}

// RunEvents streams a run's events and ends once the run is terminal, with the
// terminal event delivered.
func TestRunEventsStreamsUntilTerminal(t *testing.T) {
	c, reg := newRunRPCClient(t)
	info, err := reg.StartLoad("evt", func(ctx context.Context, s *load.LiveStats, emit engine.RunEventFunc) (load.Report, error) {
		emit("error", "ATTACH", "imsi-1", "registration rejected")
		return load.Report{}, nil
	})
	if err != nil {
		t.Fatalf("StartLoad: %v", err)
	}

	st, err := c.RunEvents(context.Background(), connect.NewRequest(&orbitv1.RunEventsRequest{RunId: info.ID}))
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	var evs []*orbitv1.RunEvent
	for st.Receive() {
		evs = append(evs, st.Msg())
	}
	if err := st.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if len(evs) < 3 {
		t.Fatalf("got %d events, want started + failure + terminal", len(evs))
	}
	// seq is monotonic from 0.
	for i, e := range evs {
		if e.GetSeq() != uint64(i) {
			t.Errorf("event %d seq = %d, want %d", i, e.GetSeq(), i)
		}
	}
	var sawFailure, sawTerminal bool
	for _, e := range evs {
		if e.GetKind() == "ATTACH" && e.GetSupi() == "imsi-1" &&
			e.GetSeverity() == orbitv1.EventSeverity_EVENT_SEVERITY_ERROR {
			sawFailure = true
		}
		if e.GetKind() == "RUN" && strings.Contains(e.GetMessage(), "COMPLETE") {
			sawTerminal = true
		}
	}
	if !sawFailure {
		t.Error("failure event not streamed")
	}
	if !sawTerminal {
		t.Error("terminal event not streamed — the stream ended without the final event")
	}
}

// A from_seq pointing into evicted history reports the drop count on the first
// delivered event.
func TestRunEventsReportsDroppedBefore(t *testing.T) {
	c, reg := newRunRPCClient(t)
	// Emit more than the ring holds, then complete, so history is evicted.
	info, err := reg.StartLoad("drop", func(ctx context.Context, s *load.LiveStats, emit engine.RunEventFunc) (load.Report, error) {
		for i := 0; i < engine.DefaultRunEventCap+50; i++ {
			emit("info", "N", "", "e")
		}
		return load.Report{}, nil
	})
	if err != nil {
		t.Fatalf("StartLoad: %v", err)
	}
	waitRunState(t, c, info.ID, orbitv1.RunState_RUN_STATE_COMPLETE)

	// Resume from seq 0, which is long gone.
	st, err := c.RunEvents(context.Background(), connect.NewRequest(&orbitv1.RunEventsRequest{RunId: info.ID, FromSeq: 0}))
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}
	if !st.Receive() {
		t.Fatalf("no first event: %v", st.Err())
	}
	if st.Msg().GetDroppedBefore() == 0 {
		t.Error("first event reported 0 dropped, but early events were evicted")
	}
	for st.Receive() {
	}
	if err := st.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
}

// RunEvents on an unknown run is NotFound.
func TestRunEventsUnknownRunIsNotFound(t *testing.T) {
	c, _ := newRunRPCClient(t)
	st, err := c.RunEvents(context.Background(), connect.NewRequest(&orbitv1.RunEventsRequest{RunId: "run-nope"}))
	if err == nil {
		st.Receive()
		err = st.Err()
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}

// gnbProgressProtos emits gNBs in name order regardless of map iteration, so a
// telemetry frame's per-gNB list is stable across samples, and returns nil for
// an unattributed run rather than an empty non-nil slice.
func TestGNBProgressProtosSortedAndStable(t *testing.T) {
	got := gnbProgressProtos(map[string]load.GNBProgress{
		"gnb-2": {Attempted: 3, Succeeded: 2, Failed: 1},
		"gnb-0": {Attempted: 5, Succeeded: 5},
		"gnb-1": {Attempted: 4, Succeeded: 4},
	})
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, want := range []string{"gnb-0", "gnb-1", "gnb-2"} {
		if got[i].GetGnb() != want {
			t.Errorf("position %d = %q, want %q (not name-sorted)", i, got[i].GetGnb(), want)
		}
	}
	if got[2].GetAttempted() != 3 || got[2].GetSucceeded() != 2 || got[2].GetFailed() != 1 {
		t.Errorf("gnb-2 counts = %d/%d/%d, want 3/2/1", got[2].GetAttempted(), got[2].GetSucceeded(), got[2].GetFailed())
	}
	if gnbProgressProtos(nil) != nil {
		t.Error("nil input must map to nil, not an empty slice")
	}
}
