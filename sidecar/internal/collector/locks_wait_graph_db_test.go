package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// collection-01 on a live server: the locks category is a bounded wait
// graph (waiters, their blockers, idle-in-transaction sessions and the
// oldest xmin holders) with redacted statement text.

const lockSecret = "secret-123"

// graphConn opens one session on dsn that lives until the test ends.
func graphConn(t *testing.T, dsn string) (*pgx.Conn, int) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	return conn, pid
}

// idleInTx leaves a new session idle in transaction after stmts.
func idleInTx(t *testing.T, dsn string, stmts ...string) int {
	t.Helper()
	conn, pid := graphConn(t, dsn)
	for _, s := range append([]string{"BEGIN"}, stmts...) {
		if _, err := conn.Exec(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return pid
}

// waiter runs sql on a new session in the background until the test ends.
func waiter(t *testing.T, dsn, sql string) int {
	t.Helper()
	conn, pid := graphConn(t, dsn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = conn.Exec(ctx, sql)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return pid
}

// awaitLockWaits waits until n sessions of this database wait on a lock.
func awaitLockWaits(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(context.Background(), `SELECT count(*)
			FROM pg_stat_activity WHERE datname = current_database()
			  AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("count lock waits: %v", err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions wait on a lock, want %d", waiting, n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func rowsOf(locks []LockInfo, pid int) []LockInfo {
	var out []LockInfo
	for _, l := range locks {
		if l.PID == pid {
			out = append(out, l)
		}
	}
	return out
}

func graphPool(t *testing.T, label string) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, dsn
}

// CHECK-01, CHECK-02: 50 sessions each holding 20 AccessShareLocks (1,000
// relation locks) store at most 200 rows per cycle, and no stored text
// holds the literal of WHERE id = 'secret-123'.
func TestLocksWaitGraph_ManyLocksStoreBoundedRedactedRows(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.CreateDatabase(t, "locks_many")
	pool := bootstrappedPool(t, ctx, dsn)
	var from []string
	for i := range 20 {
		name := fmt.Sprintf("lk_%02d", i) // short: the statement fits the 256-byte cut
		if _, err := pool.Exec(ctx, "CREATE TABLE "+name+" (id text)"); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		from = append(from, name)
	}
	stmt := "SELECT count(*) FROM " + strings.Join(from, ", ") +
		" WHERE lk_00.id = '" + lockSecret + "'"
	for range 50 {
		idleInTx(t, dsn, stmt)
	}
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	start := time.Now().UTC().Truncate(time.Second)
	for cycle := range 2 {
		snap, err := c.collect(ctx)
		if err != nil {
			t.Fatalf("collect %d: %v", cycle, err)
		}
		if len(snap.Locks) < 50 || len(snap.Locks) > lockRowCap {
			t.Fatalf("cycle %d kept %d lock rows, want the 50 idle sessions and at most %d",
				cycle, len(snap.Locks), lockRowCap)
		}
		snap.CollectedAt = start.Add(time.Duration(cycle) * time.Minute)
		if err := c.persist(ctx, snap); err != nil {
			t.Fatalf("persist %d: %v", cycle, err)
		}
	}
	var rows, maxLen, leaks, redacted int
	if err := pool.QueryRow(ctx, `SELECT count(*), max(jsonb_array_length(data)),
		count(*) FILTER (WHERE data::text LIKE '%' || $1 || '%'),
		count(*) FILTER (WHERE data::text LIKE '%lk_00.id = ''?''%')
		FROM sage.snapshots WHERE category = 'locks'`, lockSecret).
		Scan(&rows, &maxLen, &leaks, &redacted); err != nil {
		t.Fatalf("read locks rows: %v", err)
	}
	if rows != 2 || maxLen > lockRowCap || leaks != 0 || redacted != 2 {
		t.Fatalf("locks rows %d, largest %d elements, %d holding %q, %d redacted; "+
			"want 2 rows of at most %d elements, none holding it, both redacted",
			rows, maxLen, leaks, lockSecret, redacted, lockRowCap)
	}
}

// Happy path: an UPDATE blocked behind an idle-in-transaction holder is one
// waiter row naming the holder, and the holder's row carries its xmin age.
func TestLocksWaitGraph_BlockedUpdateNamesItsHolder(t *testing.T) {
	ctx := context.Background()
	pool, dsn := graphPool(t, "locks_blocked")
	if _, err := pool.Exec(ctx, `CREATE TABLE public.acct (id int PRIMARY KEY, v text);
		INSERT INTO public.acct VALUES (1, 'a')`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	holder := idleInTx(t, dsn, "UPDATE public.acct SET v = '"+lockSecret+"' WHERE id = 1")
	blocked := waiter(t, dsn, "UPDATE public.acct SET v = 'b' WHERE id = 1")
	awaitLockWaits(t, pool, 1)

	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	locks, dropped, err := c.collectLocks(ctx)
	if err != nil {
		t.Fatalf("collectLocks: %v", err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	w := rowsOf(locks, blocked)
	if len(w) != 1 || w[0].Granted || len(w[0].BlockedBy) != 1 ||
		w[0].BlockedBy[0] != holder || w[0].State == nil || *w[0].State != "active" {
		t.Fatalf("waiter rows = %+v, want one ungranted active row blocked by %d", w, holder)
	}
	h := rowsOf(locks, holder)
	if len(h) != 1 {
		t.Fatalf("holder rows = %+v, want one session row", h)
	}
	hr := h[0]
	if !hr.Granted || hr.LockType != "virtualxid" || hr.Mode != "ExclusiveLock" ||
		hr.State == nil || *hr.State != "idle in transaction" || hr.BlockedBy != nil {
		t.Errorf("holder row = %+v, want its granted virtualxid lock, idle in transaction", hr)
	}
	if hr.BackendXminAge == nil || *hr.BackendXminAge <= 0 || hr.XactStart == nil ||
		hr.BackendStart == nil {
		t.Errorf("holder xmin age %v, xact start %v, backend start %v: want all set",
			hr.BackendXminAge, hr.XactStart, hr.BackendStart)
	}
	if hr.Query == nil || strings.Contains(*hr.Query, lockSecret) ||
		!strings.Contains(*hr.Query, "SET v = '?'") {
		t.Errorf("holder query = %v, want the statement with its literal redacted", hr.Query)
	}
}

// Nil/empty: with no other session the graph is [] and the category is
// available (known empty, not unknown).
func TestLocksWaitGraph_NoOtherSessionsIsEmptyList(t *testing.T) {
	ctx := context.Background()
	pool := bootstrappedPool(t, ctx, testdb.CreateDatabase(t, "locks_empty"))
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	locks, dropped, err := c.collectLocks(ctx)
	if err != nil {
		t.Fatalf("collectLocks: %v", err)
	}
	if locks == nil || len(locks) != 0 || dropped != 0 {
		t.Fatalf("locks = %#v (dropped %d), want an empty, non-nil list", locks, dropped)
	}
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !snap.Available("locks") || snap.Locks == nil {
		t.Fatalf("locks available %v, list %#v: want an available empty list",
			snap.Available("locks"), snap.Locks)
	}
	rows, err := snapshotRows(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Category == "locks" && string(r.Data) != "[]" {
			t.Fatalf("locks document = %s, want []", r.Data)
		}
	}
}

// Invalid input: query_id is NULL when compute_query_id is off; both
// shapes scan.
func TestLocksWaitGraph_NullQueryIDScans(t *testing.T) {
	ctx := context.Background()
	pool, dsn := graphPool(t, "locks_qid")
	off := idleInTx(t, dsn, "SET LOCAL compute_query_id = off", "SELECT 1 AS qid_off")
	on := idleInTx(t, dsn, "SET LOCAL compute_query_id = on", "SELECT 1 AS qid_on")
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	locks, _, err := c.collectLocks(ctx)
	if err != nil {
		t.Fatalf("collectLocks: %v", err)
	}
	offRows, onRows := rowsOf(locks, off), rowsOf(locks, on)
	if len(offRows) != 1 || offRows[0].QueryID != nil {
		t.Errorf("compute_query_id=off rows = %+v, want one row with a NULL query_id", offRows)
	}
	if len(onRows) != 1 || onRows[0].QueryID == nil {
		t.Errorf("compute_query_id=on rows = %+v, want one row with its query_id", onRows)
	}
}

// Invalid input: every nullable column of the graph scans as NULL.
func TestScanLocks_AllNullableColumnsNull(t *testing.T) {
	pool := testPool(t)
	rows, err := pool.Query(context.Background(), `SELECT 'session'::text, ''::text,
		true, NULL::name, NULL::text, NULL::text, NULL::text, NULL::text, 7,
		NULL::timestamptz, NULL::timestamptz, NULL::timestamptz, NULL::bigint,
		NULL::int[], NULL::bigint, NULL::timestamptz`)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := scanLocks(rows)
	if err != nil {
		t.Fatalf("scanLocks: %v", err)
	}
	if len(locks) != 1 {
		t.Fatalf("scanned %d rows, want 1", len(locks))
	}
	l := locks[0]
	if l.PID != 7 || l.LockType != "session" || l.Query != nil || l.RelName != nil ||
		l.QueryID != nil || l.BackendXminAge != nil || l.BlockedBy != nil ||
		l.XactStart != nil || l.StateChange != nil {
		t.Fatalf("scanned %+v, want NULLs kept as nil", l)
	}
}

// Error propagation: a failed read is the step's error (the category is
// then unavailable, not empty).
func TestCollectLocks_CancelledContextIsAnError(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	locks, dropped, err := c.collectLocks(ctx)
	if err == nil || locks != nil || dropped != 0 {
		t.Fatalf("collectLocks on a cancelled context = %v, %d, %v; want an error",
			locks, dropped, err)
	}
}

// CHECK-04 and the 201-row boundary: 210 waiters behind one holder are
// capped at 200 rows (the truncation is reported), the holder is kept,
// and pg_blocking_pids is fed at most 200 waiters.
func TestLocksWaitGraph_WaiterStormIsCapped(t *testing.T) {
	ctx := context.Background()
	pool, dsn := graphPool(t, "locks_storm")
	// Runs after the 211 sessions close, before the database is dropped:
	// the local port forwarder can refuse connections for a moment after
	// such a burst.
	t.Cleanup(func() { awaitReachable(t, dsn) })
	if _, err := pool.Exec(ctx, "CREATE TABLE public.hot (id int)"); err != nil {
		t.Fatal(err)
	}
	holder := idleInTx(t, dsn, "LOCK TABLE public.hot IN ACCESS EXCLUSIVE MODE")
	const waiters = 210
	for range waiters {
		waiter(t, dsn, "SELECT count(*) FROM public.hot")
	}
	awaitLockWaits(t, pool, waiters)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	locks, dropped, err := c.collectLocks(ctx)
	if err != nil {
		t.Fatalf("collectLocks: %v", err)
	}
	if len(locks) != lockRowCap || dropped != 1 {
		t.Fatalf("kept %d rows, dropped %d; want %d and 1", len(locks), dropped, lockRowCap)
	}
	h := rowsOf(locks, holder)
	if len(h) != 1 || h[0].State == nil || *h[0].State != "idle in transaction" {
		t.Fatalf("holder rows = %+v, want the blocker kept inside the cap", h)
	}
	assertWaiterSetCapped(t, pool)
}

// awaitReachable waits until dsn accepts a connection again.
func awaitReachable(t *testing.T, dsn string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := pgx.Connect(ctx, dsn)
		cancel()
		if err == nil {
			_ = conn.Close(context.Background())
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("server unreachable after the waiter storm: %v", err)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// assertWaiterSetCapped explains the graph statement and checks that the
// waiter CTE, which feeds pg_blocking_pids, returned at most 200 rows.
func assertWaiterSetCapped(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		"EXPLAIN (ANALYZE, FORMAT JSON) "+locksSQL).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var doc []map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("decode plan: %v", err)
	}
	found := map[string]float64{}
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		if name, ok := n["Subplan Name"].(string); ok {
			found[name], _ = n["Actual Rows"].(float64)
		}
		kids, _ := n["Plans"].([]any)
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				walk(m)
			}
		}
	}
	walk(doc[0]["Plan"].(map[string]any))
	for _, cte := range []string{"CTE waiting", "CTE waiters"} {
		rows, ok := found[cte]
		if !ok || rows > 200 || rows < 200 {
			t.Errorf("%s returned %v rows (present %v), want exactly 200 of 210 waiters; "+
				"plan CTEs %v", cte, rows, ok, found)
		}
	}
}
