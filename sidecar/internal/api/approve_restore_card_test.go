package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/store"
)

// executor-01 review: an approval of a soft-drop restore card refused
// while the emergency stop holds must leave the card pending. Nothing
// queues it again, and it is the operator's way to restore the index once
// the stop is lifted. An ordinary card refused the same way is marked
// blocked (the control that proves the refusal path ran).
func TestApproveRestoreCardRefusedDuringStopStaysPending(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	if err := executor.SetEmergencyStop(ctx, pool, true, "test"); err != nil {
		t.Fatalf("emergency stop: %v", err)
	}
	t.Cleanup(func() {
		if err := executor.SetEmergencyStop(context.Background(), pool, false,
			"test"); err != nil {
			t.Errorf("lift emergency stop: %v", err)
		}
	})
	actionStore := store.NewActionStore(pool)
	findingID := insertActionHandlerFinding(t, pool, ctx, "public.orders")
	card := queueRestoreCard(t, pool, ctx, actionStore, findingID)
	control, err := actionStore.ProposeWithMetadata(ctx, nil, findingID,
		"ANALYZE public.orders", "", "safe", store.ActionProposalMetadata{
			ActionType: "analyze_table", PolicyDecision: "queue_for_approval"})
	if err != nil {
		t.Fatalf("queue control: %v", err)
	}
	cfg := &config.Config{}
	cfg.Trust.Level = "advisory"
	exec := executor.New(pool, cfg, time.Now().Add(-10*24*time.Hour),
		func(string, string, ...any) {})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/actions/{id}/approve",
		approveActionHandler(actionStore, exec))

	for _, id := range []int{card, control} {
		req := withUser(httptest.NewRequest("POST",
			fmt.Sprintf("/api/v1/actions/%d/approve", id), nil), testAdminUser())
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusConflict {
			t.Fatalf("approve %d: status %d, body %s, want 409", id, w.Code, w.Body.String())
		}
	}

	if status := queueStatus(t, pool, ctx, card); status != "pending" {
		t.Fatalf("restore card status = %q, want it still pending", status)
	}
	if status := queueStatus(t, pool, ctx, control); status != "blocked" {
		t.Fatalf("control card status = %q, want blocked", status)
	}
}

// queueRestoreCard records a regressed drop withheld by the stop and its
// restore card, as the executor's holdDropRestore leaves them.
func queueRestoreCard(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	actionStore *store.ActionStore, findingID int) int {
	t.Helper()
	restore := "CREATE INDEX CONCURRENTLY orders_status_idx ON public.orders " +
		"USING btree (status)"
	var actionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type,
		finding_id, sql_executed, rollback_sql, outcome)
		VALUES ('drop_index', $1, $2, $3, 'rollback_withheld') RETURNING id`, findingID,
		`DROP INDEX CONCURRENTLY "public"."orders_status_idx";`, restore).
		Scan(&actionID); err != nil {
		t.Fatalf("insert withheld drop: %v", err)
	}
	id, err := actionStore.ProposeWithMetadata(ctx, nil, findingID, restore, "", "safe",
		store.ActionProposalMetadata{ActionType: "restore_dropped_index",
			IdentityKey:    "restore_dropped_index:action:" + strconv.FormatInt(actionID, 10),
			PolicyDecision: "queue_for_approval"})
	if err != nil {
		t.Fatalf("queue restore card: %v", err)
	}
	return id
}

func queueStatus(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id int) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM sage.action_queue WHERE id = $1`,
		id).Scan(&status); err != nil {
		t.Fatalf("queue status %d: %v", id, err)
	}
	return status
}
