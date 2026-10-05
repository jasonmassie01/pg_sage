package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// Managed clouds (roadmap phase 3): GET /api/v1/managed-changes lists the
// typed parameter-group / database-flag proposals per database; POST
// .../{id}/approve|reject records an operator's decision and never
// applies anything; GET /api/v1/cloud-telemetry serves each database's
// host telemetry status, its withhold reasons and parameter drift.

func seedManagedChange(t *testing.T, pool *pgxpool.Pool, parameter, value string) int64 {
	t.Helper()
	in, err := managedparam.NewIntent("rds", parameter, value, "requires a restart")
	if err != nil {
		t.Fatal(err)
	}
	p, err := managedparam.Build(in, managedparam.Target{Provider: "rds",
		Unresolved: "no credentials"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, _, err := managedparam.NewStore(pool).Upsert(context.Background(), p, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

func cleanManagedChanges(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.managed_change_proposals")
	}
	clean()
	t.Cleanup(clean)
}

func serveManaged(t *testing.T, h http.Handler, method, target string,
	user *auth.User, id int64) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(`{"note":"tonight"}`))
	if id > 0 {
		r.SetPathValue("id", fmt.Sprint(id))
	}
	if user != nil {
		r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func TestManagedChangesListAndDecide(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cleanManagedChanges(t, pool)
	mgr := phase2MgrWithPool(pool)
	id := seedManagedChange(t, pool, "shared_buffers", "4GB")
	seedManagedChange(t, pool, "max_connections", "500")

	code, body := serveManaged(t, managedChangesHandler(mgr), "GET",
		"/api/v1/managed-changes?database=testdb", nil, 0)
	dbs, _ := body["databases"].([]any)
	if code != http.StatusOK || len(dbs) != 1 || body["meaning"] == "" {
		t.Fatalf("list = %d %v", code, body)
	}
	proposals, _ := dbs[0].(map[string]any)["proposals"].([]any)
	if len(proposals) != 2 {
		t.Fatalf("proposals = %v", proposals)
	}
	first, _ := proposals[0].(map[string]any)["proposal"].(map[string]any)
	if first["cli"] == "" || first["requires_approval"] != true || first["auto_apply"] != false {
		t.Fatalf("a proposal carries its command and stays approval-only: %v", first)
	}

	operator := &auth.User{ID: 9, Email: "op@example.com", Role: "operator"}
	approve := managedChangeDecisionHandler(mgr, true)
	code, body = serveManaged(t, approve, "POST",
		"/api/v1/managed-changes/x/approve?database=testdb", operator, id)
	if code != http.StatusOK || body["status"] != managedparam.StatusApproved ||
		body["decision_note"] != "tonight" || body["applied_by_pg_sage"] != false {
		t.Fatalf("approve = %d %v", code, body)
	}
	code, _ = serveManaged(t, approve, "POST",
		"/api/v1/managed-changes/x/approve?database=testdb", operator, id)
	if code != http.StatusConflict {
		t.Fatalf("second decision = %d, want 409", code)
	}
	code, _ = serveManaged(t, managedChangeDecisionHandler(mgr, false), "POST",
		"/api/v1/managed-changes/x/reject?database=testdb", operator, 987654)
	if code != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404", code)
	}
}

func TestManagedChangeDecisionValidation(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cleanManagedChanges(t, pool)
	mgr := phase2MgrWithPool(pool)
	id := seedManagedChange(t, pool, "work_mem", "64MB")
	h := managedChangeDecisionHandler(mgr, true)
	operator := &auth.User{ID: 9, Role: "operator"}
	if code, _ := serveManaged(t, h, "POST", "/x?database=testdb", nil, id); code !=
		http.StatusUnauthorized {
		t.Fatalf("no user = %d, want 401", code)
	}
	if code, _ := serveManaged(t, h, "POST", "/x", operator, id); code != http.StatusBadRequest {
		t.Fatalf("missing database = %d, want 400", code)
	}
	if code, _ := serveManaged(t, h, "POST", "/x?database=nope", operator, id); code !=
		http.StatusNotFound {
		t.Fatalf("unknown database = %d, want 404", code)
	}
	r := httptest.NewRequest("POST", "/x?database=testdb", nil)
	r.SetPathValue("id", "abc")
	r = r.WithContext(context.WithValue(r.Context(), userContextKey, operator))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", w.Code)
	}
	if code, _ := serveManaged(t, managedChangesHandler(mgr), "GET",
		"/api/v1/managed-changes?status=bogus", nil, 0); code != http.StatusBadRequest {
		t.Fatalf("bad status filter = %d, want 400", code)
	}
}

func TestCloudTelemetryStatusRoute(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	mgr := phase2MgrWithPool(pool)
	rt := cloudtel.Unavailable("testdb", "rds", "no AWS credentials in the default chain")
	cloudtel.Register(rt)
	t.Cleanup(func() { cloudtel.Unregister(rt) })
	code, body := serveManaged(t, cloudTelemetryHandler(mgr), "GET",
		"/api/v1/cloud-telemetry?database=testdb", nil, 0)
	dbs, _ := body["databases"].([]any)
	if code != http.StatusOK || len(dbs) != 1 {
		t.Fatalf("status = %d %v", code, body)
	}
	st, _ := dbs[0].(map[string]any)
	if st["available"] != false || !strings.Contains(fmt.Sprint(st["reason"]),
		"no AWS credentials") || st["provider"] != "rds" {
		t.Fatalf("status = %v", st)
	}
	code, body = serveManaged(t, cloudTelemetryHandler(mgr), "GET",
		"/api/v1/cloud-telemetry?database=other-db-not-managed", nil, 0)
	if code != http.StatusNotFound {
		t.Fatalf("unknown database = %d %v", code, body)
	}
}
