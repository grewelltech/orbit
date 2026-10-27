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

// DefaultGNBIDFirst and DefaultGNBIDLast are the range a rotation assigns
// from when the target gives none: 1 to the largest 22-bit ID (the
// shortest gNB ID length, TS 38.413 9.3.1.6), so an assigned ID fits
// whatever length the gNB uses.
const (
	DefaultGNBIDFirst uint32 = 1
	DefaultGNBIDLast  uint32 = 1<<22 - 1
)

// ErrGNBRangeExhausted is an Assign that found too few free IDs in the
// range: every one is held by a run or leased.
var ErrGNBRangeExhausted = errors.New("gNB ID range exhausted")

// ErrRotationPersist is an Assign whose choice could not be saved: nothing
// was assigned, so the caller can retry once the state directory is
// writable again.
var ErrRotationPersist = errors.New("could not persist gNB-ID rotation")

// rotationMaxEntries caps the use records kept per (AMF member, PLMN):
// past it the oldest go first, so a long run of fresh IDs cannot grow the
// file without bound.
const rotationMaxEntries = 65536

// GNBRotation assigns gNB IDs so a new run never reuses an ID the AMF may
// still hold from an earlier one (ADR-0008 §5, QUICKSTART §9): within the
// range a choice asks for, IDs never used come first, in order, then the
// least recently used. An AMF is known by its identity set (the name and
// every address it resolves to), and an ID's use under any member counts
// for all. The use times persist in --state-dir, so the order survives a
// restart; IDs handed out and not yet released are also kept in memory, so
// two Assigns never share one before its run takes the lease.
type GNBRotation struct {
	path string // "" = in memory only
	log  *slog.Logger
	now  func() time.Time

	mu         sync.Mutex
	state      rotationState
	held       map[string]map[uint32]bool // key → IDs handed out, not yet released
	maxEntries int                        // rotationMaxEntries; tests lower it
}

type rotationState struct {
	Version int                     `json:"version"`
	Keys    map[string]*rotationKey `json:"keys"` // "amf|plmn"
}

// rotationKey is one (AMF member, PLMN): when each ID it has handed out was
// last used (assigned, or released), unix nanoseconds.
type rotationKey struct {
	Used map[uint32]int64 `json:"used"`
}

const rotationVersion = 2

// OpenGNBRotation loads the rotation from dir ("" keeps it in memory). A
// missing file is a fresh rotation; a corrupt or older one is logged and
// replaced by a fresh one, since the worst a lost rotation costs is reusing
// an ID sooner than ideal.
func OpenGNBRotation(dir string, log *slog.Logger) (*GNBRotation, error) {
	g := &GNBRotation{log: log, now: time.Now,
		state:      rotationState{Version: rotationVersion, Keys: map[string]*rotationKey{}},
		held:       map[string]map[uint32]bool{},
		maxEntries: rotationMaxEntries}
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
	switch err := json.Unmarshal(b, &st); {
	case err != nil:
		if log != nil {
			log.Warn("gNB ID rotation file unreadable; starting a fresh rotation", "path", g.path, "err", err)
		}
		return g, nil
	case st.Version != rotationVersion || st.Keys == nil:
		if log != nil {
			log.Warn(fmt.Sprintf("gNB ID rotation file version %d replaced by a fresh v%d rotation", st.Version, rotationVersion), "path", g.path)
		}
		return g, nil
	}
	g.state = st
	return g, nil
}

func rotationKeyOf(amf, plmn string) string { return amf + "|" + plmn }

// Assign returns n gNB IDs in first..last for an AMF in plmn: never-used
// ones first, in order, then the least recently used, skipping IDs another
// Assign holds and any inUse reports (another run's lease). amfs is the
// AMF's identity set (RunRegistry.AMFIdentity), so the AMF reached by name
// and by address shares one rotation: an ID used under any member counts
// as used, at its latest use across them. Too few free IDs is
// ErrGNBRangeExhausted.
func (g *GNBRotation) Assign(amfs []string, plmn string, first, last uint32, n int, inUse func(uint32) bool) ([]uint32, error) {
	if len(amfs) == 0 {
		return nil, errors.New("gNB ID rotation: no AMF")
	}
	if last < first {
		return nil, fmt.Errorf("gNB ID rotation: bad range %d..%d", first, last)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	used := map[uint32]int64{} // latest use across the set
	held := map[uint32]bool{}
	for _, a := range amfs {
		k := g.keyLocked(a, plmn)
		for id, at := range k.Used {
			if at > used[id] {
				used[id] = at
			}
		}
		for id := range g.held[rotationKeyOf(a, plmn)] {
			held[id] = true
		}
	}
	free := func(id uint32) bool { return !held[id] && (inUse == nil || !inUse(id)) }
	out := make([]uint32, 0, n)
	// Never used, in order. Used IDs are few, so walking up from first is
	// bounded by n plus the used ones below it.
	for id := first; len(out) < n; id++ {
		if _, ok := used[id]; !ok && free(id) {
			out = append(out, id)
		}
		if id == last {
			break
		}
	}
	if len(out) < n {
		type cand struct {
			id uint32
			at int64
		}
		var cands []cand
		for id, at := range used {
			if id >= first && id <= last && free(id) {
				cands = append(cands, cand{id, at})
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
		}
	}
	if len(out) < n {
		return nil, fmt.Errorf("%w: %d..%d at AMF %s (PLMN %s) has %d free, %d wanted", ErrGNBRangeExhausted, first, last, amfs[0], plmn, len(out), n)
	}
	// Transactional: the choice is written to a copy of the state, saved,
	// and only then applied, so a failed save holds nothing.
	at := g.now().UnixNano()
	next := g.state.clone()
	for _, a := range amfs {
		k := next.key(a, plmn)
		for _, id := range out {
			k.Used[id] = at
		}
		g.prune(k, rotationKeyOf(a, plmn), out)
	}
	if err := g.save(next); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRotationPersist, err)
	}
	g.state = next
	for _, a := range amfs {
		key := rotationKeyOf(a, plmn)
		if g.held[key] == nil {
			g.held[key] = map[uint32]bool{}
		}
		for _, id := range out {
			g.held[key][id] = true
		}
	}
	return out, nil
}

// clone copies the state deeply enough to change use records on it.
func (st rotationState) clone() rotationState {
	out := rotationState{Version: st.Version, Keys: make(map[string]*rotationKey, len(st.Keys))}
	for k, v := range st.Keys {
		used := make(map[uint32]int64, len(v.Used))
		for id, at := range v.Used {
			used[id] = at
		}
		out.Keys[k] = &rotationKey{Used: used}
	}
	return out
}

// key is st's record for (amf, plmn), created empty.
func (st rotationState) key(amf, plmn string) *rotationKey {
	key := rotationKeyOf(amf, plmn)
	k := st.Keys[key]
	if k == nil {
		k = &rotationKey{}
		st.Keys[key] = k
	}
	if k.Used == nil {
		k.Used = map[uint32]int64{}
	}
	return k
}

// prune caps a key's use records at maxEntries, oldest dropped first; IDs
// held by a run, or being assigned now, are always kept. It never prunes by
// range: a key is an AMF, and other targets' blocks on the same AMF share
// it, so their history must survive this one's assignments.
func (g *GNBRotation) prune(k *rotationKey, key string, assigning []uint32) {
	keep := map[uint32]bool{}
	for id := range g.held[key] {
		keep[id] = true
	}
	for _, id := range assigning {
		keep[id] = true
	}
	if len(k.Used) <= g.maxEntries {
		return
	}
	type rec struct {
		id uint32
		at int64
	}
	recs := make([]rec, 0, len(k.Used))
	for id, at := range k.Used {
		if !keep[id] {
			recs = append(recs, rec{id, at})
		}
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].at != recs[j].at {
			return recs[i].at < recs[j].at
		}
		return recs[i].id < recs[j].id
	})
	for _, r := range recs {
		if len(k.Used) <= g.maxEntries {
			break
		}
		delete(k.Used, r.id)
	}
}

// Released records that a run's IDs at an AMF (its identity set, as for
// Assign) in plmn are free again, now, under every member: they rejoin the
// rotation behind every ID used earlier.
func (g *GNBRotation) Released(amfs []string, plmn string, ids []uint32) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	at := g.now().UnixNano()
	for _, a := range amfs {
		k := g.keyLocked(a, plmn)
		for _, id := range ids {
			k.Used[id] = at
			delete(g.held[rotationKeyOf(a, plmn)], id)
		}
	}
	g.persistLocked()
	return nil
}

// persistLocked saves the state for a Released, best effort: the IDs are
// already free in memory for live runs, and the next write catches the
// file up.
func (g *GNBRotation) persistLocked() {
	if err := g.save(g.state); err != nil && g.log != nil {
		g.log.Warn("gNB ID rotation not saved; it will be retried on the next change", "path", g.path, "err", err)
	}
}

func (g *GNBRotation) keyLocked(amf, plmn string) *rotationKey { return g.state.key(amf, plmn) }

// save writes st atomically: a temp file in the same directory, fsynced,
// renamed over the file, and the directory fsynced. Callers hold g.mu.
func (g *GNBRotation) save(st rotationState) error {
	if g.path == "" {
		return nil
	}
	b, err := json.Marshal(st)
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
