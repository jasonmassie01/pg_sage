package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

func TestModelHandlersFollowActiveConfig(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			var requests atomic.Int32
			server := modelSnapshotServer(t, &requests)
			defer server.Close()
			base := config.DefaultConfig()
			controller := config.NewConfigController(base, nil, llm.New(&base.LLM, nil))
			handler := listModelsHandler(&base.LLM, controller)
			if method == http.MethodPost {
				handler = discoverModelsHandler(&base.LLM, controller)
			}
			changed := config.Clone(base)
			changed.LLM.Enabled, changed.LLM.Endpoint = true, server.URL
			changed.LLM.APIKey, changed.LLM.Model = "local-model-key", "current-model"
			applyModelSnapshot(t, controller, changed)
			llm.InvalidateModelCache()
			response := callModelSnapshotHandler(method, handler)
			assertModelSnapshot(t, response)
			if requests.Load() != 1 {
				t.Fatalf("provider requests = %d, want 1", requests.Load())
			}
			applyModelSnapshot(t, controller, base)
			response = callModelSnapshotHandler(method, handler)
			if response.Code != 503 || !strings.Contains(response.Body.String(), "not configured") {
				t.Fatalf("reset should disable discovery: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestModelHandlersDoNotUsePendingRestartConfig(t *testing.T) {
	base := config.DefaultConfig()
	controller := config.NewConfigController(base, nil)
	changed := config.Clone(base)
	changed.LLM.Enabled, changed.LLM.Endpoint = true, "http://127.0.0.1:1"
	changed.LLM.APIKey = "never-send-pending-key"
	applyModelSnapshot(t, controller, changed)
	if len(controller.PendingRestart()) == 0 {
		t.Fatal("expected unregistered LLM owner to require restart")
	}
	response := callModelSnapshotHandler(http.MethodGet, listModelsHandler(&base.LLM, controller))
	if response.Code != 503 || !strings.Contains(response.Body.String(), "not configured") {
		t.Fatalf("pending settings leaked into active API: %d %s", response.Code, response.Body)
	}
}

func TestModelHandlerNilConfiguration(t *testing.T) {
	for _, handler := range []http.HandlerFunc{listModelsHandler(nil), discoverModelsHandler(nil)} {
		response := callModelSnapshotHandler(http.MethodPost, handler)
		if response.Code != 503 || !strings.Contains(response.Body.String(), "not configured") {
			t.Fatalf("nil config should be actionable: %d %s", response.Code, response.Body)
		}
	}
}

func TestModelConfigConcurrentPublication(t *testing.T) {
	base := config.DefaultConfig()
	base.LLM.Model = "initial-model"
	controller := config.NewConfigController(base, nil, llm.New(&base.LLM, nil))
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				current := activeLLMConfig(nil, []*config.ConfigController{controller})
				if current.Model != "initial-model" && current.Model != "changed-model" {
					t.Errorf("partial model snapshot: %q", current.Model)
				}
			}
		}()
	}
	for range 20 {
		next := config.Clone(base)
		next.LLM.Model = "changed-model"
		applyModelSnapshot(t, controller, next)
		applyModelSnapshot(t, controller, base)
	}
	readers.Wait()
	if got := activeLLMConfig(nil, []*config.ConfigController{controller}).Model; got != base.LLM.Model {
		t.Fatalf("final model = %q, want %q", got, base.LLM.Model)
	}
}

func modelSnapshotServer(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer local-model-key" {
			t.Error("provider did not receive current credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"current-model"}]}`))
	}))
}

func applyModelSnapshot(t *testing.T, c *config.ConfigController, cfg *config.Config) {
	t.Helper()
	result, err := c.Apply(context.Background(), c.Desired().Generation, cfg)
	if err != nil || result.DesiredGeneration != c.Desired().Generation {
		t.Fatalf("apply model config: generation %d, error %v", result.DesiredGeneration, err)
	}
}

func callModelSnapshotHandler(method string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler(response, httptest.NewRequest(method, "/api/v1/llm/models", strings.NewReader(`{}`)))
	return response
}

func assertModelSnapshot(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	var decoded struct {
		Current string          `json:"current"`
		Models  []llm.ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if response.Code != 200 || decoded.Current != "current-model" || len(decoded.Models) != 1 {
		t.Fatalf("stale models response: %d %s", response.Code, response.Body)
	}
	if decoded.Models[0].ID != "current-model" {
		t.Fatalf("model ID = %q", decoded.Models[0].ID)
	}
}
