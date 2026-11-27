package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// fakeProvider is an OpenAI-compatible endpoint answering every request
// with status (200 gets a valid completion). No live provider is used.
func fakeProvider(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			if status != http.StatusOK {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"},` +
				`"finish_reason":"stop"}],"usage":{"total_tokens":7}}`))
		}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func probeClient(url string) *llm.Client {
	return llm.New(&config.LLMConfig{
		Enabled: true, Endpoint: url, APIKey: "k", Model: "m",
		TimeoutSeconds: 5, TokenBudgetDaily: 100_000, CooldownSeconds: 60,
	}, func(string, string, ...any) {})
}

func probeRouter(mgr *llm.Manager, user *auth.User) http.Handler {
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withUser(r, user))
		})
	}
	return newTestRouter(nil, config.DefaultConfig(), nil, nil, nil,
		mgr, &RuntimeDeps{}, inject)
}

func postProbe(h http.Handler) (*httptest.ResponseRecorder, llm.ProbeResult) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonPost("/api/v1/llm/test"))
	var res llm.ProbeResult
	_ = json.NewDecoder(w.Body).Decode(&res)
	return w, res
}

func TestLLMTestHandler_ViewerForbidden(t *testing.T) {
	srv, calls := fakeProvider(t, http.StatusOK)
	mgr := llm.NewManager(probeClient(srv.URL), nil, false)
	w, _ := postProbe(probeRouter(mgr, testViewerUser()))
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want 403", w.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("viewer request reached the provider %d times", calls.Load())
	}
}

func TestLLMTestHandler_AdminGetsClass(t *testing.T) {
	cases := []struct {
		status int
		class  string
	}{
		{http.StatusOK, "ok"},
		{http.StatusUnauthorized, "auth"},
		{http.StatusBadRequest, "caller"},
	}
	for _, tc := range cases {
		srv, calls := fakeProvider(t, tc.status)
		client := probeClient(srv.URL)
		w, res := postProbe(probeRouter(llm.NewManager(client, nil, false),
			testAdminUser()))
		if w.Code != http.StatusOK {
			t.Fatalf("%d: status = %d, want 200", tc.status, w.Code)
		}
		if res.Class != tc.class || calls.Load() != 1 {
			t.Fatalf("%d: result=%+v calls=%d, want class %s after one request",
				tc.status, res, calls.Load(), tc.class)
		}
		if res.LatencyMS < 0 || (tc.class == "ok" && res.Tokens != 7) {
			t.Fatalf("%d: result=%+v, want latency and 7 tokens", tc.status, res)
		}
		if tc.class == "ok" && client.TokensUsedToday() != 7 {
			t.Fatalf("probe charged %d tokens, want 7", client.TokensUsedToday())
		}
	}
}

// The test is admitted as the half-open probe even inside the cooldown,
// and its success closes the breaker.
func TestLLMTestHandler_ProbeClosesOpenBreaker(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusUnauthorized)
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			if code := int(status.Load()); code != http.StatusOK {
				w.WriteHeader(code)
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}],` +
				`"usage":{"total_tokens":3}}`))
		}))
	t.Cleanup(srv.Close)
	client := probeClient(srv.URL)
	_, _, _ = client.Chat(t.Context(), "s", "u", 10)
	if !client.IsCircuitOpen() {
		t.Fatal("401 did not open the breaker")
	}
	status.Store(http.StatusOK) // the provider recovers inside the cooldown
	w, res := postProbe(probeRouter(llm.NewManager(client, nil, false),
		testAdminUser()))
	if w.Code != http.StatusOK || res.Class != "ok" || res.CircuitState != "closed" {
		t.Fatalf("status=%d result=%+v, want ok and closed", w.Code, res)
	}
	if client.IsCircuitOpen() {
		t.Fatal("successful probe left the breaker open")
	}
}

func TestLLMTestHandler_NoLLMConfigured(t *testing.T) {
	w, _ := postProbe(probeRouter(nil, testAdminUser()))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 without an LLM", w.Code)
	}
}

// CHECK-13: every client in /api/v1/llm/status carries the breaker fields.
func TestLLMStatus_CarriesBreakerFields(t *testing.T) {
	srv, _ := fakeProvider(t, http.StatusUnauthorized)
	client := probeClient(srv.URL)
	_, _ = postProbe(probeRouter(llm.NewManager(client, nil, false), testAdminUser()))
	w := httptest.NewRecorder()
	probeRouter(llm.NewManager(client, nil, false), testViewerUser()).ServeHTTP(w,
		httptest.NewRequest(http.MethodGet, "/api/v1/llm/status", nil))
	var body struct {
		Clients map[string]map[string]any `json:"clients"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil || w.Code != 200 {
		t.Fatalf("status=%d decode=%v", w.Code, err)
	}
	general := body.Clients["general"]
	for _, key := range []string{"circuit_state", "last_error_class",
		"last_error_at", "last_success_at", "consecutive_failures"} {
		if _, ok := general[key]; !ok {
			t.Fatalf("general client status lacks %s: %v", key, general)
		}
	}
	if general["last_error_class"] != "auth" || general["last_error_at"] == nil ||
		general["last_success_at"] != nil || general["consecutive_failures"] != 1.0 {
		t.Fatalf("status = %v, want one auth failure and no success", general)
	}
}
