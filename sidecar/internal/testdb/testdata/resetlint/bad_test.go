package resetlint

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"example.com/resetlint/helper"
)

// Every line marked "want" must be reported, and no other line of this
// directory.

const harnessTag = "fixture"

var ctx = context.Background()

func TestZeroArgumentReset(t *testing.T) {
	var pool *pgxpool.Pool
	_, _ = pool.Exec(ctx, "SELECT pg_stat_statements_reset()") // want
	_, _ = pool.Exec(ctx,
		"/* "+harnessTag+" */ SELECT pg_stat_statements_reset( )") // want
	_, _ = pool.Exec(ctx, fmt.Sprintf("SELECT %s, PG_STAT_STATEMENTS_RESET()", "1")) // want
	_, _ = pool.Exec(ctx, "SELECT pg_stat_statements_reset(0, 0::oid, 0)")           // want
	_, _ = pool.Exec(ctx, "SELECT pg_stat_statements_reset(0,0,0, false)")           // want
}

func TestResetShared(t *testing.T) {
	mustExec(t, nil, "SELECT pg_stat_reset_shared('bgwriter')") // want
}

func TestExecutedCheckpoint(t *testing.T) {
	var pool *pgxpool.Pool
	_ = pool.QueryRow(ctx, " CHECKPOINT; ") // want
	f := &fixture{}
	f.exec(`CHECKPOINT`) // want
	for _, sql := range []string{
		"CREATE TABLE t (id int)",
		"SELECT pg_stat_statements_reset()", // want
	} {
		f.exec(sql)
	}
}

// settleShared is called from a test that holds no exclusive server.
func settleShared(pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, "CHECKPOINT") // want
}

func TestSettlesOnTheSharedServer(t *testing.T) {
	settleShared(nil)
	helper.Settle(nil)
}

// orphan has no caller, so no Exclusive caller vouches for it.
func orphan(pool *pgxpool.Pool) {
	_, _ = pool.Exec(ctx, "CHECKPOINT") // want
}

var resetAtLoad = "SELECT pg_stat_statements_reset()" // want

type fixture struct{}

func (f *fixture) exec(sql string) { _ = sql }

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string) { _, _, _ = t, pool, sql }
