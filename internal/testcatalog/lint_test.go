package testcatalog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFreshCatalogsAreSeeded keeps the seed in use. A test that opens a catalog at a
// fresh path with waxbin.Open or sqlite.Open builds the whole schema, about a quarter of
// a second of race-built CPU, and enough of those is what tripped CI's ten-minute alarm
// three times. So every direct read-write open outside the store's own tests shows that
// it is seeded or says why not, rather than leaving a reader, or this test, to work out
// whether its path is fresh: the path is built by testcatalog.Seed, the open is
// read-only, it sits in a package's seeded helper (OpenSeeded, openSeeded), or a comment
// line above it starts "Not seeded:" and gives the reason, as for a reopen or an open the
// test expects to be refused.
//
// A catalog a CLI command creates is out of its sight, since the open happens inside the
// command: a CLI test seeds the --db path first unless the command is meant to create it.
// The store's own tests are left out: they open catalogs themselves on purpose, and their
// fixtures seed through seedCatalog.
func TestFreshCatalogsAreSeeded(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	skip := map[string]bool{
		filepath.Join(root, "store", "sqlite"):         true,
		filepath.Join(root, "internal", "testcatalog"): true,
	}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[path] || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, pos := range unseededOpens(fset, f) {
			rel, _ := filepath.Rel(root, pos.Filename)
			t.Errorf("%s:%d: a read-write open that does not show its seed; build the path with "+
				"testcatalog.Seed, open through the package's seeded helper, or say why above it "+
				"(// Not seeded: ...)", rel, pos.Line)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// seededHelpers open through testcatalog.Seed themselves.
var seededHelpers = map[string]bool{"OpenSeeded": true, "openSeeded": true}

// unseededOpens lists the opens in f that neither show a seed nor give a reason.
func unseededOpens(fset *token.FileSet, f *ast.File) []token.Position {
	excused := map[int]bool{}
	for _, group := range f.Comments {
		for _, c := range group.List {
			if strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(c.Text, "//")), "Not seeded:") {
				excused[fset.Position(group.End()).Line+1] = true // the line below the comment
				excused[fset.Position(group.Pos()).Line] = true   // or the line it ends
			}
		}
	}
	var found []token.Position
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || seededHelpers[fn.Name.Name] {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isOpen(call, f.Name.Name) || len(call.Args) < 2 || showsSeed(call.Args[1]) {
				return true
			}
			if pos := fset.Position(call.Pos()); !excused[pos.Line] {
				found = append(found, pos)
			}
			return true
		})
	}
	return found
}

// isOpen reports whether call opens a catalog: waxbin.Open, sqlite.Open, or Open itself
// inside the waxbin package's own tests.
func isOpen(call *ast.CallExpr, pkg string) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		x, ok := fun.X.(*ast.Ident)
		return ok && fun.Sel.Name == "Open" && (x.Name == "waxbin" || x.Name == "sqlite")
	case *ast.Ident:
		return fun.Name == "Open" && pkg == "waxbin"
	}
	return false
}

// showsSeed reports whether an open's options are a literal that is read-only or builds
// its path with testcatalog.Seed.
func showsSeed(opts ast.Expr) bool {
	lit, ok := opts.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		switch key, _ := kv.Key.(*ast.Ident); {
		case key == nil:
		case key.Name == "ReadOnly":
			if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "true" {
				return true
			}
		case key.Name == "DBPath" || key.Name == "Path":
			if call, ok := kv.Value.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Seed" {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "testcatalog" {
						return true
					}
				}
			}
		}
	}
	return false
}
