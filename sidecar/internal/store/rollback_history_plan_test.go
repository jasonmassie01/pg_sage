package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// RCA's rollback history reads pg_sage's rollbacks of the lookback window.
// It filtered measured_at after reading every rolled-back action through
// (outcome, executed_at): 6,667 rows per call in the small perf gate
// (perf-selfexcl). The window is an index range of the rolled-back
// actions' measured_at.
func TestRollbackHistoryIsAMeasuredAtRange(t *testing.T) {
	pool, ctx := recStorePool(t)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (executed_at, action_type,
		sql_executed, outcome, measured_at)
		SELECT now() - g * interval '1 minute', 'create_index', 'rollback_plan_probe',
		       (ARRAY['success','rolled_back'])[1 + g % 2],
		       now() - g * interval '1 minute'
		FROM generate_series(1, 6000) g`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.action_log WHERE sql_executed = 'rollback_plan_probe'")
		_, _ = pool.Exec(context.Background(), "ANALYZE sage.action_log")
	})
	if _, err := pool.Exec(ctx, "ANALYZE sage.action_log"); err != nil {
		t.Fatal(err)
	}
	plan, err := testdb.Explain(ctx, pool, "", rollbackHistorySQL, "1 hour")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "idx_action_log_rolled_back") ||
		plan.SeqScans("action_log") != 0 {
		t.Fatalf("rollback history plan:\n%s", plan)
	}
	s := NewActionStore(pool)
	got, err := s.RollbackHistory(ctx, time.Hour)
	if err != nil || len(got) < 30 {
		t.Fatalf("rollback history = %d rows (%v), want at least the 30 rollbacks of the hour",
			len(got), err)
	}
}
