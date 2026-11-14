package resetlint

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"

	"example.com/resetlint/helper"
)

// CHECKPOINT and pg_stat_statements_reset() in a comment are prose.

var classify = []struct{ name, sql string }{
	{"checkpoint", "CHECKPOINT"},
	{"pgss reset", "SELECT pg_stat_statements_reset()"},
	{"shared reset", "SELECT pg_catalog.pg_stat_reset_shared($1)"},
}

// A lower-case "checkpoint" is a log keyword, even in a []string.
var keywords = []string{"checkpoint", "vacuum"}

var keys = map[string]any{"normalized_query": "CHECKPOINT"}

func classifyOne(sql string) string { return sql }

func TestPerDatabaseStatementsAreAllowed(t *testing.T) {
	var pool *pgxpool.Pool
	_, _ = pool.Exec(ctx, "SELECT pg_stat_reset()")
	_, _ = pool.Exec(ctx, `SELECT pg_stat_statements_reset(0,
		(SELECT oid FROM pg_database WHERE datname = current_database()), 0)`)
	_, _ = pool.Exec(ctx, "SELECT pg_stat_statements_reset(0, 0, $1)", int64(7))
	_, _ = pool.Exec(ctx, "SELECT pg_stat_statements_reset(10, 0, 0)")
	_, _ = pool.Exec(ctx, "SELECT 'CHECKPOINT' AS word")
	_, _ = pool.Exec(ctx, "INSERT INTO kinds VALUES ($1)", "CHECKPOINT")
	_ = classifyOne("CHECKPOINT")
	if sql := classifyOne("x"); sql == "CHECKPOINT" {
		_ = sql
	}
	switch classifyOne("y") {
	case "CHECKPOINT":
	}
	_, _, _ = classify, keys, keywords
}

func TestCheckpointOnTheExclusiveServer(t *testing.T) {
	dsn := testdb.Exclusive(t, "checkpoint counters")
	var pool *pgxpool.Pool
	_ = dsn
	_, _ = pool.Exec(ctx, "CHECKPOINT")
	settleExclusive(pool)
	helper.Checkpoint(pool)
	t.Run("sub", func(t *testing.T) {
		_, _ = pool.Exec(ctx, "SELECT pg_stat_statements_reset()")
	})
}

// settleExclusive is called only from a test that holds the exclusive
// server, through one more helper.
func settleExclusive(pool *pgxpool.Pool) { settleAgain(pool) }

func settleAgain(pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, "CHECKPOINT")
}

// exclusiveFixture takes the exclusive server for its caller.
func exclusiveFixture(t *testing.T) *pgxpool.Pool {
	_ = testdb.Exclusive(t, "fixture helper")
	return nil
}

func TestCheckpointThroughAnExclusiveFixture(t *testing.T) {
	pool := exclusiveFixture(t)
	_, _ = pool.Exec(ctx, "CHECKPOINT")
}
