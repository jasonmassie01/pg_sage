package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// rollbackProbeTable is a table an ANALYZE rollback can name.
func rollbackProbeTable(t *testing.T, pool *pgxpool.Pool, ctx context.Context) string {
	t.Helper()
	const table = "public.rollback_handler_probe"
	if _, err := pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+" (a int)"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})
	return table
}

// policy-05: a rollback no action contract classifies (a bare ANALYZE
// names no table) is refused by the gate's recorded no_typed_contract
// decision, and the action is left as it was.
func TestRollbackActionHandler_RefusesUntypedRollback(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	exec := executor.New(pool, &config.Config{
		Trust: config.TrustConfig{Level: "advisory"},
	}, time.Now(), func(string, string, ...any) {})
	exec.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/actions/{id}/rollback", rollbackActionHandler(exec))
	id := insertActionLogForRollback(t, pool, ctx, stringPtr("ANALYZE"), "success")

	w := rollbackRequest(t, mux, id, `{"reason":"x"}`)

	if w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "no typed contract") {
		t.Fatalf("untyped rollback: got %d, body %s", w.Code, w.Body.String())
	}
	var outcome string
	if err := pool.QueryRow(ctx, `SELECT outcome FROM sage.action_log WHERE id = $1`,
		id).Scan(&outcome); err != nil || outcome != "success" {
		t.Fatalf("outcome = %q (%v), want success unchanged", outcome, err)
	}
}
