package fleet

import "testing"

// Cosmos DB for PostgreSQL is not a flexible server: readiness must not
// fold it into the azure adapter (clouds-11).
func TestAzureCosmosDoesNotNormalizeToAzure(t *testing.T) {
	if got := normalizeProviderName("azure-cosmos"); got != "azure-cosmos" {
		t.Fatalf("normalizeProviderName(azure-cosmos) = %q, want azure-cosmos", got)
	}
}
