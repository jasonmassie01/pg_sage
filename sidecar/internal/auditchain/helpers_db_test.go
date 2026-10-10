package auditchain_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/auditchain"))
}

// freshDB creates a disposable database with the sage schema bootstrapped
// (which installs the audit chains). It skips only when no test server is
// configured.
func freshDB(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn %s: %v", label, err)
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", label, err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap %s: %v", label, err)
	}
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// insertAction appends one action_log row the way the executor does and
// returns its id.
func insertAction(t *testing.T, pool *pgxpool.Pool, sql string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, before_state, outcome)
		VALUES ('create_index', $1, 'DROP INDEX x', '{"rows": 5}', 'pending')
		RETURNING id`, sql).Scan(&id)
	if err != nil {
		t.Fatalf("insert action: %v", err)
	}
	return id
}

// bypass runs sql with every user trigger disabled for the session, the
// way an attacker with superuser rights edits rows behind the chain.
func bypass(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatalf("replica role: %v", err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit bypass: %v", err)
	}
}

func verify(t *testing.T, pool *pgxpool.Pool, spec auditchain.Spec,
	w auditchain.Window) auditchain.Report {
	t.Helper()
	rep, err := auditchain.Verify(context.Background(), pool, spec, w)
	if err != nil {
		t.Fatalf("verify %s: %v", spec.Chain, err)
	}
	return rep
}

func linkCount(t *testing.T, pool *pgxpool.Pool, chain string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.audit_chain_link WHERE chain = $1", chain).Scan(&n)
	if err != nil {
		t.Fatalf("count links: %v", err)
	}
	return n
}

// problemKinds returns the kinds of rep's problems, keyed by row id.
func problemKinds(rep auditchain.Report) map[int64][]string {
	out := map[int64][]string{}
	for _, p := range rep.Problems {
		out[p.RowID] = append(out[p.RowID], p.Kind)
	}
	return out
}

func hasKind(rep auditchain.Report, kind string) bool {
	for _, p := range rep.Problems {
		if p.Kind == kind {
			return true
		}
	}
	return false
}
