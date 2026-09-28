//go:build integration

package explain

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// G6-B01: a multi-statement body must never escape the READ ONLY
// transaction. Each case targets a probe table; afterwards the probe
// must still hold exactly its seeded row and still exist.
func TestExplain_MultiStatementBodyCannotMutate(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanExplainResults(t, pool, ctx)
	_, err := pool.Exec(ctx, `DROP TABLE IF EXISTS explain_probe;
		CREATE TABLE explain_probe (v int);
		INSERT INTO explain_probe VALUES (42)`)
	if err != nil {
		t.Fatalf("create probe: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS explain_probe")
	})

	ex := New(pool, testExplainConfig(), noopLog)
	bodies := []struct {
		query  string
		params []string
	}{
		{"SELECT $1::int; COMMIT; DELETE FROM explain_probe", []string{"1"}},
		{"SELECT $1::int; COMMIT; DROP TABLE explain_probe", nil},
		{"SELECT 1; COMMIT; DELETE FROM explain_probe", nil},
		{"SELECT 1; COMMIT; UPDATE explain_probe SET v = 0", nil},
		{"DELETE FROM explain_probe", nil},
		{"DELETE FROM explain_probe WHERE v = $1", []string{"42"}},
	}
	for _, b := range bodies {
		res, err := ex.Explain(ctx, ExplainRequest{
			Query: b.query, Params: b.params,
		})
		if !errors.Is(err, ErrExplainInvalidRequest) {
			t.Errorf("Explain(%q) err = %v, want %v",
				b.query, err, ErrExplainInvalidRequest)
		}
		if res != nil {
			t.Errorf("Explain(%q) returned a plan, want nil", b.query)
		}
		assertProbeIntact(t, ex, b.query)
	}
}

func assertProbeIntact(t *testing.T, ex *Explainer, after string) {
	t.Helper()
	var count, value int
	err := ex.pool.QueryRow(t.Context(),
		"SELECT count(*), coalesce(max(v), -1) FROM explain_probe",
	).Scan(&count, &value)
	if err != nil {
		t.Fatalf("after %q: probe table unreadable: %v", after, err)
	}
	if count != 1 || value != 42 {
		t.Fatalf("after %q: probe = (count %d, v %d), want (1, 42)",
			after, count, value)
	}
}

// A legitimate single parameterized SELECT still returns a plan, and
// the prepared statement is released so a second call on the same
// pooled connection does not collide.
func TestExplain_ParameterizedSingleSelectStillWorks(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanExplainResults(t, pool, ctx)
	ex := New(pool, testExplainConfig(), noopLog)

	for i, q := range []string{
		"SELECT $1::int + 1;",
		"SELECT $1::int + 2",
	} {
		res, err := ex.Explain(ctx, ExplainRequest{
			Query: q, Params: []string{"5"},
		})
		if err != nil {
			t.Fatalf("call %d Explain(%q): %v", i, q, err)
		}
		if len(res.NodeBreakdown) == 0 {
			t.Fatalf("call %d: empty node breakdown", i)
		}
		if res.NodeBreakdown[0].NodeType != "Result" {
			t.Errorf("call %d root node = %q, want Result",
				i, res.NodeBreakdown[0].NodeType)
		}
	}
}

// After a failing EXPLAIN EXECUTE (bad param cast aborts the
// transaction) the prepared statement must not survive on the
// pooled connection (G1-B29, explain half).
func TestExplain_FailedExecuteDoesNotLeakPreparedStatement(t *testing.T) {
	shared, ctx := requireDB(t)
	cleanExplainResults(t, shared, ctx)
	cfg, err := pgxpool.ParseConfig(testDSN())
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 1 // every call reuses the same backend
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("single-conn pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ex := New(pool, testExplainConfig(), noopLog)

	_, err = ex.Explain(ctx, ExplainRequest{
		Query: "SELECT $1::int", Params: []string{"not-an-int"},
	})
	if err == nil {
		t.Fatal("Explain with an invalid int param: want error, got nil")
	}
	var leaked int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM pg_prepared_statements "+
			"WHERE name = '_sage_explain'").Scan(&leaked); err != nil {
		t.Fatalf("query prepared statements: %v", err)
	}
	if leaked != 0 {
		t.Fatal("_sage_explain leaked on the pooled connection")
	}
	res, err := ex.Explain(ctx, ExplainRequest{
		Query: "SELECT $1::int", Params: []string{"7"},
	})
	if err != nil {
		t.Fatalf("follow-up Explain on same connection: %v", err)
	}
	if res.NodeBreakdown[0].NodeType != "Result" {
		t.Errorf("follow-up root node = %q, want Result",
			res.NodeBreakdown[0].NodeType)
	}
}
