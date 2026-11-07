package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/startup"
)

// No concurrent access tests: the check runs once per database at startup.

// isoRow scans the self-check's columns (level, role, database, sources).
type isoRow struct {
	vals []string
	err  error
}

func (r isoRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scan %d columns into %d destinations", len(r.vals), len(dest))
	}
	for i, d := range dest {
		*(d.(*string)) = r.vals[i]
	}
	return nil
}

type isoQuerier struct{ row isoRow }

func (q isoQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not used")
}

func (q isoQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }

type warnings []string

func (w *warnings) warn(component, msg string, args ...any) {
	*w = append(*w, component+": "+fmt.Sprintf(msg, args...))
}

func TestWarnIsolationDefault_WarnsOnceNamingTheDatabase(t *testing.T) {
	var got warnings
	q := isoQuerier{row: isoRow{vals: []string{"repeatable read", "sage", "app",
		"role sage in database app = repeatable read"}}}
	warnIsolationDefault(context.Background(), q, "prod", got.warn)
	if len(got) != 1 {
		t.Fatalf("warnings = %v, want exactly one", got)
	}
	for _, want := range []string{"audit-chain", `db "prod"`, "'repeatable read'",
		"SQLSTATE 55000"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q is missing %q", got[0], want)
		}
	}
}

func TestWarnIsolationDefault_SilentForReadCommitted(t *testing.T) {
	var got warnings
	q := isoQuerier{row: isoRow{vals: []string{"read committed", "sage", "app", ""}}}
	warnIsolationDefault(context.Background(), q, "prod", got.warn)
	if len(got) != 0 {
		t.Fatalf("warnings = %v, want none for a read committed default", got)
	}
}

// A failed read is reported, not mistaken for a safe default.
func TestWarnIsolationDefault_ReportsAFailedRead(t *testing.T) {
	var got warnings
	q := isoQuerier{row: isoRow{err: errors.New("permission denied for pg_db_role_setting")}}
	warnIsolationDefault(context.Background(), q, "prod", got.warn)
	if len(got) != 1 || !strings.Contains(got[0], "permission denied") ||
		!strings.Contains(got[0], `db "prod"`) {
		t.Fatalf("warnings = %v, want one naming the database and the read error", got)
	}
}

// Startup wiring: preparing a monitored database runs the self-check on
// its pool, and an unreadable default is a WARN, not silence. The check
// only warns, so the database is still prepared (the chain's guard is
// what fails closed, at write time).
func TestPrepareMonitoredDatabaseRunsTheIsolationCheck(t *testing.T) {
	stubPreparation(t, &startup.CheckResult{PGVersionNum: 160004}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, "postgres://u@127.0.0.1:1/db?connect_timeout=2")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	var checks *startup.CheckResult
	out := captureStderr(t, func() {
		checks, err = prepareMonitoredDatabase(ctx, pool, "orders", true)
	})
	if err != nil || checks == nil || checks.PGVersionNum != 160004 {
		t.Fatalf("prepare = %+v, %v; want the stubbed checks and no error", checks, err)
	}
	want := `[WARN] [audit-chain] db "orders": could not check the isolation default`
	if strings.Count(out, want) != 1 {
		t.Fatalf("stderr %q must hold exactly one %q", out, want)
	}
}
