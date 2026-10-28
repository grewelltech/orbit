package cli

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/join"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/server"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

var updateRunConfig = flag.Bool("update-runconfig", false, "write the GetRunConfig answer for TestEnvOverTargetOnAPastRun's run, for the dashboard's tests")

// runConfigFixture is where the dashboard's provenance tests read a real
// GetRunConfig answer: a run whose env beat its target.
var runConfigFixture = filepath.Join("..", "..", "web", "src", "data", "testdata", "runconfig", "env-over-target.json")

// The debug view's case (ADR-0008 §3): an ORBIT_MCC left exported in the
// shell beats the target's mcc. --explain says so before anything starts,
// a waited-for run says so as it starts, and the finished run, archived,
// still says so through GetRunConfig, after the server restarts too: the
// provenance archived at StartRun is what the notice is drawn from.
func TestEnvOverTargetOnAPastRun(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	addr := sctpfront.Listen(t, amf)
	state := t.TempDir()
	serve := func() (*httptest.Server, orbitv1connect.RunServiceClient) {
		reg, err := join.OpenRegistry(state)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(server.New(observability.NewLogger(io.Discard, slog.LevelError), "test",
			observability.NewRegistry(), server.Options{StateDir: state, Responders: reg}))
		return srv, orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL)
	}
	srv, client := serve()

	// The target says 001/01; the shell says 208/93, which is the mock's.
	clearOrbitEnv(t)
	t.Setenv("ORBIT_MCC", "208")
	t.Setenv("ORBIT_MNC", "93")
	loc := targetDir(t, `version: 1
kind: target
name: core
core:
  amf: `+addr+`
  plmn: { mcc: "001", mnc: "01" }
  slices:
    - { sst: 1, sd: "010203" }
ran:
  n2_source_ips: [127.0.0.1]
`)
	file := filepath.Join(t.TempDir(), "suite.yaml")
	if err := os.WriteFile(file, []byte("kind: run\nworkloads:\n  - name: suite\n    conformance: {categories: [negative-ie], per_test_timeout_ms: 3000}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const notice = `notice: core.plmn.mcc: env $ORBIT_MCC (208) overrode target `
	explain := runOrbit(t, loc, "run", file, "-t", "core", "--explain")
	if explain.execErr != nil || !strings.Contains(explain.err, notice) || !strings.Contains(explain.err, `(001)`) {
		t.Fatalf("--explain: %v\nstderr: %s\nstdout: %s", explain.execErr, explain.err, explain.out)
	}
	waited := runOrbit(t, loc, "run", file, "-t", "core", "--server", srv.URL)
	if !strings.Contains(waited.err, notice) {
		t.Errorf("a waited-for run does not say it: %v\n%s", waited.execErr, waited.err)
	}
	runs, err := client.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{}))
	if err != nil || len(runs.Msg.GetRuns()) != 1 {
		t.Fatalf("runs: %v %v", runs, err)
	}
	id := runs.Msg.GetRuns()[0].GetRunId()

	check := func(when string, c orbitv1connect.RunServiceClient) {
		t.Helper()
		cfg, err := c.GetRunConfig(context.Background(), connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: id}))
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		m := cfg.Msg
		if !m.GetResolved() || m.GetPlan() == nil {
			t.Errorf("%s: resolved %t, plan %v", when, m.GetResolved(), m.GetPlan() != nil)
		}
		var mcc *orbitv1.FieldProvenance
		for _, p := range m.GetProvenance() {
			if p.GetPath() == "core.plmn.mcc" {
				mcc = p
			}
		}
		if mcc == nil || mcc.GetSource().GetLayer() != orbitv1.ProvenanceLayer_PROVENANCE_LAYER_ENV || mcc.GetValue() != "208" ||
			len(mcc.GetOverridden()) == 0 || mcc.GetOverridden()[len(mcc.GetOverridden())-1].GetValue() != "001" {
			t.Errorf("%s: core.plmn.mcc provenance %v", when, mcc)
		}
		paths := map[string]bool{}
		for _, n := range m.GetNotices() {
			paths[n.GetPath()] = true
		}
		if !paths["core.plmn.mcc"] || !paths["core.plmn.mnc"] || len(paths) != 2 {
			t.Errorf("%s: notices %v, want core.plmn.mcc and core.plmn.mnc", when, m.GetNotices())
		}
	}
	check("finished", client)
	if *updateRunConfig {
		cfg, err := client.GetRunConfig(context.Background(), connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: id}))
		if err != nil {
			t.Fatal(err)
		}
		b, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(cfg.Msg)
		if err != nil {
			t.Fatal(err)
		}
		// The test's temporary paths, as a user's would read: lab/…
		text := strings.ReplaceAll(string(b), filepath.Dir(loc.Dir+"/x"), "lab")
		text = strings.ReplaceAll(text, filepath.Dir(file), "lab")
		if err := os.MkdirAll(filepath.Dir(runConfigFixture), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(runConfigFixture, []byte(text+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The archive is written as the run ends, just after it: wait for it.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(25 * time.Millisecond) {
		if matches, _ := filepath.Glob(filepath.Join(state, id+".*")); len(matches) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run was never archived")
		}
	}
	// A new server over the same state: the run exists only in its archive.
	srv.Close()
	_, again := serve()
	check("archived, after a restart", again)
}
