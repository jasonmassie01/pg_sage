package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

const tenantFixAgentToken = "agt_tenant_fix_token_value_for_agent_a_000000"

type tenantFixture struct {
	st   *agentdb.Store
	ctx  context.Context
	pool *pgxpool.Pool
	mux  *http.ServeMux
}

func newTenantFixture(t *testing.T) *tenantFixture {
	t.Helper()
	st, ctx, pool := requireAgentDBAPIStore(t)
	t.Cleanup(pool.Close)
	f := &tenantFixture{st: st, ctx: ctx, pool: pool, mux: http.NewServeMux()}
	for _, row := range []struct{ tenant, agent, dep string }{
		{"tenant_fix_a", "agent_fix_a", "dep_fix_tenant_a"},
		{"tenant_fix_b", "agent_fix_b", "dep_fix_tenant_b"},
	} {
		cleanupAgentDBTestRows(t, ctx, pool, row.dep)
		if _, err := st.UpsertAgentIdentity(ctx, agentdb.AgentIdentityRequest{
			AgentID: row.agent, TenantID: row.tenant}); err != nil {
			t.Fatalf("identity: %v", err)
		}
		if _, err := st.Register(ctx, agentdb.RegisterRequest{DeploymentID: row.dep,
			TenantID: row.tenant, AgentID: row.agent, IsolationType: "schema"}); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	sum := sha256.Sum256([]byte(tenantFixAgentToken))
	for _, sql := range []string{
		`DELETE FROM sage.agent_db_agent_tokens WHERE token_id='agt_fix_a'`,
		`DELETE FROM sage.agent_db_requests WHERE tenant_id IN ('tenant_fix_a','tenant_fix_b')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("reset: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.agent_db_agent_tokens
		(token_id, tenant_id, agent_id, token_hash, expires_at)
		VALUES ('agt_fix_a', 'tenant_fix_a', 'agent_fix_a', $1, now()+interval '1 hour')`,
		base64.RawURLEncoding.EncodeToString(sum[:])); err != nil {
		t.Fatalf("seed agent token: %v", err)
	}
	registerAgentDBRoutesWithAuthority(f.mux, st, nil, nil)
	return f
}

func (f *tenantFixture) agentRequest(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+tenantFixAgentToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.mux.ServeHTTP(rr, req)
	return rr
}

// G8-B05: an agent token only ever sees its own tenant.
func TestAgentPrincipalCannotReadOtherTenant(t *testing.T) {
	f := newTenantFixture(t)
	list := f.agentRequest(http.MethodGet, "/api/v1/agent-api/agent-dbs", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), "dep_fix_tenant_a") ||
		strings.Contains(list.Body.String(), "dep_fix_tenant_b") {
		t.Fatalf("agent list leaked or missed tenants: %s", list.Body.String())
	}
	own := f.agentRequest(http.MethodGet, "/api/v1/agent-api/agent-dbs/dep_fix_tenant_a", "")
	if own.Code != http.StatusOK {
		t.Fatalf("own get status = %d", own.Code)
	}
	other := f.agentRequest(http.MethodGet, "/api/v1/agent-api/agent-dbs/dep_fix_tenant_b", "")
	if other.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get status = %d, want 404", other.Code)
	}
}

// G8-B05: tenant and agent come from the token, never from the body.
func TestAgentPrincipalRequestTenantComesFromToken(t *testing.T) {
	f := newTenantFixture(t)
	rr := f.agentRequest(http.MethodPost, "/api/v1/agent-api/agent-db-requests",
		`{"tenant_id":"tenant_fix_b","agent_id":"agent_fix_b",
		"requested_isolation_type":"schema","idempotency_key":"fix-tenant-1"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", rr.Code, rr.Body.String())
	}
	var created agentdb.Request
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.TenantID != "tenant_fix_a" || created.AgentID != "agent_fix_a" {
		t.Fatalf("request owner = %s/%s, want token owner", created.TenantID, created.AgentID)
	}
}

// G8-B05: agent tokens cannot mint, approve or destroy; bad tokens are 401.
func TestAgentPrincipalDeniedManagementAndBadTokens(t *testing.T) {
	f := newTenantFixture(t)
	for _, path := range []string{
		"/api/v1/agent-dbs/dep_fix_tenant_b/ping-tokens",
		"/api/v1/agent-dbs/dep_fix_tenant_b/provision/destroy-live",
		"/api/v1/agent-dbs/requests/anything/approve",
	} {
		if rr := f.agentRequest(http.MethodPost, path, `{}`); rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s with agent token = %d, want 401", path, rr.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent-api/agent-dbs", nil)
	req.Header.Set("Authorization", "Bearer agt_wrong")
	rr := httptest.NewRecorder()
	f.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("invalid agent token = %d, want 401", rr.Code)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.agent_db_agent_tokens
		SET status='revoked' WHERE token_id='agt_fix_a'`); err != nil {
		t.Fatal(err)
	}
	if rr := f.agentRequest(http.MethodGet, "/api/v1/agent-api/agent-dbs", ""); rr.Code !=
		http.StatusUnauthorized {
		t.Fatalf("revoked agent token = %d, want 401", rr.Code)
	}
	if !shouldSkipAuth("/api/v1/agent-api/agent-dbs") {
		t.Fatal("agent API paths must bypass session auth (they use agent tokens)")
	}
}

// G8-B05: an admin mints an agent token; tenant comes from the identity row.
func TestAdminMintsAgentTokenBoundToIdentityTenant(t *testing.T) {
	f := newTenantFixture(t)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agent-dbs/identities/agent_fix_a/tokens",
		bytes.NewReader([]byte(`{"tenant_id":"tenant_fix_b","expires_seconds":600}`)))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey,
		&auth.User{ID: 7, Email: "admin@example.com", Role: "admin"}))
	rr := httptest.NewRecorder()
	f.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("mint status = %d body=%s", rr.Code, rr.Body.String())
	}
	var minted map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	if minted["tenant_id"] != "tenant_fix_a" || minted["token"] == "" {
		t.Fatalf("minted token = %#v", minted)
	}
	_, _ = f.pool.Exec(f.ctx, `DELETE FROM sage.agent_db_agent_tokens
		WHERE agent_id='agent_fix_a' AND token_id <> 'agt_fix_a'`)
}

// G8-B05: allowed_regions is server policy, never caller-supplied.
func TestRequestAllowedRegionsIgnoredFromBody(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	defer pool.Close()
	_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_requests WHERE tenant_id='tenant_fix_region'")
	authority := newAgentDBLiveAuthority(config.AgentDBConfig{
		Providers: map[string]config.AgentDBProviderConfig{
			agentdb.ProviderAWSRDS: {Enabled: true, AllowedRegions: []string{"us-east-1"}},
		},
	})
	handler := agentDBSubrouterWithRegistry(st, agentdb.DefaultRunnerRegistry(), nil, authority)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-dbs/requests",
		bytes.NewReader([]byte(`{"tenant_id":"tenant_fix_region","agent_id":"agent_region",
		"requested_isolation_type":"instance","provider":"aws_rds","budget_usd":5,
		"region":"eu-west-9","allowed_regions":["eu-west-9"]}`)))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var created agentdb.Request
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.PolicyDecision != "deny" {
		t.Fatalf("caller-supplied allowlist widened policy: decision=%s", created.PolicyDecision)
	}
}
