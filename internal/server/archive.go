package server

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// archiveVersion is the on-disk format version. Bumped only for a change proto
// cannot absorb — adding a field does not need it.
const archiveVersion = 1

// archiveExt is deliberately not ".pb": the extension names the format the file
// is in, and a reader that finds an unexpected extension should skip it rather
// than try to parse it as an archive.
const archiveExt = ".orbitrun"

// archiveStore persists terminal runs and serves them back after a restart.
//
// Runs are archived when they go TERMINAL, not journalled as they go. The
// per-frame write cost would otherwise be paid by every run to protect the one
// case where the data is least trustworthy anyway — a run whose own outcome was
// never decided.
//
// The consequence is that a run still in progress when the server stops is
// LOST, and that includes an ordinary `systemctl restart`: `orbit serve` does
// not handle SIGTERM, so runs are not cancelled and drained on the way out.
// Archiving an interrupted run needs graceful shutdown first, which is a
// question about server lifecycle rather than about storage.
//
// The store holds every archive in memory as well as on disk. A trimmed run
// measures well under a megabyte, the registry already caps history at 50, and
// keeping them resident means a restored run is served exactly like a live one
// with no read-through path to get wrong.
type archiveStore struct {
	log *slog.Logger
	dir string
	// max bounds archives on disk and in memory, mirroring the registry's own
	// history bound so the two do not disagree about how far back "history"
	// goes.
	max int

	mu sync.RWMutex
	// byID is every archive held, keyed by run id.
	byID map[string]*orbitv1.RunArchive
	// order is run ids oldest-first, for eviction and for stable listing.
	order []string
}

// newArchiveStore opens (creating if needed) a store rooted at dir. A store with
// an empty dir is disabled: every operation is a no-op and nothing touches the
// filesystem, which is what tests and CI want.
func newArchiveStore(log *slog.Logger, dir string, max int) (*archiveStore, error) {
	s := &archiveStore{log: log, dir: dir, max: max, byID: make(map[string]*orbitv1.RunArchive)}
	if dir == "" {
		return s, nil
	}
	if max <= 0 {
		return nil, fmt.Errorf("archive store: max must be positive, got %d", max)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("archive store %s: %w", dir, err)
	}
	if err := s.loadAll(); err != nil {
		return nil, err
	}
	return s, nil
}

// enabled reports whether the store persists anything.
func (s *archiveStore) enabled() bool { return s != nil && s.dir != "" }

// path returns the file an id is stored at. Run ids are server-generated
// ("run-" + 8 hex digits), but the value still reaches here from a request in
// principle, so the base name is checked rather than trusted.
func (s *archiveStore) path(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return "", fmt.Errorf("invalid run id %q", id)
	}
	return filepath.Join(s.dir, id+archiveExt), nil
}

// loadAll reads every archive in the directory, newest last in order.
//
// A single unreadable archive is logged and skipped rather than failing
// startup: one corrupt file (a truncated write from a kill mid-save) must not
// stop the server or hide the other fifty runs.
func (s *archiveStore) loadAll() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("archive store %s: %w", s.dir, err)
	}
	var loaded []*orbitv1.RunArchive
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != archiveExt {
			continue
		}
		full := filepath.Join(s.dir, e.Name())
		a, err := readArchive(full)
		if err != nil {
			s.log.Warn("skipping unreadable run archive", "path", full, "err", err)
			continue
		}
		loaded = append(loaded, a)
	}
	// Oldest first, so order matches the registry's insertion order and
	// eviction drops the oldest.
	sort.SliceStable(loaded, func(i, j int) bool {
		return loaded[i].GetRun().GetStartedUnixNano() < loaded[j].GetRun().GetStartedUnixNano()
	})
	for _, a := range loaded {
		id := a.GetRun().GetRunId()
		if id == "" {
			continue
		}
		s.byID[id] = a
		s.order = append(s.order, id)
	}
	s.evictLocked() // a lowered --run-history should take effect on restart
	if n := len(s.order); n > 0 {
		s.log.Info("restored run history", "runs", n, "dir", s.dir)
	}
	return nil
}

// marshalArchive encodes an archive for storage.
func marshalArchive(a *orbitv1.RunArchive) ([]byte, error) { return proto.Marshal(a) }

func readArchive(path string) (*orbitv1.RunArchive, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a orbitv1.RunArchive
	if err := proto.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	if v := a.GetVersion(); v != archiveVersion {
		return nil, fmt.Errorf("archive version %d, want %d", v, archiveVersion)
	}
	if a.GetRun().GetRunId() == "" {
		return nil, errors.New("archive has no run id")
	}
	return &a, nil
}

// save writes an archive and evicts anything beyond the bound. Errors are
// returned for logging, not for failing the run — the run itself succeeded, and
// losing its archive must not change that.
func (s *archiveStore) save(a *orbitv1.RunArchive) error {
	if !s.enabled() {
		return nil
	}
	id := a.GetRun().GetRunId()
	path, err := s.path(id)
	if err != nil {
		return err
	}
	a.Version = archiveVersion
	b, err := marshalArchive(a)
	if err != nil {
		return fmt.Errorf("marshal archive %s: %w", id, err)
	}
	// Write to a temporary file and rename: a reader (this process on its next
	// start, or someone copying the directory) then sees either the previous
	// archive or the complete new one, never a half-written file.
	tmp, err := os.CreateTemp(s.dir, "."+id+".*")
	if err != nil {
		return fmt.Errorf("archive %s: %w", id, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("archive %s: %w", id, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("archive %s: %w", id, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("archive %s: %w", id, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[id]; !exists {
		s.order = append(s.order, id)
	}
	s.byID[id] = a
	s.evictLocked()
	return nil
}

// evictLocked trims to max, oldest first, removing the file too. Callers hold
// mu, except loadAll which runs before the store is shared.
func (s *archiveStore) evictLocked() {
	for len(s.order) > s.max {
		id := s.order[0]
		s.order = s.order[1:]
		delete(s.byID, id)
		if path, err := s.path(id); err == nil {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				s.log.Warn("could not remove evicted run archive", "run_id", id, "err", err)
			}
		}
	}
}

// get returns an archived run.
func (s *archiveStore) get(id string) (*orbitv1.RunArchive, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.byID[id]
	return a, ok
}

// list returns every archived run, newest first — the order ListRuns reports.
func (s *archiveStore) list() []*orbitv1.RunArchive {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*orbitv1.RunArchive, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		if a, ok := s.byID[s.order[i]]; ok {
			out = append(out, a)
		}
	}
	return out
}

// ids returns every archived run id, oldest first.
func (s *archiveStore) ids() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}
