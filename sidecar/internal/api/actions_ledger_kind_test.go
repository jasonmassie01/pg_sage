package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// G9-B01 / G6-B05 / G9-B14: every ledger row carries record_kind and a
// ledger_key that is unique across the queue and log id spaces.
func TestActionsLedger_RecordKindDistinguishesQueueAndLog(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanLedgerTables(t, pool, ctx)
	findingID := insertActionHandlerFinding(t, pool, ctx, "public.orders")
	logID := insertLedgerLogRow(t, pool, ctx, "success")
	var queueID int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.action_queue
		 (id, finding_id, proposed_sql, rollback_sql, action_risk, status,
		  action_type)
		 VALUES ($1, $2, 'CREATE INDEX CONCURRENTLY idx_q ON t(c)',
		  'DROP INDEX CONCURRENTLY idx_q', 'moderate', 'pending',
		  'create_index')
		 RETURNING id`, logID, findingID).Scan(&queueID)
	if err != nil {
		t.Fatalf("insert queue row with colliding id: %v", err)
	}

	resp := getLedger(t, pool)
	actions := resp["actions"].([]any)
	if len(actions) != 2 {
		t.Fatalf("ledger rows = %d, want 2", len(actions))
	}
	kinds := map[string]string{}
	keys := map[string]bool{}
	for _, raw := range actions {
		row := raw.(map[string]any)
		kind, _ := row["record_kind"].(string)
		key, _ := row["ledger_key"].(string)
		kinds[kind] = row["id"].(string)
		keys[key] = true
	}
	if kinds["executed"] == "" || kinds["queued"] == "" {
		t.Fatalf("record_kind values = %v, want executed and queued", kinds)
	}
	if !keys["log:"+itoa64(logID)] || !keys["queue:"+itoa64(queueID)] {
		t.Fatalf("ledger_key values = %v, want log:%d and queue:%d",
			keys, logID, queueID)
	}
}

func getLedger(t *testing.T, pool *pgxpool.Pool) map[string]any {
	t.Helper()
	handler := actionsListHandler(phase2MgrWithPool(pool))
	req := httptest.NewRequest("GET", "/api/v1/actions?database=testdb", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// The rollback endpoint refuses a request the client labels as a
// queued proposal, before the executor is consulted (nil executor
// would panic if reached).
func TestRollbackHandler_RejectsQueuedRecordKind(t *testing.T) {
	for _, kind := range []string{"queued", "queue", "unknown"} {
		h := rollbackActionHandler(nil)
		body := `{"reason":"x","record_kind":"` + kind + `"}`
		req := withUser(httptest.NewRequest(http.MethodPost,
			"/api/v1/actions/7/rollback", strings.NewReader(body)),
			testOperatorUser())
		req.SetPathValue("id", "7")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("record_kind %q reached the executor: %v",
						kind, p)
				}
			}()
			h.ServeHTTP(w, req)
		}()
		if w.Code != http.StatusBadRequest {
			t.Errorf("record_kind %q: status = %d, want 400",
				kind, w.Code)
		}
		if !strings.Contains(w.Body.String(), "only executed actions") {
			t.Errorf("record_kind %q: body = %s", kind, w.Body.String())
		}
	}
}

func TestFleetRollbackHandler_RejectsQueuedRecordKind(t *testing.T) {
	h := fleetRollbackActionHandler(fleet.NewManager(&config.Config{}))
	req := withUser(httptest.NewRequest(http.MethodPost,
		"/api/v1/actions/7/rollback?database=testdb",
		strings.NewReader(`{"record_kind":"queued"}`)), testOperatorUser())
	req.SetPathValue("id", "7")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("queued rollback reached the executor: %v", p)
			}
		}()
		h.ServeHTTP(w, req)
	}()
	if w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "only executed actions") {
		t.Fatalf("status = %d body = %s, want 400 only executed actions",
			w.Code, w.Body.String())
	}
}

func cleanLedgerTables(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
) {
	t.Helper()
	for _, q := range []string{
		"DELETE FROM sage.verification",
		"DELETE FROM sage.decision",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	phase2CleanTables(t, pool, ctx)
}

func insertLedgerLogRow(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context, outcome string,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		 (action_type, sql_executed, rollback_sql, outcome, executed_at)
		 VALUES ('create_index', 'CREATE INDEX CONCURRENTLY idx_l ON t(c)',
		  'DROP INDEX CONCURRENTLY idx_l', $1, $2)
		 RETURNING id`, outcome, time.Now().Add(-time.Minute)).Scan(&id)
	if err != nil {
		t.Fatalf("insert action_log: %v", err)
	}
	return id
}

func itoa64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
