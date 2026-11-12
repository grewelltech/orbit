package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSite writes every page, removes a .md page that is no longer
// generated, leaves other files alone, and refuses a page name with a path.
func TestWriteSite(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"gone.md": "old", "keep.md": "old", "notes.txt": "mine"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeSite(dir, map[string]string{"keep.md": "new", "index.md": "idx"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"keep.md": "new", "index.md": "idx", "notes.txt": "mine"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", name, b, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.md")); !os.IsNotExist(err) {
		t.Errorf("gone.md was not removed: %v", err)
	}
	if err := writeSite(dir, map[string]string{"sub/x.md": ""}); err == nil {
		t.Error("a page name with a path was accepted")
	}
}
