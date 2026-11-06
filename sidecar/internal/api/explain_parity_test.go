package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// security-02: for people, EXPLAIN over REST (POST /api/v1/explain) and
// over MCP (explain_query) admit the same roles, plan-only and ANALYZE
// alike, and both honour explain.enabled. Agents are outside REST explain;
// their plan-only EXPLAIN under their own role is covered in internal/mcp.
// No concurrent access tests: both checks are per-request and stateless.

// explainCase pairs one REST body with the MCP arguments asking the same.
type explainCase struct {
	name, rest, mcp string
}

var explainCases = []explainCase{
	{"plan-only", `{"query":"SELECT 1","plan_only":true}`, `{"query":"SELECT 1"}`},
	{"analyze", `{"query":"SELECT 1"}`, `{"query":"SELECT 1","analyze":true}`},
}

func explainRouter(cfg *config.Config, user *auth.User) http.Handler {
	mgr := fleet.NewManager(cfg)
	withUser := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(),
				userContextKey, user)))
		})
	}
	return NewRouterFull(mgr, cfg, nil, nil, nil, nil, withUser)
}

func restExplain(cfg *config.Config, user *auth.User, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/explain", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	explainRouter(cfg, user).ServeHTTP(w, req)
	return w.Code
}

func TestExplainRoleParityRESTAndMCP(t *testing.T) {
	cfg := &config.Config{Mode: "fleet", Explain: config.ExplainConfig{Enabled: true}}
	users := map[string]*auth.User{"viewer": testViewerUser(),
		"operator": testOperatorUser(), "admin": testAdminUser()}
	admitted := map[string]bool{}
	for role, user := range users {
		for _, c := range explainCases {
			restAllowed := restExplain(cfg, user, c.rest) != http.StatusForbidden
			scope, ok := mcp.RequiredScope("explain_query", json.RawMessage(c.mcp))
			if !ok {
				t.Fatal("explain_query is not an MCP tool")
			}
			session := mcp.Principal{Actor: "user:1", Role: user.Role}
			mcpAllowed := session.Has(scope)
			if restAllowed != mcpAllowed {
				t.Errorf("%s %s: REST allowed=%v, MCP allowed=%v (scope %s)", role, c.name,
					restAllowed, mcpAllowed, scope)
			}
			if scope != mcp.ScopePropose {
				t.Errorf("%s: MCP scope = %s, want propose (REST's operator minimum)",
					c.name, scope)
			}
			admitted[role] = admitted[role] || restAllowed
		}
	}
	if admitted["viewer"] || !admitted["operator"] || !admitted["admin"] {
		t.Errorf("admitted = %v, want operator and admin only", admitted)
	}
}

func TestExplainDisabledParityRESTAndMCP(t *testing.T) {
	cfg := &config.Config{Mode: "fleet", Explain: config.ExplainConfig{Enabled: false}}
	tools := agenttools.New(nil, agenttools.Options{Explain: &cfg.Explain})
	for _, c := range explainCases {
		if code := restExplain(cfg, testOperatorUser(), c.rest); code !=
			http.StatusServiceUnavailable {
			t.Errorf("REST %s with explain.enabled=false: status %d, want 503", c.name, code)
		}
		var req agenttools.ExplainRequest
		if err := json.Unmarshal([]byte(c.mcp), &req); err != nil {
			t.Fatalf("decode %s: %v", c.mcp, err)
		}
		_, err := tools.ExplainQuery(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "explain.enabled") {
			t.Errorf("MCP %s with explain.enabled=false: error %v, want explain.enabled",
				c.name, err)
		}
		planner := func(context.Context, readapi.ExplainRequest) (readapi.ExplainResult, error) {
			t.Errorf("MCP %s: an agent explain reached the planner while disabled", c.name)
			return readapi.ExplainResult{}, nil
		}
		if _, err := tools.ExplainAsAgent(context.Background(), req, planner); err == nil ||
			!strings.Contains(err.Error(), "explain.enabled") {
			t.Errorf("agent %s with explain.enabled=false: error %v", c.name, err)
		}
	}
}
