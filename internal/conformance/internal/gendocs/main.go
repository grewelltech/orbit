// Command gendocs writes docs/conformance/checks.md and the documentation
// site's per-check pages from the conformance registry. Run it through go
// generate ./internal/conformance.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bgrewell/orbit/internal/conformance"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: gendocs <checks.md> <site checks dir>")
		os.Exit(2)
	}
	tests := conformance.NewRegistry().Tests()
	if err := os.WriteFile(os.Args[1], []byte(conformance.ChecksMarkdown(tests)), 0o644); err != nil {
		fail(err)
	}
	if err := writeSite(os.Args[2], conformance.SitePages(tests)); err != nil {
		fail(err)
	}
}

// writeSite replaces the .md files of dir with pages: the directory holds
// only generated pages, so a page of a check that is gone is removed.
func writeSite(dir string, pages map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	old, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return err
	}
	for _, f := range old {
		if _, keep := pages[filepath.Base(f)]; !keep {
			if err := os.Remove(f); err != nil {
				return err
			}
		}
	}
	for name, body := range pages {
		if strings.ContainsRune(name, filepath.Separator) {
			return fmt.Errorf("page name %q is not a file name", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
