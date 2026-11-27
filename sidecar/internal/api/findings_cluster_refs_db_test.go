package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// forecast-09: a fleet list of a non-owner's findings carries its
// cluster owner's open cluster findings under cluster_refs, read-only,
// and never in its own page or total.

func clusterRefsManager(pools map[string]*pgxpool.Pool) *fleet.DatabaseManager {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet",
		Trust: config.TrustConfig{Level: "advisory"}})
	for name, pool := range pools {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
			Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Connected: true, PGVersion: "17",
				TrustLevel: "advisory"}})
	}
	return mgr
}

// seedClusterFinding opens a finding in pool with detail.
func seedClusterFinding(t *testing.T, pool *pgxpool.Pool, ident, status string,
	detail map[string]any) {
	t.Helper()
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, status)
		VALUES ('forecast_wal_runway', 'warning', 'replication_slot', $1, 'slot fills',
		$2::jsonb, 'investigate', $3)`, ident, string(raw), status); err != nil {
		t.Fatalf("seed %s: %v", ident, err)
	}
}

func listFor(t *testing.T, mgr *fleet.DatabaseManager, database string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	findingsListHandler(mgr)(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/findings?database="+database, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list %s: %d %s", database, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestClusterRefs_NonOwnerListsTheOwnersOpenClusterFindings(t *testing.T) {
	alpha, beta := auditPool(t, "rw09_alpha"), auditPool(t, "rw09_beta")
	members := []string{"alpha", "beta"}
	seedClusterFinding(t, alpha, "slot:fill", "open", map[string]any{"scope": "cluster",
		"owner_database": "alpha", "cluster_databases": members})
	seedClusterFinding(t, alpha, "slot:old", "resolved", map[string]any{
		"scope": "cluster", "owner_database": "alpha", "cluster_databases": members})
	seedClusterFinding(t, alpha, "slot:other", "open", map[string]any{
		"scope": "cluster", "owner_database": "alpha", "cluster_databases": []string{"alpha"}})
	seedClusterFinding(t, alpha, "slot:plain", "open", map[string]any{"subject": "x"})
	seedClusterFinding(t, alpha, "slot:bad", "open", map[string]any{"scope": "cluster",
		"owner_database": "alpha", "cluster_databases": "beta"})
	mgr := clusterRefsManager(map[string]*pgxpool.Pool{"alpha": alpha, "beta": beta})

	body := listFor(t, mgr, "beta")
	refs, _ := body["cluster_refs"].([]any)
	if len(refs) != 1 {
		t.Fatalf("beta cluster_refs = %v, want the one open slot:fill", body["cluster_refs"])
	}
	ref := refs[0].(map[string]any)
	if ref["object_identifier"] != "slot:fill" || ref["cluster_ref"] != true ||
		ref["read_only"] != true || ref["database_name"] != "alpha" {
		t.Fatalf("ref = %v, want alpha's slot:fill marked read-only", ref)
	}
	if total := body["total"]; total != float64(0) {
		t.Fatalf("beta total = %v, want its own findings only (0)", total)
	}
	if refs, _ := listFor(t, mgr, "alpha")["cluster_refs"].([]any); len(refs) != 0 {
		t.Fatalf("the owner lists its own findings as references: %v", refs)
	}
	if refs, _ := listFor(t, mgr, "all")["cluster_refs"].([]any); len(refs) != 0 {
		t.Fatalf("an all-database list carries references: %v", refs)
	}
}

// Nil/empty: one database, or no manager, adds no references.
func TestClusterRefs_SingleDatabaseHasNone(t *testing.T) {
	if refs := clusterRefs(context.Background(), nil, "beta"); refs == nil ||
		len(refs) != 0 {
		t.Fatalf("nil manager refs = %v, want an empty list", refs)
	}
	mgr := clusterRefsManager(map[string]*pgxpool.Pool{"beta": nil})
	if refs := clusterRefs(context.Background(), mgr, "beta"); len(refs) != 0 {
		t.Fatalf("single database refs = %v", refs)
	}
}

// Error propagation: an unreadable owner is skipped, the list still serves.
func TestClusterRefs_UnreadableOwnerIsSkipped(t *testing.T) {
	beta := auditPool(t, "rw09_beta_only")
	gone := auditPool(t, "rw09_gone")
	gone.Close()
	mgr := clusterRefsManager(map[string]*pgxpool.Pool{"alpha": gone, "beta": beta})
	body := listFor(t, mgr, "beta")
	if refs, ok := body["cluster_refs"].([]any); !ok || len(refs) != 0 {
		t.Fatalf("cluster_refs = %v, want an empty list despite the closed owner",
			body["cluster_refs"])
	}
}
