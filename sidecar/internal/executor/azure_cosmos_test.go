package executor

import (
	"strings"
	"testing"
)

// Cosmos DB for PostgreSQL runs the same portable SQL actions as flexible
// server; ALTER SYSTEM is unavailable and parameters are guidance-only.
func TestAzureCosmosSupportsPortableActions(t *testing.T) {
	for _, actionType := range []string{
		"analyze_table", "vacuum_table", "create_index_concurrently",
		"drop_unused_index", "cancel_backend", "alter_table",
	} {
		contract, ok := ContractForActionType(actionType)
		if !ok || !contractSupportsProvider(contract, "azure-cosmos") {
			t.Errorf("%s does not support azure-cosmos", actionType)
		}
	}
	contract, _ := ContractForActionType("alter_system_guc")
	if contractSupportsProvider(contract, "azure-cosmos") {
		t.Fatal("azure-cosmos must not execute ALTER SYSTEM")
	}
}

func TestAzureCosmosIsManagedWithCosmosGuidance(t *testing.T) {
	if !isManagedProvider("azure-cosmos") {
		t.Fatal("azure-cosmos is not treated as managed")
	}
	guidance := managedConfigGuidance("azure-cosmos", "work_mem")
	for _, want := range []string{"az cosmosdb postgres configuration", "work_mem"} {
		if !strings.Contains(guidance, want) {
			t.Errorf("guidance %q lacks %q", guidance, want)
		}
	}
	if strings.Contains(guidance, "flexible-server") {
		t.Errorf("cosmos guidance names the flexible-server command: %q", guidance)
	}
}
