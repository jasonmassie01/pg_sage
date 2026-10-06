package main

import (
	"context"
	"go/ast"
	"go/token"
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
	fn := productionFunction(t, "runFleetDBCycle")
	if !callsSelectorWithPrefix(fn, "RecordHealth") {
		t.Fatal("fleet orchestrator cycle never persists a health-history sample")
	}

	// Error propagation is deliberately not applicable here: one database's
	// health-history write must not terminate another database's orchestrator.
}

func TestAgentDBCollectorHasProcessLifetimeAndLifecycleOwnership(t *testing.T) {
	fn := productionFunction(t, "connectAgentDBToFleet")
	parent := compositeFieldIdentifier(fn, "databaseRuntimeSpec", "Parent")
	if parent == "" {
		t.Fatal("AgentDB runtime has no process-lifetime parent context")
	}
	if parent == "ctx" {
		t.Fatal("AgentDB runtime inherits the 60-second reconcile context")
	}
	lifecycle := productionFunction(t, "newDatabaseRuntime")
	if firstArgumentToSelectorCall(lifecycle, "context", "WithCancel") != "parent" {
		t.Fatal("database runtime does not derive a cancellable context from its parent")
	}
	start := productionFunction(t, "databaseRuntime.start")
	if !callsIdentifier(start, "startInstanceWorker") {
		t.Fatal("runtime workers are not tracked as instance-owned workers")
	}
	fields := databaseInstanceFields(productionFunction(t, "databaseRuntime.instance"))
	for _, required := range []string{"Collector", "Cancel", "Workers"} {
		if !fields[required] {
			t.Errorf("database runtime does not publish lifecycle field %s", required)
		}
	}
	if positionOfSelectorCall(fn, "publish") <
		positionOfIdentifier(fn, "buildDatabaseRuntime") {
		t.Error("AgentDB instance is registered before its runtime is built")
	}

	// Invalid deployment shapes are covered by TestEligibleForFleet_Rejections.
	// No state-transition test is needed here: removal/drain behavior belongs to
	// fleet manager lifecycle tests; this test verifies the missing ownership link.
}

// compositeFieldIdentifier returns the identifier assigned to field in the
// first composite literal of typeName, "" when absent.
func compositeFieldIdentifier(fn *ast.FuncDecl, typeName, field string) string {
	value := ""
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		ident, isIdent := literalType(literal).(*ast.Ident)
		if !ok || !isIdent || ident.Name != typeName {
			return true
		}
		for _, element := range literal.Elts {
			item, isItem := element.(*ast.KeyValueExpr)
			key, isKey := itemKey(item).(*ast.Ident)
			if isItem && isKey && key.Name == field {
				value = expressionIdentifier(item.Value)
			}
		}
		return false
	})
	return value
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
	fn := productionFunction(t, "initMetaDBFleet")
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
	resolve := productionFunction(t, "databaseRuntime.resolveLLM")
	if !callsIdentifier(resolve, "newFleetDBLLMClients") {
		t.Fatal("database runtime bypasses the per-database client factory")
	}
	clients := productionFunction(t, "newFleetDBLLMClients")
	if !callsIdentifier(clients, "attachFleetBudget") {
		t.Fatal("per-database LLM clients never attach the fleet budget")
	}
	if !callsPackageSelector(clients, "llm", "NewManager") {
		t.Error("advisor and tuner do not receive a database-scoped LLM manager")
	}
	attach := productionFunction(t, "attachFleetBudget")
	if !callsSelector(attach, "SetBudget") {
		t.Fatal("attachFleetBudget does not scope the client to its budget")
	}
	// Only resolveLLM may read the process clients (standalone shares them);
	// every consumer takes the runtime's resolved client and manager.
	advisorFn := productionFunction(t, "databaseRuntime.newAdvisor")
	assertCallOmitsGlobal(t, advisorFn, "advisor", "New", "llmMgr")
	execution := productionFunction(t, "databaseRuntime.startExecution")
	assertCallOmitsGlobal(t, execution, "briefing", "New", "llmClient")
	tuningFn := productionFunction(t, "databaseRuntime.newTuningAgent")
	if expressionContainsIdentifier(tuningFn.Body, "llmMgr") {
		t.Error("the tuning agent reads the shared global llmMgr")
	}
	modelsFn := productionFunction(t, "tuningModels")
	assertMethodReceiverOmitsGlobal(t, modelsFn, "ForPurpose", "llmMgr")
	tunerFn := productionFunction(t, "databaseRuntime.newTuner")
	if callsSelector(tunerFn, "WithLLM") {
		t.Error("the tuner has no LLM path: hints come from the tuning agent")
	}
	rcaFn := productionFunction(t, "databaseRuntime.wireRCA")
	if !callsSelector(rcaFn, "WithLLM") {
		t.Error("RCA engine is not wired to a database-scoped LLM client")
	} else {
		assertMethodArgumentOmitsGlobal(t, rcaFn, "WithLLM", "llmClient")
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
	oldRegistry := llmClients
	llmClients = &llmClientRegistry{}
	t.Cleanup(func() {
		llmClients = oldRegistry
		cfg = oldCfg
		fleetMgr = oldManager
		llmClient = oldClient
		llmMgr = oldLLMManager
		analyzeSem = oldSemaphore
		fleetLLMBudget = oldBudget
		configController = oldController
	})
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
