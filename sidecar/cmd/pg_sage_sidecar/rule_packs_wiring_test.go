package main

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-11 wiring: the YAML databases[].disabled_rules list becomes the
// analyzer's override; without one the database inherits the global list.
// No concurrent access tests: attachRulePacks runs once, before the
// analyzer goroutine starts.

func rulePackRuntime(t *testing.T, dbCfg config.DatabaseConfig) *databaseRuntime {
	t.Helper()
	old := cfg
	cfg = config.DefaultConfig()
	t.Cleanup(func() { cfg = old })
	rt := &databaseRuntime{ctx: context.Background(),
		spec: databaseRuntimeSpec{Name: "orders", Config: dbCfg}}
	rt.analyzer = analyzer.New(nil, cfg, nil, nil, nil, nil, nil, logStructuredWrapper)
	return rt
}

func TestAttachRulePacks_YAMLListBecomesTheOverride(t *testing.T) {
	rt := rulePackRuntime(t, config.DatabaseConfig{DisabledRules: []string{"unused_indexes"}})
	rt.attachRulePacks()
	names, ok := rt.analyzer.DisabledRulesOverride()
	if !ok || len(names) != 1 || names[0] != "unused_indexes" {
		t.Fatalf("override = %v, %v; want [unused_indexes]", names, ok)
	}
}

func TestAttachRulePacks_NoListInheritsAndNoStoredRowWithoutID(t *testing.T) {
	rt := rulePackRuntime(t, config.DatabaseConfig{})
	rt.attachRulePacks()
	if names, ok := rt.analyzer.DisabledRulesOverride(); ok {
		t.Fatalf("override = %v, want none (inherit the global list)", names)
	}
	if _, _, ok := rt.storedDisabledRules(); ok {
		t.Fatal("a database without a control-plane id read a stored override")
	}
}
