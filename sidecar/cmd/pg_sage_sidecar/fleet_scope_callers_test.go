package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// fleetLearningScope is computed once at startup, where each elector takes
// its lease. A caller that recomputes it later reads a live config that a
// reload may have changed, and fences against a scope with no lease row
// (fleet-02). Every other job takes the scope from the elector's token.
// startReviewFleet (review-04b) is a startup site too: api_server calls it
// once, right after startFleetLearning, and its stores keep that scope.
func TestFleetLearningScopeIsUsedOnlyAtStartup(t *testing.T) {
	got := identifierUsers(t, "fleetLearningScope")
	want := []string{"agentGovernanceLeader", "startFleetLearning", "startReviewFleet"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fleetLearningScope is used by %v, want only %v", got, want)
	}
}

// identifierUsers lists the functions of this package's non-test files
// that mention name, excluding its own declaration; "<package>" marks a
// use outside any function.
func identifierUsers(t *testing.T, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	users := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		file := entry.Name()
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			owner := "<package>"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				owner = fn.Name.Name
			}
			ast.Inspect(decl, func(node ast.Node) bool {
				if fn, ok := node.(*ast.FuncDecl); ok && fn.Name.Name == name {
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok && id.Name == name {
							users[name] = true
						}
						return true
					})
					return false
				}
				if id, ok := node.(*ast.Ident); ok && id.Name == name {
					users[owner] = true
				}
				return true
			})
		}
	}
	out := make([]string, 0, len(users))
	for u := range users {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}
