package conformance

import (
	"context"
	"github.com/bgrewell/orbit/internal/gnb"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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
		if uses[typ]["TargetGNB"] && !slices.Contains(n.GNBRoles, RoleHandover) {
			t.Errorf("%s (%s) hands over but does not declare the handover target role", c.ID(), typ)
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
// every selected check's gNB IDs from the base, in either layout.
func TestRegistryNeeds(t *testing.T) {
	r := NewRegistry()
	n := uint32(len(r.Tests()))
	em := uint32(r.indexOf(endMarkerOnPathSwitch{}))
	tests := []struct {
		name   string
		packed bool
		// want are IDs the full suite presents from base 700; span is
		// the furthest from it.
		want []uint32
		span uint32
	}{
		{name: "legacy, unbounded", want: []uint32{700, 700 + em, 700 + em + 0x8000, 700 + em + 0x9000}, span: n - 1 + 0x9000},
		{name: "packed, in a bounded block", packed: true, want: []uint32{700, 700 + em, 700 + n + em, 700 + 2*n + em}, span: 3*n - 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			all := r.Needs(700, tc.packed)
			if all.UEs != 1 {
				t.Errorf("all checks: %d UEs, want 1", all.UEs)
			}
			for _, want := range tc.want {
				if !slices.Contains(all.GNBIDs, want) {
					t.Errorf("no gNB ID %d in %v", want, all.GNBIDs)
				}
			}
			if !slices.IsSorted(all.GNBIDs) || all.GNBIDs[len(all.GNBIDs)-1]-700 > tc.span {
				t.Errorf("gNB IDs %v not ascending within %d of the base", all.GNBIDs, tc.span)
			}
			// One handover target, every check's own and probe: 2N+1.
			if len(all.GNBIDs) != int(2*n+1) {
				t.Errorf("%d IDs, want %d", len(all.GNBIDs), 2*n+1)
			}
			if ng := r.Needs(700, tc.packed, Procedural); ng.UEs != 0 || len(ng.GNBIDs) == 0 {
				t.Errorf("procedural only: %+v", ng)
			}
		})
	}
	if r.PackedSpan() != int(3*n) {
		t.Errorf("packed span = %d, want 3N = %d", r.PackedSpan(), 3*n)
	}
}

// Each check's IDs come from RunStream, never from the check: no source
// outside the layout adds a legacy offset or names one.
func TestNoCheckComputesItsGNBIDs(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || path == "needs.go" {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind == token.INT && (x.Value == "0x8000" || x.Value == "0x9000") {
					t.Errorf("%s: %s, a legacy gNB offset outside the layout", fset.Position(x.Pos()), x.Value)
				}
			case *ast.Ident:
				if x.Name == "legacyTargetOffset" || x.Name == "legacyLivenessOffset" {
					t.Errorf("%s: %s outside the layout", fset.Position(x.Pos()), x.Name)
				}
			}
			return true
		})
	}
}

// RunStream gives each check its IDs: packed, base+i, base+N+i, base+2N+i;
// legacy, base+i and +0x8000, +0x9000 from it.
func TestRunStreamAssignsGNBIDs(t *testing.T) {
	r := &Registry{}
	seen := map[string]Env{}
	for i := 0; i < 3; i++ {
		r.Register(envProbe{id: "P" + string(rune('A'+i)), seen: seen})
	}
	for _, packed := range []bool{false, true} {
		r.RunStream(context.Background(), Env{GNB: gnb.Config{ID: 5000}, PackedGNBIDs: packed}, time.Second, func(Result, int, int) {})
		for i, id := range []string{"PA", "PB", "PC"} {
			e := seen[id]
			own, target, live := uint32(5000+i), uint32(5000+i+0x8000), uint32(5000+i+0x9000)
			if packed {
				target, live = uint32(5000+3+i), uint32(5000+6+i)
			}
			if e.GNB.ID != own || e.TargetGNBID != target || e.LivenessGNBID != live ||
				e.targetGNBID() != target || e.livenessGNBID() != live {
				t.Errorf("packed %t, %s: IDs %d/%d/%d, want %d/%d/%d", packed, id, e.GNB.ID, e.TargetGNBID, e.LivenessGNBID, own, target, live)
			}
		}
	}
}

// envProbe is a check that records the Env it ran with.
type envProbe struct {
	id   string
	seen map[string]Env
}

func (p envProbe) ID() string        { return p.id }
func (envProbe) Category() Category  { return Procedural }
func (envProbe) SpecRefs() []SpecRef { return nil }
func (p envProbe) Run(_ context.Context, e Env) Result {
	p.seen[p.id] = e
	return Result{Verdict: Pass}
}
