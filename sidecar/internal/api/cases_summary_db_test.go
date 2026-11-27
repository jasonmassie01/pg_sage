package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// cases-11b CHECK-02 on real PostgreSQL: summary=1&limit=5 returns the
// first five rows of the full global rank, without evidence payloads.
// No concurrent access test here: TestCasesSummaryLimit_ConcurrentRequests
// covers it on the seam; this test pins the SQL-backed projection.

func seedSummaryCases(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	_, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql, status, last_seen)
		SELECT 'stale_statistics', (ARRAY['info','warning','critical'])[g % 3 + 1],
		       'table', 'public.sum_t' || g, 'summary finding ' || g,
		       jsonb_build_object('n_mod_since_analyze', g * 1000), 'analyze it',
		       'ANALYZE public.sum_t' || g, 'open',
		       now() - make_interval(mins => g)
		FROM generate_series(1, 8) g`)
	if err != nil {
		t.Fatalf("seed findings: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.incidents
		(id, detected_at, last_detected_at, severity, root_cause, source, database_name)
		VALUES (gen_random_uuid(), now() - interval '30 minutes', now(), 'critical',
		        'lock chain blocks writers', 'deterministic', 'prod')`)
	if err != nil {
		t.Fatalf("seed incident: %v", err)
	}
}

func getCasesBody(
	t *testing.T, mgr *fleet.DatabaseManager, query string,
) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cases"+query, nil)
	rec := httptest.NewRecorder()
	casesHandler(mgr).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d body %s", query, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", query, err)
	}
	return body
}

func TestCasesSummaryLimit_RealDatabaseTopFive(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	seedSummaryCases(t, ctx)
	mgr := dbCaseManager(t, ctx, pool)
	full := getCasesBody(t, mgr, "?database=prod")
	fullRows := rawCaseRows(t, full)
	if len(fullRows) != 9 || full["total"] != float64(9) {
		t.Fatalf("full rows = %d total = %v, want 9 (8 findings + 1 incident)",
			len(fullRows), full["total"])
	}
	if _, ok := fullRows[0]["evidence"]; !ok {
		t.Fatalf("full row has no evidence: %v", fullRows[0])
	}
	body := getCasesBody(t, mgr, "?database=prod&summary=1&limit=5")
	rows := rawCaseRows(t, body)
	if len(rows) != 5 || body["total"] != float64(9) || body["limit"] != float64(5) {
		t.Fatalf("summary rows = %d total = %v limit = %v, want 5/9/5",
			len(rows), body["total"], body["limit"])
	}
	want := rawIDs(fullRows)[:5]
	if got := rawIDs(rows); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("summary ids = %v\nwant the full rank's first five %v", got, want)
	}
	for i, row := range rows {
		if row["severity"] != "critical" && i < 3 {
			t.Fatalf("row %d severity %v, want the 3 critical cases first", i, row["severity"])
		}
		for _, key := range payloadKeys {
			if _, ok := row[key]; ok {
				t.Fatalf("summary row %v carries %q", row["id"], key)
			}
		}
		if row["database_name"] != "prod" || row["title"] == "" || row["rank_basis"] == "" {
			t.Fatalf("summary row lacks display fields: %v", row)
		}
	}
	if !strings.HasPrefix(rawIDs(rows)[0], "incident:") {
		t.Fatalf("first row %v, want the newest critical case (the incident)", rows[0]["id"])
	}
}
