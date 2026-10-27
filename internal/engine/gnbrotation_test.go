package engine

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// smallRotation is a rotation over IDs 1..5 with a controllable clock.
func smallRotation(t *testing.T, dir string, now *time.Time) *GNBRotation {
	t.Helper()
	g, err := OpenGNBRotation(dir, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return *now }
	return g
}

func TestGNBRotationOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	g := smallRotation(t, "", &now)
	const amf, plmn = "10.0.0.1:38412", "001/01"
	assign := func(n int, inUse func(uint32) bool) []uint32 {
		t.Helper()
		ids, err := g.Assign([]string{amf}, plmn, 1, 5, n, inUse)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	// Never-used IDs first, in order, skipping one another run holds.
	if got := assign(2, func(id uint32) bool { return id == 2 }); !reflect.DeepEqual(got, []uint32{1, 3}) {
		t.Fatalf("first assignment = %v, want [1 3]", got)
	}
	// 3 released early, 1 later.
	now = now.Add(time.Second)
	if err := g.Released([]string{amf}, plmn, []uint32{3}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := g.Released([]string{amf}, plmn, []uint32{1}); err != nil {
		t.Fatal(err)
	}
	// The never-used IDs first, in order: 2 (only skipped while another
	// run held it), 4, 5; then the least recently used, 3 before 1.
	if got := assign(4, nil); !reflect.DeepEqual(got, []uint32{2, 4, 5, 3}) {
		t.Fatalf("second assignment = %v, want [2 4 5 3]", got)
	}
	if got := assign(1, nil); !reflect.DeepEqual(got, []uint32{1}) {
		t.Fatalf("third assignment = %v, want [1]", got)
	}
	// Every ID is now held by an Assign whose run has not released it.
	if _, err := g.Assign([]string{amf}, plmn, 1, 5, 1, nil); !errors.Is(err, ErrGNBRangeExhausted) {
		t.Errorf("with every ID held: err = %v, want ErrGNBRangeExhausted", err)
	}
	now = now.Add(time.Second)
	if err := g.Released([]string{amf}, plmn, []uint32{2}); err != nil {
		t.Fatal(err)
	}
	if got := assign(1, nil); !reflect.DeepEqual(got, []uint32{2}) {
		t.Errorf("after 2 was released: %v, want [2]", got)
	}
}

func TestGNBRotationExhaustion(t *testing.T) {
	now := time.Unix(1000, 0)
	g := smallRotation(t, "", &now)
	const amf, plmn = "10.0.0.1:38412", "001/01"
	ids, err := g.Assign([]string{amf}, plmn, 1, 5, 5, nil)
	if err != nil || !reflect.DeepEqual(ids, []uint32{1, 2, 3, 4, 5}) {
		t.Fatalf("Assign = %v, %v", ids, err)
	}
	if _, err := g.Assign([]string{amf}, plmn, 1, 5, 1, nil); err == nil {
		t.Fatal("assigned an ID with the range used up and none released")
	}
	for i, id := range []uint32{4, 2, 5} {
		now = time.Unix(2000+int64(i), 0)
		if err := g.Released([]string{amf}, plmn, []uint32{id}); err != nil {
			t.Fatal(err)
		}
	}
	// Least recently released first, and one held by another run skipped.
	ids, err = g.Assign([]string{amf}, plmn, 1, 5, 2, func(id uint32) bool { return id == 4 })
	if err != nil || !reflect.DeepEqual(ids, []uint32{2, 5}) {
		t.Fatalf("Assign after release = %v, %v; want [2 5]", ids, err)
	}
	// Another (AMF, PLMN) has its own rotation.
	if ids, err := g.Assign([]string{"10.0.0.2:38412"}, plmn, 1, 5, 1, nil); err != nil || ids[0] != 1 {
		t.Errorf("another AMF = %v, %v; want [1]", ids, err)
	}
}

// The rotation survives a restart: a reopened rotation continues where the
// last left off, and the file is written atomically at mode 0600.
func TestGNBRotationPersists(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000, 0)
	g := smallRotation(t, dir, &now)
	const amf, plmn = "10.0.0.1:38412", "001/01"
	if _, err := g.Assign([]string{amf}, plmn, 1, 5, 5, nil); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(2000, 0)
	if err := g.Released([]string{amf}, plmn, []uint32{3}); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(3000, 0)
	if err := g.Released([]string{amf}, plmn, []uint32{1}); err != nil {
		t.Fatal(err)
	}

	// A restart ends every run, so IDs assigned before it are free again;
	// used least recently (2, 4, 5 at their assignment) they come first,
	// then 3 and 1 in release order.
	again := smallRotation(t, dir, &now)
	ids, err := again.Assign([]string{amf}, plmn, 1, 5, 5, nil)
	if err != nil || !reflect.DeepEqual(ids, []uint32{2, 4, 5, 3, 1}) {
		t.Fatalf("after a restart Assign = %v, %v; want [2 4 5 3 1]", ids, err)
	}
	st, err := os.Stat(filepath.Join(dir, GNBRotationFile))
	if err != nil || st.Mode().Perm()&0o077 != 0 {
		t.Errorf("rotation file: %v %v", st, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left %d files, want only %s", len(entries), GNBRotationFile)
	}
}

// A corrupt or foreign rotation file is replaced by a fresh rotation, not
// an error that stops the server.
func TestGNBRotationCorruptFile(t *testing.T) {
	for name, content := range map[string]string{
		"not JSON":      "{nope",
		"other version": `{"version": 99, "keys": {}}`,
		"no keys":       `{"version": 1}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, GNBRotationFile), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			now := time.Unix(1000, 0)
			g := smallRotation(t, dir, &now)
			ids, err := g.Assign([]string{"a"}, "p", 1, 5, 1, nil)
			if err != nil || ids[0] != 1 {
				t.Errorf("Assign on a fresh rotation = %v, %v", ids, err)
			}
		})
	}
}

// One AMF reached by name and by address shares a rotation: an ID used and
// released under the name is not handed out again by address while unused
// IDs remain, and a release under one member ages the ID for all.
func TestGNBRotationSharedAcrossIdentity(t *testing.T) {
	now := time.Unix(1000, 0)
	g := smallRotation(t, "", &now)
	const plmn = "001/01"
	byName := []string{"10.0.0.1:38412", "amf.lab:38412"} // the name and its address
	byIP := []string{"10.0.0.1:38412"}
	a, err := g.Assign(byName, plmn, 1, 5, 1, nil)
	if err != nil || a[0] != 1 {
		t.Fatalf("by name: %v, %v", a, err)
	}
	now = now.Add(time.Second)
	if err := g.Released(byName, plmn, a); err != nil {
		t.Fatal(err)
	}
	b, err := g.Assign(byIP, plmn, 1, 5, 1, nil)
	if err != nil || b[0] == a[0] {
		t.Fatalf("by address after the name's release: %v, %v; want an ID other than %d while unused ones remain", b, err, a[0])
	}
	// And the other way: an ID released by address is the newest release
	// for the name too.
	now = now.Add(time.Second)
	if err := g.Released(byIP, plmn, b); err != nil {
		t.Fatal(err)
	}
	c, err := g.Assign(byName, plmn, 1, 5, 3, nil)
	if err != nil || reflect.DeepEqual(c[:1], b) || reflect.DeepEqual(c[:1], a) {
		t.Fatalf("by name again: %v, %v; want never-used IDs first", c, err)
	}
}

// The range is a parameter of each choice: IDs come from it, never-used
// first, and a later, narrower range on the same AMF draws only from its
// own block; a range whose every ID is held is ErrGNBRangeExhausted.
func TestGNBRotationRanges(t *testing.T) {
	now := time.Unix(1000, 0)
	g := smallRotation(t, "", &now)
	amf := []string{"10.0.0.1:38412"}
	const plmn = "001/01"
	got, err := g.Assign(amf, plmn, 5000, 5019, 6, nil)
	if err != nil || !reflect.DeepEqual(got, []uint32{5000, 5001, 5002, 5003, 5004, 5005}) {
		t.Fatalf("block 5000..5019: %v, %v", got, err)
	}
	got, err = g.Assign(amf, plmn, 5004, 5007, 2, nil)
	if err != nil || !reflect.DeepEqual(got, []uint32{5006, 5007}) {
		t.Fatalf("narrower 5004..5007: %v, %v; want its two unheld IDs", got, err)
	}
	if _, err := g.Assign(amf, plmn, 5004, 5007, 1, nil); !errors.Is(err, ErrGNBRangeExhausted) || !strings.Contains(err.Error(), "5004..5007") {
		t.Errorf("exhausted 5004..5007: err = %v, want ErrGNBRangeExhausted naming the range", err)
	}
	// Leased by another run (inUse) counts as unavailable too.
	if _, err := g.Assign(amf, plmn, 6000, 6000, 1, func(id uint32) bool { return id == 6000 }); !errors.Is(err, ErrGNBRangeExhausted) {
		t.Errorf("a one-ID range already leased: err = %v", err)
	}
}

// Assign is transactional: a save that fails (the state directory gone, a
// full disk) assigns nothing, so nothing is held and a retry gets the same
// IDs. Released on a failed save still frees the IDs in memory, logs it,
// and the next write catches the file up.
func TestGNBRotationSaveFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	now := time.Unix(1000, 0)
	var logged bytes.Buffer
	g, err := OpenGNBRotation(dir, slog.New(slog.NewTextHandler(&logged, nil)))
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return now }
	amf := []string{"10.0.0.1:38412"}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if ids, err := g.Assign(amf, "p", 1, 2, 2, nil); !errors.Is(err, ErrRotationPersist) || ids != nil {
		t.Fatalf("Assign with no state dir = %v, %v; want ErrRotationPersist and no IDs", ids, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ids, err := g.Assign(amf, "p", 1, 2, 2, nil)
	if err != nil || !reflect.DeepEqual(ids, []uint32{1, 2}) {
		t.Fatalf("Assign once saving works = %v, %v; want [1 2], nothing held by the failed one", ids, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := g.Released(amf, "p", ids); err != nil {
		t.Fatalf("Released with no state dir: %v", err)
	}
	if !strings.Contains(logged.String(), "gNB ID rotation not saved") {
		t.Errorf("the failed save was not logged: %s", logged.String())
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := g.Assign(amf, "p", 1, 2, 2, nil); err != nil || len(got) != 2 {
		t.Fatalf("Assign after a released-but-unsaved run = %v, %v; want both IDs free", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, GNBRotationFile)); err != nil {
		t.Errorf("the next save did not write the file: %v", err)
	}
}

// The use records are pruned to the range being assigned from and capped,
// oldest first; held IDs are kept.
func TestGNBRotationPrune(t *testing.T) {
	now := time.Unix(1000, 0)
	g := smallRotation(t, "", &now)
	g.maxEntries = 3
	amf := []string{"a"}
	assign := func(first, last uint32, n int) []uint32 {
		t.Helper()
		now = now.Add(time.Second)
		ids, err := g.Assign(amf, "p", first, last, n, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	release := func(ids []uint32) {
		t.Helper()
		now = now.Add(time.Second)
		if err := g.Released(amf, "p", ids); err != nil {
			t.Fatal(err)
		}
	}
	release(assign(100, 199, 2)) // 100, 101
	release(assign(100, 199, 2)) // 102, 103: four records, over the cap of 3
	used := func() []uint32 {
		var out []uint32
		for id := range g.state.Keys[rotationKeyOf("a", "p")].Used {
			out = append(out, id)
		}
		slices.Sort(out)
		return out
	}
	// The cap dropped the oldest record (100): with none left it counts as
	// never used, which is right, since it was the least recently used.
	held := assign(100, 199, 1)
	if held[0] != 100 {
		t.Errorf("after the cap: assigned %v, want [100]", held)
	}
	if got := used(); !reflect.DeepEqual(got, []uint32{100, 102, 103}) {
		t.Errorf("records after the cap: %v, want [100 102 103] (101 dropped as the oldest)", got)
	}
	// A different range drops records outside it, but never a held ID.
	assign(500, 599, 1)
	if got := used(); !reflect.DeepEqual(got, []uint32{held[0], 500}) {
		t.Errorf("after assigning from 500..599: %v, want [%d 500]", got, held[0])
	}
}
