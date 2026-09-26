package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestPreflightSurfaceMCPViewerCannotPersistPolicyProposal(t *testing.T) {
	pool := surfacePool(t)
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled, cfg.MCP.Transport = true, "http"
	f := surfaceRouter(t, pool, cfg, surfaceMCP(t, pool, false))
	f.login(t, "viewer")
	status, body := f.request(t, "POST", "/api/v1/policy/proposals", `{}`)
	if status != 403 {
		t.Fatalf("REST role control: status=%d body=%s", status, body)
	}
	before := surfaceCount(t, pool, `SELECT count(*) FROM sage.policy WHERE status='proposed'`)
	status, body = f.request(t, "POST", "/api/v1/mcp", surfaceRPC("propose_policy_change",
		`{"delta":{"lock_duration_ceiling_ms":1000}}`))
	after := surfaceCount(t, pool, `SELECT count(*) FROM sage.policy WHERE status='proposed'`)
	t.Logf("REST=403 MCP=%d proposal rows before=%d after=%d response=%s", status, before, after, body)
	if after != before {
		t.Errorf("viewer persisted a policy proposal through mounted MCP")
	}
}

func TestPreflightSurfaceMCPViewerCannotRegisterConsumer(t *testing.T) {
	pool := surfacePool(t)
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled, cfg.MCP.Transport = true, "http"
	f := surfaceRouter(t, pool, cfg, surfaceMCP(t, pool, false))
	f.login(t, "viewer")
	status, body := f.request(t, "POST", "/api/v1/mcp", surfaceRPC("register_consumer",
		`{"slot_name":"surface_viewer_probe","owner":"fixture"}`))
	count := surfaceCount(t, pool, `SELECT count(*) FROM sage.slot_consumer_registry
  WHERE slot_name='surface_viewer_probe'`)
	t.Logf("viewer MCP=%d persisted consumers=%d response=%s", status, count, body)
	if count != 0 {
		t.Error("viewer changed operational consumer registry through mounted MCP")
	}
}

func TestPreflightSurfaceMCPAuthenticationAndStopControls(t *testing.T) {
	pool := surfacePool(t)
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled, cfg.MCP.Transport = true, "http"
	f := surfaceRouter(t, pool, cfg, surfaceMCP(t, pool, true))
	rpc := surfaceRPC("register_consumer", `{"slot_name":"surface_stop_probe","owner":"fixture"}`)
	status, _ := f.request(t, "POST", "/api/v1/mcp", rpc)
	if status != 401 {
		t.Fatalf("anonymous MCP status=%d want401", status)
	}
	f.login(t, "operator")
	status, body := f.request(t, "POST", "/api/v1/mcp", rpc)
	if status != 200 || !strings.Contains(body, "emergency_stop") {
		t.Errorf("stop response: status=%d body=%s", status, body)
	}
	if n := surfaceCount(t, pool, `SELECT count(*) FROM sage.slot_consumer_registry
  WHERE slot_name='surface_stop_probe'`); n != 0 {
		t.Errorf("stop persisted %d consumers", n)
	}
	t.Log("anonymous route denied; emergency-stop policy denied write; persisted row count=0")
}

func surfaceOIDCProvider(t *testing.T, identity map[string]any) string {
	t.Helper()
	var provider *httptest.Server
	provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": provider.URL,
				"authorization_endpoint": provider.URL + "/authorize", "token_endpoint": provider.URL + "/token",
				"userinfo_endpoint": provider.URL + "/userinfo", "jwks_uri": provider.URL + "/jwks"})
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "local-fixture-token",
				"token_type": "Bearer"})
		case "/userinfo":
			_ = json.NewEncoder(w).Encode(identity)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	return provider.URL
}

func surfaceOIDCFlow(t *testing.T, f *surfaceFixture) (int, string) {
	t.Helper()
	status, body := f.request(t, "GET", "/api/v1/auth/oauth/authorize", "")
	if status != 200 {
		t.Fatalf("authorize=%d %s", status, body)
	}
	var response map[string]string
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(response["url"])
	if err != nil {
		t.Fatal(err)
	}
	state := target.Query().Get("state")
	if state == "" {
		t.Fatal("authorization returned no state")
	}
	return f.request(t, "GET", "/api/v1/auth/oauth/callback?code=fixture&state="+url.QueryEscape(state), "")
}

func TestPreflightSurfaceOIDCRejectsUnverifiedExistingAdmin(t *testing.T) {
	for _, mode := range []string{"false", "missing"} {
		t.Run(mode, func(t *testing.T) {
			pool := surfacePool(t)
			email := "existing-admin-" + mode + "@fixture.invalid"
			if _, err := auth.CreateUser(context.Background(), pool, email, "local-fixture-password", "admin"); err != nil {
				t.Fatal(err)
			}
			identity := map[string]any{"sub": "untrusted-subject-" + mode, "email": email}
			if mode == "false" {
				identity["email_verified"] = false
			}
			issuer := surfaceOIDCProvider(t, identity)
			cfg := config.DefaultConfig()
			cfg.OAuth = config.OAuthConfig{Enabled: true, Provider: "oidc", IssuerURL: issuer,
				ClientID: "fixture", ClientSecret: "fixture", RedirectURL: "https://fixture.invalid/callback",
				DefaultRole: "viewer"}
			f := surfaceRouter(t, pool, cfg, nil)
			status, body := surfaceOIDCFlow(t, f)
			meStatus, meBody := f.request(t, "GET", "/api/v1/auth/me", "")
			t.Logf("email_verified=%s callback=%d me=%d identity=%s", mode, status, meStatus, meBody)
			if status != 401 || meStatus != 401 {
				t.Errorf("unverified identity must not receive admin session: callback=%d body=%s me=%d %s",
					status, body, meStatus, meBody)
			}
		})
	}
}

func TestPreflightSurfaceOIDCVerifiedNewUserGetsViewer(t *testing.T) {
	pool := surfacePool(t)
	email := "verified-new-user@fixture.invalid"
	issuer := surfaceOIDCProvider(t, map[string]any{"sub": "verified-subject",
		"email": email, "email_verified": true})
	cfg := config.DefaultConfig()
	cfg.OAuth = config.OAuthConfig{Enabled: true, Provider: "oidc", IssuerURL: issuer,
		ClientID: "fixture", ClientSecret: "fixture", RedirectURL: "https://fixture.invalid/callback",
		DefaultRole: "viewer"}
	f := surfaceRouter(t, pool, cfg, nil)
	status, body := surfaceOIDCFlow(t, f)
	if status != 302 {
		t.Fatalf("verified callback=%d %s", status, body)
	}
	status, body = f.request(t, "GET", "/api/v1/auth/me", "")
	if status != 200 || !strings.Contains(body, `"role":"viewer"`) || !strings.Contains(body, email) {
		t.Fatalf("verified identity=%d %s", status, body)
	}
	t.Log("verified new identity authenticated as viewer through real callback and session middleware")
}
