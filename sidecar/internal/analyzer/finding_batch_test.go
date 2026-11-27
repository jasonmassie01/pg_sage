package analyzer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rules-06 review: a batch keeps going past a rejected row but stops on a
// lost connection, so an unreachable database costs one timeout per
// cycle, not one per finding.

func TestConnectionLost(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"cancelled", fmt.Errorf("x: %w", context.Canceled), true},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), true},
		{"connection failure 08006", &pgconn.PgError{Code: "08006"}, true},
		{"admin shutdown 57P01", fmt.Errorf("x: %w", &pgconn.PgError{Code: "57P01"}), true},
		{"cannot connect now 57P03", &pgconn.PgError{Code: "57P03"}, true},
		{"too many connections 53300", &pgconn.PgError{Code: "53300"}, true},
		{"statement timeout 57014 is per row", &pgconn.PgError{Code: "57014"}, false},
		{"bad byte 22021 is per row", &pgconn.PgError{Code: "22021"}, false},
		{"unique violation is per row", &pgconn.PgError{Code: "23505"}, false},
		{"network error", fmt.Errorf("x: %w", &net.OpError{Op: "read",
			Err: errors.New("connection reset by peer")}), true},
		{"unexpected EOF", fmt.Errorf("x: %w", io.ErrUnexpectedEOF), true},
		{"marshal error", errors.New("json: unsupported type: func()"), false},
		{"lost race", errUpsertRaced, false},
	}
	for _, tc := range cases {
		if got := connectionLost(tc.err); got != tc.want {
			t.Errorf("%s: connectionLost = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// unreachablePool is a pool to a closed local port that counts connect
// attempts.
func unreachablePool(t *testing.T, attempts *atomic.Int32) *pgxpool.Pool {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg, err := pgxpool.ParseConfig("postgres://u:p@" + addr + "/db?sslmode=disable")
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.BeforeConnect = func(context.Context, *pgx.ConnConfig) error {
		attempts.Add(1)
		return nil
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestUpsertFindings_UnreachableDatabaseStopsBatch(t *testing.T) {
	var attempts atomic.Int32
	pool := unreachablePool(t, &attempts)
	ff := make([]Finding, 5)
	for i := range ff {
		ff[i] = lifecycleFinding("cat_down", fmt.Sprintf("public.t%d", i), "warning")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	res, err := UpsertFindingsWithResult(ctx, pool, ff)
	if err == nil || !strings.Contains(err.Error(), "stopped with 4 findings left") ||
		!strings.Contains(err.Error(), "public.t0") {
		t.Fatalf("err = %v, want public.t0's error and a stop with 4 left", err)
	}
	if len(res.Failed) != 5 || len(res.Opened) != 0 {
		t.Fatalf("Failed = %d Opened = %d, want 5 and 0", len(res.Failed), len(res.Opened))
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("connect attempts = %d, want 1 (one timeout per batch)", n)
	}
}

// installConnLossOn makes an event (INSERT or UPDATE) on sage.findings
// fail with SQLSTATE 08006 for one test object identifier (a constant).
func installConnLossOn(t *testing.T, pool *pgxpool.Pool, event, ident string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, fmt.Sprintf(`
CREATE OR REPLACE FUNCTION public.test_phase2_rules06_connloss() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.object_identifier = '%s' THEN
    RAISE EXCEPTION 'rules06 connection lost' USING ERRCODE = '08006';
  END IF;
  RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS test_phase2_rules06_connloss ON sage.findings;
CREATE TRIGGER test_phase2_rules06_connloss BEFORE %s ON sage.findings
  FOR EACH ROW EXECUTE FUNCTION public.test_phase2_rules06_connloss();`,
		ident, event))
	if err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_phase2_rules06_connloss
			ON sage.findings;
			DROP FUNCTION IF EXISTS public.test_phase2_rules06_connloss()`)
	})
}

func TestLifecycle_ConnectionClassErrorStopsUpsert(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	installConnLossOn(t, pool, "INSERT", "public.t3")
	ff := make([]Finding, 5)
	for i := range ff {
		ff[i] = lifecycleFinding("test_phase2_rules06_conn", fmt.Sprintf("public.t%d", i+1),
			"warning")
	}

	res, err := UpsertFindingsWithResult(context.Background(), pool, ff)
	if err == nil || !strings.Contains(err.Error(), "stopped with 2 findings left") {
		t.Fatalf("err = %v, want a stop with 2 findings left", err)
	}
	if len(res.Opened) != 2 || len(res.Failed) != 3 {
		t.Fatalf("Opened = %d Failed = %d, want 2 and 3", len(res.Opened), len(res.Failed))
	}
	if n := countCategoryRows(t, pool, ff[0].Category); n != 2 {
		t.Fatalf("rows = %d, want 2 (t4 and t5 unattempted)", n)
	}
}

func TestLifecycle_ConnectionClassErrorStopsResolve(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	ctx := context.Background()
	cat := "test_phase2_rules06_conn_resolve"
	var ff []Finding
	for _, ident := range []string{"public.a", "public.boom", "public.c"} {
		ff = append(ff, lifecycleFinding(cat, ident, "warning"))
	}
	if err := UpsertFindings(ctx, pool, ff); err != nil {
		t.Fatalf("seed: %v", err)
	}
	installConnLossOn(t, pool, "UPDATE", "public.boom")

	cleared, err := ResolveClearedFindings(ctx, pool, nil, cat)
	if err == nil || !strings.Contains(err.Error(), "rules06 connection lost") {
		t.Fatalf("err = %v, want the connection-class error", err)
	}
	if len(cleared) != 1 || cleared[0].ObjectIdentifier != "public.a" {
		t.Fatalf("cleared = %+v, want only public.a (resolved before the loss)", cleared)
	}
	if n := countFindingRows(t, pool, cat, "public.c", "open"); n != 1 {
		t.Fatalf("open rows for public.c = %d, want 1 (not attempted)", n)
	}
}

// Concurrent access: two upserters reopening one action-resolved identity
// end with that single row open and neither reports an error.
func TestLifecycle_ConcurrentReopenOneRow(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	for i := 0; i < 10; i++ {
		f := lifecycleFinding("test_phase2_rules06_reopen_race",
			fmt.Sprintf("public.r%d", i), "warning")
		id, _ := insertActionResolved(t, pool, f, 300*time.Second)
		ev := Evidence{CollectedAt: dbNow(t, pool), Window: 1200 * time.Second}
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for g := range errs {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				_, errs[g] = UpsertFindingsObserved(context.Background(), pool,
					[]Finding{f}, ev)
			}(g)
		}
		close(start)
		wg.Wait()
		for g, err := range errs {
			if err != nil {
				t.Fatalf("iteration %d upserter %d: %v", i, g, err)
			}
		}
		var n int
		var openID int64
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) OVER (), id FROM sage.findings
			  WHERE category = $1 AND object_identifier = $2`,
			f.Category, f.ObjectIdentifier).Scan(&n, &openID)
		if err != nil || n != 1 || openID != id {
			t.Fatalf("iteration %d: rows = %d id = %d err = %v, want only row %d",
				i, n, openID, err, id)
		}
	}
}

// Invalid input: a stored non-integer persisted_after_action must not
// make every later refresh of the row fail.
func TestLifecycle_NonIntegerPersistedLinkStillRefreshes(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	t.Cleanup(func() { phase2CleanFindings(t, pool) })
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_rules06_badlink", "public.x", "warning")
	if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := pool.Exec(ctx, `UPDATE sage.findings
		SET detail = detail || '{"persisted_after_action": 1.5}'::jsonb
		WHERE category = $1`, f.Category)
	if err != nil {
		t.Fatalf("set link: %v", err)
	}

	if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
		t.Fatalf("refresh with a non-integer link: %v", err)
	}
	var count int
	err = pool.QueryRow(ctx, `SELECT occurrence_count FROM sage.findings
		WHERE category = $1 AND status = 'open'`, f.Category).Scan(&count)
	if err != nil || count != 2 {
		t.Fatalf("occurrence_count = %d err = %v, want 2", count, err)
	}
}
