package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Regression tests for SURF-19 (resolution reason discarded) and R04
// (resolution actor not recorded; detail omits lifecycle fields).

func singleIncidentManager(db tempIncidentDB) *fleet.DatabaseManager {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "lifecycle", Pool: db.pool,
	})
	return mgr
}

func resolveRequest(id, body string, user *auth.User) *http.Request {
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/incidents/"+id+"/resolve?database=lifecycle",
		strings.NewReader(body))
	req.SetPathValue("id", id)
	if user != nil {
		req = req.WithContext(context.WithValue(
			req.Context(), userContextKey, user))
	}
	return req
}

func TestIncidentResolveHandlerPersistsActorAndReason(t *testing.T) {
	adminPool, ctx := phase2RequireDB(t)
	db := phase2TempIncidentDB(t, ctx, adminPool, "actor")
	id := "66666666-6666-4666-8666-666666666666"
	seedIncidentWithID(t, ctx, db.pool, id, db.name)

	rec := httptest.NewRecorder()
	user := &auth.User{ID: 7, Email: "ops@example.com", Role: "operator"}
	incidentResolveHandler(singleIncidentManager(db)).ServeHTTP(rec,
		resolveRequest(id, `{"reason":"rebuilt the index"}`, user))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var by, reason *string
	if err := db.pool.QueryRow(ctx, `SELECT resolved_by,
		resolution_reason FROM sage.incidents WHERE id = $1`,
		id).Scan(&by, &reason); err != nil {
		t.Fatalf("read resolution: %v", err)
	}
	if by == nil || *by != "user:ops@example.com" {
		t.Errorf("resolved_by = %v, want user:ops@example.com", by)
	}
	if reason == nil || *reason != "rebuilt the index" {
		t.Errorf("resolution_reason = %v, want 'rebuilt the index'", reason)
	}
}

func TestIncidentResolveHandlerAlreadyResolvedIsConflict(t *testing.T) {
	adminPool, ctx := phase2RequireDB(t)
	db := phase2TempIncidentDB(t, ctx, adminPool, "conflict")
	id := "77777777-7777-4777-8777-777777777777"
	seedIncidentWithID(t, ctx, db.pool, id, db.name)
	mgr := singleIncidentManager(db)

	first := httptest.NewRecorder()
	incidentResolveHandler(mgr).ServeHTTP(first,
		resolveRequest(id, `{"reason":"a"}`, nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d body=%s",
			first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	incidentResolveHandler(mgr).ServeHTTP(second,
		resolveRequest(id, `{"reason":"b"}`, nil))
	if second.Code != http.StatusConflict {
		t.Fatalf("second status = %d, want 409 body=%s",
			second.Code, second.Body.String())
	}
	var reason *string
	if err := db.pool.QueryRow(ctx, `SELECT resolution_reason
		FROM sage.incidents WHERE id = $1`, id).Scan(&reason); err != nil {
		t.Fatalf("read reason: %v", err)
	}
	if reason == nil || *reason != "a" {
		t.Errorf("reason = %v, want first reason 'a' preserved", reason)
	}
}

func TestIncidentResolveHandlerRejectsOverlongReason(t *testing.T) {
	adminPool, ctx := phase2RequireDB(t)
	db := phase2TempIncidentDB(t, ctx, adminPool, "longreason")
	id := "88888888-8888-4888-8888-888888888888"
	seedIncidentWithID(t, ctx, db.pool, id, db.name)

	body, _ := json.Marshal(map[string]string{
		"reason": strings.Repeat("x", 2001),
	})
	rec := httptest.NewRecorder()
	incidentResolveHandler(singleIncidentManager(db)).ServeHTTP(rec,
		resolveRequest(id, string(body), nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if at := incidentResolvedAt(t, ctx, db.pool, id); at != nil {
		t.Fatalf("incident resolved despite rejected request: %v", *at)
	}
}

func TestIncidentDetailHandlerReturnsLifecycleFields(t *testing.T) {
	adminPool, ctx := phase2RequireDB(t)
	db := phase2TempIncidentDB(t, ctx, adminPool, "detailfields")
	prev := "99999999-9999-4999-8999-999999999990"
	id := "99999999-9999-4999-8999-999999999999"
	seedIncidentWithID(t, ctx, db.pool, prev, db.name)
	seedIncidentWithID(t, ctx, db.pool, id, db.name)
	if _, err := db.pool.Exec(ctx, `UPDATE sage.incidents SET
		rollback_sql = 'DROP INDEX CONCURRENTLY i',
		resolved_at = now(), resolved_by = 'user:ops@example.com',
		resolution_reason = 'done', previous_incident_id = $2
		WHERE id = $1`, id, prev); err != nil {
		t.Fatalf("seed lifecycle fields: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/incidents/"+id+"?database=lifecycle", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	incidentDetailHandler(singleIncidentManager(db)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{
		"rollback_sql":         "DROP INDEX CONCURRENTLY i",
		"resolved_by":          "user:ops@example.com",
		"resolution_reason":    "done",
		"previous_incident_id": prev,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %q", k, got[k], v)
		}
	}
}
