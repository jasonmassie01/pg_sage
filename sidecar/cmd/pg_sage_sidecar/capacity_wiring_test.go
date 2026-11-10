package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// clouds-07 wiring lock: the production call sites hand the database's
// provider storage capacity on. The behavioural tests build the custodian
// and the runway options directly, so without this a call site passing
// nil (or the plain runwayOptions) would leave managed providers with no
// disk denominator while every test still passed.

// capacityArg is the argument of one call that carries the capacity.
type capacityArg struct {
	index int
	// want lists the accepted expressions; empty accepts anything but nil.
	want []string
}

// The indexes follow the merged signatures, which also carry the shared
// freeze custodian (vacuum-08) and the HA monitor (fleet-04).
var capacityCallSites = map[string]capacityArg{
	"startInstanceAutonomy": {8, []string{"rt.diskCapacity()"}},
	"newRunwayAdvisorFor":   {4, []string{"rt.diskCapacity()"}},
	"runway.NewMonitor":     {3, []string{"rt.runwayMonitorOptions()"}},
	"newDatabaseAutonomy":   {6, nil},
	"newWALCustodian":       {3, nil},
}

func TestProviderCapacityReachesEveryProductionCallSite(t *testing.T) {
	seen := map[string]int{}
	for _, call := range productionCalls(t) {
		name := types.ExprString(call.Fun)
		site, ok := capacityCallSites[name]
		if !ok {
			continue
		}
		seen[name]++
		if len(call.Args) <= site.index {
			t.Fatalf("%s called with %d arguments, want the capacity at %d",
				name, len(call.Args), site.index)
		}
		got := types.ExprString(call.Args[site.index])
		if got == "nil" {
			t.Errorf("%s passes nil capacity: managed providers lose their disk size", name)
			continue
		}
		if len(site.want) > 0 && !slices.Contains(site.want, got) {
			t.Errorf("%s passes %s, want one of %v", name, got, site.want)
		}
	}
	for name := range capacityCallSites {
		if seen[name] == 0 {
			t.Errorf("no production call of %s found; update this wiring lock", name)
		}
	}
}

// productionCalls are the call expressions of the package's non-test files.
func productionCalls(t *testing.T) []*ast.CallExpr {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source directory")
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(current), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var calls []*ast.CallExpr
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				calls = append(calls, call)
			}
			return true
		})
	}
	return calls
}
