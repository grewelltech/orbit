package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// GNBRotationFile is the gNB ID rotation's file in --state-dir.
const GNBRotationFile = "gnb-ids.json"

// Default gNB ID range a rotation assigns from: 1 to the largest 22-bit ID
// (the shortest gNB ID length, TS 38.413 9.3.1.6), so an assigned ID fits
// whatever length the gNB uses.
const (
	gnbIDFirst uint32 = 1
	gnbIDLast  uint32 = 1<<22 - 1
)

// GNBRotation assigns gNB IDs per (AMF, PLMN) so a new run never reuses an
// ID the AMF may still hold from an earlier one (ADR-0008 §5, QUICKSTART
// §9): IDs never used come first, in order; once the range is used up, the
// least recently released. Its state persists in --state-dir, so the order
// survives a restart. The IDs a live run holds are leases (leaseTable);
// Assign skips them through inUse.
//
// Until R1 gives the API an "assign" field, specs carry exact gNB IDs (pinned
// leases), so nothing calls Assign from the server yet.
type GNBRotation struct {
	path  string // "" = in memory only
	log   *slog.Logger
	now   func() time.Time
	first uint32
	last  uint32

	mu    sync.Mutex
	state rotationState
}

type rotationState struct {
	Version int                     `json:"version"`
	Keys    map[string]*rotationKey `json:"keys"` // "amf|plmn"
}

// rotationKey is one (AMF, PLMN)'s rotation: the next never-used ID, and
// when each used ID was last released (unix nanoseconds).
type rotationKey struct {
	Next     uint32           `json:"next"`
	Released map[uint32]int64 `json:"released"`
}

const rotationVersion = 1

// OpenGNBRotation loads the rotation from dir ("" keeps it in memory). A
// missing file is a fresh rotation; a corrupt one is logged and replaced by
// a fresh one, since the worst a lost rotation costs is reusing an ID
// sooner than ideal.
func OpenGNBRotation(dir string, log *slog.Logger) (*GNBRotation, error) {
	g := &GNBRotation{log: log, now: time.Now, first: gnbIDFirst, last: gnbIDLast,
		state: rotationState{Version: rotationVersion, Keys: map[string]*rotationKey{}}}
	if dir == "" {
		return g, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	g.path = filepath.Join(dir, GNBRotationFile)
	b, err := os.ReadFile(g.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return g, nil
	case err != nil:
		return nil, err
	}
	var st rotationState
	if err := json.Unmarshal(b, &st); err != nil || st.Version != rotationVersion || st.Keys == nil {
		if log != nil {
			log.Warn("gNB ID rotation file unreadable; starting a fresh rotation", "path", g.path, "err", err)
		}
		return g, nil
	}
	g.state = st
	return g, nil
}

func rotationKeyOf(amf, plmn string) string { return amf + "|" + plmn }

// Assign returns n gNB IDs for (amf, plmn): never-used ones first, then the
// least recently released, skipping any inUse reports (another run's
// lease). The AMF is keyed as given; callers pass the lease table's
// canonical form.
func (g *GNBRotation) Assign(amf, plmn string, n int, inUse func(uint32) bool) ([]uint32, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := g.keyLocked(amf, plmn)
	taken := map[uint32]bool{}
	out := make([]uint32, 0, n)
	for len(out) < n && k.Next != 0 && k.Next <= g.last {
		id := k.Next
		k.Next++
		if k.Next > g.last {
			k.Next = 0 // used up: from here on, least recently released
		}
		if inUse != nil && inUse(id) {
			continue
		}
		out = append(out, id)
		taken[id] = true
	}
	if len(out) < n {
		type rel struct {
			id uint32
			at int64
		}
		var cands []rel
		for id, at := range k.Released {
			if !taken[id] && (inUse == nil || !inUse(id)) {
				cands = append(cands, rel{id, at})
			}
		}
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].at != cands[j].at {
				return cands[i].at < cands[j].at
			}
			return cands[i].id < cands[j].id
		})
		for _, c := range cands {
			if len(out) == n {
				break
			}
			out = append(out, c.id)
			delete(k.Released, c.id) // in use until released again
		}
	}
	if len(out) < n {
		return nil, fmt.Errorf("no free gNB ID at AMF %s (PLMN %s): %d wanted, %d available", amf, plmn, n, len(out))
	}
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	return out, nil
}

// Released records that a run's IDs at (amf, plmn) are free again, now:
// they rejoin the rotation behind every ID released earlier.
func (g *GNBRotation) Released(amf, plmn string, ids []uint32) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := g.keyLocked(amf, plmn)
	at := g.now().UnixNano()
	for _, id := range ids {
		k.Released[id] = at
	}
	return g.saveLocked()
}

func (g *GNBRotation) keyLocked(amf, plmn string) *rotationKey {
	key := rotationKeyOf(amf, plmn)
	k := g.state.Keys[key]
	if k == nil {
		k = &rotationKey{Next: g.first, Released: map[uint32]int64{}}
		g.state.Keys[key] = k
	}
	if k.Released == nil {
		k.Released = map[uint32]int64{}
	}
	return k
}

// saveLocked writes the state atomically: a temp file in the same
// directory, fsynced, renamed over the file, and the directory fsynced.
func (g *GNBRotation) saveLocked() error {
	if g.path == "" {
		return nil
	}
	b, err := json.Marshal(g.state)
	if err != nil {
		return err
	}
	dir := filepath.Dir(g.path)
	f, err := os.CreateTemp(dir, "."+GNBRotationFile+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, g.path); err != nil {
		return err
	}
	ok = true
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
