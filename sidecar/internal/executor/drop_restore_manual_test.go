package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// CHECK-02 for drops an operator runs: the stored rollback is the
// CONCURRENTLY restore too, so the button and a resumed monitor run it.
func TestStoredRollbackSQLNormalizesOnlyDrops(t *testing.T) {
	for _, c := range dropRuleRollbacks {
		if got := storedRollbackSQL(c.drop, c.rollback); got != c.want {
			t.Fatalf("%s: stored = %q, want %q", c.rule, got, c.want)
		}
	}
	for _, c := range []struct{ sql, rollback string }{
		{"ALTER SYSTEM SET work_mem = '64MB'", "ALTER SYSTEM RESET work_mem;"},
		{"CREATE INDEX CONCURRENTLY i ON public.t (a)", "DROP INDEX CONCURRENTLY public.i;"},
		{dropRuleRollbacks[0].drop, ""},
		{dropRuleRollbacks[0].drop, "   "},
	} {
		if got := storedRollbackSQL(c.sql, c.rollback); got != c.rollback {
			t.Fatalf("storedRollbackSQL(%q, %q) = %q, want it unchanged", c.sql,
				c.rollback, got)
		}
	}
}

func TestManualDropLogsTheConcurrentRestore(t *testing.T) {
	pool, ctx := isolatedSageDB(t)
	e := New(pool, restoreTrustConfig(), time.Now().Add(-90*24*time.Hour), noopExecLog)
	c := dropRuleRollbacks[0]
	id := e.logManualActionWithDecision(ctx, 0, c.drop, c.rollback, map[string]any{}, nil,
		nil, 0, nil)
	if id <= 0 {
		t.Fatal("manual drop not logged")
	}
	var stored string
	if err := pool.QueryRow(context.Background(), `SELECT rollback_sql FROM sage.action_log
		WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != c.want || !strings.HasPrefix(stored, "CREATE INDEX CONCURRENTLY ") {
		t.Fatalf("stored rollback = %q, want %q", stored, c.want)
	}
}
