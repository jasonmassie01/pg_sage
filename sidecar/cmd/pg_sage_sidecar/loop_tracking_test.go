package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// footprint-07: every periodic worker reports its tick to the loop
// tracker under its component name, keyed by the database its ctx
// carries. Asserted from the source, not by convention: a new worker or
// a refactor that drops the call fails here.

type trackedTick struct {
	dir, recv, fn, component string
}

var trackedTicks = []trackedTick{
	{"../../internal/collector", "Collector", "trackedCycle", "collector"},
	{"../../internal/analyzer", "Analyzer", "trackedCycle", "analyzer"},
	{"../../internal/executor", "Executor", "RunCycle", "executor"},
	{"../../internal/tuner", "Tuner", "Tune", "tuner"},
	{"../../internal/runway", "Monitor", "Tick", "runways"},
	{"../../internal/sre", "Coordinator", "trackedTick", "sre"},
	{"../../internal/briefing", "Worker", "GenerateBriefing", "briefing"},
	{"../../internal/autoexplain", "Collector", "Collect", "auto_explain"},
	{"../../internal/facts", "Worker", "RunOnce", "facts"},
	{"../../internal/retention", "Cleaner", "RunOnce", "retention"},
	{"../../internal/shadow", "Scorer", "RunOnce", "shadow_mode"},
	{".", "selfConfigRunner", "pass", "self_config"},
	{".", "metricsRefresher", "refresh", "metrics_refresh"},
}

// findMethod returns the body of recv.fn among the package's non-test
// files.
func findMethod(t *testing.T, dir, recv, fn string) *ast.FuncDecl {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list %s: %v (%d files)", dir, err, len(files))
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if matched, _ := filepath.Match("*_test.go", filepath.Base(path)); matched {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if ok && fd.Name.Name == fn && receiverName(fd) == recv {
				return fd
			}
		}
	}
	return nil
}

func receiverName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// trackForComponent is the component a statement passes to TrackFor
// ("" when the statement makes no such call).
func trackForComponent(stmt ast.Stmt) string {
	component := ""
	ast.Inspect(stmt, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "TrackFor" && sel.Sel.Name != "TrackForErr") ||
			len(call.Args) != 2 {
			return true
		}
		if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			component, _ = strconv.Unquote(lit.Value)
		}
		return false
	})
	return component
}

func TestLoopTracking_EveryWorkerTickCallsTrackFor(t *testing.T) {
	for _, tick := range trackedTicks {
		t.Run(tick.component, func(t *testing.T) {
			fd := findMethod(t, tick.dir, tick.recv, tick.fn)
			if fd == nil || fd.Body == nil {
				t.Fatalf("(%s).%s not found in %s", tick.recv, tick.fn, tick.dir)
			}
			// At the top of the tick: within the first two statements, so a
			// nil guard may come first.
			for i, stmt := range fd.Body.List {
				if i >= 2 {
					break
				}
				if got := trackForComponent(stmt); got != "" {
					if got != tick.component {
						t.Fatalf("(%s).%s tracks %q, want %q", tick.recv, tick.fn, got,
							tick.component)
					}
					return
				}
			}
			t.Fatalf("(%s).%s does not call TrackFor(ctx, %q) at its top", tick.recv,
				tick.fn, tick.component)
		})
	}
}

// Components are unique: two workers under one name would merge in
// top_loops and in the loop metrics.
func TestLoopTracking_ComponentsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, tick := range trackedTicks {
		if seen[tick.component] {
			t.Fatalf("component %q tracked twice", tick.component)
		}
		seen[tick.component] = true
	}
}

// The database runtime puts its database name in the ctx every worker
// inherits, so their loops are keyed component:database.
func TestLoopTracking_RuntimeContextCarriesTheDatabase(t *testing.T) {
	fd := findMethod(t, ".", "", "newDatabaseRuntime")
	if fd == nil {
		t.Fatal("newDatabaseRuntime not found")
	}
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WithDatabase" {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "selfbudget" {
					found = true
				}
			}
		}
		return !found
	})
	if !found {
		t.Fatal("newDatabaseRuntime does not set selfbudget.WithDatabase on its ctx")
	}
}
