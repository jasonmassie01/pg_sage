package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestDatabricksAuthRefreshesChatRetryAndKeepsGatewayPath(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if r.URL.Path != "/ai-gateway/mlflow/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer test-refreshed-%d", n) {
			t.Error("request did not use refreshed unified authentication")
		}
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":4}}`))
	}))
	defer s.Close()
	c := New(&config.LLMConfig{Enabled: true, Auth: "databricks", Endpoint: s.URL + "/ai-gateway/mlflow/v1", Model: "system.ai.test"}, func(string, string, ...any) {})
	var signed atomic.Int32
	c.authorizeDatabricks = func(r *http.Request) error {
		r.Header.Set("Authorization", fmt.Sprintf("Bearer test-refreshed-%d", signed.Add(1)))
		return nil
	}
	if !c.IsEnabled() {
		t.Fatal("unified auth should not require a static API key")
	}
	result, tokens, err := c.Chat(context.Background(), "test", "hello", 20)
	if err != nil || result != "ok" || tokens != 4 || signed.Load() != 2 {
		t.Fatalf("Chat = %q, %d, %v; authentications=%d", result, tokens, err, signed.Load())
	}
}

func TestDatabricksAuthAlsoSignsToolCalls(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-unified-tools" {
			t.Error("tool request bypassed unified authentication")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":4}}`))
	}))
	defer s.Close()
	c := New(&config.LLMConfig{Enabled: true, Auth: "databricks", Endpoint: s.URL, Model: "test"}, func(string, string, ...any) {})
	c.authorizeDatabricks = func(r *http.Request) error {
		r.Header.Set("Authorization", "Bearer test-unified-tools")
		return nil
	}
	res, err := c.ChatWithTools(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil, ToolOptions{MaxTokens: 20})
	if err != nil || res.Content != "ok" {
		t.Fatalf("tools result=%+v, error=%v", res, err)
	}
}

func TestNewOptimizerClientUsesActualOverrideForAuthentication(t *testing.T) {
	parent := &config.LLMConfig{Enabled: true, Auth: "databricks", Endpoint: "https://workspace/ai-gateway/mlflow/v1"}
	c := NewOptimizerClient(parent, &config.OptimizerLLMConfig{Enabled: true, Endpoint: "https://other-provider/v1", APIKey: "test-key"}, func(string, string, ...any) {})
	cfg, _ := c.configSnapshot()
	if cfg.Auth != "api_key" {
		t.Fatal("optimizer used workspace authentication instead of its explicit provider credentials")
	}
}

func TestDatabricksRequestsRejectRedirects(t *testing.T) {
	for _, mode := range []string{"chat", "tools"} {
		for _, auth := range []string{"databricks", "api_key"} {
			t.Run(mode+"/"+auth, func(t *testing.T) {
				var redirected atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					redirected.Add(1)
					_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":4}}`))
				}))
				defer target.Close()
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				}))
				defer origin.Close()
				c := New(&config.LLMConfig{Enabled: true, Auth: auth, APIKey: "test-key", Endpoint: origin.URL, Model: "test"}, func(string, string, ...any) {})
				c.authorizeDatabricks = func(r *http.Request) error {
					r.Header.Set("Authorization", "Bearer test-native")
					return nil
				}
				var err error
				if mode == "chat" {
					_, _, err = c.Chat(context.Background(), "test", "hello", 20)
				} else {
					_, err = c.ChatWithTools(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil, ToolOptions{MaxTokens: 20})
				}
				if auth == "databricks" {
					if err == nil || redirected.Load() != 0 {
						t.Fatalf("native request followed redirect: err=%v, target requests=%d", err, redirected.Load())
					}
				} else if err != nil || redirected.Load() != 1 {
					t.Fatalf("API-key redirect behavior changed: err=%v, target requests=%d", err, redirected.Load())
				}
			})
		}
	}
}

func TestDatabricksAuthDenialsAreTerminalWithoutCredentialFallback(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests, signed atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(status) }))
			defer s.Close()
			c := New(&config.LLMConfig{Enabled: true, Auth: "databricks", Endpoint: s.URL, Model: "test"}, func(string, string, ...any) {})
			c.authorizeDatabricks = func(r *http.Request) error {
				signed.Add(1)
				r.Header.Set("Authorization", "Bearer test-native")
				return nil
			}
			if _, _, err := c.Chat(context.Background(), "test", "hello", 20); err == nil || requests.Load() != 1 || signed.Load() != 1 {
				t.Fatalf("auth denial retried or fell back: err=%v requests=%d authorizations=%d", err, requests.Load(), signed.Load())
			}
		})
	}
}

func TestDatabricksAppBlocksUnsafeReconfiguredChatAndToolsBeforeNetwork(t *testing.T) {
	t.Setenv("SAGE_AUTH_MODE", "databricks_app")
	t.Setenv("DATABRICKS_HOST", "https://workspace.cloud.databricks.com")
	for _, tools := range []bool{false, true} {
		c := New(&config.LLMConfig{Enabled: true, Auth: "databricks", Endpoint: "https://workspace.cloud.databricks.com/ai-gateway/mlflow/v1"}, func(string, string, ...any) {})
		c.httpClient.Transport = auditRoundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unsafe configuration reached transport")
			return nil, fmt.Errorf("unexpected transport")
		})
		c.Reconfigure(&config.LLMConfig{Enabled: true, Auth: "api_key", APIKey: "test-key", Endpoint: "https://other.example/v1"})
		c.authorizeDatabricks = func(*http.Request) error { t.Fatal("unsafe configuration authenticated"); return nil }
		var err error
		if tools {
			_, err = c.ChatWithTools(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil, ToolOptions{MaxTokens: 20})
		} else {
			_, _, err = c.Chat(context.Background(), "test", "hello", 20)
		}
		if err == nil || c.IsEnabled() {
			t.Fatal("App used reconfigured static-key provider")
		}
	}
}
