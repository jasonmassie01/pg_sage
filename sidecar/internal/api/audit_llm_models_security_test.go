package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func auditLLMModelsRouter(cfg *config.Config) http.Handler {
	mux := http.NewServeMux()
	registerAPIRoutes(mux, nil, cfg, nil, nil, nil, false)
	return mux
}

func auditModelsRequest(
	t *testing.T,
	handler http.Handler,
	userRole string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/llm/models",
		strings.NewReader(body),
	)
	if userRole != "" {
		user := testViewerUser()
		user.Role = userRole
		req = withUser(req, user)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestAuditLLMModelDiscoveryRequiresAdmin(t *testing.T) {
	// Invalid JSON is deliberate: authorization must run before body parsing
	// or any provider request, so unauthorized callers cannot probe behavior.
	handler := auditLLMModelsRouter(&config.Config{})
	tests := []struct {
		name string
		role string
		want int
	}{
		{name: "anonymous", role: "", want: http.StatusUnauthorized},
		{name: "viewer", role: "viewer", want: http.StatusForbidden},
		{name: "operator", role: "operator", want: http.StatusForbidden},
		{name: "admin reaches handler", role: "admin", want: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := auditModelsRequest(t, handler, test.role, `{`)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s",
					response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestAuditLLMModelDiscoveryRejectsPrivateEndpointOverride(t *testing.T) {
	var privateRequests atomic.Int32
	privateServer := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			privateRequests.Add(1)
			_, _ = w.Write([]byte(`{"data":[{"id":"stolen"}]}`))
		},
	))
	defer privateServer.Close()

	cfg := &config.Config{}
	cfg.LLM.Endpoint = "https://api.openai.com/v1"
	cfg.LLM.APIKey = "configured-secret"
	handler := auditLLMModelsRouter(cfg)
	body := `{"config":{"llm.endpoint":"` + privateServer.URL + `"}}`
	response := auditModelsRequest(t, handler, "admin", body)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("private override status = %d, want 400; body=%s",
			response.Code, response.Body.String())
	}
	if got := privateRequests.Load(); got != 0 {
		t.Fatalf("private endpoint received %d requests, want 0", got)
	}
}

func TestAuditLLMModelDiscoveryRejectsDangerousEndpointForms(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
	}{
		{name: "cloud metadata", endpoint: "http://169.254.169.254/latest"},
		{name: "ipv6 loopback", endpoint: "http://[::1]:8080/v1"},
		{name: "integer loopback", endpoint: "http://2130706433/v1"},
		{name: "unsupported file scheme", endpoint: "file:///etc/passwd"},
		{name: "credential smuggling", endpoint: "https://user:pass@example.com/v1"},
		{name: "empty host", endpoint: "https:///v1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateLLMDiscoveryEndpoint(
				t.Context(), test.endpoint,
			); err == nil {
				t.Fatalf("dangerous endpoint %q was accepted", test.endpoint)
			}
		})
	}
}

func TestAuditLLMModelDiscoveryFailsClosedOnDNSFailure(t *testing.T) {
	err := validateLLMDiscoveryEndpoint(
		t.Context(), "https://does-not-exist.invalid/v1",
	)
	if err == nil {
		t.Fatal("unresolvable endpoint was accepted")
	}
}

func TestAuditEndpointOverrideCannotReuseConfiguredCredential(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.Endpoint = "https://api.openai.com/v1"
	cfg.LLM.APIKey = "configured-secret"
	handler := auditLLMModelsRouter(cfg)
	body := `{"config":{"llm.endpoint":"https://example.com/v1"}}`
	response := auditModelsRequest(t, handler, "admin", body)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("credential reuse status = %d, want 400; body=%s",
			response.Code, response.Body.String())
	}
	if !strings.Contains(strings.ToLower(response.Body.String()), "api key") {
		t.Fatalf("credential reuse error was not actionable: %s",
			response.Body.String())
	}
}

func TestAuditLLMModelDiscoveryRoleGateIsConcurrent(t *testing.T) {
	var privateRequests atomic.Int32
	privateServer := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			privateRequests.Add(1)
			_, _ = w.Write([]byte(`{"data":[]}`))
		},
	))
	defer privateServer.Close()

	cfg := &config.Config{}
	cfg.LLM.APIKey = "configured-secret"
	handler := auditLLMModelsRouter(cfg)
	body := `{"config":{"llm.endpoint":"` + privateServer.URL + `"}}`
	const callers = 16
	statuses := make(chan int, callers)
	for range callers {
		go func() {
			statuses <- auditModelsRequest(t, handler, "viewer", body).Code
		}()
	}
	for range callers {
		if got := <-statuses; got != http.StatusForbidden {
			t.Fatalf("concurrent viewer status = %d, want 403", got)
		}
	}
	if got := privateRequests.Load(); got != 0 {
		t.Fatalf("private endpoint received %d requests, want 0", got)
	}
}

// No database integration test: model discovery is stateless and its only
// side effect is the HTTP request asserted by the in-process provider above.
// Nil/empty input is covered by invalid JSON and the configured-field checks
// already present in llm_handlers_test.go.
