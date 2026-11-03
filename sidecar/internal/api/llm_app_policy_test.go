package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
)

func TestDatabricksAppDiscoveryRejectsRequestScopedAuthDowngrade(t *testing.T) {
	t.Setenv("SAGE_AUTH_MODE", "databricks_app")
	t.Setenv("DATABRICKS_HOST", "https://workspace.cloud.databricks.com")
	cfg := &config.LLMConfig{Auth: "databricks", Endpoint: "https://workspace.cloud.databricks.com/ai-gateway/mlflow/v1"}
	discover := func(context.Context, string, string) ([]databricksauth.GatewayModel, error) {
		t.Fatal("invalid discovery reached provider")
		return nil, nil
	}
	h := discoverModelsWithDiscovery(cfg, nil, discover)
	for _, body := range []string{`{"config":{"llm.auth":"api_key","llm.api_key":"test-key"}}`, `{"config":{"llm.api_key":"test-key"}}`, `{"config":{"llm.endpoint":"https://other.example/v1"}}`} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/llm/models/discover", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "test-key") {
			t.Fatalf("unsafe discovery status=%d body=%s", w.Code, w.Body.String())
		}
	}
}
