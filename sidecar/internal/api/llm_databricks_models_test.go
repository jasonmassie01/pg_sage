package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/llm"
)

func TestGatewayModelHandlersFollowActiveAuthWithoutAPIKey(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			base := config.DefaultConfig()
			controller := config.NewConfigController(base, nil, llm.New(&base.LLM, nil))
			next := config.Clone(base)
			next.LLM.Auth, next.LLM.Endpoint, next.LLM.Model = "databricks", "https://workspace.example/ai-gateway/mlflow/v1", "main.models.current"
			applyModelSnapshot(t, controller, next)
			calls := 0
			discover := func(_ context.Context, endpoint, schema string) ([]databricksauth.GatewayModel, error) {
				calls++
				if endpoint != next.LLM.Endpoint || schema != "main.models" {
					t.Errorf("stale discovery settings: %s %s", endpoint, schema)
				}
				return []databricksauth.GatewayModel{{ID: "main.models.current"}}, nil
			}
			h := listModelsWithDiscovery(&base.LLM, []*config.ConfigController{controller}, discover)
			if method == http.MethodPost {
				h = discoverModelsWithDiscovery(&base.LLM, []*config.ConfigController{controller}, discover)
			}
			response := callModelSnapshotHandler(method, h)
			var body struct {
				Current string          `json:"current"`
				Models  []llm.ModelInfo `json:"models"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 200 || calls != 1 || body.Current != next.LLM.Model || len(body.Models) != 1 || body.Models[0].ID != next.LLM.Model {
				t.Fatalf("native-auth discovery: %d %s, calls=%d, error=%v", response.Code, response.Body, calls, err)
			}
		})
	}
}

func TestGatewayDiscoveryOverridePreservesOriginAndRequestScope(t *testing.T) {
	cfg := &config.LLMConfig{Auth: "databricks", Endpoint: "https://workspace.example/ai-gateway/mlflow/v1", Model: "system.ai.current"}
	calls := 0
	discover := func(_ context.Context, endpoint, schema string) ([]databricksauth.GatewayModel, error) {
		calls++
		if schema != "main.models" || endpoint != cfg.Endpoint {
			t.Errorf("wrong discovery scope: %s %s", endpoint, schema)
		}
		return []databricksauth.GatewayModel{}, nil
	}
	h := discoverModelsWithDiscovery(cfg, nil, discover)
	for _, body := range []string{
		`{"config":{"llm.endpoint":"https://other.example/v1","llm.api_key":"explicit-key"}}`,
		`{"config":{"llm.endpoint":"http://workspace.example/v1"}}`,
		`{"config":{"llm.endpoint":"https://user@workspace.example/v1"}}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/llm/models", strings.NewReader(body))
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != 400 || calls != 0 {
			t.Fatalf("origin override accepted: %d %s", w.Code, w.Body)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/llm/models", strings.NewReader(`{"config":{"llm.endpoint":"https://workspace.example/ai-gateway/mlflow/v1","llm.model":"main.models.preview"}}`))
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != 200 || calls != 1 || cfg.Model != "system.ai.current" || cfg.APIKey != "" {
		t.Fatalf("preview config was rejected or persisted: %d %s", w.Code, w.Body)
	}
}

func TestGatewayDiscoveryPreviewFromUnconfiguredProvider(t *testing.T) {
	for _, previousEndpoint := range []string{"", "https://api.openai.com/v1"} {
		cfg := &config.LLMConfig{Endpoint: previousEndpoint}
		calls := 0
		// The real native lister separately verifies this endpoint against
		// the selected SDK workspace, covered by its SDK transport test.
		discover := func(_ context.Context, endpoint, schema string) ([]databricksauth.GatewayModel, error) {
			calls++
			if endpoint != "https://workspace.example/ai-gateway/mlflow/v1" || schema != "system.ai" {
				t.Errorf("initial setup lost native endpoint/schema: %s %s", endpoint, schema)
			}
			return []databricksauth.GatewayModel{{ID: "system.ai.current"}}, nil
		}
		h := discoverModelsWithDiscovery(cfg, nil, discover)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/llm/models", strings.NewReader(`{"config":{"llm.auth":"databricks","llm.endpoint":"https://workspace.example/ai-gateway/mlflow/v1"}}`))
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != 200 || calls != 1 || cfg.Auth != "" || cfg.Endpoint != previousEndpoint {
			t.Fatalf("initial native preview rejected or persisted: %d %s", w.Code, w.Body)
		}
	}
}
