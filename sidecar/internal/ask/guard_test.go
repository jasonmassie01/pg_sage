package ask

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Owner decision 3: Ask Sage can open an investigation or queue a typed
// proposal, and nothing else: it can never execute, approve, confirm
// facts or bypass the gate, whatever the user or retrieved text asks.
// These tests pin that structurally: the exact tool set per caller, and
// the package's imports and calls.

var readTools = []string{"describe_table", "explain_concept", "explain_config", "get_action",
	"get_finding", "get_investigation", "list_actions", "list_approvals", "list_facts",
	"list_findings", "list_incidents", "list_investigations", "top_queries", "trust_ledger"}

func fullDeps(f *fixture) Deps {
	d := f.deps(nil)
	d.Investigations, d.Trust = &fakeInvestigations{}, fakeTrust(`{}`)
	d.Queries = fakeQueries{}
	d.Proposer, d.Starter = &fakeProposer{}, &fakeStarter{}
	return d
}

func sortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func TestGuard_ReadCallersGetOnlyReadTools(t *testing.T) {
	f := newFixture(t)
	s := f.service(fullDeps(f))
	got := sortedNames(toolNames(s.newSession(viewer).tools()))
	if strings.Join(got, ",") != strings.Join(readTools, ",") {
		t.Fatalf("viewer tools = %v, want %v", got, readTools)
	}
}

func TestGuard_ProposersGetExactlyTwoWriteTools(t *testing.T) {
	f := newFixture(t)
	s := f.service(fullDeps(f))
	for _, c := range []Caller{operator, agent} {
		names := toolNames(s.newSession(c).tools())
		want := append(append([]string{}, readTools...), "open_investigation", "propose_action")
		sort.Strings(want)
		if got := sortedNames(names); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s tools = %v, want %v", c.Actor, got, want)
		}
	}
}

func TestGuard_NoToolCanExecuteApproveOrConfirm(t *testing.T) {
	f := newFixture(t)
	s := f.service(fullDeps(f))
	forbidden := []string{"exec", "run_sql", "approve", "reject", "confirm", "decide",
		"apply", "drop", "alter", "set_", "grant", "rollback", "cancel", "terminate"}
	for _, tool := range s.newSession(operator).tools() {
		for _, word := range forbidden {
			if strings.Contains(tool.Name, word) {
				t.Errorf("tool %s looks like a mutation (%s)", tool.Name, word)
			}
		}
	}
	if FinalTool != "answer" {
		t.Fatalf("final tool = %s", FinalTool)
	}
}

// The package may not reach any execution or approval path: it imports
// neither the executor, the policy gate, the API nor MCP, and calls no
// method by those names. Writes go only through the Proposer and
// InvestigationStarter interfaces the wiring supplies.
func TestGuard_PackageCannotReachExecutionOrApproval(t *testing.T) {
	banned := []string{"/internal/executor", "/internal/policy", "/internal/api",
		"/internal/mcp", "/internal/store"}
	bannedCalls := map[string]bool{"Authorize": true, "Apply": true, "Execute": true,
		"Approve": true, "Decide": true, "DecideFact": true, "ExecuteConcrete": true,
		"RequestExecution": true, "Confirm": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range file.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, b := range banned {
				if strings.HasSuffix(path, b) {
					t.Errorf("%s imports %s", name, path)
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && bannedCalls[sel.Sel.Name] {
				t.Errorf("%s refers to %s at %s", name, sel.Sel.Name, fset.Position(sel.Pos()))
			}
			return true
		})
	}
	if checked < 5 {
		t.Fatalf("checked %d files; the guard is not looking at the package", checked)
	}
}
