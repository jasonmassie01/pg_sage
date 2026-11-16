package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// evid-02a: the workload evidence pack API. Building is an operator write
// on the database in the path; listing, reading a pack and resolving one
// evidence id are viewer reads scoped to that database.
// No concurrency test here: concurrent builds of one window are covered in
// workpack's build_db_test.go (one row, the second reused).

const packsPath = "/api/v1/databases/%s/workload-packs"

func packMux(mgr *fleet.DatabaseManager) *http.ServeMux {
	mux := http.NewServeMux()
	registerWorkloadPackRoutes(mux, mgr, config.DefaultConfig(), WorkloadPackKeys{})
	return mux
}

func packDo(t *testing.T, mux http.Handler, method, path, body string,
	user *auth.User) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, withUser(req, user))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: body %q is not JSON: %v", method, path, w.Body.String(), err)
	}
	return w.Code, out
}

func packURL(db string, rest ...string) string {
	return strings.Replace(packsPath, "%s", db, 1) + strings.Join(rest, "")
}

func TestWorkloadPacks_ViewerCannotBuild(t *testing.T) {
	code, body := packDo(t, packMux(noPoolMgr()), "POST", packURL("alpha"), `{}`,
		testViewerUser())
	if code != http.StatusForbidden {
		t.Fatalf("viewer build: %d %v, want 403", code, body)
	}
}

func TestWorkloadPacks_InvalidWindowsAre400(t *testing.T) {
	mux := packMux(noPoolMgr())
	for _, b := range []string{
		`{"from":"2026-10-01T00:00:00Z","to":"2026-10-14T23:00:00Z"}`,
		`{"from":"2026-01-01T00:00:00Z","to":"2026-04-11T01:00:00Z"}`,
		`{"from":"2026-10-20T00:00:00Z","to":"2026-10-01T00:00:00Z"}`,
		`{"from":"2026-10-01T00:30:00Z","to":"2026-10-29T00:30:00Z"}`,
		`{"from":"yesterday"}`,
		`not json`,
	} {
		code, out := packDo(t, mux, "POST", packURL("alpha"), b, testOperatorUser())
		if code != http.StatusBadRequest || out["error"] == nil {
			t.Errorf("%s: %d %v, want 400 with an error", b, code, out)
		}
	}
}

func TestWorkloadPacks_UnknownDatabaseIs404AndNoPoolIs503(t *testing.T) {
	mux := packMux(noPoolMgr())
	code, body := packDo(t, mux, "GET", packURL("gamma"), "", testViewerUser())
	if code != http.StatusNotFound {
		t.Fatalf("unknown database: %d %v, want 404", code, body)
	}
	code, body = packDo(t, mux, "GET", packURL("alpha"), "", testViewerUser())
	if code != http.StatusServiceUnavailable {
		t.Fatalf("database without a pool: %d %v, want 503", code, body)
	}
}

func TestWorkloadPacks_MalformedHashIs400(t *testing.T) {
	mux := packMux(noPoolMgr())
	for _, sha := range []string{"abc", strings.Repeat("Z", 64)} {
		code, body := packDo(t, mux, "GET", packURL("alpha", "/", sha), "", testViewerUser())
		if code != http.StatusBadRequest {
			t.Fatalf("sha %q: %d %v, want 400", sha, code, body)
		}
	}
}

func TestWorkloadPacks_GETsAreDatabaseScopedReads(t *testing.T) {
	for _, p := range []string{
		"GET /api/v1/databases/{db}/workload-packs",
		"GET /api/v1/databases/{db}/workload-packs/{sha}",
		"GET /api/v1/databases/{db}/workload-packs/{sha}/evidence/{id...}",
	} {
		if s, ok := readRouteScopes[p]; !ok || s.String() != "database:path:db" {
			t.Errorf("%s: read scope %v, want database:path:db", p, s)
		}
	}
}

func TestWorkloadPacks_UserGrantedAnotherDatabaseIsRefused(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"A": auth.RoleOperator})
	h := dbAuthzRouter(t, true, src, dbOperator(7))
	for _, req := range []struct{ method, path string }{
		{"GET", packURL("B")},
		{"GET", packURL("B", "/", strings.Repeat("a", 64))},
		{"GET", packURL("B", "/", strings.Repeat("a", 64), "/evidence/coverage/window/all")},
	} {
		code, body := dbAuthzDo(h, req.method, req.path, "", nil)
		if code != http.StatusNotFound {
			t.Errorf("%s %s for a user granted A: %d %v, want 404 as unknown", req.method,
				req.path, code, body)
		}
	}
	code, body := dbAuthzDo(h, "POST", packURL("B"), `{}`, nil)
	requireForbidden(t, "build on B for a user granted A", code, body)
	code, body = dbAuthzDo(h, "GET", packURL("A"), "", nil)
	if code == http.StatusNotFound || code == http.StatusForbidden {
		t.Fatalf("list on the granted database: %d %v, want the handler's answer", code, body)
	}
}

func clearWorkloadPacks(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() {
		_, _ = pool.Exec(t.Context(), "DELETE FROM sage.workload_packs WHERE database_id IS NULL")
	}
	clean()
	t.Cleanup(clean)
}

func TestWorkloadPacksDB_OperatorBuildsViewerReads(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	clearWorkloadPacks(t, pool)
	mux := packMux(phase2MgrWithPool(pool))
	to := time.Now().UTC().Truncate(time.Hour)
	body := `{"from":"` + to.Add(-21*24*time.Hour).Format(time.RFC3339) + `","to":"` +
		to.Format(time.RFC3339) + `"}`
	code, built := packDo(t, mux, "POST", packURL("testdb"), body, testOperatorUser())
	sha, _ := built["content_sha256"].(string)
	if code != http.StatusCreated || built["reused"] != false || len(sha) != 64 ||
		built["built_by"] != "operator@test.com" || built["format"] != "pg_sage-workload/1" {
		t.Fatalf("build: %d %v, want 201 with a new pack", code, built)
	}
	code, again := packDo(t, mux, "POST", packURL("testdb"), body, testOperatorUser())
	if code != http.StatusOK || again["reused"] != true || again["content_sha256"] != sha {
		t.Fatalf("rebuild: %d %v, want 200 reused with the same hash", code, again)
	}
	code, list := packDo(t, mux, "GET", packURL("testdb"), "", testViewerUser())
	packs, _ := list["packs"].([]any)
	if code != http.StatusOK || len(packs) != 1 {
		t.Fatalf("list: %d %v, want one pack", code, list)
	}
	code, one := packDo(t, mux, "GET", packURL("testdb", "/", sha), "", testViewerUser())
	hdr, _ := one["header"].(map[string]any)
	if code != http.StatusOK || one["manifest"] == nil || hdr["database"] != "testdb" {
		t.Fatalf("get: %d %v, want the manifest and a header naming testdb", code, one)
	}
	code, fact := packDo(t, mux, "GET", packURL("testdb", "/", sha,
		"/evidence/coverage/window/all"), "", testViewerUser())
	if code != http.StatusOK || fact["id"] != "coverage/window/all" ||
		fact["source"] == "" || fact["value"] == nil {
		t.Fatalf("evidence: %d %v, want the coverage fact", code, fact)
	}
	code, miss := packDo(t, mux, "GET", packURL("testdb", "/", sha,
		"/evidence/workload/query/123456789"), "", testViewerUser())
	if code != http.StatusNotFound || miss["code"] != "unknown_evidence" {
		t.Fatalf("unknown evidence: %d %v, want 404 unknown_evidence", code, miss)
	}
	code, gone := packDo(t, mux, "GET", packURL("testdb", "/", strings.Repeat("b", 64)), "",
		testViewerUser())
	if code != http.StatusNotFound {
		t.Fatalf("unknown pack: %d %v, want 404", code, gone)
	}
}
