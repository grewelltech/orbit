package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/bgrewell/orbit/internal/target"
)

// Every flag the CLI registers, on every command, maps to a canonical path
// in internal/target or is excluded with a reason (ADR-0008 §3): a flag in
// neither would silently keep its default once the resolver drives the
// commands. In the other direction, every flag the table names exists.
func TestEveryFlagIsMappedOrExcluded(t *testing.T) {
	seen := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name == "help" {
				return
			}
			seen[f.Name] = true
			cmd := c.CommandPath()
			_, bound := target.FlagBinding(cmd, f.Name)
			_, excluded := target.FlagExclusion(cmd, f.Name)
			if !bound && !excluded {
				t.Errorf("%s --%s is neither mapped to a path nor excluded in internal/target/mapping.go", cmd, f.Name)
			}
		})
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(New("test"))
	for _, b := range target.Bindings {
		if b.Flag != "" && !seen[b.Flag] && b.Flag != "target" {
			t.Errorf("binding for --%s, which no command registers", b.Flag)
		}
	}
	for _, e := range target.Exclusions {
		if e.Flag != "" && !seen[e.Flag] {
			t.Errorf("exclusion for --%s, which no command registers", e.Flag)
		}
	}
}

// Every ORBIT_* variable the shipped code reads is mapped or excluded too,
// and every variable the table names is read.
// The scan reads string literals in non-test Go files under internal/ and
// cmd/, leaving out internal/target, whose table names them all.
func TestEveryEnvVarIsMappedOrExcluded(t *testing.T) {
	re := regexp.MustCompile(`^ORBIT_[A-Z0-9_]+$`)
	found := map[string]string{}
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// The table itself names every variable; it reads none.
			if d.IsDir() && filepath.ToSlash(path) == "../../internal/target" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if s, err := strconv.Unquote(lit.Value); err == nil && re.MatchString(s) {
					found[s] = path
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(found) == 0 {
		t.Fatal("the scan found no ORBIT_* variables")
	}
	for name, path := range found {
		if !target.Covered(name) {
			t.Errorf("$%s (read in %s) is neither mapped nor excluded in internal/target/mapping.go", name, path)
		}
	}
	// $ORBIT_TARGET is the resolver's own: it names the target rather than
	// mapping onto a field, so no command reads it.
	for _, b := range target.Bindings {
		if b.Env != "" && found[b.Env] == "" && b.Env != "ORBIT_TARGET" {
			t.Errorf("binding for $%s, which no code reads", b.Env)
		}
	}
	for _, e := range target.Exclusions {
		if e.Env != "" && found[e.Env] == "" {
			t.Errorf("exclusion for $%s, which no code reads", e.Env)
		}
	}
}
