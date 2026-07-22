package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// stubRunService is a canned RunService for exercising the `orbit runs` client.
type stubRunService struct {
	orbitv1connect.UnimplementedRunServiceHandler
	runs        []*orbitv1.Run
	startReq    *orbitv1.StartRunRequest
	startRunID  string
	getProgress *orbitv1.LoadProgress
	stoppedID   string
	frames      []*orbitv1.TelemetryFrame
}

func (s *stubRunService) StartRun(_ context.Context, req *connect.Request[orbitv1.StartRunRequest]) (*connect.Response[orbitv1.StartRunResponse], error) {
	s.startReq = req.Msg
	return connect.NewResponse(&orbitv1.StartRunResponse{Run: &orbitv1.Run{RunId: s.startRunID}}), nil
}

func (s *stubRunService) StopRun(_ context.Context, req *connect.Request[orbitv1.StopRunRequest]) (*connect.Response[orbitv1.StopRunResponse], error) {
	s.stoppedID = req.Msg.GetRunId()
	return connect.NewResponse(&orbitv1.StopRunResponse{Run: &orbitv1.Run{RunId: req.Msg.GetRunId(), State: orbitv1.RunState_RUN_STATE_DRAINING}}), nil
}

func (s *stubRunService) ListRuns(_ context.Context, _ *connect.Request[orbitv1.ListRunsRequest]) (*connect.Response[orbitv1.ListRunsResponse], error) {
	return connect.NewResponse(&orbitv1.ListRunsResponse{Runs: s.runs}), nil
}

func (s *stubRunService) GetRun(_ context.Context, req *connect.Request[orbitv1.GetRunRequest]) (*connect.Response[orbitv1.GetRunResponse], error) {
	return connect.NewResponse(&orbitv1.GetRunResponse{
		Run:          &orbitv1.Run{RunId: req.Msg.GetRunId(), Kind: orbitv1.RunKind_RUN_KIND_LOAD, State: orbitv1.RunState_RUN_STATE_RUNNING},
		LoadProgress: s.getProgress,
	}), nil
}

func (s *stubRunService) RunTelemetry(_ context.Context, _ *connect.Request[orbitv1.RunTelemetryRequest], stream *connect.ServerStream[orbitv1.TelemetryFrame]) error {
	for _, f := range s.frames {
		if err := stream.Send(f); err != nil {
			return err
		}
	}
	return nil
}

func stubRunServer(t *testing.T, s *stubRunService) *string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewRunServiceHandler(s))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	url := srv.URL
	return &url
}

func runRunsCmd(t *testing.T, url *string, args ...string) string {
	t.Helper()
	cmd := newRunsCmd(url)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("runs %v: %v", args, err)
	}
	return out.String()
}

func TestRunsList(t *testing.T) {
	s := &stubRunService{runs: []*orbitv1.Run{
		{RunId: "run-aaaa", Kind: orbitv1.RunKind_RUN_KIND_LOAD, State: orbitv1.RunState_RUN_STATE_RUNNING, Name: "soak"},
		{RunId: "run-bbbb", Kind: orbitv1.RunKind_RUN_KIND_FLEET, State: orbitv1.RunState_RUN_STATE_COMPLETE},
	}}
	out := runRunsCmd(t, stubRunServer(t, s), "list")
	if !strings.Contains(out, "run-aaaa") || !strings.Contains(out, "RUNNING") || !strings.Contains(out, "soak") {
		t.Errorf("list output missing the load run:\n%s", out)
	}
	if !strings.Contains(out, "run-bbbb") || !strings.Contains(out, "fleet") {
		t.Errorf("list output missing the fleet run:\n%s", out)
	}
}

func TestRunsGetShowsProgress(t *testing.T) {
	s := &stubRunService{getProgress: &orbitv1.LoadProgress{
		Attempted: 100, Succeeded: 98, Failed: 2, AchievedRate: 9.5,
		Latency: []*orbitv1.ProcedureLatency{{Procedure: "attach", P50Ms: 12, P99Ms: 45, MaxMs: 90}},
	}}
	out := runRunsCmd(t, stubRunServer(t, s), "get", "run-x")
	if !strings.Contains(out, "98 ok / 100 attempted") || !strings.Contains(out, "9.5 attach/s") {
		t.Errorf("get did not render progress:\n%s", out)
	}
	if !strings.Contains(out, "attach") || !strings.Contains(out, "P99 45") {
		t.Errorf("get did not render latency:\n%s", out)
	}
}

func TestRunsStop(t *testing.T) {
	s := &stubRunService{}
	out := runRunsCmd(t, stubRunServer(t, s), "stop", "run-z")
	if s.stoppedID != "run-z" {
		t.Errorf("stop sent id %q, want run-z", s.stoppedID)
	}
	if !strings.Contains(out, "DRAINING") {
		t.Errorf("stop output = %q, want the DRAINING state", out)
	}
}

// start-load must build a LoadRunSpec faithfully from the flags.
func TestRunsStartLoadBuildsSpec(t *testing.T) {
	s := &stubRunService{startRunID: "run-new"}
	out := runRunsCmd(t, stubRunServer(t, s),
		"start-load", "--name", "storm", "--amf", "10.0.0.1:38412",
		"--base-imsi", "208930100007500", "--count", "250", "--rate", "12",
		"--ki", "00112233445566778899aabbccddeeff", "--opc", "000102030405060708090a0b0c0d0e0f",
		"--mcc", "208", "--mnc", "93", "--gnb-count", "2", "--sst", "1", "--sd", "010203")
	if strings.TrimSpace(out) != "run-new" {
		t.Errorf("start-load printed %q, want the run id", out)
	}
	spec := s.startReq.GetLoad()
	if spec == nil {
		t.Fatal("StartRun did not carry a load spec")
	}
	if spec.GetAmfAddress() != "10.0.0.1:38412" || spec.GetCount() != 250 || spec.GetRate() != 12 {
		t.Errorf("spec basics wrong: %+v", spec)
	}
	if len(spec.GetGnbs()) != 2 {
		t.Errorf("gnb-count 2 produced %d gnbs", len(spec.GetGnbs()))
	}
	if spec.GetCredentials().GetKi() == "" || spec.GetGnbs()[0].GetMcc() != "208" {
		t.Errorf("credentials/plmn not threaded: %+v", spec)
	}
	if s.startReq.GetName() != "storm" {
		t.Errorf("name = %q, want storm", s.startReq.GetName())
	}
}

// start-fleet reads the scenario file and takes credentials from flags/env, not
// the scenario's ${ENV}.
func TestRunsStartFleetSendsYAMLAndCreds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(path, []byte("kind: fleet\nname: f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &stubRunService{startRunID: "run-fleet"}
	runRunsCmd(t, stubRunServer(t, s), "start-fleet", path,
		"--ki", "00112233445566778899aabbccddeeff", "--opc", "000102030405060708090a0b0c0d0e0f")
	spec := s.startReq.GetFleet()
	if spec == nil {
		t.Fatal("StartRun did not carry a fleet spec")
	}
	if !strings.Contains(spec.GetScenarioYaml(), "kind: fleet") {
		t.Errorf("scenario yaml not sent: %q", spec.GetScenarioYaml())
	}
	if spec.GetCredentials().GetKi() == "" {
		t.Error("credentials not sent")
	}
}

// watch tails telemetry frames to stdout and ends when the stream ends.
func TestRunsWatchTailsFrames(t *testing.T) {
	s := &stubRunService{
		runs: []*orbitv1.Run{{RunId: "run-live", State: orbitv1.RunState_RUN_STATE_RUNNING}},
		frames: []*orbitv1.TelemetryFrame{
			{State: orbitv1.RunState_RUN_STATE_RUNNING, ElapsedMs: 1000, Progress: &orbitv1.TelemetryFrame_Load{Load: &orbitv1.LoadProgress{Succeeded: 10, Attempted: 12, AchievedRate: 10}}},
			{State: orbitv1.RunState_RUN_STATE_COMPLETE, ElapsedMs: 2000, Progress: &orbitv1.TelemetryFrame_Load{Load: &orbitv1.LoadProgress{Succeeded: 20, Attempted: 20}}},
		},
	}
	// No run id → watch the active run (found via ListRuns).
	out := runRunsCmd(t, stubRunServer(t, s), "watch")
	if !strings.Contains(out, "10 ok / 12 attempted") {
		t.Errorf("watch did not print the first frame:\n%s", out)
	}
	if !strings.Contains(out, "COMPLETE") {
		t.Errorf("watch did not print the terminal frame:\n%s", out)
	}
}

// start-load maps the ramp, soak, and PDU-session flags — the fields most
// likely to drift, and previously unexercised.
func TestRunsStartLoadMapsRampSoakPDU(t *testing.T) {
	s := &stubRunService{startRunID: "run-x"}
	runRunsCmd(t, stubRunServer(t, s),
		"start-load", "--amf", "10.0.0.1:38412", "--base-imsi", "208930100007500",
		"--ki", "00112233445566778899aabbccddeeff", "--opc", "000102030405060708090a0b0c0d0e0f",
		"--mcc", "208", "--mnc", "93",
		"--ramp", "5:50:30", "--duration", "5m", "--sample-interval", "10s",
		"--pdu-session", "--dnn", "internet", "--gnb-n3", "172.17.50.12", "--sd", "010203")
	spec := s.startReq.GetLoad()
	if spec.GetRampStart() != 5 || spec.GetRampEnd() != 50 || spec.GetRampSeconds() != 30 {
		t.Errorf("ramp = %v/%v/%v, want 5/50/30", spec.GetRampStart(), spec.GetRampEnd(), spec.GetRampSeconds())
	}
	if spec.GetDurationMs() != 300000 || spec.GetSampleIntervalMs() != 10000 {
		t.Errorf("soak ms = %d / %d, want 300000 / 10000", spec.GetDurationMs(), spec.GetSampleIntervalMs())
	}
	if spec.GetPduSession() == nil || spec.GetPduSession().GetDnn() != "internet" || spec.GetGnbN3Addr() != "172.17.50.12" {
		t.Errorf("pdu session not mapped: %+v", spec.GetPduSession())
	}
}

// A malformed ramp is rejected locally.
func TestRunsStartLoadRejectsBadRamp(t *testing.T) {
	s := &stubRunService{startRunID: "run-x"}
	cmd := newRunsCmd(stubRunServer(t, s))
	cmd.SetArgs([]string{"start-load", "--amf", "a:1", "--base-imsi", "1", "--ki", "00112233445566778899aabbccddeeff",
		"--opc", "000102030405060708090a0b0c0d0e0f", "--mcc", "208", "--mnc", "93", "--ramp", "nope"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.Execute(); err == nil {
		t.Error("a malformed --ramp was accepted")
	}
}

// Credentials fall back to the environment, consistently across start-load and
// start-fleet; a missing key is rejected locally with a clear error.
func TestRunsCredsFromEnv(t *testing.T) {
	t.Setenv("ORBIT_KI", "00112233445566778899aabbccddeeff")
	t.Setenv("ORBIT_OPC", "000102030405060708090a0b0c0d0e0f")
	s := &stubRunService{startRunID: "run-x"}
	runRunsCmd(t, stubRunServer(t, s),
		"start-load", "--amf", "a:1", "--base-imsi", "1", "--mcc", "208", "--mnc", "93")
	if s.startReq.GetLoad().GetCredentials().GetKi() == "" {
		t.Error("Ki not taken from the environment")
	}
}

func TestRunsRejectsMissingCreds(t *testing.T) {
	t.Setenv("ORBIT_KI", "")
	t.Setenv("ORBIT_OPC", "")
	s := &stubRunService{startRunID: "run-x"}
	cmd := newRunsCmd(stubRunServer(t, s))
	cmd.SetArgs([]string{"start-load", "--amf", "a:1", "--base-imsi", "1", "--mcc", "208", "--mnc", "93"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("missing creds error = %v, want a 'required' message", err)
	}
	if s.startReq != nil {
		t.Error("a run was started despite missing credentials")
	}
}

// A malformed Ki is rejected locally, before any request.
func TestRunsRejectsBadHexKey(t *testing.T) {
	s := &stubRunService{startRunID: "run-x"}
	cmd := newRunsCmd(stubRunServer(t, s))
	cmd.SetArgs([]string{"start-load", "--amf", "a:1", "--base-imsi", "1", "--mcc", "208", "--mnc", "93",
		"--ki", "not-hex", "--opc", "000102030405060708090a0b0c0d0e0f"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.Execute(); err == nil {
		t.Error("a malformed Ki was accepted")
	}
	if s.startReq != nil {
		t.Error("a run was started despite a bad key")
	}
}

// watch with no run id and no active run reports a clear error, not a hang.
func TestRunsWatchNoActiveRun(t *testing.T) {
	s := &stubRunService{} // ListRuns returns nothing
	cmd := newRunsCmd(stubRunServer(t, s))
	cmd.SetArgs([]string{"watch"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no active run") {
		t.Errorf("watch with no active run = %v, want a 'no active run' error", err)
	}
}

// An overlong soak duration is rejected rather than silently wrapping the
// uint32 milliseconds field.
func TestMsDurationRejectsOverflow(t *testing.T) {
	if _, err := msDuration("--duration", 60*24*time.Hour); err == nil {
		t.Error("a 60-day duration was accepted; uint32 ms overflows at ~49 days")
	}
	if ms, err := msDuration("--duration", 5*time.Minute); err != nil || ms != 300000 {
		t.Errorf("5m = %d, %v; want 300000, nil", ms, err)
	}
}
