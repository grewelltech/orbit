package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// usesOf maps each type with a method that calls name to true, walking the
// package's own (non-test) source.
func usesOf(t *testing.T, names ...string) map[string]map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]bool{}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			recv := fn.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			typ, ok := recv.(*ast.Ident)
			if !ok {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				var name string
				switch x := n.(type) {
				case *ast.Ident:
					name = x.Name
				case *ast.SelectorExpr:
					name = x.Sel.Name
				}
				if slices.Contains(names, name) {
					if out[typ.Name] == nil {
						out[typ.Name] = map[string]bool{}
					}
					out[typ.Name][name] = true
				}
				return true
			})
		}
	}
	return out
}

// Every check that registers a UE declares one, and every check that hands
// over to the harness's target gNB declares that offset: a run leases what
// the declarations say, so a missing one is a check running on a resource
// no lease covers.
func TestEveryCheckDeclaresItsNeeds(t *testing.T) {
	uses := usesOf(t, "NewUEHarness", "TargetGNB")
	for _, c := range NewRegistry().Tests() {
		typ := reflect.TypeOf(c).Name()
		n := NeedsOf(c)
		if uses[typ]["NewUEHarness"] && n.UEs < 1 {
			t.Errorf("%s (%s) registers a UE but declares %d", c.ID(), typ, n.UEs)
		}
		if uses[typ]["TargetGNB"] && !slices.Contains(n.GNBOffsets, targetGNBOffset) {
			t.Errorf("%s (%s) hands over but does not declare the target gNB offset %#x", c.ID(), typ, targetGNBOffset)
		}
		if n.UEs > 1 {
			t.Errorf("%s declares %d UEs; the harness registers one at a time", c.ID(), n.UEs)
		}
	}
	if len(uses) == 0 {
		t.Fatal("found no UE harness use; the walk is broken")
	}
}

// A suite's needs: the most UEs any selected check needs, and the union of
// every selected check's gNB IDs from the base.
func TestRegistryNeeds(t *testing.T) {
	r := NewRegistry()
	idx := func(id string) uint32 {
		for i, c := range r.Tests() {
			if c.ID() == id {
				return uint32(i)
			}
		}
		t.Fatalf("no check %s", id)
		return 0
	}
	all := r.Needs(700)
	if all.UEs != 1 {
		t.Errorf("all checks: %d UEs, want 1", all.UEs)
	}
	em := idx(endMarkerOnPathSwitch{}.ID())
	for _, want := range []uint32{700, 700 + em, 700 + em + targetGNBOffset, 700 + em + aliveGNBOffset} {
		if !slices.Contains(all.GNBIDs, want) {
			t.Errorf("all checks: no gNB ID %d in %v", want, all.GNBIDs)
		}
	}
	if !slices.IsSorted(all.GNBIDs) {
		t.Errorf("gNB IDs not ascending: %v", all.GNBIDs)
	}
	ng := r.Needs(700, Procedural)
	if ng.UEs != 0 {
		t.Errorf("procedural only: %d UEs, want 0", ng.UEs)
	}
	for _, id := range ng.GNBIDs {
		if id-700 >= targetGNBOffset && id-700 < aliveGNBOffset {
			t.Errorf("procedural only presents a handover target %d", id)
		}
	}
	if p := r.GNBPattern(); p[0] != 0 || !slices.Equal(p, r.Needs(0).GNBIDs) {
		t.Errorf("pattern = %v", p)
	}
}
