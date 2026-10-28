package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// launchFixture is a request the Launch page sends, as its tests keep it
// (web/src/data/launch/fixtures.test.ts): the documents, and the names of
// the ${VAR}s whose values the page holds in the tab.
type launchFixture struct {
	TargetYAML string   `json:"targetYaml"`
	RunYAML    string   `json:"runYaml"`
	Template   string   `json:"template"`
	Vars       []string `json:"vars"`
}

// The Launch page's requests, replayed as the page sends them: PlanRun
// resolves each, StartRun runs the plan it gave, unchanged, against a mock
// core, and the run ends COMPLETE with every gate the plan has judged.
func TestLaunchRequestsRunEndToEnd(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "web", "src", "data", "launch", "testdata", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Launch fixtures: %v", err)
	}
	for _, path := range files {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fx launchFixture
			if err := json.Unmarshal(b, &fx); err != nil {
				t.Fatal(err)
			}
			ki, _ := auth.ParseHexKey("Ki", archKi)
			opc, _ := auth.ParseHexKey("OPc", archOpc)
			amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203"})
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]string{"LAB_AMF": sctpfront.Listen(t, amf), "ORBIT_KI": archKi, "ORBIT_OPC": archOpc}
			req := &orbitv1.ResolveRequest{TargetYaml: fx.TargetYAML, RunYaml: fx.RunYAML, Template: fx.Template}
			for _, v := range fx.Vars {
				val, ok := values[v]
				if !ok {
					t.Fatalf("the fixture names ${%s}, which this test has no value for", v)
				}
				req.Vars = append(req.Vars, &orbitv1.Variable{Name: v, Value: val})
			}
			dir := t.TempDir()
			h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", prometheus.NewRegistry(), Options{StateDir: dir, Responders: openResponders(t, dir)})
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			c := orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL)
			ctx := context.Background()

			planned, err := c.PlanRun(ctx, connect.NewRequest(req))
			if err != nil {
				t.Fatalf("PlanRun: %v", err)
			}
			p := planned.Msg.GetPlan()
			sent := proto.Clone(p).(*orbitv1.RunPlan)
			started, err := c.StartRun(ctx, connect.NewRequest(&orbitv1.StartRunRequest{Name: planned.Msg.GetRunName(),
				Spec: &orbitv1.StartRunRequest_Plan{Plan: p}, EventVerbosity: planned.Msg.GetEventVerbosity()}))
			if err != nil {
				t.Fatalf("StartRun with PlanRun's plan: %v", err)
			}
			if !proto.Equal(p, sent) {
				t.Error("StartRun changed the plan it was sent")
			}
			id := started.Msg.GetRun().GetRunId()
			run := waitTerminal(t, c, id, 60*time.Second)
			if run.GetState() != orbitv1.RunState_RUN_STATE_COMPLETE {
				t.Fatalf("run %s ended %s (%s), want COMPLETE", id, run.GetState(), run.GetError())
			}
			rep, err := c.GetRunReport(ctx, connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id}))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := len(rep.Msg.GetGates()), len(p.GetGates()); got != want {
				t.Errorf("%d gates judged, want the plan's %d", got, want)
			}
			// A suite ran its checks: results, one per check selected.
			suites := []*orbitv1.ConformanceSummary{rep.Msg.GetConformance()}
			for _, w := range rep.Msg.GetWorkloads() {
				suites = append(suites, w.GetConformance())
			}
			for _, w := range p.GetWorkloads() {
				if w.GetConformance() == nil {
					continue
				}
				ran := 0
				for _, cs := range suites {
					ran += len(cs.GetResults())
				}
				if ran == 0 {
					t.Errorf("suite %s ran no checks", w.GetName())
				}
			}
			// A population against the mock core attaches every UE: its
			// SLO gates pass. A suite's verdicts are the mock's to give.
			for _, g := range rep.Msg.GetGates() {
				if strings.HasPrefix(g.GetGate().GetName(), "attach_success") && !g.GetPassed() {
					t.Errorf("gate %s failed: %s", g.GetGate().GetName(), g.GetDetail())
				}
			}
		})
	}
}

// waitTerminal polls run id until it is no longer pending, running or draining.
func waitTerminal(t *testing.T, c orbitv1connect.RunServiceClient, id string, within time.Duration) *orbitv1.Run {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		r, err := c.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		switch r.Msg.GetRun().GetState() {
		case orbitv1.RunState_RUN_STATE_PENDING, orbitv1.RunState_RUN_STATE_RUNNING, orbitv1.RunState_RUN_STATE_DRAINING:
			continue
		}
		return r.Msg.GetRun()
	}
	t.Fatalf("run %s did not end within %s", id, within)
	return nil
}
