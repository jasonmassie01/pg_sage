package databricksauth

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	databricks "github.com/databricks/databricks-sdk-go"
)

func TestGatewayModelDiscoveryUsesSDKPaginationAndChatCapabilities(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/databricks-config" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		if r.URL.Path != "/api/2.1/unity-catalog/model-services" || r.URL.Query().Get("parent") != "schemas/main.models" {
			t.Errorf("incorrect catalog request: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-sdk-token" {
			t.Error("catalog request lost SDK authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page_token") == "second" {
			_, _ = w.Write([]byte(`{"model_services":[{"name":"model-services/main.models.alpha","comment":"test model","supported_api_types":["mlflow/v1/chat/completions"]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"model_services":[{"name":"model-services/main.models.zeta","supported_api_types":["mlflow/v1/chat/completions"]},{"name":"model-services/main.models.embed","supported_api_types":["mlflow/v1/embeddings"]},{"name":"model-services/main.models.responses_only","supported_api_types":["mlflow/v1/responses"]}],"next_page_token":"second"}`))
	}))
	defer server.Close()
	w, err := databricks.NewWorkspaceClient(&databricks.Config{Host: server.URL, Token: "synthetic-sdk-token", HTTPTransport: server.Client().Transport})
	if err != nil {
		t.Fatal(err)
	}
	models, err := listModelServices(t.Context(), server.URL+"/ai-gateway/mlflow/v1", "main.models", w)
	if err != nil || len(models) != 2 {
		t.Fatalf("models=%+v, error=%v", models, err)
	}
	if calls.Load() != 2 || models[0].ID != "main.models.alpha" || models[1].ID != "main.models.zeta" || models[0].Description != "test model" {
		t.Fatalf("pagination, filtering or service names incorrect: %+v (%d requests)", models, calls.Load())
	}
	if _, err := listModelServices(t.Context(), "https://another.example/ai-gateway/mlflow/v1", "main.models", w); err == nil {
		t.Fatal("cross-origin discovery accepted")
	}
	if calls.Load() != 2 {
		t.Fatal("rejected origin made a catalog request")
	}
}
