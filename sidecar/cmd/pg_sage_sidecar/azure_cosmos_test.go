package main

import (
	"strings"
	"testing"
)

// Azure Cosmos DB for PostgreSQL (Citus) coordinators live under
// postgres.cosmos.azure.com. They are Azure, but not flexible servers.
func TestHostedProviderFromHostAzureCosmos(t *testing.T) {
	for host, want := range map[string]string{
		"c-sage.abc123xyz.postgres.cosmos.azure.com":      "azure-cosmos",
		"C-SAGE.ABC123XYZ.POSTGRES.COSMOS.AZURE.COM.":     "azure-cosmos",
		"w0-sage.abc123xyz.postgres.cosmos.azure.com":     "azure-cosmos",
		"postgres.cosmos.azure.com.attacker.example":      "",
		"c-sage.abc123xyz.postgres.cosmos.azure.com:5432": "",
		"c-sage.abc123xyz.mongo.cosmos.azure.com":         "",
	} {
		if got := hostedProviderFromHost(host); got != want {
			t.Errorf("hostedProviderFromHost(%q) = %q, want %q", host, got, want)
		}
	}
}

// Cosmos has its own ARM resource type (serverGroupsv2); the flexible-server
// adapter must never be pointed at it, even when azure.server_name is set.
func TestAzureManagedConfigCosmosIsGuidanceOnly(t *testing.T) {
	stubAzureToken(t, nil)
	adapter, note := azureManagedConfig(azureCfg("s", "rg", "some-flex-server"),
		"azure-cosmos", "c-sage.abc123xyz.postgres.cosmos.azure.com")
	if adapter != nil {
		t.Fatal("cosmos target got the flexible-server ARM adapter")
	}
	if !strings.Contains(note, "guidance-only") || !strings.Contains(note, "cosmos") {
		t.Fatalf("note %q does not explain cosmos guidance-only", note)
	}
}
