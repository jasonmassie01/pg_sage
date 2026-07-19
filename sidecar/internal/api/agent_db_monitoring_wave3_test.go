package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentdb"
)

type wave3AgentDBListResponse struct {
	Deployments []agentdb.Deployment `json:"deployments"`
	NextCursor  string               `json:"next_cursor"`
}

func TestWave3AgentDBListPaginationAndFiltering(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	t.Cleanup(pool.Close)
	const tenant = "wave3_api_filter_tenant"
	cleanupWave3APIListFixtures(t, ctx, pool, tenant)
	seedWave3APIListFixtures(t, ctx, st, tenant)

	query := url.Values{
		"tenant_id": {tenant},
		"provider":  {agentdb.ProviderAWSRDS},
		"status":    {"active"},
		"limit":     {"2"},
	}
	first := requestWave3AgentDBPage(t, st, query)
	if len(first.Deployments) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %#v, want two rows and a cursor", first)
	}
	assertWave3APIFilters(t, first.Deployments, tenant)

	query.Set("cursor", first.NextCursor)
	second := requestWave3AgentDBPage(t, st, query)
	if len(second.Deployments) != 1 || second.NextCursor != "" {
		t.Fatalf("second page = %#v, want final single row", second)
	}
	assertWave3APIFilters(t, second.Deployments, tenant)
	assertWave3NoDuplicatePages(t, first.Deployments, second.Deployments)
}

func TestWave3AgentDBListBoundsTenThousandRowPayload(t *testing.T) {
	st, ctx, pool := requireAgentDBAPIStore(t)
	t.Cleanup(pool.Close)
	const tenant = "wave3_api_10k_tenant"
	cleanupWave3APIListFixtures(t, ctx, pool, tenant)
	seedWave3APITenThousandRows(t, ctx, pool, tenant)

	query := url.Values{"tenant_id": {tenant}}
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agent-dbs?"+query.Encode(), nil)
	rec := httptest.NewRecorder()
	agentDBListHandler(st).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var page wave3AgentDBListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode 10k page: %v body=%s", err, rec.Body.String())
	}
	if len(page.Deployments) == 0 || len(page.Deployments) > 100 {
		t.Fatalf("default page returned %d rows, want 1..100", len(page.Deployments))
	}
	if page.NextCursor == "" {
		t.Fatal("10k-row result omitted next_cursor")
	}
	if rec.Body.Len() > 256*1024 {
		t.Fatalf("bounded page payload = %d bytes, want <=256KiB", rec.Body.Len())
	}
}

func requestWave3AgentDBPage(t *testing.T, st *agentdb.Store,
	query url.Values) wave3AgentDBListResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agent-dbs?"+query.Encode(), nil)
	rec := httptest.NewRecorder()
	agentDBListHandler(st).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var page wave3AgentDBListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v body=%s", err, rec.Body.String())
	}
	return page
}

func seedWave3APIListFixtures(t *testing.T, ctx context.Context,
	st *agentdb.Store, tenant string) {
	t.Helper()
	for i := 0; i < 5; i++ {
		provider := agentdb.ProviderAWSRDS
		if i == 3 {
			provider = agentdb.ProviderGCPCloudSQL
		}
		id := fmt.Sprintf("wave3_api_filter_%d", i)
		_, err := st.Register(ctx, agentdb.RegisterRequest{
			DeploymentID:       id,
			TenantID:           tenant,
			AgentID:            "agent-" + id,
			DatabaseName:       "app",
			IsolationType:      agentdb.LevelInstance,
			SchemaName:         fmt.Sprintf("schema_%d", i),
			Provider:           provider,
			ProvisioningLevel:  agentdb.LevelInstance,
			ProvisioningStatus: "available",
			LeaseSeconds:       3600,
			BackupRequired:     false,
		})
		if err != nil {
			t.Fatalf("register API fixture %s: %v", id, err)
		}
	}
	if _, err := st.Archive(ctx, "wave3_api_filter_4"); err != nil {
		t.Fatalf("archive nonmatching fixture: %v", err)
	}
}

func seedWave3APITenThousandRows(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool, tenant string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO sage.agent_db_deployments
		(deployment_id, tenant_id, agent_id, database_name, status, safety_mode,
		 isolation_type, schema_name, provider, provisioning_level,
		 provisioning_status, backup_required, lease_expires_at, metadata)
		SELECT 'wave3_api_10k_'||g, $1, 'agent-'||g, 'app', 'active',
		 'observation', 'schema', 'schema_'||g, 'aws_rds', 'schema',
		 'available', false, now()+interval '1 hour', '{}'::jsonb
		FROM generate_series(1, 10000) AS g`, tenant)
	if err != nil {
		t.Fatalf("seed API 10k rows: %v", err)
	}
}

func cleanupWave3APIListFixtures(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool, tenant string) {
	t.Helper()
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.agent_db_monitoring_work
			WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.agent_db_deployments
			WHERE tenant_id=$1`, tenant)
	}
	cleanup()
	t.Cleanup(cleanup)
}

func assertWave3APIFilters(t *testing.T, deployments []agentdb.Deployment,
	tenant string) {
	t.Helper()
	for _, dep := range deployments {
		if dep.TenantID != tenant || dep.Provider != agentdb.ProviderAWSRDS ||
			dep.Status != "active" {
			t.Fatalf("unfiltered deployment returned: %#v", dep)
		}
	}
}

func assertWave3NoDuplicatePages(t *testing.T, pages ...[]agentdb.Deployment) {
	t.Helper()
	seen := map[string]bool{}
	for _, page := range pages {
		for _, dep := range page {
			if seen[dep.DeploymentID] {
				t.Fatalf("deployment %q repeated across cursor pages", dep.DeploymentID)
			}
			seen[dep.DeploymentID] = true
		}
	}
	if len(seen) != 3 {
		t.Fatalf("filtered pagination returned %d unique rows, want 3", len(seen))
	}
}
