package server

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testArchive(id string, startedNano int64) *orbitv1.RunArchive {
	return &orbitv1.RunArchive{
		Version: archiveVersion,
		Run: &orbitv1.Run{
			RunId:           id,
			Kind:            orbitv1.RunKind_RUN_KIND_FLEET,
			State:           orbitv1.RunState_RUN_STATE_COMPLETE,
			Name:            "run " + id,
			StartedUnixNano: startedNano,
		},
	}
}

func TestArchiveStoreRoundTripsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := newArchiveStore(quietLog(), dir, 10)
	if err != nil {
		t.Fatalf("newArchiveStore: %v", err)
	}
	if err := s.save(testArchive("run-aaaa", 100)); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A second store over the same directory is what a restart looks like.
	restarted, err := newArchiveStore(quietLog(), dir, 10)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := restarted.get("run-aaaa")
	if !ok {
		t.Fatal("run did not survive the restart, which is the whole point")
	}
	if got.GetRun().GetName() != "run run-aaaa" {
		t.Errorf("name = %q, want it preserved", got.GetRun().GetName())
	}
	if got.GetRun().GetState() != orbitv1.RunState_RUN_STATE_COMPLETE {
		t.Errorf("state = %v, want COMPLETE", got.GetRun().GetState())
	}
}

func TestArchiveStoreListsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	s, _ := newArchiveStore(quietLog(), dir, 10)
	for i, id := range []string{"run-old", "run-mid", "run-new"} {
		if err := s.save(testArchive(id, int64(i+1)*100)); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	// Ordering must survive a reload: it is reconstructed from the stored start
	// stamps, not from the order files happen to be read in.
	restarted, _ := newArchiveStore(quietLog(), dir, 10)
	var ids []string
	for _, a := range restarted.list() {
		ids = append(ids, a.GetRun().GetRunId())
	}
	want := []string{"run-new", "run-mid", "run-old"}
	for i := range want {
		if i >= len(ids) || ids[i] != want[i] {
			t.Fatalf("list = %v, want %v", ids, want)
		}
	}
}

func TestArchiveStoreEvictsOldestBeyondMax(t *testing.T) {
	dir := t.TempDir()
	s, _ := newArchiveStore(quietLog(), dir, 2)
	for i, id := range []string{"run-1", "run-2", "run-3"} {
		if err := s.save(testArchive(id, int64(i+1)*100)); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if _, ok := s.get("run-1"); ok {
		t.Error("run-1 should have been evicted past the bound")
	}
	for _, id := range []string{"run-2", "run-3"} {
		if _, ok := s.get(id); !ok {
			t.Errorf("%s should have been kept", id)
		}
	}
	// Eviction must remove the FILE too, or a restart resurrects it and the
	// bound means nothing across restarts.
	if _, err := os.Stat(filepath.Join(dir, "run-1"+archiveExt)); !os.IsNotExist(err) {
		t.Error("evicted archive file still on disk; it would return on restart")
	}
}

func TestArchiveStoreLoweredBoundAppliesOnRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := newArchiveStore(quietLog(), dir, 10)
	for i := range 5 {
		if err := s.save(testArchive(string(rune('a'+i))+"-run", int64(i+1)*100)); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	restarted, err := newArchiveStore(quietLog(), dir, 2)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n := len(restarted.list()); n != 2 {
		t.Errorf("kept %d runs, want the lowered bound of 2", n)
	}
}

func TestArchiveStoreSkipsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	s, _ := newArchiveStore(quietLog(), dir, 10)
	if err := s.save(testArchive("run-good", 100)); err != nil {
		t.Fatalf("save: %v", err)
	}
	// A truncated write, as a kill mid-save would leave.
	if err := os.WriteFile(filepath.Join(dir, "run-bad"+archiveExt), []byte("\xff\xfenot a proto"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := newArchiveStore(quietLog(), dir, 10)
	if err != nil {
		t.Fatalf("one corrupt archive must not fail startup: %v", err)
	}
	if _, ok := restarted.get("run-good"); !ok {
		t.Error("a corrupt neighbour hid a readable run")
	}
}

func TestArchiveStoreRejectsForeignVersion(t *testing.T) {
	dir := t.TempDir()
	s, _ := newArchiveStore(quietLog(), dir, 10)
	a := testArchive("run-future", 100)
	if err := s.save(a); err != nil {
		t.Fatal(err)
	}
	// Rewrite it claiming a version this build does not know.
	a.Version = archiveVersion + 99
	path := filepath.Join(dir, "run-future"+archiveExt)
	b, _ := marshalArchive(a)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := newArchiveStore(quietLog(), dir, 10)
	if err != nil {
		t.Fatalf("an unknown version must be skipped, not fatal: %v", err)
	}
	if _, ok := restarted.get("run-future"); ok {
		t.Error("archive of an unknown version was loaded rather than skipped")
	}
}

func TestArchiveStoreDisabledTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	s, err := newArchiveStore(quietLog(), "", 10)
	if err != nil {
		t.Fatalf("a disabled store must construct cleanly: %v", err)
	}
	if s.enabled() {
		t.Error("store with no directory reports itself enabled")
	}
	if err := s.save(testArchive("run-x", 100)); err != nil {
		t.Errorf("save on a disabled store must be a no-op, got %v", err)
	}
	if _, ok := s.get("run-x"); ok {
		t.Error("disabled store retained a run")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("disabled store wrote to the filesystem")
	}
}

func TestArchiveStoreRejectsTraversalInRunID(t *testing.T) {
	dir := t.TempDir()
	s, _ := newArchiveStore(quietLog(), dir, 10)
	for _, bad := range []string{"", "..", "../escape", "a/b"} {
		if _, err := s.path(bad); err == nil {
			t.Errorf("path(%q) was accepted; a run id must not select a file outside the store", bad)
		}
	}
}
