package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestIncidentResolveHandlerScopesToRequestedDatabase(t *testing.T) {
	adminPool, ctx := phase2RequireDB(t)

	alpha := phase2TempIncidentDB(t, ctx, adminPool, "alpha")
	bravo := phase2TempIncidentDB(t, ctx, adminPool, "bravo")

	incidentID := "11111111-1111-1111-1111-111111111111"
	seedIncidentWithID(t, ctx, alpha.pool, incidentID, "alpha")
	seedIncidentWithID(t, ctx, bravo.pool, incidentID, "bravo")

	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "alpha",
		Pool: alpha.pool,
	})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "bravo",
		Pool: bravo.pool,
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/incidents/"+incidentID+"/resolve?database=bravo",
		strings.NewReader(`{"reason":"fixed"}`),
	)
	req.SetPathValue("id", incidentID)
	rec := httptest.NewRecorder()

	incidentResolveHandler(mgr).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 body=%s",
			rec.Code, rec.Body.String())
	}

	var resp struct {
		Database string `json:"database"`
		Status   string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Database != "bravo" || resp.Status != "resolved" {
		t.Fatalf("response: got database=%q status=%q",
			resp.Database, resp.Status)
	}

	if resolvedAt := incidentResolvedAt(t, ctx, bravo.pool, incidentID); resolvedAt == nil {
		t.Fatal("bravo incident was not resolved")
	}
	if resolvedAt := incidentResolvedAt(t, ctx, alpha.pool, incidentID); resolvedAt != nil {
		t.Fatalf("alpha incident should remain unresolved, got %v",
			*resolvedAt)
	}
}

func TestIncidentResolveHandlerRequiresDatabaseForFleet(t *testing.T) {
	adminPool, ctx := phase2RequireDB(t)

	alpha := phase2TempIncidentDB(t, ctx, adminPool, "needdb_a")
	bravo := phase2TempIncidentDB(t, ctx, adminPool, "needdb_b")

	incidentID := "22222222-2222-2222-2222-222222222222"
	seedIncidentWithID(t, ctx, alpha.pool, incidentID, "needdb_a")
	seedIncidentWithID(t, ctx, bravo.pool, incidentID, "needdb_b")

	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "needdb_a",
		Pool: alpha.pool,
	})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "needdb_b",
		Pool: bravo.pool,
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/incidents/"+incidentID+"/resolve",
		strings.NewReader(`{"reason":"fixed"}`),
	)
	req.SetPathValue("id", incidentID)
	rec := httptest.NewRecorder()

	incidentResolveHandler(mgr).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 body=%s",
			rec.Code, rec.Body.String())
	}

	if resolvedAt := incidentResolvedAt(t, ctx, alpha.pool, incidentID); resolvedAt != nil {
		t.Fatalf("alpha incident should remain unresolved, got %v",
			*resolvedAt)
	}
	if resolvedAt := incidentResolvedAt(t, ctx, bravo.pool, incidentID); resolvedAt != nil {
		t.Fatalf("bravo incident should remain unresolved, got %v",
			*resolvedAt)
	}
}

func TestIncidentDetailHandlerScopesToRequestedDatabase(t *testing.T) {
	adminPool, ctx := phase2RequireDB(t)

	alpha := phase2TempIncidentDB(t, ctx, adminPool, "detail_alpha")
	bravo := phase2TempIncidentDB(t, ctx, adminPool, "detail_bravo")

	incidentID := "44444444-4444-4444-4444-444444444444"
	seedIncidentWithID(t, ctx, alpha.pool, incidentID, alpha.name)
	seedIncidentWithID(t, ctx, bravo.pool, incidentID, bravo.name)

	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "friendly_alpha",
		Pool: alpha.pool,
	})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "friendly_bravo",
		Pool: bravo.pool,
	})

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/incidents/"+incidentID+"?database=friendly_bravo",
		nil,
	)
	req.SetPathValue("id", incidentID)
	rec := httptest.NewRecorder()

	incidentDetailHandler(mgr).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 body=%s",
			rec.Code, rec.Body.String())
	}

	var incident map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&incident); err != nil {
		t.Fatalf("decode incident detail: %v", err)
	}
	if incident["database_name"] != bravo.name {
		t.Fatalf("database_name = %v, want %q",
			incident["database_name"], bravo.name)
	}
	if incident["fleet_database_name"] != "friendly_bravo" {
		t.Fatalf("fleet_database_name = %v, want friendly_bravo",
			incident["fleet_database_name"])
	}
}

func TestIncidentDetailHandlerRequiresDatabaseForFleet(t *testing.T) {
	adminPool, ctx := phase2RequireDB(t)

	alpha := phase2TempIncidentDB(t, ctx, adminPool, "detail_needdb_a")
	bravo := phase2TempIncidentDB(t, ctx, adminPool, "detail_needdb_b")

	incidentID := "55555555-5555-5555-5555-555555555555"
	seedIncidentWithID(t, ctx, alpha.pool, incidentID, alpha.name)
	seedIncidentWithID(t, ctx, bravo.pool, incidentID, bravo.name)

	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "detail_needdb_a",
		Pool: alpha.pool,
	})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "detail_needdb_b",
		Pool: bravo.pool,
	})

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/incidents/"+incidentID,
		nil,
	)
	req.SetPathValue("id", incidentID)
	rec := httptest.NewRecorder()

	incidentDetailHandler(mgr).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 body=%s",
			rec.Code, rec.Body.String())
	}
}

func TestIncidentsListUsesFleetAliasAsResolveTarget(t *testing.T) {
	adminPool, ctx := phase2RequireDB(t)

	alpha := phase2TempIncidentDB(t, ctx, adminPool, "alias_target")
	incidentID := "33333333-3333-3333-3333-333333333333"
	seedIncidentWithID(t, ctx, alpha.pool, incidentID, alpha.name)

	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "friendly_alias",
		Pool: alpha.pool,
	})

	listReq := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/incidents?database=friendly_alias",
		nil,
	)
	listRec := httptest.NewRecorder()
	incidentsListHandler(mgr).ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status: got %d want 200 body=%s",
			listRec.Code, listRec.Body.String())
	}

	var listResp struct {
		Incidents []map[string]any `json:"incidents"`
		Total     int              `json:"total"`
	}
	if err := json.NewDecoder(listRec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listResp.Total != 1 || len(listResp.Incidents) != 1 {
		t.Fatalf("incidents: total=%d len=%d body=%s",
			listResp.Total, len(listResp.Incidents),
			listRec.Body.String())
	}
	row := listResp.Incidents[0]
	if row["database_name"] != alpha.name {
		t.Fatalf("database_name = %v, want physical name %q",
			row["database_name"], alpha.name)
	}
	if row["fleet_database_name"] != "friendly_alias" {
		t.Fatalf("fleet_database_name = %v, want friendly_alias",
			row["fleet_database_name"])
	}

	resolveReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/incidents/"+incidentID+
			"/resolve?database=friendly_alias",
		strings.NewReader(`{"reason":"fixed"}`),
	)
	resolveReq.SetPathValue("id", incidentID)
	resolveRec := httptest.NewRecorder()
	incidentResolveHandler(mgr).ServeHTTP(resolveRec, resolveReq)
	if resolveRec.Code != http.StatusOK {
		t.Fatalf("resolve status: got %d want 200 body=%s",
			resolveRec.Code, resolveRec.Body.String())
	}
}

type tempIncidentDB struct {
	name string
	pool *pgxpool.Pool
}

func phase2TempIncidentDB(
	t *testing.T,
	ctx context.Context,
	adminPool *pgxpool.Pool,
	suffix string,
) tempIncidentDB {
	t.Helper()

	name := fmt.Sprintf("pg_sage_incident_%s_%d",
		suffix, time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Skipf("create temp database %s: %v", name, err)
	}

	pool := phase2PoolForDatabase(t, ctx, name)
	if err := createIncidentOnlySchema(ctx, pool); err != nil {
		pool.Close()
		dropPhase2TempDB(t, ctx, adminPool, name)
		t.Fatalf("create incident schema in %s: %v", name, err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropPhase2TempDB(t, context.Background(), adminPool, name)
	})
	return tempIncidentDB{name: name, pool: pool}
}

func phase2PoolForDatabase(
	t *testing.T, ctx context.Context, database string,
) *pgxpool.Pool {
	t.Helper()

	cfg, err := pgxpool.ParseConfig(phase2DSN())
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.Database = database
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect temp database %s: %v", database, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping temp database %s: %v", database, err)
	}
	return pool
}

func createIncidentOnlySchema(
	ctx context.Context, pool *pgxpool.Pool,
) error {
	_, err := pool.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS sage;
		CREATE TABLE IF NOT EXISTS sage.incidents (
			id uuid PRIMARY KEY,
			detected_at timestamptz NOT NULL DEFAULT now(),
			last_detected_at timestamptz,
			severity text NOT NULL,
			root_cause text NOT NULL,
			causal_chain jsonb NOT NULL DEFAULT '[]'::jsonb,
			affected_objects text[] NOT NULL DEFAULT '{}',
			signal_ids text[] NOT NULL DEFAULT '{}',
			recommended_sql text,
			action_risk text,
			source text NOT NULL,
			confidence double precision NOT NULL DEFAULT 1.0,
			resolved_at timestamptz,
			database_name text,
			occurrence_count integer NOT NULL DEFAULT 1,
			escalated_at timestamptz
		)`)
	return err
}

func dropPhase2TempDB(
	t *testing.T,
	ctx context.Context,
	adminPool *pgxpool.Pool,
	name string,
) {
	t.Helper()

	_, _ = adminPool.Exec(ctx,
		`SELECT pg_terminate_backend(pid)
		 FROM pg_stat_activity
		 WHERE datname = $1 AND pid <> pg_backend_pid()`,
		name,
	)
	if _, err := adminPool.Exec(ctx,
		"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize(),
	); err != nil {
		t.Logf("drop temp database %s: %v", name, err)
	}
}

func seedIncidentWithID(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	id, dbName string,
) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO sage.incidents
		 (id, severity, root_cause, source, database_name)
		 VALUES ($1, 'warning', 'scoped incident',
		         'deterministic', $2)`,
		id, dbName,
	)
	if err != nil {
		t.Fatalf("seed incident %s in %s: %v", id, dbName, err)
	}
}

func incidentResolvedAt(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	id string,
) *time.Time {
	t.Helper()
	var resolvedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT resolved_at FROM sage.incidents WHERE id = $1`,
		id,
	).Scan(&resolvedAt); err != nil {
		t.Fatalf("query resolved_at for %s: %v", id, err)
	}
	return resolvedAt
}
