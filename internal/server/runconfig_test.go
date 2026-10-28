package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/join"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/plan"
	"github.com/bgrewell/orbit/internal/secrets"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

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
// (no state dir) is NotFound, as GetRun answers it: its spec, still held
// in memory, is not served.
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
	if _, held := s.specs.peek(first); !held {
		t.Fatal("the first run's spec is not held: the case this test is for does not arise")
	}
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rot, err := engine.OpenGNBRotation("", log)
	if err != nil {
		t.Fatal(err)
	}
	arch, err := newArchiveStore(log, dir, 50)
	if err != nil {
		t.Fatal(err)
	}
	return &runService{log: log, reg: engine.NewRunRegistry(log, 0), rotation: rot, specs: newSpecStore(), archive: arch}
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

// archiveTried waits for run id's archiveRun to have run, saved or not:
// its sampler closes the run's frame log just after.
func archiveTried(t *testing.T, s *runService, id string) {
	t.Helper()
	sub := s.frameLogFor(id).subscribeFrom(0)
	deadline := time.After(30 * time.Second)
	for {
		select {
		case _, ok := <-sub.Ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the run's frame log never closed")
		}
	}
}

// A run whose archive cannot be saved keeps its spec held: its config is
// still served, resolved, never an empty unresolved answer. The state dir
// is replaced by a regular file, so the save fails whoever runs the test
// (a read-only dir does not stop root).
func TestGetRunConfigSaveFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s := archivedService(t, dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := startEnded(t, s)
	archiveTried(t, s, id) // archiveRun has run, and failed
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

// Through GetRunConfig on an archived run, as a past run is read: the
// notices' same_value is claimed only for a field that is not secret. A
// different ORBIT_KI over the target's ki, and an identical ORBIT_OPC
// over its opc, are both masked, and neither is claimed the same; an
// ORBIT_MCC equal to the target's is.
func TestGetRunConfigSameValueOnAnArchivedRun(t *testing.T) {
	s := archivedService(t, t.TempDir())
	env := func(v string) *orbitv1.ProvenanceSource {
		return &orbitv1.ProvenanceSource{Layer: orbitv1.ProvenanceLayer_PROVENANCE_LAYER_ENV, Detail: "env $" + v}
	}
	tgt := func(line int) *orbitv1.ProvenanceSource {
		return &orbitv1.ProvenanceSource{Layer: orbitv1.ProvenanceLayer_PROVENANCE_LAYER_TARGET, Detail: fmt.Sprintf("target lab1.yaml:%d", line)}
	}
	const keyA, keyB = "00112233445566778899aabbccddeeff", "ffeeddccbbaa99887766554433221100"
	p := loadPlan()
	p.Provenance = []*orbitv1.FieldProvenance{
		{Path: "core.plmn.mcc", Source: env("ORBIT_MCC"), Value: "001", Overridden: []*orbitv1.ProvenanceOverride{{Source: tgt(6), Value: "001"}}},
		{Path: "core.amf", Source: env("ORBIT_AMF"), Value: "192.0.2.2:38412", Overridden: []*orbitv1.ProvenanceOverride{{Source: tgt(5), Value: "192.0.2.1:38412"}}},
		{Path: "subscribers[main].ki", Source: env("ORBIT_KI"), Value: keyB, Overridden: []*orbitv1.ProvenanceOverride{{Source: tgt(9), Value: keyA}}},
		{Path: "subscribers[main].opc", Source: env("ORBIT_OPC"), Value: keyA, Overridden: []*orbitv1.ProvenanceOverride{{Source: tgt(10), Value: keyA}}},
	}
	ctx := context.Background()
	r, err := s.StartRun(ctx, connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: p}}))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Msg.GetRun().GetRunId()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, ok := s.archive.get(id); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never archived")
		}
	}
	got, err := s.GetRunConfig(ctx, connect.NewRequest(&orbitv1.GetRunConfigRequest{RunId: id}))
	if err != nil {
		t.Fatal(err)
	}
	same := map[string]bool{}
	for _, n := range got.Msg.GetNotices() {
		same[n.GetPath()] = n.GetSameValue()
		if text := plan.NoticeText(n); strings.Contains(text, keyA) || strings.Contains(text, keyB) {
			t.Errorf("a notice shows a key: %s", text)
		}
	}
	want := map[string]bool{"core.plmn.mcc": true, "core.amf": false, "subscribers[main].ki": false, "subscribers[main].opc": false}
	if !maps.Equal(same, want) {
		t.Errorf("same_value by path %v, want %v", same, want)
	}
}
