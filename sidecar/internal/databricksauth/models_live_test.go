package databricksauth

import (
	"os"
	"strings"
	"testing"
)

// Catalog metadata only; no inference requests or database queries.
func TestGatewayModelDiscoveryLive(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_LLM") != "1" || os.Getenv("SAGE_LLM_AUTH") != "databricks" {
		t.Skip("set PG_SAGE_LIVE_LLM=1 and SAGE_LLM_AUTH=databricks")
	}
	endpoint := os.Getenv("SAGE_LLM_ENDPOINT")
	if endpoint == "" {
		t.Fatal("SAGE_LLM_ENDPOINT is required")
	}
	models, err := ListModelServices(t.Context(), endpoint, "system.ai")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 {
		t.Fatal("no chat model services found")
	}
	found := false
	for _, model := range models {
		if !strings.HasPrefix(model.ID, "system.ai.") {
			t.Fatalf("model is not a fully qualified service: %q", model.ID)
		}
		found = found || model.ID == os.Getenv("SAGE_LLM_MODEL")
	}
	if selected := os.Getenv("SAGE_LLM_MODEL"); selected != "" && !found {
		t.Fatal("selected model absent from accessible chat services")
	}
	t.Logf("Discovered %d accessible unified Chat Completions model services; selected model present=%v", len(models), found)
}
