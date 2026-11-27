package executor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// sagePool is a pool of size connections named as pg_sage names its own,
// and the shared test pool to observe it from. requireDB is called once: a
// second call would wait forever for the cross-package lock this test
// already holds on another connection.
func sagePool(t *testing.T, size int32) (*pgxpool.Pool, *pgxpool.Pool, context.Context) {
	t.Helper()
	observer, ctx := requireDB(t)
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = size
	cfg.ConnConfig.RuntimeParams["application_name"] = "pg_sage"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, observer, ctx
}

func showApplicationName(t *testing.T, ctx context.Context, q searchPathQuerier) string {
	t.Helper()
	var name string
	if err := q.QueryRow(ctx, "SHOW application_name").Scan(&name); err != nil {
		t.Fatalf("SHOW application_name: %v", err)
	}
	return name
}

// activityName polls pg_stat_activity until backend pid reports want.
func activityName(ctx context.Context, q searchPathQuerier, pid uint32, want string) string {
	var got string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		_ = q.QueryRow(ctx, `SELECT application_name FROM pg_stat_activity
			WHERE pid = $1`, int32(pid)).Scan(&got)
		if got == want {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	return got
}

// State transition: while a tagged CREATE INDEX CONCURRENTLY waits and
// runs, pg_stat_activity names its decision; after release the pooled
// connection reads pg_sage and the role's search_path again.
func TestActionTagExecConcurrentlyNamesDecision(t *testing.T) {
	pool, observer, ctx := sagePool(t, 1)
	table := fmt.Sprintf("public.action_tag_%d", time.Now().UnixNano())
	mustExecTag(t, ctx, observer, "CREATE TABLE "+table+" (a int)")
	t.Cleanup(func() { _, _ = observer.Exec(context.Background(), "DROP TABLE "+table) })
	roleDefault := showSearchPath(t, ctx, pool)
	blocker, err := observer.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	lock := "LOCK TABLE " + table + " IN SHARE UPDATE EXCLUSIVE MODE"
	if _, err := blocker.Exec(ctx, lock); err != nil {
		t.Fatalf("lock: %v", err)
	}
	pids := make(chan uint32, 1)
	done := make(chan error, 1)
	go func() {
		done <- ExecConcurrently(ctx, pool, "CREATE INDEX CONCURRENTLY action_tag_idx ON "+
			table+" (a)", time.Minute, WithLockTimeout(30000), WithActionTag(4242),
			withBackendPID(func(pid uint32) { pids <- pid }))
	}()
	pid := <-pids
	got := activityName(ctx, observer, pid, "pg_sage action:4242")
	_ = blocker.Rollback(ctx)
	if err := <-done; err != nil {
		t.Fatalf("ExecConcurrently: %v", err)
	}
	if got != "pg_sage action:4242" {
		t.Fatalf("running action's application_name = %q, want pg_sage action:4242", got)
	}
	if name := showApplicationName(t, ctx, pool); name != "pg_sage" {
		t.Fatalf("pooled connection application_name = %q, want pg_sage", name)
	}
	if path := showSearchPath(t, ctx, pool); path != roleDefault {
		t.Fatalf("pooled connection search_path = %q, want role default %q", path,
			roleDefault)
	}
}

// State transition (transaction path): SET LOCAL names the decision inside
// the transaction; the connection reads pg_sage once it commits.
func TestActionTagInTransactionNamesDecision(t *testing.T) {
	pool, observer, ctx := sagePool(t, 1)
	table := fmt.Sprintf("public.action_tag_tx_%d", time.Now().UnixNano())
	mustExecTag(t, ctx, observer, "CREATE TABLE "+table+" (a int)")
	t.Cleanup(func() { _, _ = observer.Exec(context.Background(), "DROP TABLE "+table) })
	var inside string
	after := func(o *ddlOpts) {
		o.after = func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SHOW application_name").Scan(&inside)
		}
	}
	if err := ExecInTransaction(ctx, pool, "CREATE INDEX action_tag_tx_idx ON "+table+
		" (a)", time.Minute, WithActionTag(77), after); err != nil {
		t.Fatalf("ExecInTransaction: %v", err)
	}
	if inside != "pg_sage action:77" {
		t.Fatalf("application_name inside the transaction = %q", inside)
	}
	if name := showApplicationName(t, ctx, pool); name != "pg_sage" {
		t.Fatalf("after commit application_name = %q, want pg_sage", name)
	}
}

// Concurrent access: two tagged actions running at once each carry their
// own decision, as pg_stat_activity reports them.
func TestActionTagConcurrentActionsKeepTheirOwnDecision(t *testing.T) {
	pool, observer, ctx := sagePool(t, 2)
	var wg, inside sync.WaitGroup
	release := make(chan struct{})
	seen := make([]string, 2)
	errs := make([]error, 2)
	inside.Add(2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = withTopLevelSession(ctx, pool, "", time.Minute,
				ddlOpts{actionTag: int64(100 + i)}, func(exec pooledExecutor) error {
					pid := backendPID(t, ctx, sessionQuerier(t, exec))
					inside.Done()
					inside.Wait()
					seen[i] = activityName(ctx, observer, uint32(pid),
						fmt.Sprintf("pg_sage action:%d", 100+i))
					<-release
					return nil
				})
		}(i)
	}
	inside.Wait()
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("session %d: %v", i, errs[i])
		}
		if want := fmt.Sprintf("pg_sage action:%d", 100+i); seen[i] != want {
			t.Fatalf("session %d application_name = %q, want %q", i, seen[i], want)
		}
	}
}

func mustExecTag(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
