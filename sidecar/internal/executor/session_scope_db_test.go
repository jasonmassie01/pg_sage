package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

const pinnedSearchPath = "pg_catalog, pg_temp"

// singleConnPool is a pool of one connection, so the session a test
// inspects after an action is the one the action ran on.
func singleConnPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	_, ctx := requireDB(t)
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open single-connection pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

type searchPathQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func showSearchPath(t *testing.T, ctx context.Context, q searchPathQuerier) string {
	t.Helper()
	var path string
	if err := q.QueryRow(ctx, "SHOW search_path").Scan(&path); err != nil {
		t.Fatalf("SHOW search_path: %v", err)
	}
	return path
}

func backendPID(t *testing.T, ctx context.Context, q searchPathQuerier) int {
	t.Helper()
	var pid int
	if err := q.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("pg_backend_pid: %v", err)
	}
	return pid
}

func sessionQuerier(t *testing.T, exec pooledExecutor) searchPathQuerier {
	t.Helper()
	q, ok := exec.(searchPathQuerier)
	if !ok {
		t.Fatalf("top-level session %T cannot run queries", exec)
	}
	return q
}

// State transition: pinned inside the top-level session, the role default
// again once the connection is back in the pool, on success and on failure.
func TestSessionScopeTopLevelPinsAndRestoresSearchPath(t *testing.T) {
	pool, ctx := singleConnPool(t)
	roleDefault := showSearchPath(t, ctx, pool)
	if roleDefault == pinnedSearchPath {
		t.Fatalf("role default search_path is already %q; the test cannot tell", roleDefault)
	}
	errRun := errors.New("statement failed")
	for _, runErr := range []error{nil, errRun} {
		var inside string
		pidInside := 0
		err := withTopLevelSession(ctx, pool, "", time.Minute, ddlOpts{lockTimeoutMs: 1000},
			func(exec pooledExecutor) error {
				q := sessionQuerier(t, exec)
				inside = showSearchPath(t, ctx, q)
				pidInside = backendPID(t, ctx, q)
				return runErr
			})
		if !errors.Is(err, runErr) {
			t.Fatalf("withTopLevelSession error = %v, want %v", err, runErr)
		}
		if inside != pinnedSearchPath {
			t.Fatalf("search_path inside the session = %q, want %q", inside, pinnedSearchPath)
		}
		if pid := backendPID(t, ctx, pool); pid != pidInside {
			t.Fatalf("pool handed out backend %d, want the action's backend %d", pid, pidInside)
		}
		if after := showSearchPath(t, ctx, pool); after != roleDefault {
			t.Fatalf("search_path after the action = %q, want role default %q",
				after, roleDefault)
		}
	}
}

// State transition: SET LOCAL pins the transaction for the hooks and the
// statements, and ends with it on commit and on rollback.
func TestSessionScopeTransactionPinsAndRestoresSearchPath(t *testing.T) {
	pool, ctx := singleConnPool(t)
	roleDefault := showSearchPath(t, ctx, pool)
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS public.sp_scope_tx;
		CREATE TABLE public.sp_scope_tx (id int)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.sp_scope_tx")
	})
	cases := []struct {
		name, sql string
		wantErr   bool
	}{
		{"commit", "CREATE INDEX sp_scope_tx_id ON public.sp_scope_tx (id)", false},
		{"rollback", "CREATE INDEX sp_scope_tx_x ON public.sp_scope_tx (missing_col)", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var before, after string
			o := ddlOpts{lockTimeoutMs: 1000,
				before: func(ctx context.Context, tx pgx.Tx) error {
					before = showSearchPath(t, ctx, tx)
					return nil
				},
				after: func(ctx context.Context, tx pgx.Tx) error {
					after = showSearchPath(t, ctx, tx)
					return nil
				}}
			err := execStatementsInTransaction(ctx, pool, time.Minute, o, tc.sql)
			if (err != nil) != tc.wantErr {
				t.Fatalf("execStatementsInTransaction error = %v, wantErr %v", err, tc.wantErr)
			}
			if before != pinnedSearchPath {
				t.Fatalf("search_path before the statements = %q, want %q",
					before, pinnedSearchPath)
			}
			if !tc.wantErr && after != pinnedSearchPath {
				t.Fatalf("search_path after the statements = %q, want %q",
					after, pinnedSearchPath)
			}
			if got := showSearchPath(t, ctx, pool); got != roleDefault {
				t.Fatalf("search_path after the transaction = %q, want role default %q",
					got, roleDefault)
			}
		})
	}
}

// Error propagation on a real connection: a session whose RESET fails (here
// an aborted transaction left behind) is discarded, never pooled again.
func TestSessionScopeFailedResetDiscardsRealConnection(t *testing.T) {
	pool, ctx := singleConnPool(t)
	pidInside := 0
	err := withTopLevelSession(ctx, pool, "", time.Minute, ddlOpts{},
		func(exec pooledExecutor) error {
			pidInside = backendPID(t, ctx, sessionQuerier(t, exec))
			if _, err := exec.Exec(ctx, "BEGIN"); err != nil {
				t.Fatalf("BEGIN: %v", err)
			}
			_, _ = exec.Exec(ctx, "SELECT 1/0")
			return nil
		})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25P02" {
		t.Fatalf("error = %v, want the aborted-transaction reset failure (25P02)", err)
	}
	if !strings.Contains(err.Error(), "resetting ") {
		t.Fatalf("error %q does not say which reset failed", err)
	}
	if pid := backendPID(t, ctx, pool); pid == pidInside {
		t.Fatalf("pool returned backend %d whose reset failed", pid)
	}
	if got := showSearchPath(t, ctx, pool); got == pinnedSearchPath {
		t.Fatalf("fresh connection search_path = %q, want the role default", got)
	}
}
