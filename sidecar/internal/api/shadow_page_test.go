package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// earned-04: the Shadow Mode page reads the shadow ledger
// (GET /api/v1/shadow-decisions) and the heuristic report over open
// cases is gone.

// retiredShadowPath is the removed heuristic report, spelled in two parts
// so a repository grep for the old name only finds live references.
const retiredShadowPath = "/api/v1/shadow-" + "report"

func TestRetiredShadowReportIsGone(t *testing.T) {
	r := testRouter("db1")
	for _, path := range []string{retiredShadowPath, retiredShadowPath + "?database=db1"} {
		if w := get(t, r, path); w.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d (%s), want 404", path, w.Code, w.Body.String())
		}
	}
	if w := get(t, r, "/api/v1/cases"); w.Code != http.StatusOK {
		t.Fatalf("cases route lost with the report: %d", w.Code)
	}
}

// Invalid input: a well-formed class nobody recorded is an empty summary
// and an empty list, not a 500 and not the other classes' summary.
func TestShadowDecisionsUnknownClassIsAnEmptySummary(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanShadows(t, pool, ctx)
	seedShadow(t, pool, ctx, "index_create", `CREATE INDEX CONCURRENTLY b1 ON public.o (a)`,
		"correct")
	code, body := getShadows(t, pool, "?database=testdb&class=bogus")
	if code != http.StatusOK {
		t.Fatalf("class=bogus status %d: %v", code, body)
	}
	db := body["databases"].([]any)[0].(map[string]any)
	if s, ok := db["summary"].([]any); !ok || len(s) != 0 {
		t.Fatalf("class=bogus summary = %v, want []", db["summary"])
	}
	if d, ok := db["decisions"].([]any); !ok || len(d) != 0 {
		t.Fatalf("class=bogus decisions = %v, want []", db["decisions"])
	}
	_, body = getShadows(t, pool, "?database=testdb&class=index_create")
	db = body["databases"].([]any)[0].(map[string]any)
	if s := db["summary"].([]any); len(s) != 1 {
		t.Fatalf("class=index_create summary = %v, want its one row", s)
	}
}

// Happy path: the summary row carries potential minutes, labelled as
// such; an unmodelled class carries null, never a default.
func TestShadowDecisionsServePotentialMinutes(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanShadows(t, pool, ctx)
	for _, col := range []string{"a", "b"} {
		seedShadow(t, pool, ctx, "index_create",
			`CREATE INDEX CONCURRENTLY p_`+col+` ON public.o (`+col+`)`, "correct")
	}
	id := seedShadow(t, pool, ctx, "vacuum", `VACUUM public.p`, "correct")
	if _, err := pool.Exec(ctx, `UPDATE sage.shadow_decision
		SET action_class = 'unclassified' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var base float64
	if err := pool.QueryRow(ctx, `SELECT base_minutes::float8 FROM sage.toil_model
		WHERE action_type = 'create_index_concurrently' AND effective_to IS NULL`).
		Scan(&base); err != nil {
		t.Fatal(err)
	}
	_, body := getShadows(t, pool, "?database=testdb")
	rows := map[string]map[string]any{}
	for _, s := range body["databases"].([]any)[0].(map[string]any)["summary"].([]any) {
		row := s.(map[string]any)
		rows[row["class"].(string)] = row
	}
	if got := rows["index_create"]["potential_minutes"]; got != 2*base {
		t.Fatalf("index_create potential_minutes = %v, want %v", got, 2*base)
	}
	un, ok := rows["unclassified"]
	if !ok {
		t.Fatalf("no unclassified row: %v", rows)
	}
	if v, present := un["potential_minutes"]; !present || v != nil {
		t.Fatalf("unclassified potential_minutes = %v (present %v), want null", v, present)
	}
}

// Role: every signed-in role reads the page's data through the router
// (the retired report was operator only).
func TestShadowModePageDataIsReadableByAViewer(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanShadows(t, pool, ctx)
	mux := http.NewServeMux()
	registerShadowRoutes(mux, phase2MgrWithPool(pool))
	req := httptest.NewRequest("GET", "/api/v1/shadow-decisions?database=testdb", nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey,
		&auth.User{ID: 9, Email: "viewer@example.com", Role: "viewer"}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("viewer: %d (%s), want 200", w.Code, w.Body.String())
	}
}
