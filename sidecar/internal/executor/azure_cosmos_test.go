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

// Cosmos parameters are guidance-only, so the gate blocks ALTER DATABASE
// ... SET there; flexible server and Lakebase keep it.
func TestAzureCosmosHasNoDatabaseGUCContract(t *testing.T) {
	contract, ok := ContractForActionType("alter_database_guc")
	if !ok || contractSupportsProvider(contract, "azure-cosmos") {
		t.Fatalf("alter_database_guc supports azure-cosmos: %v", contract.ProviderSupport)
	}
	for _, provider := range []string{"azure", "lakebase", "postgres", "rds"} {
		if !contractSupportsProvider(contract, provider) {
			t.Errorf("alter_database_guc lost %s: %v", provider, contract.ProviderSupport)
		}
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
