package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/store"
)

// --- approveActionHandler ---

func TestApproveActionHandler_InvalidID(t *testing.T) {
	handler := approveActionHandler(nil, nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"POST /api/v1/actions/{id}/approve", handler)

	req := httptest.NewRequest(
		"POST", "/api/v1/actions/abc/approve", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid action id" {
		t.Errorf("error: got %q, want 'invalid action id'",
			resp["error"])
	}
}

func TestApproveActionHandler_NoAuth(t *testing.T) {
	handler := approveActionHandler(nil, nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"POST /api/v1/actions/{id}/approve", handler)

	req := httptest.NewRequest(
		"POST", "/api/v1/actions/1/approve", nil)
	// No user in context.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "authentication required" {
		t.Errorf("error: got %q, want 'authentication required'",
			resp["error"])
	}
}

func TestApproveActionHandler_ContentType(t *testing.T) {
	handler := approveActionHandler(nil, nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"POST /api/v1/actions/{id}/approve", handler)

	req := httptest.NewRequest(
		"POST", "/api/v1/actions/abc/approve", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json",
			ct)
	}
}

// --- rejectActionHandler ---

func TestRejectActionHandler_InvalidID(t *testing.T) {
	handler := rejectActionHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"POST /api/v1/actions/{id}/reject", handler)

	req := httptest.NewRequest(
		"POST", "/api/v1/actions/xyz/reject",
		strings.NewReader(`{"reason":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid action id" {
		t.Errorf("error: got %q, want 'invalid action id'",
			resp["error"])
	}
}

func TestRejectActionHandler_NoAuth(t *testing.T) {
	handler := rejectActionHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"POST /api/v1/actions/{id}/reject", handler)

	req := httptest.NewRequest(
		"POST", "/api/v1/actions/1/reject",
		strings.NewReader(`{"reason":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "authentication required" {
		t.Errorf("error: got %q, want 'authentication required'",
			resp["error"])
	}
}

func TestRejectActionHandler_MalformedJSON(t *testing.T) {
	handler := rejectActionHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"POST /api/v1/actions/{id}/reject", handler)

	req := httptest.NewRequest(
		"POST", "/api/v1/actions/1/reject",
		strings.NewReader("bad json"))
	req.Header.Set("Content-Type", "application/json")
	req = withUser(req, testAdminUser())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid JSON" {
		t.Errorf("error: got %q, want 'invalid JSON'",
			resp["error"])
	}
}

// --- manualExecuteHandler ---

func TestManualExecuteHandler_NoAuth(t *testing.T) {
	handler := manualExecuteHandler(nil)

	w := doRequest(
		handler, "POST", "/api/v1/actions/execute", "{}")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "authentication required" {
		t.Errorf("error: got %q, want 'authentication required'",
			resp["error"])
	}
}

func TestManualExecuteHandler_MalformedJSON(t *testing.T) {
	handler := manualExecuteHandler(nil)

	w := doRequestWithUser(
		handler, "POST", "/api/v1/actions/execute",
		"bad json", testAdminUser())

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid JSON" {
		t.Errorf("error: got %q, want 'invalid JSON'",
			resp["error"])
	}
}

func TestManualExecuteHandler_MissingFindingID(t *testing.T) {
	handler := manualExecuteHandler(nil)

	w := doRequestWithUser(
		handler, "POST", "/api/v1/actions/execute",
		`{"sql":"CREATE INDEX ..."}`, testAdminUser())

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "finding_id and sql are required" {
		t.Errorf("error: got %q, want 'finding_id and sql "+
			"are required'", resp["error"])
	}
}

func TestManualExecuteHandler_MissingSQL(t *testing.T) {
	handler := manualExecuteHandler(nil)

	w := doRequestWithUser(
		handler, "POST", "/api/v1/actions/execute",
		`{"finding_id":42}`, testAdminUser())

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "finding_id and sql are required" {
		t.Errorf("error: got %q", resp["error"])
	}
}

func TestManualExecuteHandler_ZeroFindingID(t *testing.T) {
	handler := manualExecuteHandler(nil)

	w := doRequestWithUser(
		handler, "POST", "/api/v1/actions/execute",
		`{"finding_id":0,"sql":"SELECT 1"}`, testAdminUser())

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "finding_id and sql are required" {
		t.Errorf("error: got %q", resp["error"])
	}
}

func TestManualExecuteHandler_EmptySQL(t *testing.T) {
	handler := manualExecuteHandler(nil)

	w := doRequestWithUser(
		handler, "POST", "/api/v1/actions/execute",
		`{"finding_id":42,"sql":""}`, testAdminUser())

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

func TestManualExecuteHandler_EmptyBody(t *testing.T) {
	handler := manualExecuteHandler(nil)

	w := doRequestWithUser(
		handler, "POST", "/api/v1/actions/execute",
		`{}`, testAdminUser())

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

// --- queuedActionMap ---

func TestQueuedActionMap(t *testing.T) {
	dbID := 5
	action := store.QueuedAction{
		ID:          1,
		DatabaseID:  &dbID,
		FindingID:   42,
		ProposedSQL: "CREATE INDEX idx_test ON t(a)",
		RollbackSQL: "DROP INDEX idx_test",
		ActionRisk:  "safe",
		Status:      "pending",
	}

	m := queuedActionMap(action)

	if m["id"] != 1 {
		t.Errorf("id: got %v, want 1", m["id"])
	}
	if m["finding_id"] != 42 {
		t.Errorf("finding_id: got %v, want 42",
			m["finding_id"])
	}
	if m["proposed_sql"] != "CREATE INDEX idx_test ON t(a)" {
		t.Errorf("proposed_sql: got %v",
			m["proposed_sql"])
	}
	if m["rollback_sql"] != "DROP INDEX idx_test" {
		t.Errorf("rollback_sql: got %v",
			m["rollback_sql"])
	}
	if m["action_risk"] != "safe" {
		t.Errorf("action_risk: got %v", m["action_risk"])
	}
	if m["status"] != "pending" {
		t.Errorf("status: got %v", m["status"])
	}
	if *(m["database_id"].(*int)) != 5 {
		t.Errorf("database_id: got %v", m["database_id"])
	}
}

func TestQueuedActionMap_NilDatabaseID(t *testing.T) {
	action := store.QueuedAction{
		ID:         1,
		DatabaseID: nil,
		FindingID:  42,
		ActionRisk: "moderate",
		Status:     "approved",
	}

	m := queuedActionMap(action)

	// database_id is a *int stored as any — typed nil (*int)(nil)
	// is not equal to untyped nil, so use reflect.
	dbID := m["database_id"]
	if dbID != nil && dbID != (*int)(nil) {
		t.Errorf("database_id: expected nil *int, got %v",
			m["database_id"])
	}
}

func TestQueuedActionMap_EmptyStrings(t *testing.T) {
	action := store.QueuedAction{
		ID:          1,
		ProposedSQL: "",
		RollbackSQL: "",
		Reason:      "",
	}

	m := queuedActionMap(action)

	if m["proposed_sql"] != "" {
		t.Errorf("proposed_sql: got %v", m["proposed_sql"])
	}
	if m["rollback_sql"] != "" {
		t.Errorf("rollback_sql: got %v", m["rollback_sql"])
	}
	if m["reason"] != "" {
		t.Errorf("reason: got %v", m["reason"])
	}
}

func TestQueuedActionMap_AllFields(t *testing.T) {
	action := store.QueuedAction{}
	m := queuedActionMap(action)

	// Verify all expected keys are present.
	expectedKeys := []string{
		"id", "database_id", "finding_id", "proposed_sql",
		"rollback_sql", "action_risk", "status",
		"proposed_at", "decided_by", "decided_at",
		"expires_at", "reason",
	}
	for _, key := range expectedKeys {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %q in queuedActionMap",
				key)
		}
	}

	// Verify no extra keys.
	if len(m) != len(expectedKeys) {
		t.Errorf("expected %d keys, got %d",
			len(expectedKeys), len(m))
	}
}

// --- pendingActionsHandler ---
// Requires a real ActionStore. Only the query param parsing can
// be tested without a DB.

// --- pendingCountHandler ---
// Requires a real ActionStore. No request validation to test.
