package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// No concurrent access tests: the model check sends one probe per run.

const okCompletion = `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`

// fakeModel answers every chat request with status and body after delay.
func fakeModel(t *testing.T, status int, body string, delay time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "0")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// discardLog is the client's log; llm.New with a nil log panics when a
// failure opens the breaker, so every caller passes one.
func discardLog(string, string, ...any) {}

const modelKey = "sk-doctor-test-key-0123456789"

// probeAgainst runs the model check with a real llm.Client, the client
// POST /api/v1/llm/test uses, pointed at endpoint.
func probeAgainst(t *testing.T, endpoint string) Check {
	t.Helper()
	client := llm.New(&config.LLMConfig{Enabled: true, Endpoint: endpoint,
		APIKey: modelKey, Model: "doctor-model", TimeoutSeconds: 1,
		TokenBudgetDaily: 100000, CooldownSeconds: 1}, discardLog)
	r := Run(context.Background(), Inputs{Model: client.Probe,
		Secrets: []string{modelKey}})
	c := mustCheck(t, r, IDModel)
	if strings.Contains(c.Detail+c.Fix, modelKey) {
		t.Fatalf("model check leaks the key: %+v", c)
	}
	return c
}

// Happy path: a provider that answers passes, naming the model.
func TestModelCheckPassesOnAnswer(t *testing.T) {
	c := probeAgainst(t, fakeModel(t, http.StatusOK, okCompletion, 0))
	if c.Status != StatusPass || !strings.Contains(c.Detail, "doctor-model") {
		t.Fatalf("model = %+v, want pass naming the model", c)
	}
}

// Error propagation: each provider failure is a fail with its own,
// actionable detail.
func TestModelCheckFailuresAreDistinct(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		delay  time.Duration
		want   string
		fix    string
	}{
		{"401", http.StatusUnauthorized, `{"error":"bad key"}`, 0, "refused the key",
			"llm.api_key"},
		{"429", http.StatusTooManyRequests, `{"error":"slow down"}`, 0, "rate limit",
			"quota"},
		{"timeout", http.StatusOK, okCompletion, 3 * time.Second, "did not answer",
			"llm.timeout_seconds"},
		{"empty body", http.StatusOK, "", 0, "empty reply", "llm.endpoint"},
		{"markdown", http.StatusOK, "```json\n" + okCompletion + "\n```", 0,
			"markdown fence", "OpenAI-compatible"},
		{"malformed", http.StatusOK, `{"choices": [}`, 0, "not valid JSON",
			"OpenAI-compatible"},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		c := probeAgainst(t, fakeModel(t, tc.status, tc.body, tc.delay))
		if c.Status != StatusFail || !strings.Contains(c.Detail, tc.want) ||
			!strings.Contains(c.Fix, tc.fix) {
			t.Errorf("%s: model = %+v, want fail with %q and a fix naming %q",
				tc.name, c, tc.want, tc.fix)
		}
		if prev, dup := seen[c.Detail]; dup {
			t.Errorf("%s and %s share the detail %q", tc.name, prev, c.Detail)
		}
		seen[c.Detail] = tc.name
	}
}

// Nil/empty: a client with no endpoint or key reports "no model
// configured" as unknown, not as a fail.
func TestModelCheckDisabledClientIsUnknown(t *testing.T) {
	client := llm.New(&config.LLMConfig{Enabled: true}, discardLog)
	r := Run(context.Background(), Inputs{Model: client.Probe})
	c := mustCheck(t, r, IDModel)
	if c.Status != StatusUnknown || !strings.Contains(c.Detail, "no model configured") {
		t.Fatalf("model = %+v, want unknown \"no model configured\"", c)
	}
}

// The mapping covers the local refusals the probe can report.
func TestModelResultMapping(t *testing.T) {
	cases := map[string]Status{
		"ok": StatusPass, "disabled": StatusUnknown, "canceled": StatusUnknown,
		"budget": StatusWarn, "circuit_open": StatusFail, "server": StatusFail,
		"caller": StatusFail, "transport": StatusFail, "empty": StatusFail, "error": StatusFail,
	}
	for class, want := range cases {
		c := modelResultCheck(llm.ProbeResult{Class: class, Model: "m", Error: "boom"})
		if c.Status != want || c.Detail == "" {
			t.Errorf("class %s = %+v, want %s with a detail", class, c, want)
		}
		if want == StatusFail && c.Fix == "" {
			t.Errorf("class %s fails without a fix", class)
		}
	}
}
