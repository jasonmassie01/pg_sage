package testdb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ci-13: ResetStatements without a server. The live behaviour (only the
// fixture database's entries go) is in stats_db_test.go.
//
// No state-transition tests: ResetStatements is one statement, not a
// state machine.

// errQuerier fails every call with err and counts the calls.
type errQuerier struct {
	err   error
	calls int
}

func (q *errQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	q.calls++
	return pgconn.CommandTag{}, q.err
}

func (q *errQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	q.calls++
	return errRow{q.err}
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func TestResetStatementsRejectsANilQuerier(t *testing.T) {
	var typedNil *pgxpool.Pool
	for name, q := range map[string]StatementsQuerier{
		"nil interface": nil,
		"nil pool":      typedNil,
	} {
		dealloc, err := ResetStatements(context.Background(), q)
		if err == nil || !strings.Contains(err.Error(), "no querier") {
			t.Errorf("%s: ResetStatements = %d, %v; want a \"no querier\" error",
				name, dealloc, err)
		}
		if dealloc != 0 {
			t.Errorf("%s: dealloc = %d, want 0 on error", name, dealloc)
		}
	}
}

// Error propagation: a failing server call comes back wrapped, naming
// pg_stat_statements, and nothing runs after the first failure.
func TestResetStatementsWrapsTheServerError(t *testing.T) {
	cause := errors.New("connection reset by peer")
	q := &errQuerier{err: cause}
	dealloc, err := ResetStatements(context.Background(), q)
	if !errors.Is(err, cause) {
		t.Fatalf("ResetStatements error = %v, want it to wrap %v", err, cause)
	}
	if !strings.Contains(err.Error(), "pg_stat_statements") {
		t.Fatalf("error %q does not name pg_stat_statements", err)
	}
	if dealloc != 0 || q.calls != 1 {
		t.Fatalf("dealloc %d after %d calls, want 0 after 1", dealloc, q.calls)
	}
}

// Invalid input: no extension in the database is a plain error naming the
// extension, never a panic and never a reset attempt.
func TestResetStatementsWithoutTheExtensionNamesIt(t *testing.T) {
	q := &errQuerier{err: pgx.ErrNoRows}
	_, err := ResetStatements(context.Background(), q)
	if err == nil || !strings.Contains(err.Error(), "pg_stat_statements is not installed") {
		t.Fatalf("ResetStatements error = %v, want \"pg_stat_statements is not installed\"",
			err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("error %v leaks pgx.ErrNoRows; want the extension named instead", err)
	}
	if q.calls != 1 {
		t.Fatalf("%d calls, want only the extension lookup", q.calls)
	}
}
