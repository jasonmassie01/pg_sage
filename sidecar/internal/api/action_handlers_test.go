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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

func TestActionsTimelineResponseIncludesStatusRiskAndVerification(t *testing.T) {
	row := map[string]any{
		"id":                  1,
		"status":              "pending",
		"action_type":         "analyze_table",
		"risk_tier":           "safe",
		"verification_status": "not_started",
		"lifecycle_state":     "blocked",
		"blocked_reason":      "action is in cooldown",
		"attempt_count":       2,
	}

	got := actionTimelineMap(row)

	for _, key := range []string{
		"id", "status", "action_type", "risk_tier", "verification_status",
		"lifecycle_state", "blocked_reason", "attempt_count",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing %s in timeline map", key)
		}
	}
}

func TestQueuedActionMapIncludesLifecycleMetadata(t *testing.T) {
	cooldownUntil := time.Date(2026, 4, 27, 12, 30, 0, 0, time.UTC)
	action := store.QueuedAction{
		ID:                 7,
		FindingID:          99,
		ActionType:         "analyze_table",
		ActionRisk:         "safe",
		Status:             "pending",
		ProposedSQL:        "ANALYZE public.orders",
		PolicyDecision:     "execute",
		Guardrails:         []string{"dedicated connection"},
		AttemptCount:       2,
		CooldownUntil:      &cooldownUntil,
		VerificationStatus: "not_started",
		ShadowToilMinutes:  15,
	}
	logID := int64(123)
	action.ActionLogID = &logID

	got := queuedActionMap(action)

	if got["action_type"] != "analyze_table" {
		t.Fatalf("action_type = %v, want analyze_table", got["action_type"])
	}
	if got["policy_decision"] != "execute" {
		t.Fatalf("policy_decision = %v, want execute", got["policy_decision"])
	}
	if got["attempt_count"] != 2 {
		t.Fatalf("attempt_count = %v, want 2", got["attempt_count"])
	}
	if got["shadow_toil_minutes"] != 15 {
		t.Fatalf("shadow_toil_minutes = %v, want 15",
			got["shadow_toil_minutes"])
	}
	if got["cooldown_until"] == nil {
		t.Fatalf("cooldown_until missing")
	}
	if got["action_log_id"] != int64(123) {
		t.Fatalf("action_log_id = %v, want 123", got["action_log_id"])
	}
	guardrails, ok := got["guardrails"].([]string)
	if !ok || len(guardrails) != 1 || guardrails[0] != "dedicated connection" {
		t.Fatalf("guardrails = %#v, want dedicated connection", got["guardrails"])
	}
}

func TestQueuedActionMapIncludesScriptOutputForDDL(t *testing.T) {
	action := store.QueuedAction{
		ID:          12,
		FindingID:   99,
		ActionType:  "create_index_concurrently",
		ActionRisk:  "moderate",
		Status:      "pending",
		ProposedSQL: "CREATE INDEX CONCURRENTLY idx_orders_customer ON orders(customer_id)",
		RollbackSQL: "DROP INDEX CONCURRENTLY idx_orders_customer",
	}

	got := queuedActionMap(action)

	modes, ok := got["output_modes"].([]string)
	if !ok || len(modes) != 2 || modes[1] != "generate_pr_or_script" {
		t.Fatalf("output_modes = %#v, want script mode", got["output_modes"])
	}
	script, ok := got["script_output"].(map[string]any)
	if !ok {
		t.Fatalf("script_output = %#v, want map", got["script_output"])
	}
	if script["filename"] != "99_create_index_concurrently.sql" {
		t.Fatalf("filename = %v", script["filename"])
	}
	if script["migration_sql"] != action.ProposedSQL {
		t.Fatalf("migration_sql = %v", script["migration_sql"])
	}
	if script["rollback_sql"] != action.RollbackSQL {
		t.Fatalf("rollback_sql = %v", script["rollback_sql"])
	}
}

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

func TestFindingPendingActionsHandler_InvalidID(t *testing.T) {
	handler := findingPendingActionsHandler(&ActionDeps{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/findings/{id}/pending-actions", handler)

	req := httptest.NewRequest("GET",
		"/api/v1/findings/not-an-id/pending-actions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] != "invalid finding id" {
		t.Errorf("error: got %q, want invalid finding id", resp["error"])
	}
}

func TestFindingPendingActionsHandler_EmptyAndScopedByFinding(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	firstID := insertActionHandlerFinding(t, pool, ctx, "pending_first")
	secondID := insertActionHandlerFinding(t, pool, ctx, "pending_second")
	actionStore := store.NewActionStore(pool)
	_, err := actionStore.Propose(ctx, nil, firstID,
		"CREATE INDEX idx_pending_first ON public.pending_first(id)",
		"DROP INDEX idx_pending_first", "safe")
	if err != nil {
		t.Fatalf("propose first action: %v", err)
	}
	_, err = actionStore.Propose(ctx, nil, secondID,
		"CREATE INDEX idx_pending_second ON public.pending_second(id)",
		"DROP INDEX idx_pending_second", "safe")
	if err != nil {
		t.Fatalf("propose second action: %v", err)
	}

	handler := findingPendingActionsHandler(&ActionDeps{Store: actionStore})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/findings/{id}/pending-actions", handler)

	req := httptest.NewRequest("GET",
		"/api/v1/findings/99999/pending-actions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("empty status: got %d, body %s", w.Code, w.Body.String())
	}
	var emptyResp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&emptyResp); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if emptyResp["total"].(float64) != 0 {
		t.Fatalf("empty total: got %v, want 0", emptyResp["total"])
	}
	if len(emptyResp["pending"].([]any)) != 0 {
		t.Fatalf("empty pending: got %#v", emptyResp["pending"])
	}

	req = httptest.NewRequest("GET",
		"/api/v1/findings/"+itoa(firstID)+"/pending-actions", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("scoped status: got %d, body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode scoped: %v", err)
	}
	if resp["total"].(float64) != 1 {
		t.Fatalf("scoped total: got %v, want 1", resp["total"])
	}
	pending := resp["pending"].([]any)
	action := pending[0].(map[string]any)
	if action["finding_id"].(float64) != float64(firstID) {
		t.Errorf("finding_id: got %v, want %d",
			action["finding_id"], firstID)
	}
}

func TestFindingPendingActionsHandler_StoreError(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler := findingPendingActionsHandler(&ActionDeps{
		Store: store.NewActionStore(pool),
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/findings/{id}/pending-actions", handler)

	req := httptest.NewRequest("GET",
		"/api/v1/findings/1/pending-actions", nil).WithContext(cctx)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500, body %s",
			w.Code, w.Body.String())
	}
}

func TestFindingPendingActionsHandler_FleetFilterAndUnknownDatabase(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	findingID := insertActionHandlerFinding(t, pool, ctx, "fleet_pending")
	actionStore := store.NewActionStore(pool)
	_, err := actionStore.Propose(ctx, nil, findingID,
		"CREATE INDEX idx_fleet_pending ON public.fleet_pending(id)",
		"DROP INDEX idx_fleet_pending", "safe")
	if err != nil {
		t.Fatalf("propose action: %v", err)
	}
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "alpha", Pool: pool,
		Config: config.DatabaseConfig{Name: "alpha"},
	})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "offline", Pool: nil,
		Config: config.DatabaseConfig{Name: "offline"},
	})
	handler := findingPendingActionsHandler(&ActionDeps{Fleet: mgr})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/findings/{id}/pending-actions", handler)

	req := httptest.NewRequest("GET",
		"/api/v1/findings/"+itoa(findingID)+
			"/pending-actions?database=unknown",
		nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown database status: got %d, want 404", w.Code)
	}

	req = httptest.NewRequest("GET",
		"/api/v1/findings/"+itoa(findingID)+
			"/pending-actions?database=alpha",
		nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("fleet status: got %d, body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode fleet: %v", err)
	}
	if resp["total"].(float64) != 1 {
		t.Fatalf("fleet total: got %v, want 1", resp["total"])
	}
	action := resp["pending"].([]any)[0].(map[string]any)
	if action["database_name"] != "alpha" {
		t.Errorf("database_name: got %v, want alpha",
			action["database_name"])
	}
}

// --- rollbackActionHandler ---

func TestRollbackActionHandler_RequestValidation(t *testing.T) {
	handler := rollbackActionHandler(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/actions/{id}/rollback", handler)

	req := httptest.NewRequest("POST",
		"/api/v1/actions/1/rollback", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status: got %d, want 401", w.Code)
	}

	req = httptest.NewRequest("POST",
		"/api/v1/actions/bad/rollback", strings.NewReader(`{}`))
	req = withUser(req, testAdminUser())
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status: got %d, want 400", w.Code)
	}

	req = httptest.NewRequest("POST",
		"/api/v1/actions/1/rollback", strings.NewReader(`bad json`))
	req = withUser(req, testAdminUser())
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON status: got %d, want 400", w.Code)
	}
}

func TestRollbackActionHandler_StateTransitions(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	exec := executor.New(pool, &config.Config{}, nil, time.Now(),
		func(string, string, ...any) {})
	handler := rollbackActionHandler(exec)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/actions/{id}/rollback", handler)

	missingRollbackID := insertActionLogForRollback(t, pool, ctx,
		nil, "success")
	w := rollbackRequest(t, mux, missingRollbackID, `{"reason":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing rollback status: got %d, want 400", w.Code)
	}

	alreadyID := insertActionLogForRollback(t, pool, ctx,
		stringPtr("SET work_mem = '4MB'"), "rolled_back")
	w = rollbackRequest(t, mux, alreadyID, `{"reason":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("already rolled back status: got %d, want 400", w.Code)
	}

	successID := insertActionLogForRollback(t, pool, ctx,
		stringPtr("SET work_mem = '4MB'"), "success")
	w = rollbackRequest(t, mux, successID, `{"reason":"   "}`)
	if w.Code != http.StatusOK {
		t.Fatalf("success status: got %d, body %s",
			w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode success: %v", err)
	}
	if resp["status"] != "rolled_back" {
		t.Errorf("response status: got %v, want rolled_back", resp["status"])
	}
	var outcome, reason string
	err := pool.QueryRow(ctx,
		`SELECT outcome, rollback_reason
		   FROM sage.action_log WHERE id = $1`,
		successID).Scan(&outcome, &reason)
	if err != nil {
		t.Fatalf("query action log: %v", err)
	}
	if outcome != "rolled_back" {
		t.Errorf("outcome: got %s, want rolled_back", outcome)
	}
	if reason != "manual rollback" {
		t.Errorf("reason: got %q, want manual rollback", reason)
	}
}

// --- pendingCountHandler ---
// Requires a real ActionStore. No request validation to test.

func insertActionHandlerFinding(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	objectID string,
) int {
	t.Helper()
	var id int
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.findings
		 (category, severity, title, detail, status,
		  object_identifier)
		 VALUES ('action_handler', 'warning', 'Action handler test',
		  '{}', 'open', $1)
		 RETURNING id`,
		objectID).Scan(&id)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	return id
}

func insertActionLogForRollback(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	rollbackSQL *string, outcome string,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.action_log
		 (action_type, sql_executed, rollback_sql, outcome)
		 VALUES ('manual', 'SET work_mem = ''8MB''', $1, $2)
		 RETURNING id`,
		rollbackSQL, outcome).Scan(&id)
	if err != nil {
		t.Fatalf("insert action log: %v", err)
	}
	return id
}

func rollbackRequest(
	t *testing.T, mux *http.ServeMux, id int64, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST",
		"/api/v1/actions/"+fmt.Sprintf("%d", id)+"/rollback",
		strings.NewReader(body))
	req = withUser(req, testAdminUser())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func stringPtr(s string) *string {
	return &s
}
