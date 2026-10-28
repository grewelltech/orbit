package testnet

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// blocks reads the block constants from net24.go's source, so a constant
// added there is checked without being listed again here.
func blocks(t *testing.T) map[string]int {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "net24.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			v := s.(*ast.ValueSpec)
			for i, name := range v.Names {
				lit, ok := v.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("%s is not an integer literal: a block must be readable here", name.Name)
				}
				n, err := strconv.Atoi(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out[name.Name] = n
			}
		}
	}
	return out
}

// No block is given twice, and every block is a usable 127.0.N.0/24 (N from
// 1 to 254: 127.0.0.x is the shared loopback every other test binds on).
func TestBlocksAreUnique(t *testing.T) {
	bs := blocks(t)
	if len(bs) == 0 {
		t.Fatal("no blocks read from net24.go")
	}
	owner := map[int]string{}
	for name, n := range bs {
		if n < 1 || n > 254 {
			t.Errorf("%s = %d: a block is 1 to 254", name, n)
		}
		if other, ok := owner[n]; ok {
			a, b := other, name
			if b < a {
				a, b = b, a
			}
			t.Errorf("%s and %s are both block %d: two tests would bind the same addresses", a, b, n)
		}
		owner[n] = name
	}
}

// literalBlock is an address in a 127.0.N.0/24 other than 127.0.0.x, written
// out or built by hand ("127.0.%d.", "127.0." + n).
var literalBlock = regexp.MustCompile(`127\.0\.([1-9][0-9]{0,2}\.|%d|"\s*\+)`)

// Every block address is taken from this package: none is written out
// anywhere else in the module, so the table above is the whole allocation.
func TestNoLiteralBlocks(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case path == here, d.Name() == "node_modules", d.Name() == "web", strings.HasPrefix(d.Name(), "."):
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }() // read-only: a close error loses nothing
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for line := 1; sc.Scan(); line++ {
			if literalBlock.MatchString(sc.Text()) {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					rel = path
				}
				t.Errorf("%s:%d writes a 127.0.N. block address: take it from internal/testnet", rel, line)
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
}
