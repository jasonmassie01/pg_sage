package fleet

import "testing"

func TestAzureCosmosNormalizesToAzure(t *testing.T) {
	if got := normalizeProviderName("azure-cosmos"); got != "azure" {
		t.Fatalf("normalizeProviderName(azure-cosmos) = %q, want azure", got)
	}
}
