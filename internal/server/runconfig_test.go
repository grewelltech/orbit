package server

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/join"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/plan"
	"github.com/bgrewell/orbit/internal/secrets"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

var updateRunConfig = flag.Bool("update-runconfig", false, "write TestGetRunConfig's archived answer for the dashboard's tests")

// runConfigKeysFixture is a real GetRunConfig answer for a run with keys,
// a key a provenance entry overrode, and the server's own (LEASE) entries:
// the dashboard's provenance tests read its masked rows.
var runConfigKeysFixture = filepath.Join("..", "..", "web", "src", "data", "testdata", "runconfig", "keys-and-leases.json")

// writeNoticeTexts writes, next to a GetRunConfig fixture, the text
// plan.NoticeText gives each of its notices, in order: the dashboard's
// noticeText is pinned to say exactly the same. rewrite is the fixture's
// own rewrite (its temporary paths and addresses), applied to each text.
func writeNoticeTexts(t *testing.T, fixture string, notices []*orbitv1.OverrideNotice, rewrite func(string) string) {
	t.Helper()
	texts := make([]string, 0, len(notices))
	for _, n := range notices {
		texts = append(texts, rewrite(plan.NoticeText(n)))
	}
	b, err := json.MarshalIndent(texts, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSuffix(fixture, ".json")+".text.json", append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// GetRunConfig serves a run's plan and provenance as the archive holds
// them: from the held spec while the run is live (read, not taken: the
// archive still gets it), from the archive after. Nothing secret is in it:
// not the keys or tokens the plan carried, not a key a provenance entry
// overrode, not the loom token the registry filled in at the start.
func TestGetRunConfig(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", archKi)
	opc, _ := auth.ParseHexKey("OPc", archOpc)
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	addr := sctpfront.Listen(t, amf)
	dir := t.TempDir()
	reg, loomToken := onlineResponder(t, dir)
	srv := httptest.NewServer(New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", prometheus.NewRegistry(), Options{StateDir: dir, Responders: reg}))
	t.Cleanup(srv.Close)
	c := orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	// A fleet that keeps its UEs a few seconds (live long enough to read),
	// its latency probe echoing app1, a responder registered by join: the
	// server fills app1's loom token into its own copy at the start.
	p := loadPlan(func(p *orbitv1.RunPlan) {
		p.Target.Core.Amf = addr
		p.Target.App.Responders = append([]*orbitv1.Responder{{Name: "app1"}}, p.Target.App.Responders...)
		pop(p).Behaviors = &orbitv1.Behaviors{Latency: &orbitv1.LatencyProbe{IntervalMs: 500}}
		p.Until = &orbitv1.Until{Stop: &orbitv1.Until_DurationMs{DurationMs: 3000}}
		// A gate named after the token: no field marks it, so only the
		// scrub of the request's secret values takes it out.
		p.Gates = []*orbitv1.Gate{{Name: archToken, Check: &orbitv1.Gate_Slo{Slo: &orbitv1.SLOGate{
			Metric: metricSuccessRatio, Op: orbitv1.CompareOp_COMPARE_OP_GE, Threshold: 0}}}}
	})
	resp, err := c.StartRun(ctx, connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: p}}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetRun().GetRunId()
	read := func(when string) *orbitv1.GetRunConfigResponse {
		t.Helper()
		r, err := c.GetRunConfig(ctx, connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: id}))
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		b, _ := proto.Marshal(r.Msg)
		j, _ := json.Marshal(r.Msg)
		for _, k := range []string{archKi, archOpc, archToken, oldKi, loomToken} {
			if strings.Contains(string(b), k) || strings.Contains(string(j), k) {
				t.Errorf("%s: the reply holds %q", when, k)
			}
		}
		if r.Msg.GetPlan().GetTarget().GetSubscribers()[0].GetKi() != secrets.Masked || !r.Msg.GetResolved() {
			t.Errorf("%s: plan ki %q, resolved %t", when, r.Msg.GetPlan().GetTarget().GetSubscribers()[0].GetKi(), r.Msg.GetResolved())
		}
		lease := false
		for _, pv := range r.Msg.GetProvenance() {
			lease = lease || pv.GetSource().GetLayer() == orbitv1.ProvenanceLayer_PROVENANCE_LAYER_LEASE
		}
		if !lease {
			t.Errorf("%s: no LEASE entry (the server's own: app1 from the registry)", when)
		}
		return r.Msg
	}
	live := read("live")
	if again := read("live, again"); !proto.Equal(again, live) {
		t.Error("a second read differs: the first changed what is held")
	}
	if info, err := c.GetRun(ctx, connect.NewRequest(&orbitv1.GetRunRequest{RunId: id})); err != nil || info.Msg.GetRun().GetState() != orbitv1.RunState_RUN_STATE_RUNNING {
		t.Logf("read while %v (%v)", info.Msg.GetRun().GetState(), err)
	}
	path := filepath.Join(dir, id+archiveExt)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(25 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never archived")
		}
	}
	archived := read("archived")
	if *updateRunConfig {
		b, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(archived)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(b), addr, "192.0.2.10:38412")
		if err := os.MkdirAll(filepath.Dir(runConfigKeysFixture), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(runConfigKeysFixture, []byte(text+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeNoticeTexts(t, runConfigKeysFixture, archived.GetNotices(), func(s string) string {
			return strings.ReplaceAll(s, addr, "192.0.2.10:38412")
		})
	}
	// Reading the live spec took nothing from the archive.
	if len(archived.GetProvenance()) < len(live.GetProvenance()) || archived.GetPlan() == nil {
		t.Errorf("archived: %d provenance entries and plan %t; live had %d", len(archived.GetProvenance()), archived.GetPlan() != nil, len(live.GetProvenance()))
	}

	// A run the server does not know.
	if _, err := c.GetRunConfig(ctx, connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: "run-00000000"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown run: %v, want NotFound", err)
	}

	// A plan built without the resolver (start-load, start-fleet) has no
	// client provenance: resolved is false.
	// Its gNB IDs assigned, so the server adds its own (LEASE) entry: still
	// not resolved.
	bare := loadPlan(func(p *orbitv1.RunPlan) {
		p.Provenance = nil
		p.Target.Core.Amf = addr
		p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{First: 7, Count: 4, Bits: 24}
		pop(p).Topology = &orbitv1.Topology{GnbIds: &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_Assign{Assign: true}}}
	})
	r2, err := c.StartRun(ctx, connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: bare}}))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := c.GetRunConfig(ctx, connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: r2.Msg.GetRun().GetRunId()}))
	if err != nil || cfg.Msg.GetResolved() {
		t.Errorf("a plan without provenance: %v, resolved %t, want false", err, cfg.Msg.GetResolved())
	}
	if len(cfg.Msg.GetProvenance()) == 0 {
		t.Error("the server's own entry (the assigned gNB IDs) is missing: the row tests nothing")
	}
}

// A run the registry has let go (beyond its history) and no archive holds
// (no state dir) is NotFound, as GetRun answers it: a spec of it still held
// in memory is not served.
func TestGetRunConfigEvictedRun(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rot, err := engine.OpenGNBRotation("", log)
	if err != nil {
		t.Fatal(err)
	}
	s := &runService{log: log, reg: engine.NewRunRegistry(log, 1), rotation: rot, specs: newSpecStore()}
	ctx := context.Background()
	start := func() string {
		r, err := s.StartRun(ctx, connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: loadPlan()}}))
		if err != nil {
			t.Fatal(err)
		}
		id := r.Msg.GetRun().GetRunId()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if info, err := s.reg.Get(id); err == nil && info.State.Terminal() {
				return id
			}
			if time.Now().After(deadline) {
				t.Fatal("the run never ended")
			}
		}
	}
	first := start()
	if _, err := s.GetRunConfig(ctx, connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: first})); err != nil {
		t.Fatalf("while the registry holds it: %v", err)
	}
	start()
	start() // the history keeps one terminal run: the first is gone
	if _, err := s.reg.Get(first); err == nil {
		t.Fatal("the first run was not evicted")
	}
	// Its spec was let go with it (TestHeldSpecsBoundedByHistory), by the
	// last run's end: wait for that, then hold one again, as a run whose
	// spec outlived it, which still must not be served.
	for deadline := time.Now().Add(5 * time.Second); !slices.Equal(heldIDs(s), registryIDs(s)) && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	s.specs.put(first, &orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: loadPlan()}})
	if _, err := s.GetRunConfig(ctx, connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: first})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an evicted, never-archived run: %v, want NotFound", err)
	}
}

// onlineResponder is a registry under dir holding app1, registered by join
// and online, and its loom token.
func onlineResponder(t *testing.T, dir string) (*join.Registry, string) {
	t.Helper()
	reg, err := join.OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := join.NewStore()
	_, secret, err := store.Create("", 0, join.Spec{UEPools: []string{"10.250.0.0/16"}, UPFN6: "198.51.100.1", N6Subnet: "198.51.100.0/24", Management: "192.0.2.10:8412"})
	if err != nil {
		t.Fatal(err)
	}
	cred := newCred(t)
	rs, err := reg.Register(store, join.RegisterRequest{JoinToken: secret, Credential: cred, Name: "app1", Control: "127.0.0.1:9",
		DataIP: "127.0.0.1", PortLow: 32768, PortHigh: 60999})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Heartbeat(cred, "127.0.0.1:1", "", true, false); err != nil {
		t.Fatal(err)
	}
	return reg, rs.LoomToken
}

// archivedService is a runService over dir's archive, built by hand.
func archivedService(t *testing.T, dir string) *runService {
	t.Helper()
	return historyService(t, dir, engine.DefaultRunHistory)
}

// historyService is a runService whose registry and archive (under dir;
// none if dir is "") each keep history terminal runs.
func historyService(t *testing.T, dir string, history int) *runService {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rot, err := engine.OpenGNBRotation("", log)
	if err != nil {
		t.Fatal(err)
	}
	arch, err := newArchiveStore(log, dir, history)
	if err != nil {
		t.Fatal(err)
	}
	return &runService{log: log, reg: engine.NewRunRegistry(log, history), rotation: rot, specs: newSpecStore(), archive: arch}
}

// heldIDs is the ids of the runs whose specs are held, sorted.
func heldIDs(s *runService) []string {
	s.specs.mu.Lock()
	defer s.specs.mu.Unlock()
	ids := make([]string, 0, len(s.specs.specs))
	for id := range s.specs.specs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// registryIDs is the ids of the runs the registry holds, sorted.
func registryIDs(s *runService) []string {
	var ids []string
	for _, info := range s.reg.List() {
		ids = append(ids, info.ID)
	}
	sort.Strings(ids)
	return ids
}

// A spec that is never saved, after a failed save or on every run when
// there is no state dir, is let go when the registry lets its run go: the
// specs held, each with its request's secret values, are bounded by the
// registry's history, and are exactly its runs'.
func TestHeldSpecsBoundedByHistory(t *testing.T) {
	const history = 2
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) string // the state dir, "" for none
	}{
		{"no state dir", func(*testing.T) string { return "" }},
		{"every save fails", func(t *testing.T) string {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			return dir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := historyService(t, tc.setup(t), history)
			for range history + 2 {
				startEnded(t, s)
			}
			// The last run's end sweeps, just after the registry has it ended.
			want := registryIDs(s)
			for deadline := time.Now().Add(5 * time.Second); !slices.Equal(heldIDs(s), want) && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			}
			if got := heldIDs(s); !slices.Equal(got, want) || len(got) > history {
				t.Errorf("specs held for %v; want the registry's %d runs %v", got, history, want)
			}
		})
	}
}

// The registry evicts in the order runs start, the archive in the order
// they end, so a run can still be in the registry, ended, with its spec let
// go and its archive evicted. It is NotFound, not "starting; try again".
// With a history of 2: A starts, B starts, B ends, A ends, C runs. The
// registry keeps B and C; the archive, A and C.
func TestGetRunConfigEndedRunWhoseArchiveWasEvicted(t *testing.T) {
	s := historyService(t, t.TempDir(), 2)
	ctx := context.Background()
	start := func() (string, chan struct{}) {
		release := make(chan struct{})
		info, err := s.reg.StartLoad(engine.RunOptions{}, func(ctx context.Context, _ *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
			<-release
			return load.Report{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		s.specs.put(info.ID, &orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: loadPlan()}})
		s.startSampler(info.ID)
		return info.ID, release
	}
	archived := func(id string) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, ok := s.archive.get(id); ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s was never archived", id)
			}
		}
	}
	a, relA := start()
	b, relB := start()
	close(relB)
	archived(b)
	close(relA)
	archived(a)
	c, relC := start()
	close(relC)
	archived(c)

	if info, err := s.reg.Get(b); err != nil || !info.State.Terminal() {
		t.Fatalf("B in the registry: %v %v; the case this test is for does not arise", info.State, err)
	}
	if _, ok := s.archive.get(b); ok {
		t.Fatal("B still archived: the case this test is for does not arise")
	}
	_, err := s.GetRunConfig(ctx, connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: b}))
	if connect.CodeOf(err) != connect.CodeNotFound || !strings.Contains(err.Error(), "archive was evicted") {
		t.Errorf("B, ended, its archive evicted: %v, want NotFound", err)
	}
	// A, which the registry let go, is served from its archive.
	if got, err := s.GetRunConfig(ctx, connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: a})); err != nil || got.Msg.GetPlan() == nil {
		t.Errorf("A, from its archive: %v", err)
	}
}

// startEnded starts a plan run (its AMF unreachable, so it ends at once)
// and returns its id once the registry has it terminal.
func startEnded(t *testing.T, s *runService) string {
	t.Helper()
	r, err := s.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: loadPlan()}}))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Msg.GetRun().GetRunId()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if info, err := s.reg.Get(id); err == nil && info.State.Terminal() {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never ended")
		}
	}
}

// A run whose archive cannot be saved keeps its spec held: its config is
// still served, resolved, never an empty unresolved answer.
func TestGetRunConfigSaveFails(t *testing.T) {
	dir := t.TempDir()
	s := archivedService(t, dir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	id := startEnded(t, s)
	time.Sleep(200 * time.Millisecond) // archiveRun has run, and failed
	if _, ok := s.archive.get(id); ok {
		t.Fatal("archived after all: the case this test is for does not arise")
	}
	r, err := s.GetRunConfig(context.Background(), connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: id}))
	if err != nil || !r.Msg.GetResolved() || r.Msg.GetPlan() == nil {
		t.Errorf("after a failed save: %v, resolved %t, plan %t; want the held config", err, r.Msg.GetResolved(), r.Msg.GetPlan() != nil)
	}
}

// Reads straddling a run's end and its archiving (under -race: the held
// spec is read and copied, never shared) always answer the run's config,
// resolved: there is no moment between the spec going and the archive
// being there.
func TestGetRunConfigDuringArchiving(t *testing.T) {
	s := archivedService(t, t.TempDir())
	ctx := context.Background()
	r, err := s.StartRun(ctx, connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: loadPlan()}}))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Msg.GetRun().GetRunId()
	reads := 0
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); reads++ {
		got, err := s.GetRunConfig(ctx, connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: id}))
		if connect.CodeOf(err) == connect.CodeUnavailable && reads == 0 {
			continue // the moment before StartRun held the spec
		}
		if err != nil || !got.Msg.GetResolved() || got.Msg.GetPlan() == nil {
			t.Fatalf("read %d: %v, resolved %t", reads, err, got.Msg.GetResolved())
		}
		if _, archived := s.archive.get(id); archived {
			if _, held := s.specs.get(id); !held {
				break // archived and let go: every read before was whole
			}
		}
	}
	if _, held := s.specs.get(id); held {
		t.Error("the spec is still held after the archive was saved")
	}
	if got, err := s.GetRunConfig(ctx, connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: id})); err != nil || !got.Msg.GetResolved() {
		t.Errorf("from the archive: %v", err)
	}
}

// In the moment the registry has a run and StartRun has not yet held its
// spec, the answer is Unavailable (try again), not an empty config.
func TestGetRunConfigBeforeTheSpecIsHeld(t *testing.T) {
	s := archivedService(t, t.TempDir())
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	info, err := s.reg.StartLoad(engine.RunOptions{}, func(ctx context.Context, _ *load.LiveStats, _ engine.RunEventFunc) (load.Report, error) {
		<-release
		return load.Report{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.GetRunConfig(context.Background(), connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: info.ID}))
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "starting; try again") {
		t.Errorf("before the spec is held: %v, want Unavailable", err)
	}
}
