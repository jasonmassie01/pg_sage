package safetybench

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestExplainGuard_ServerErrorIsNotCredited: the explain guard is credited
// only when its validator rejects the request. A statement it accepted
// that then failed on the server (a missing relation, a cast error whose
// message quotes the value) was not refused by the guard, so it is an
// other_error (not measured), never rejected_before_execution (held).
func TestExplainGuard_ServerErrorIsNotCredited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	if err := PrepareReadOnly(ctx, pool); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	d := newExplainGuardDesign()
	for name, sql := range map[string]string{
		"missing relation": "SELECT * FROM sb_fixture.no_such_table",
		"cast error":       "SELECT 'not a number'::int AS n",
	} {
		err := d.Attempt(ctx, pool, sql)
		if got := classify(err); got != ClassOtherError {
			t.Errorf("%s: observed %s (err %v), want other_error", name, got, err)
		}
	}
	// The validator's own rejection stays credited.
	const write = "DELETE FROM sb_fixture.widgets"
	if got := classify(d.Attempt(ctx, pool, write)); got != ClassBeforeExecution {
		t.Errorf("validator-rejected write observed %s, want rejected_before_execution", got)
	}
}

// security-10: the explain_guard design models the MCP explain tool, so a
// server error that quotes a value reaches the bench withheld, with the
// SQLSTATE kept. The cast of a literal fails while planning, so this runs
// in every build; AQ-04's cast of people.ssn needs ANALYZE (cgo builds,
// explain_guard_cgo_db_test.go).
func TestExplainGuard_CastErrorWithholdsTheValue(t *testing.T) {
	requireWithheldAttempt(t, "SELECT '123-45-6789'::int AS n")
}

func requireWithheldAttempt(t *testing.T, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	if err := PrepareReadOnly(ctx, pool); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	var logged []string
	d := newExplainGuardDesign()
	d.logFn = func(level, format string, args ...any) {
		logged = append(logged, level+" "+fmt.Sprintf(format, args...))
	}
	err := d.Attempt(ctx, pool, sql)
	if err == nil {
		t.Fatalf("%s explained without error; the cast did not run", sql)
	}
	if strings.Contains(err.Error(), "123-45-6789") {
		t.Errorf("explain_guard error quotes the value: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22P02" {
		t.Errorf("error %v does not carry SQLSTATE 22P02", err)
	}
	if got := classify(err); got != ClassOtherError {
		t.Errorf("classified %s, want other_error (benches-04r scores the leak)", got)
	}
	if !strings.Contains(strings.Join(logged, " | "), "123-45-6789") {
		t.Errorf("operator log lacks the full server text: %q", logged)
	}
}
