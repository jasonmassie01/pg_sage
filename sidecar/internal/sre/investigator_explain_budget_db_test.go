package sre

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-07 review: the statement budget starts once the explainer holds a
// connection, and only a 42501 from PREPARE or EXPLAIN is missing_select.

func TestStatementExplainer_StatementBudgetStartsAfterThePoolWait(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	tbl := explainTable(t, ctx, pool)
	id := statementID(t, ctx, pool, "SELECT v FROM "+tbl+" WHERE id = $1",
		"FROM "+tbl+" WHERE id", 9)
	cfg := pool.Config().Copy()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer one.Close()
	busy, err := one.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Held longer than the whole 1.5 s statement budget, well inside the
	// acquire budget: a shared budget would expire before the statement.
	hold := probes.MaxStatementTimeout + 1500*time.Millisecond
	go func() {
		time.Sleep(hold)
		busy.Release()
	}()
	start := time.Now()
	res := newExplainer(one, cachedTexts(t, ctx, pool, id)).ExplainStatement(ctx, id)
	waited := time.Since(start)
	if res.Status != probes.StatusOK || len(res.Rows) == 0 {
		t.Fatalf("explain after a %s pool wait = %+v, want ok with plan rows", hold, res)
	}
	if waited < hold {
		t.Fatalf("explain returned after %s, before the connection was free (%s)",
			waited, hold)
	}
}

func TestStatementExplainer_NoConnectIsNotMissingSelect(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	tbl := explainTable(t, ctx, pool)
	id := statementID(t, ctx, pool, "SELECT v FROM "+tbl+" WHERE id = $1",
		"FROM "+tbl+" WHERE id", 10)
	db, role := tbl+"_noconn", tbl+"_noconn_r"
	password := fmt.Sprintf("pw-%d", time.Now().UnixNano())
	for _, sql := range []string{"CREATE DATABASE " + db,
		"REVOKE CONNECT ON DATABASE " + db + " FROM PUBLIC",
		"CREATE ROLE " + role + " LOGIN PASSWORD '" + password + "' IN ROLE pg_monitor"} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	cfg := pool.Config().Copy()
	cfg.ConnConfig.Database = db
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, password
	denied, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool as %s: %v", role, err)
	}
	t.Cleanup(func() {
		denied.Close()
		bg := context.Background()
		_, _ = pool.Exec(bg, "DROP DATABASE "+db)
		_, _ = pool.Exec(bg, "DROP ROLE "+role)
	})
	res := newExplainer(denied, cachedTexts(t, ctx, pool, id)).ExplainStatement(ctx, id)
	if res.Status != probes.StatusNoPrivilege || res.Reason != "insufficient_privilege" ||
		res.Phase != probes.PhasePoolAcquire || len(res.Rows) != 0 {
		t.Fatalf("explain without CONNECT = %+v, want no_privilege "+
			"insufficient_privilege in %s", res, probes.PhasePoolAcquire)
	}
}
