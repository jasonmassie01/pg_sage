package partition

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// A failed step can leave the session's statement timeout anywhere (the
// test's 1 ms, or a step's own). The undo must reset it before it runs
// anything else: the leftover lookup, the DROP CONSTRAINT and the advisory
// unlock otherwise ran under it, and on a loaded server the lookup timed out
// and left the cutover CHECK behind (CI, PG17: "1 cutover constraints left
// behind"). No concurrency test: the undo runs on the one session holding
// the conversion lock.
func TestConvert_UndoResetsTheTimeoutBeforeAnyStatement(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, []string{"id"})
	fill(t, ctx, pool, tbl, 2000)
	rec := &testdb.QueryRecorder{}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = rec
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	withHook(t, func(ctx context.Context, db DB, phase string) error {
		if phase != "validate" {
			return nil
		}
		_, err := db.Exec(ctx, "SET statement_timeout = '1ms'")
		return err
	})
	if _, err := Convert(ctx, traced, tbl); pgCode(err) != "57014" {
		t.Fatalf("Convert = %v, want a statement timeout", err)
	}
	after := statementsAfter(t, rec, "VALIDATE CONSTRAINT")
	if len(after) == 0 || !strings.Contains(after[0], "statement_timeout") ||
		!strings.Contains(after[0], "set_config") {
		t.Fatalf("first statement after the failed VALIDATE = %q, want the timeout reset",
			first(after))
	}
	for _, s := range after[1:] {
		if strings.Contains(s, "SET statement_timeout = '1ms'") {
			t.Fatalf("the undo set the failed step's timeout again: %q", s)
		}
	}
	assertPlainAndClean(t, ctx, pool, tbl)
}

// statementsAfter lists the recorded statements after the first one
// containing marker.
func statementsAfter(t *testing.T, rec *testdb.QueryRecorder, marker string) []string {
	t.Helper()
	all := rec.Matching("")
	for i, q := range all {
		if strings.Contains(q.SQL, marker) {
			out := make([]string, 0, len(all)-i-1)
			for _, r := range all[i+1:] {
				out = append(out, r.SQL)
			}
			return out
		}
	}
	t.Fatalf("no recorded statement contains %q", marker)
	return nil
}

func first(xs []string) string {
	if len(xs) == 0 {
		return "(none)"
	}
	return xs[0]
}
