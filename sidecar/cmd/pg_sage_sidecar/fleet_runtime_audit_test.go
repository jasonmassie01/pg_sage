package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestFleetRuntimeInitializesAnalyzeSemaphore(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		want       int
	}{
		{name: "zero uses safe default", configured: 0, want: 1},
		{name: "negative uses safe default", configured: -1, want: 1},
		{name: "configured fleet limit", configured: 3, want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preserveFleetRuntimeGlobals(t)
			cfg = config.DefaultConfig()
			cfg.Mode = "fleet"
			cfg.Databases = nil
			cfg.Tuner.MaxConcurrentAnalyze = tt.configured
			analyzeSem = nil
			fleetLLMBudget = nil
			configController = nil

			initFleetMultiDB()

			if analyzeSem == nil {
				t.Fatal("fleet bootstrap left the ANALYZE semaphore nil")
			}
			if got := cap(analyzeSem); got != tt.want {
				t.Fatalf("ANALYZE semaphore capacity = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFleetOrchestratorRecordsHealthHistory(t *testing.T) {
	fn := productionFunction(t, "main.go", "fleetDBOrchestrator")
	if !callsSelectorWithPrefix(fn, "RecordHealth") {
		t.Fatal("fleetDBOrchestrator never persists a health-history sample")
	}

	// Error propagation is deliberately not applicable here: one database's
	// health-history write must not terminate another database's orchestrator.
}

func TestAgentDBCollectorHasProcessLifetimeAndLifecycleOwnership(t *testing.T) {
	fn := productionFunction(t, "agentdb_fleet.go", "connectAgentDBToFleet")
	withCancelArg := firstArgumentToSelectorCall(fn, "context", "WithCancel")
	if withCancelArg == "" {
		t.Fatal("AgentDB runtime does not create a cancellable instance context")
	}
	if withCancelArg == "ctx" {
		t.Fatal("AgentDB collector inherits the 60-second reconcile context")
	}
	if !callsIdentifier(fn, "startInstanceWorker") {
		t.Fatal("AgentDB collector is not tracked as an instance-owned worker")
	}

	fields := databaseInstanceFields(fn)
	for _, required := range []string{"Collector", "Cancel", "Workers"} {
		if !fields[required] {
			t.Errorf("AgentDB instance does not publish lifecycle field %s", required)
		}
	}
	if positionOfSelectorCall(fn, "RegisterInstance") <
		positionOfIdentifier(fn, "dbColl") {
		t.Error("AgentDB instance is registered before its collector is built")
	}

	// Invalid deployment shapes are covered by TestEligibleForFleet_Rejections.
	// No state-transition test is needed here: removal/drain behavior belongs to
	// fleet manager lifecycle tests; this test verifies the missing ownership link.
}

func TestAgentDBCollectorRejectsCanceledRegistration(t *testing.T) {
	manager := fleet.NewManager(config.DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	connectAgentDBToFleet(ctx, manager, config.DatabaseConfig{
		Name: "agentdb:late", Host: "127.0.0.1", Database: "late",
	})

	if got := manager.InstanceCount(); got != 0 {
		t.Fatalf("canceled reconcile registered %d AgentDB instances, want 0", got)
	}
}

func TestMetaDBBootstrapBuildsGeneralLLMRuntime(t *testing.T) {
	fn := productionFunction(t, "metadb.go", "initMetaDBFleet")
	assignments := assignedPackageCalls(fn)
	if assignments["llmClient"] != "llm.New" {
		t.Error("meta-db bootstrap does not construct the general LLM client")
	}
	if assignments["llmMgr"] != "llm.NewManager" {
		t.Error("meta-db bootstrap does not construct the LLM manager")
	}

	// Provider failures and malformed LLM responses are tested by internal/llm;
	// this regression is specifically the missing meta-mode state transition.
}

func TestFleetLLMBudgetScopesEveryConsumer(t *testing.T) {
	fn := productionFunction(t, "main.go", "buildFleetLLMFeatures")
	if !callsSelector(fn, "SetBudget") {
		t.Fatal("fleet LLM feature builder never attaches the per-database budget")
	}
	if !callsPackageSelector(fn, "llm", "NewManager") {
		t.Error("advisor and tuner do not receive a database-scoped LLM manager")
	}
	assertCallOmitsGlobal(t, fn, "advisor", "New", "llmMgr")
	assertCallOmitsGlobal(t, fn, "briefing", "New", "llmClient")
	assertMethodReceiverOmitsGlobal(t, fn, "ForPurpose", "llmMgr")

	fleetInit := productionFunction(t, "main.go", "initFleetMultiDB")
	assertMethodArgumentOmitsGlobal(t, fleetInit, "WithLLM", "llmClient")

	metaRuntime := productionFunction(t, "metadb.go", "buildStoreDatabaseRuntime")
	if !callsSelector(metaRuntime, "WithLLM") {
		t.Error("meta-db RCA engine is not wired to a database-scoped LLM client")
	} else {
		assertMethodArgumentOmitsGlobal(t, metaRuntime, "WithLLM", "llmClient")
	}
}

func preserveFleetRuntimeGlobals(t *testing.T) {
	t.Helper()
	oldCfg := cfg
	oldManager := fleetMgr
	oldClient := llmClient
	oldLLMManager := llmMgr
	oldSemaphore := analyzeSem
	oldBudget := fleetLLMBudget
	oldController := configController
	t.Cleanup(func() {
		cfg = oldCfg
		fleetMgr = oldManager
		llmClient = oldClient
		llmMgr = oldLLMManager
		analyzeSem = oldSemaphore
		fleetLLMBudget = oldBudget
		configController = oldController
	})
}

func productionFunction(t *testing.T, file, name string) *ast.FuncDecl {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source directory")
	}
	path := filepath.Join(filepath.Dir(current), file)
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, declaration := range parsed.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("function %s not found in %s", name, path)
	return nil
}

func callsSelector(fn *ast.FuncDecl, selector string) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && selectorName(call.Fun) == selector {
			found = true
		}
		return !found
	})
	return found
}

func callsSelectorWithPrefix(fn *ast.FuncDecl, prefix string) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && strings.HasPrefix(selectorName(call.Fun), prefix) {
			found = true
		}
		return !found
	})
	return found
}

func callsPackageSelector(fn *ast.FuncDecl, pkg, selector string) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && packageSelector(call.Fun) == pkg+"."+selector {
			found = true
		}
		return !found
	})
	return found
}

func callsIdentifier(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		ident, isIdent := callFun(call).(*ast.Ident)
		if ok && isIdent && ident.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func callFun(call *ast.CallExpr) ast.Expr {
	if call == nil {
		return nil
	}
	return call.Fun
}

func selectorName(expr ast.Expr) string {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	return selector.Sel.Name
}

func packageSelector(expr ast.Expr) string {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + selector.Sel.Name
}

func firstArgumentToSelectorCall(
	fn *ast.FuncDecl, pkg, selector string,
) string {
	argument := ""
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || packageSelector(call.Fun) != pkg+"."+selector ||
			len(call.Args) == 0 {
			return true
		}
		argument = expressionIdentifier(call.Args[0])
		return false
	})
	return argument
}

func expressionIdentifier(expr ast.Expr) string {
	ident, ok := expr.(*ast.Ident)
	if ok {
		return ident.Name
	}
	return "non-identifier"
}

func databaseInstanceFields(fn *ast.FuncDecl) map[string]bool {
	fields := make(map[string]bool)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		selector, isSelector := literalType(literal).(*ast.SelectorExpr)
		if !ok || !isSelector || selector.Sel.Name != "DatabaseInstance" {
			return true
		}
		for _, element := range literal.Elts {
			item, isItem := element.(*ast.KeyValueExpr)
			key, isKey := itemKey(item).(*ast.Ident)
			if isItem && isKey {
				fields[key.Name] = true
			}
		}
		return false
	})
	return fields
}

func literalType(literal *ast.CompositeLit) ast.Expr {
	if literal == nil {
		return nil
	}
	return literal.Type
}

func itemKey(item *ast.KeyValueExpr) ast.Expr {
	if item == nil {
		return nil
	}
	return item.Key
}

func positionOfSelectorCall(fn *ast.FuncDecl, selector string) token.Pos {
	position := token.NoPos
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if position == token.NoPos && ok &&
			selectorName(call.Fun) == selector {
			position = call.Pos()
			return false
		}
		return true
	})
	return position
}

func positionOfIdentifier(fn *ast.FuncDecl, name string) token.Pos {
	position := token.NoPos
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if position == token.NoPos && ok && ident.Name == name {
			position = ident.Pos()
			return false
		}
		return true
	})
	return position
}

func assignedPackageCalls(fn *ast.FuncDecl) map[string]string {
	assignments := make(map[string]string)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			return true
		}
		lhs, lhsOK := assignment.Lhs[0].(*ast.Ident)
		call, callOK := assignment.Rhs[0].(*ast.CallExpr)
		if lhsOK && callOK {
			assignments[lhs.Name] = packageSelector(call.Fun)
		}
		return true
	})
	return assignments
}

func assertCallOmitsGlobal(
	t *testing.T,
	fn *ast.FuncDecl,
	pkg, selector, forbidden string,
) {
	t.Helper()
	for _, call := range packageCalls(fn, pkg, selector) {
		if expressionContainsIdentifier(call, forbidden) {
			t.Errorf("%s.%s still receives shared global %s", pkg, selector, forbidden)
		}
	}
}

func assertMethodReceiverOmitsGlobal(
	t *testing.T, fn *ast.FuncDecl, method, forbidden string,
) {
	t.Helper()
	for _, call := range selectorCalls(fn, method) {
		selector := call.Fun.(*ast.SelectorExpr)
		if expressionContainsIdentifier(selector.X, forbidden) {
			t.Errorf("%s is still called on shared global %s", method, forbidden)
		}
	}
}

func assertMethodArgumentOmitsGlobal(
	t *testing.T, fn *ast.FuncDecl, method, forbidden string,
) {
	t.Helper()
	for _, call := range selectorCalls(fn, method) {
		for _, argument := range call.Args {
			if expressionContainsIdentifier(argument, forbidden) {
				t.Errorf("%s still receives shared global %s", method, forbidden)
			}
		}
	}
}

func packageCalls(fn *ast.FuncDecl, pkg, selector string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && packageSelector(call.Fun) == pkg+"."+selector {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

func selectorCalls(fn *ast.FuncDecl, selector string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && selectorName(call.Fun) == selector {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

func expressionContainsIdentifier(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(child ast.Node) bool {
		ident, ok := child.(*ast.Ident)
		if ok && ident.Name == name {
			found = true
		}
		return !found
	})
	return found
}
