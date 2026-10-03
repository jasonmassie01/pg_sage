package probes

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// CHECK-21: probe timeout, row, byte, concurrency and global caps hold
// against a real PostgreSQL server. Probes run read-only with a fixed
// search_path, and failures come back typed, never as healthy.

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func testSpec(id ID, sql string, mutate ...func(*Spec)) Spec {
	s := Spec{ID: id, Version: "v1", Family: FamilyLocks,
		Variants:         []Variant{{SQL: sql}},
		StatementTimeout: 400 * time.Millisecond, LockTimeout: 100 * time.Millisecond,
		MaxRows: 50, MaxBytes: 64 << 10}
	for _, m := range mutate {
		m(&s)
	}
	return s
}

func testRunner(t *testing.T, pool *pgxpool.Pool, specs ...Spec) *Runner {
	t.Helper()
	reg, err := NewRegistry(specs...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return NewRunner(pool, reg, NewLimiter(MaxSidecarConcurrency))
}

func TestRunner_OKResultIsTypedAndStamped(t *testing.T) {
	pool, ctx := livePool(t)
	r := testRunner(t, pool, testSpec("typed", `SELECT 1::int4 AS a,
		2::int8 AS b, 1.5::float8 AS c, 'x'::text AS d, true AS e,
		now() AS f, NULL::int8 AS g LIMIT $1`))
	before := time.Now().Add(-time.Second)
	res := r.Run(ctx, "typed", Args{})
	if res.Status != StatusOK || len(res.Rows) != 1 || res.Truncated {
		t.Fatalf("result = %+v", res)
	}
	if strings.Join(res.Columns, ",") != "a,b,c,d,e,f,g" {
		t.Fatalf("columns = %v", res.Columns)
	}
	row := res.Rows[0]
	if row["a"] != int64(1) || row["b"] != int64(2) || row["c"] != 1.5 ||
		row["d"] != "x" || row["e"] != true || row["g"] != nil {
		t.Fatalf("row = %#v (ints must normalize to int64)", row)
	}
	if _, ok := row["f"].(time.Time); !ok {
		t.Fatalf("timestamptz decoded as %T", row["f"])
	}
	if res.ProbeID != "typed" || res.Version != "v1" ||
		res.ObservedAt.Before(before) || res.ObservedAt.After(time.Now()) {
		t.Fatalf("stamp = %s %s %v", res.ProbeID, res.Version, res.ObservedAt)
	}
}

func TestRunner_EmptyIsExplicit(t *testing.T) {
	pool, ctx := livePool(t)
	r := testRunner(t, pool, testSpec("none", "SELECT 1 AS a WHERE false LIMIT $1"))
	res := r.Run(ctx, "none", Args{})
	if res.Status != StatusEmpty || len(res.Rows) != 0 || res.Reason != "no_rows" {
		t.Fatalf("result = %+v, want empty/no_rows", res)
	}
}

func TestRunner_RowCapBoundary(t *testing.T) {
	pool, ctx := livePool(t)
	for _, c := range []struct {
		produced  int
		truncated bool
	}{{49, false}, {50, false}, {51, true}, {10000, true}} {
		sql := fmt.Sprintf("SELECT n FROM generate_series(1, %d) AS n LIMIT $1",
			c.produced)
		r := testRunner(t, pool, testSpec("rows", sql))
		res := r.Run(ctx, "rows", Args{})
		want := c.produced
		if want > 50 {
			want = 50
		}
		if res.Status != StatusOK || len(res.Rows) != want ||
			res.Truncated != c.truncated {
			t.Errorf("produced %d: rows=%d truncated=%v status=%s, want %d/%v",
				c.produced, len(res.Rows), res.Truncated, res.Status, want,
				c.truncated)
		}
	}
}

func TestRunner_ByteCap(t *testing.T) {
	pool, ctx := livePool(t)
	r := testRunner(t, pool, testSpec("bytes",
		"SELECT repeat('x', 2048) AS blob FROM generate_series(1, 400) LIMIT $1",
		func(s *Spec) { s.MaxRows = 500 }))
	res := r.Run(ctx, "bytes", Args{})
	if res.Status != StatusOK || !res.Truncated {
		t.Fatalf("status=%s truncated=%v, want ok and truncated", res.Status,
			res.Truncated)
	}
	raw, err := res.Payload()
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if len(raw) > 64<<10 || len(res.Rows) == 0 || len(res.Rows) >= 400 {
		t.Fatalf("payload %d bytes with %d rows, want <= 64 KiB and some rows",
			len(raw), len(res.Rows))
	}
}

// The statement would sleep 30 s; the 150 ms timeout must end it. The
// 10 s budget leaves room for a loaded runner (-race, a busy server)
// while staying far below the sleep.
func TestRunner_StatementTimeout(t *testing.T) {
	pool, ctx := livePool(t)
	r := testRunner(t, pool, testSpec("slow", "SELECT pg_sleep(30) AS s LIMIT $1",
		func(s *Spec) { s.StatementTimeout = 150 * time.Millisecond }))
	start := time.Now()
	res := r.Run(ctx, "slow", Args{})
	if res.Status != StatusError || res.Reason != "statement_timeout" {
		t.Fatalf("result = %+v, want error/statement_timeout", res)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("timed-out probe took %s", el)
	}
}

func TestRunner_LockTimeout(t *testing.T) {
	pool, ctx := livePool(t)
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS sre_probe_locked; "+
		"CREATE TABLE sre_probe_locked (id int)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS sre_probe_locked")
	})
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx,
		"LOCK TABLE sre_probe_locked IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	r := testRunner(t, pool, testSpec("locked",
		"SELECT count(*) AS n FROM public.sre_probe_locked LIMIT $1",
		func(s *Spec) { s.LockTimeout = 50 * time.Millisecond }))
	res := r.Run(ctx, "locked", Args{})
	if res.Status != StatusError || res.Reason != "lock_timeout" {
		t.Fatalf("result = %+v, want error/lock_timeout", res)
	}
}

func TestRunner_ReadOnlyTransaction(t *testing.T) {
	pool, ctx := livePool(t)
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS sre_probe_ro; "+
		"CREATE TABLE sre_probe_ro (id int)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS sre_probe_ro")
	})
	r := testRunner(t, pool, testSpec("write", `WITH w AS (INSERT INTO
		public.sre_probe_ro VALUES (1) RETURNING id) SELECT id FROM w LIMIT $1`))
	res := r.Run(ctx, "write", Args{})
	if res.Status != StatusError || res.Reason != "read_only_violation" {
		t.Fatalf("result = %+v, want error/read_only_violation", res)
	}
	var n int
	err := pool.QueryRow(ctx, "SELECT count(*) FROM sre_probe_ro").Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("rows written by a probe = %d (%v)", n, err)
	}
}

func TestRunner_SessionSettingsAreFixed(t *testing.T) {
	pool, ctx := livePool(t)
	r := testRunner(t, pool, testSpec("settings", `SELECT
		current_setting('search_path') AS search_path,
		current_setting('statement_timeout') AS statement_timeout,
		current_setting('lock_timeout') AS lock_timeout,
		current_setting('transaction_read_only') AS read_only LIMIT $1`))
	res := r.Run(ctx, "settings", Args{})
	if res.Status != StatusOK {
		t.Fatalf("result = %+v", res)
	}
	row := res.Rows[0]
	if row["search_path"] != "pg_catalog, pg_temp" ||
		row["statement_timeout"] != "400ms" || row["lock_timeout"] != "100ms" ||
		row["read_only"] != "on" {
		t.Fatalf("session settings = %#v", row)
	}
	var sp string
	if err := pool.QueryRow(ctx, "SELECT current_setting('search_path')").
		Scan(&sp); err != nil || sp == "pg_catalog, pg_temp" {
		t.Fatalf("probe settings leaked into the pool: %q (%v)", sp, err)
	}
}

// Error propagation: a role without privileges gets no_privilege, not an
// empty (healthy) answer.
func TestRunner_NoPrivilegeIsTyped(t *testing.T) {
	pool, ctx := livePool(t)
	bootstrapQueryStore(t, ctx, pool)
	role := fmt.Sprintf("sre_probe_np_%d", os.Getpid())
	ident := pgx.Identifier{role}.Sanitize()
	if _, err := pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident+
		"; CREATE ROLE "+ident+" LOGIN PASSWORD 'sre-probe-test'"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+ident)
	})
	u, _ := url.Parse(os.Getenv(testdb.EnvName))
	u.User = url.UserPassword(role, "sre-probe-test")
	restricted, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect restricted: %v", err)
	}
	t.Cleanup(restricted.Close)
	r := NewRunner(restricted, Catalog(), NewLimiter(1))
	res := r.Run(ctx, PlanRegressions, Args{})
	if res.Status != StatusNoPrivilege || res.Reason != "insufficient_privilege" {
		t.Fatalf("result = %+v, want no_privilege", res)
	}
	if len(res.Rows) != 0 {
		t.Fatal("a denied probe returned rows")
	}
}

func bootstrapQueryStore(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS sage;
		CREATE TABLE IF NOT EXISTS sage.query_store (id bigserial PRIMARY KEY,
		captured_at timestamptz NOT NULL DEFAULT now(), queryid bigint NOT NULL,
		calls bigint NOT NULL, total_exec_time double precision NOT NULL,
		mean_exec_time double precision NOT NULL, rows bigint NOT NULL DEFAULT 0,
		plan_hash text, stats_epoch timestamptz)`); err != nil {
		t.Fatalf("query_store: %v", err)
	}
}

// Error propagation: a missing extension or sage table is unsupported.
func TestRunner_MissingExtensionAndTableAreUnsupported(t *testing.T) {
	_, ctx := livePool(t)
	fresh, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "sre_probe_fresh"))
	if err != nil {
		t.Fatalf("connect fresh: %v", err)
	}
	t.Cleanup(fresh.Close)
	r := NewRunner(fresh, mustTestRegistry(t, testSpec("hypopg",
		"SELECT count(*) AS n FROM hypopg_list_indexes LIMIT $1")), NewLimiter(1))
	res := r.Run(ctx, "hypopg", Args{})
	if res.Status != StatusUnsupported || res.Reason != "undefined_table" {
		t.Fatalf("missing extension = %+v, want unsupported", res)
	}
	cat := NewRunner(fresh, Catalog(), NewLimiter(1))
	res = cat.Run(ctx, PlanRegressions, Args{})
	if res.Status != StatusUnsupported || res.Reason != "undefined_table" {
		t.Fatalf("missing sage.query_store = %+v, want unsupported", res)
	}
}

func mustTestRegistry(t *testing.T, specs ...Spec) *Registry {
	t.Helper()
	reg, err := NewRegistry(specs...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

func TestRunner_VersionGate(t *testing.T) {
	pool, ctx := livePool(t)
	r := testRunner(t, pool, testSpec("future", "SELECT 1 AS a LIMIT $1",
		func(s *Spec) { s.Variants[0].MinVersion = 990000 }))
	res := r.Run(ctx, "future", Args{})
	if res.Status != StatusUnsupported || res.Reason != "pg_version" {
		t.Fatalf("result = %+v, want unsupported/pg_version", res)
	}
}

func TestRunner_InvalidCalls(t *testing.T) {
	pool, ctx := livePool(t)
	r := NewRunner(pool, Catalog(), NewLimiter(1))
	if res := r.Run(ctx, "drop_table", Args{}); res.Status != StatusError ||
		res.Reason != "unknown_probe" {
		t.Errorf("unknown probe = %+v", res)
	}
	if res := r.Run(ctx, BackendIdentity, Args{}); res.Status != StatusError ||
		res.Reason != "invalid_args" {
		t.Errorf("missing backend args = %+v", res)
	}
	if res := r.Run(ctx, LockGraph, Args{PID: 1}); res.Status != StatusError ||
		res.Reason != "invalid_args" {
		t.Errorf("unexpected args = %+v", res)
	}
	var nilRunner *Runner
	if res := nilRunner.Run(ctx, LockGraph, Args{}); res.Status != StatusError ||
		res.Reason != "not_configured" {
		t.Errorf("nil runner = %+v", res)
	}
	if res := NewRunner(nil, Catalog(), nil).Run(ctx, LockGraph, Args{}); res.Status !=
		StatusError || res.Reason != "not_configured" {
		t.Errorf("nil pool = %+v", res)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if res := r.Run(canceled, LockGraph, Args{}); res.Status != StatusError ||
		(res.Reason != "canceled" && res.Reason != "concurrency_limit") {
		t.Errorf("canceled context = %+v", res)
	}
}

// maxConcurrent polls pg_stat_activity for active marker statements
// until stop is closed and returns the highest count seen.
func maxConcurrent(
	t *testing.T, pool *pgxpool.Pool, marker string, stop <-chan struct{},
) *atomic.Int32 {
	t.Helper()
	var peak atomic.Int32
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			var n int32
			_ = pool.QueryRow(context.Background(), `SELECT count(*)::int4
				FROM pg_stat_activity WHERE state = 'active'
				AND pid <> pg_backend_pid() AND query LIKE $1`,
				"%"+marker+"%").Scan(&n)
			for {
				cur := peak.Load()
				if n <= cur || peak.CompareAndSwap(cur, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return &peak
}

func sleepSpec(id ID, marker string) Spec {
	return testSpec(id, "SELECT pg_sleep(0.25) AS s, '"+marker+"' AS m LIMIT $1")
}

func runConcurrently(ctx context.Context, n int, run func(i int) Result) []Result {
	out := make([]Result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); out[i] = run(i) }(i)
	}
	wg.Wait()
	return out
}

func TestRunner_OneProbePerDatabase(t *testing.T) {
	pool, ctx := livePool(t)
	marker := fmt.Sprintf("sre_perdb_%d", time.Now().UnixNano())
	r := testRunner(t, pool, sleepSpec("sleep", marker))
	stop := make(chan struct{})
	peak := maxConcurrent(t, pool, marker, stop)
	start := time.Now()
	results := runConcurrently(ctx, 3, func(int) Result {
		return r.Run(ctx, "sleep", Args{})
	})
	close(stop)
	for i, res := range results {
		if res.Status != StatusOK {
			t.Fatalf("probe %d = %+v", i, res)
		}
	}
	if p := peak.Load(); p != 1 {
		t.Fatalf("peak concurrent probes on one database = %d, want 1", p)
	}
	if el := time.Since(start); el < 700*time.Millisecond {
		t.Fatalf("3 x 250 ms probes finished in %s: not serialized", el)
	}
}

func TestRunner_SidecarWideLimit(t *testing.T) {
	pool, ctx := livePool(t)
	marker := fmt.Sprintf("sre_global_%d", time.Now().UnixNano())
	reg := mustTestRegistry(t, sleepSpec("sleep", marker))
	global := NewLimiter(2)
	runners := make([]*Runner, 4)
	for i := range runners {
		runners[i] = NewRunner(pool, reg, global)
	}
	stop := make(chan struct{})
	peak := maxConcurrent(t, pool, marker, stop)
	results := runConcurrently(ctx, 4, func(i int) Result {
		return runners[i].Run(ctx, "sleep", Args{})
	})
	close(stop)
	for i, res := range results {
		if res.Status != StatusOK {
			t.Fatalf("probe %d = %+v", i, res)
		}
	}
	if p := peak.Load(); p < 1 || p > 2 {
		t.Fatalf("peak concurrent probes across the sidecar = %d, want 1..2", p)
	}
}

// A probe queued behind the sidecar-wide limit gives up at its own
// deadline. The test holds the only slot itself (a busy probe started
// first, after a fixed pause, could lose the slot to the queued one on a
// loaded runner) and frees it after 5 s; the 2 s budget leaves room for
// load while a probe that ignores its deadline would wait for the slot.
func TestRunner_QueueWaitIsBounded(t *testing.T) {
	pool, ctx := livePool(t)
	marker := fmt.Sprintf("sre_wait_%d", time.Now().UnixNano())
	global := NewLimiter(1)
	idle := NewRunner(pool, mustTestRegistry(t, sleepSpec("sleep", marker)), global)
	if err := global.acquire(ctx); err != nil {
		t.Fatalf("hold the slot: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(5 * time.Second)
		global.release()
	}()
	short, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := idle.Run(short, "sleep", Args{})
	el := time.Since(start)
	<-released
	if res.Status != StatusError || res.Reason != "concurrency_limit" {
		t.Fatalf("queued probe = %+v, want error/concurrency_limit", res)
	}
	if el > 2*time.Second {
		t.Fatalf("queued probe waited %s past its 80 ms deadline", el)
	}
}

func TestNewLimiter_ClampsToCeiling(t *testing.T) {
	if got := NewLimiter(0).Size(); got != 1 {
		t.Errorf("NewLimiter(0).Size() = %d, want 1", got)
	}
	if got := NewLimiter(-5).Size(); got != 1 {
		t.Errorf("NewLimiter(-5).Size() = %d, want 1", got)
	}
	if got := NewLimiter(MaxSidecarConcurrency + 10).Size(); got != MaxSidecarConcurrency {
		t.Errorf("oversized limiter = %d, want %d", got, MaxSidecarConcurrency)
	}
}
