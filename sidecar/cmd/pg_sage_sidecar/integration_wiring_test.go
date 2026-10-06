package main

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

// G3-B15: standalone built its Manager without the optimizer client, so
// query_tuning silently used the general client (and its fallback was the
// same client, retrying every failure on it).
func TestStandaloneLLMManagerUsesDedicatedOptimizerClient(t *testing.T) {
	llmTestGlobals(t)
	cfg.LLM.OptimizerLLM.FallbackToGeneral = true

	manager := newStandaloneLLMManager(llmClient)

	tuning := manager.ForPurpose("query_tuning")
	if tuning == nil || tuning == llmClient {
		t.Fatal("query_tuning is routed to the general client")
	}
	if manager.ForPurpose("index_optimization") != tuning {
		t.Fatal("index optimization and query tuning use different optimizer clients")
	}
	primary, fallback := tuningModels(manager)
	if primary != tuning || fallback != llmClient {
		t.Fatalf("tuning clients = (%p, %p), want (optimizer, general)", primary, fallback)
	}
	if _, ok := llmClients.TokenStatus()["optimizer"]; !ok {
		t.Fatal("standalone optimizer client is not tracked by the registry")
	}
}

func TestStandaloneTunerFallbackNeverRetriesSameClient(t *testing.T) {
	llmTestGlobals(t)
	cfg.LLM.OptimizerLLM.Enabled = false
	cfg.LLM.OptimizerLLM.FallbackToGeneral = true

	manager := newStandaloneLLMManager(llmClient)

	primary, fallback := tuningModels(manager)
	if primary != llmClient {
		t.Fatal("without optimizer_llm, tuning must use the general client")
	}
	if fallback != nil {
		t.Fatal("fallback is the primary client: every failure would be retried on it")
	}
}

func TestTuningModelsNilManager(t *testing.T) {
	var none *llm.Manager
	if primary, fallback := tuningModels(none); primary != nil || fallback != nil {
		t.Fatal("nil manager yielded LLM clients")
	}
}

// Meta-db runtimes wired the analyzer and executor dispatcher but not the
// RCA engine, so incident notifications never left meta mode. Every mode now
// builds its RCA engine in the shared runtime.
func TestMetaRuntimeWiresRCANotifications(t *testing.T) {
	fn := productionFunction(t, "databaseRuntime.startExecution")
	if !callsSelector(fn, "WithDispatcher") {
		t.Fatal("the database runtime's RCA engine never receives the dispatcher")
	}
	meta := loadPackageCallGraph(t).reachableCalls("prepareStoreDatabaseConnection")
	if !meta["buildDatabaseRuntime"] {
		t.Fatal("meta-db does not build its runtime with the shared constructor")
	}
}

// Standalone used a private dispatcher without the channel key or target
// policy; every mode must share the configured one.
func TestStandaloneUsesSharedNotifyDispatcher(t *testing.T) {
	fn := productionFunction(t, "initStandalone")
	reached := loadPackageCallGraph(t).reachableCalls("initStandalone")
	if reached["notify.NewDispatcher"] {
		t.Fatal("standalone builds an unkeyed notify dispatcher")
	}
	if !reached["sharedNotifyDispatcher"] {
		t.Fatal("standalone does not use the shared keyed dispatcher")
	}
	if !callsIdentifier(fn, "newStandaloneLLMManager") {
		t.Fatal("standalone does not build its manager with the optimizer client")
	}
}

// The API process must receive every runtime registry the fixes rely on.
func TestAPIServerWiresRuntimeRegistries(t *testing.T) {
	wire := productionFunction(t, "wireRouter")
	fields := compositeFields(wire, "RuntimeDeps")
	for _, field := range []string{
		"ConfigBaseLoader", "LLMBudgets",
		"NotificationSecretKey", "NotificationTargetPolicy",
	} {
		if !fields[field] {
			t.Errorf("wireRouter RuntimeDeps omits %s", field)
		}
	}
	start := productionFunction(t, "startAPIServer")
	params := compositeFields(start, "WireParams")
	for _, field := range []string{"ConfigBaseLoader", "LLMBudgets"} {
		if !params[field] {
			t.Errorf("startAPIServer WireParams omits %s", field)
		}
	}
}

// compositeFields returns the keyed fields of every composite literal whose
// type name (possibly package-qualified) is typeName.
func compositeFields(fn *ast.FuncDecl, typeName string) map[string]bool {
	fields := map[string]bool{}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if !ok || !compositeTypeIs(lit.Type, typeName) {
			return true
		}
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok {
					fields[key.Name] = true
				}
			}
		}
		return true
	})
	return fields
}

func compositeTypeIs(expr ast.Expr, typeName string) bool {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name == typeName
	case *ast.SelectorExpr:
		return typed.Sel.Name == typeName
	}
	return false
}

// G5-B03: the API's config-delete base is the CURRENT file config (plus
// runtime facts), not the startup clone.
func TestFileConfigBaseLoaderReadsCurrentYAML(t *testing.T) {
	prepareMetaGlobals(t)
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(strings.Join([]string{
		"mode: standalone", "postgres:", "  host: db.example",
		"  database: app", "  user: u", "trust:", "  level: observation", "",
	}, "\n"))
	os.Args = []string{"pg_sage", "--config", path}
	cfg.CloudEnvironment = "rds"
	cfg.PGVersionNum = 170000

	base, err := loadFileConfigBase()
	if err != nil {
		t.Fatalf("load base: %v", err)
	}
	if base.Trust.Level != "observation" {
		t.Fatalf("base trust = %q, want current file value observation", base.Trust.Level)
	}
	if base.CloudEnvironment != "rds" || base.PGVersionNum != 170000 {
		t.Fatalf("runtime facts lost: env=%q pg=%d", base.CloudEnvironment, base.PGVersionNum)
	}

	write("trust: [broken")
	if _, err := loadFileConfigBase(); err == nil {
		t.Fatal("invalid YAML produced a delete base")
	}
}
