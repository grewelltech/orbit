package engine

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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
	g.first, g.last = 1, 5
	g.now = func() time.Time { return *now }
	return g
}

func TestGNBRotationOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	g := smallRotation(t, "", &now)
	const amf, plmn = "10.0.0.1:38412", "001/01"
	assign := func(n int, inUse func(uint32) bool) []uint32 {
		t.Helper()
		ids, err := g.Assign(amf, plmn, n, inUse)
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
	if err := g.Released(amf, plmn, []uint32{3}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := g.Released(amf, plmn, []uint32{1}); err != nil {
		t.Fatal(err)
	}
	// The rest of the never-used range (4, 5), then the least recently
	// released (3 before 1). 2 was passed over while another run held it:
	// that run releases it into the rotation when it ends.
	if got := assign(4, nil); !reflect.DeepEqual(got, []uint32{4, 5, 3, 1}) {
		t.Fatalf("second assignment = %v, want [4 5 3 1]", got)
	}
	if _, err := g.Assign(amf, plmn, 1, nil); err == nil {
		t.Error("assigned an ID while all were in use")
	}
	now = now.Add(time.Second)
	if err := g.Released(amf, plmn, []uint32{2}); err != nil {
		t.Fatal(err)
	}
	if got := assign(1, nil); !reflect.DeepEqual(got, []uint32{2}) {
		t.Errorf("after the other run released 2: %v, want [2]", got)
	}
}

func TestGNBRotationExhaustion(t *testing.T) {
	now := time.Unix(1000, 0)
	g := smallRotation(t, "", &now)
	const amf, plmn = "10.0.0.1:38412", "001/01"
	ids, err := g.Assign(amf, plmn, 5, nil)
	if err != nil || !reflect.DeepEqual(ids, []uint32{1, 2, 3, 4, 5}) {
		t.Fatalf("Assign = %v, %v", ids, err)
	}
	if _, err := g.Assign(amf, plmn, 1, nil); err == nil {
		t.Fatal("assigned an ID with the range used up and none released")
	}
	for i, id := range []uint32{4, 2, 5} {
		now = time.Unix(2000+int64(i), 0)
		if err := g.Released(amf, plmn, []uint32{id}); err != nil {
			t.Fatal(err)
		}
	}
	// Least recently released first, and one held by another run skipped.
	ids, err = g.Assign(amf, plmn, 2, func(id uint32) bool { return id == 4 })
	if err != nil || !reflect.DeepEqual(ids, []uint32{2, 5}) {
		t.Fatalf("Assign after release = %v, %v; want [2 5]", ids, err)
	}
	// Another (AMF, PLMN) has its own rotation.
	if ids, err := g.Assign("10.0.0.2:38412", plmn, 1, nil); err != nil || ids[0] != 1 {
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
	if _, err := g.Assign(amf, plmn, 5, nil); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(2000, 0)
	if err := g.Released(amf, plmn, []uint32{3}); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(3000, 0)
	if err := g.Released(amf, plmn, []uint32{1}); err != nil {
		t.Fatal(err)
	}

	again := smallRotation(t, dir, &now)
	ids, err := again.Assign(amf, plmn, 2, nil)
	if err != nil || !reflect.DeepEqual(ids, []uint32{3, 1}) {
		t.Fatalf("after a restart Assign = %v, %v; want [3 1]", ids, err)
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
			ids, err := g.Assign("a", "p", 1, nil)
			if err != nil || ids[0] != 1 {
				t.Errorf("Assign on a fresh rotation = %v, %v", ids, err)
			}
		})
	}
}
