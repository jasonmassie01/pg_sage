package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Roadmap 1.4: GET /api/v1/shadow-decisions serves, per database, what
// pg_sage would have done below each class's earned level (SQL,
// prediction, the verdict had it been trusted) and how those decisions
// scored: per class counts and the newest decisions, filterable.

func seedShadow(t *testing.T, pool *pgxpool.Pool, ctx context.Context, class, sql,
	score string) int64 {
	t.Helper()
	family, _ := shadow.ClassOf(map[string]string{
		"index_create": "create_index_concurrently", "vacuum": "vacuum_table"}[class], sql)
	expected := -35.0
	d, ok, err := shadow.NewStore(pool).Record(ctx, shadow.Decision{Database: "testdb",
		Fingerprint: shadow.Fingerprint(class, "public.o", shadow.Shape(sql)),
		Family:      family, Class: class, Title: "shadow " + class, Object: "public.o",
		SQL: sql, Shape: shadow.Shape(sql),
		Prediction: verify.Prediction{Class: class, Metric: verify.MetricMeanExecTime,
			Method: verify.MethodModel, ExpectedChangePct: &expected},
		GateVerdict: "observe_only", GateReason: "autonomy_level", TrustedVerdict: "execute",
		TrustedReason: "autonomy_l3", GrantedLevel: 1})
	if err != nil || !ok {
		t.Fatalf("record shadow: %v %v", ok, err)
	}
	if score != "" {
		if _, err := pool.Exec(ctx, `UPDATE sage.shadow_decision SET status = 'scored',
			score = $2, score_source = 'hypopg', counted = true, scored_at = now()
			WHERE id = $1`, d.ID, score); err != nil {
			t.Fatal(err)
		}
	}
	return d.ID
}

func cleanShadows(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	clean := func() { _, _ = pool.Exec(context.Background(), "DELETE FROM sage.shadow_decision") }
	clean()
	t.Cleanup(clean)
}

func getShadows(t *testing.T, pool *pgxpool.Pool, query string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	shadowDecisionsHandler(phase2MgrWithPool(pool)).ServeHTTP(w,
		httptest.NewRequest("GET", "/api/v1/shadow-decisions"+query, nil))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func TestShadowDecisionsServesSummaryAndDecisions(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanShadows(t, pool, ctx)
	seedShadow(t, pool, ctx, "index_create", `CREATE INDEX CONCURRENTLY s1 ON public.o (a)`,
		"correct")
	seedShadow(t, pool, ctx, "index_create", `CREATE INDEX CONCURRENTLY s2 ON public.o (b)`, "")
	seedShadow(t, pool, ctx, "vacuum", `VACUUM public.o`, "incorrect")
	code, body := getShadows(t, pool, "?database=testdb")
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if body["meaning"] == "" {
		t.Fatal("no meaning")
	}
	dbs, _ := body["databases"].([]any)
	if len(dbs) != 1 {
		t.Fatalf("databases %v", body["databases"])
	}
	db := dbs[0].(map[string]any)
	if db["database"] != "testdb" {
		t.Fatalf("database %v", db["database"])
	}
	summary, _ := db["summary"].([]any)
	byClass := map[string]map[string]any{}
	for _, s := range summary {
		row := s.(map[string]any)
		byClass[row["class"].(string)] = row
	}
	if idx := byClass["index_create"]; idx == nil || idx["total"] != 2.0 ||
		idx["correct"] != 1.0 || idx["pending"] != 1.0 || idx["family"] != "tuning" {
		t.Fatalf("index_create summary %v", byClass["index_create"])
	}
	if v := byClass["vacuum"]; v == nil || v["incorrect"] != 1.0 {
		t.Fatalf("vacuum summary %v", byClass["vacuum"])
	}
	decisions, _ := db["decisions"].([]any)
	if len(decisions) != 3 {
		t.Fatalf("decisions %d", len(decisions))
	}
	first := decisions[0].(map[string]any)
	pred, _ := first["prediction"].(map[string]any)
	if first["sql"] == "" || first["trusted_verdict"] != "execute" || pred == nil ||
		pred["expected_change_pct"] != -35.0 {
		t.Fatalf("decision %v", first)
	}
}

func TestShadowDecisionsFilters(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanShadows(t, pool, ctx)
	seedShadow(t, pool, ctx, "index_create", `CREATE INDEX CONCURRENTLY f1 ON public.o (a)`,
		"correct")
	seedShadow(t, pool, ctx, "vacuum", `VACUUM public.o`, "")
	_, body := getShadows(t, pool, "?database=testdb&class=vacuum")
	db := body["databases"].([]any)[0].(map[string]any)
	if d := db["decisions"].([]any); len(d) != 1 || d[0].(map[string]any)["class"] != "vacuum" {
		t.Fatalf("class filter: %v", d)
	}
	_, body = getShadows(t, pool, "?database=testdb&score=correct&limit=1")
	db = body["databases"].([]any)[0].(map[string]any)
	if d := db["decisions"].([]any); len(d) != 1 || d[0].(map[string]any)["score"] != "correct" {
		t.Fatalf("score filter: %v", d)
	}
	// The summary is the whole ledger whatever the list filter.
	if s := db["summary"].([]any); len(s) != 2 {
		t.Fatalf("summary under a filter: %v", s)
	}
}

func TestShadowDecisionsRejectsBadInput(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanShadows(t, pool, ctx)
	for _, q := range []string{"?limit=0", "?limit=abc", "?limit=1001", "?score=great",
		"?status=done", "?class=DROP%20TABLE"} {
		if code, body := getShadows(t, pool, q); code != http.StatusBadRequest {
			t.Errorf("%s: status %d (%v), want 400", q, code, body)
		}
	}
	if code, _ := getShadows(t, pool, "?database=nope"); code != http.StatusNotFound {
		t.Fatalf("unknown database: %d, want 404", code)
	}
}

func TestShadowDecisionsAllDatabasesAndEmpty(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanShadows(t, pool, ctx)
	code, body := getShadows(t, pool, "")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	dbs := body["databases"].([]any)
	if len(dbs) != 1 {
		t.Fatalf("databases %v", dbs)
	}
	db := dbs[0].(map[string]any)
	if s, _ := db["summary"].([]any); len(s) != 0 {
		t.Fatalf("empty ledger summary %v", s)
	}
	if d, ok := db["decisions"].([]any); !ok || len(d) != 0 {
		t.Fatalf("empty ledger decisions must be [] not null: %v", db["decisions"])
	}
}

func TestShadowRoutesRequireASignedInUser(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	mux := http.NewServeMux()
	registerShadowRoutes(mux, phase2MgrWithPool(pool))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/shadow-decisions", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d, want 401", w.Code)
	}
	for _, role := range []string{"viewer", "operator", "admin"} {
		w = httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v1/shadow-decisions", nil)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey,
			&auth.User{ID: 1, Email: role + "@example.com", Role: role}))
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d, want 200", role, w.Code)
		}
	}
}

// The Trust page reads one endpoint: GET /api/v1/trust carries each
// database's shadow summary per class next to its ledger rows.
func TestTrustAPICarriesTheShadowSummary(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	inst := f.mgr.GetInstance("orders")
	if inst == nil || inst.Pool == nil {
		t.Fatal("fixture has no orders pool")
	}
	ctx := context.Background()
	cleanShadows(t, inst.Pool, ctx)
	seedShadow(t, inst.Pool, ctx, "vacuum", `VACUUM public.o`, "correct")
	code, body := autonomyCall(t, f.router(testViewerUser()), "GET",
		"/api/v1/trust?database=orders", "")
	dbs, _ := body["databases"].([]any)
	if code != 200 || len(dbs) != 1 {
		t.Fatalf("trust = %d %v", code, body)
	}
	db := dbs[0].(map[string]any)
	if rows, _ := db["rows"].([]any); len(rows) == 0 {
		t.Fatalf("ledger rows lost: %v", db)
	}
	list, _ := db["shadow"].([]any)
	if len(list) != 1 {
		t.Fatalf("shadow summary = %v", db["shadow"])
	}
	s := list[0].(map[string]any)
	if s["class"] != "vacuum" || s["family"] != "hygiene" || s["total"] != 1.0 ||
		s["correct"] != 1.0 {
		t.Fatalf("shadow summary row = %v", s)
	}
}
