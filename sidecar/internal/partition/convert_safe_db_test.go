package partition

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Conversion safety (perf-storage follow-up): lifeos has a 9.3 GB
// sage.snapshots and an 852 MB sage.query_store. The heap is read only
// outside the ACCESS EXCLUSIVE window (VALIDATE CONSTRAINT under SHARE UPDATE
// EXCLUSIVE), no row is copied, and every failure leaves the plain table as
// it was, without the cutover CHECK or the pre-built key.

// fill inserts n rows dated over the last 30 days.
func fill(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tbl Table, n int) {
	t.Helper()
	exec(t, ctx, pool, fmt.Sprintf(`INSERT INTO sage.%s (at, v)
		SELECT now() - (g %% 720) * interval '1 hour', g FROM generate_series(1, %d) g`,
		tbl.Name, n))
	exec(t, ctx, pool, "ANALYZE sage."+tbl.Name)
}

// withHook installs convertHook for one test.
func withHook(t *testing.T, hook func(context.Context, DB, string) error) {
	t.Helper()
	convertHook = hook
	t.Cleanup(func() { convertHook = nil })
}

func requireForceFlush(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if count(t, ctx, pool, "SELECT current_setting('server_version_num')::int") < 150000 {
		t.Skip("pg_stat_force_next_flush() needs PostgreSQL 15+")
	}
}

// blocksFetched is how many heap blocks of rel were requested (hit or read).
func blocksFetched(t *testing.T, ctx context.Context, pool *pgxpool.Pool, oid uint32) int64 {
	t.Helper()
	return count(t, ctx, pool, "SELECT pg_stat_get_blocks_fetched($1::oid)", oid)
}

func relOID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rel string) uint32 {
	t.Helper()
	var oid uint32
	if err := pool.QueryRow(ctx, "SELECT to_regclass($1)::oid", rel).Scan(&oid); err != nil {
		t.Fatalf("oid of %s: %v", rel, err)
	}
	return oid
}

// assertPlainAndClean checks a failed conversion left nothing behind, and
// that a later conversion still succeeds.
func assertPlainAndClean(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tbl Table) {
	t.Helper()
	if k := relkind(t, ctx, pool, "sage."+tbl.Name); k != "r" {
		t.Fatalf("relkind = %q after a failed conversion, want the plain table", k)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM pg_constraint
		WHERE conname LIKE $1`, tbl.Name+"%cutover%"); n != 0 {
		t.Fatalf("%d cutover constraints left behind", n)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM pg_class
		WHERE relnamespace = 'sage'::regnamespace AND relname LIKE $1`,
		tbl.Name+"%cutover%"); n != 0 {
		t.Fatalf("%d cutover indexes left behind", n)
	}
	for _, rel := range []string{tbl.HistoryName(), tbl.DefaultName()} {
		if n := count(t, ctx, pool, "SELECT count(*) FROM pg_class WHERE oid = to_regclass($1)",
			"sage."+rel); n != 0 {
			t.Fatalf("sage.%s left behind", rel)
		}
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND database = (SELECT oid
		FROM pg_database WHERE datname = current_database())`); n != 0 {
		t.Fatalf("%d advisory locks still held", n)
	}
	convertHook = nil
	converted(t, ctx, pool, tbl)
}

// The whole point: ATTACH must find the bound proven by the validated
// CHECK and the key already built, so the exclusive transaction reads no
// heap block of the old table (before: one full scan under the lock).
func TestConvert_ExclusiveWindowReadsNoHeap(t *testing.T) {
	pool, ctx := requireDB(t)
	requireForceFlush(t, ctx, pool)
	for _, key := range [][]string{nil, {"id"}} {
		t.Run(fmt.Sprintf("key=%v", key), func(t *testing.T) {
			tbl := scratch(t, ctx, pool, key)
			fill(t, ctx, pool, tbl, 60000)
			oid := relOID(t, ctx, pool, "sage."+tbl.Name)
			pages := count(t, ctx, pool, "SELECT relpages FROM pg_class WHERE oid = $1", oid)
			var before, after int64
			withHook(t, func(ctx context.Context, db DB, phase string) error {
				if phase != "exclusive" && phase != "committed" {
					return nil
				}
				if _, err := db.Exec(ctx, "SELECT pg_stat_force_next_flush()"); err != nil {
					return err
				}
				if phase == "exclusive" {
					before = blocksFetched(t, ctx, pool, oid)
				} else {
					after = blocksFetched(t, ctx, pool, oid)
				}
				return nil
			})
			converted(t, ctx, pool, tbl)
			if after == 0 || pages < 100 {
				t.Fatalf("hook did not measure (after %d) or fixture too small (%d pages)",
					after, pages)
			}
			if d := after - before; d > 8 {
				t.Fatalf("exclusive window read %d heap blocks of a %d-page table; "+
					"want none (no validation scan, no index build)", d, pages)
			}
		})
	}
}

// Disk headroom: the old table becomes the history partition as is (same
// relfilenode: no rewrite) and no row moves, also when rows are dated a
// few days ahead (the cutover moves past them instead).
func TestConvert_CopiesNoRows(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, []string{"id"})
	fill(t, ctx, pool, tbl, 5000)
	ahead := DayStart(time.Now()).Add(3*day + 5*time.Hour)
	exec(t, ctx, pool, "INSERT INTO sage."+tbl.Name+" (at, v) VALUES ($1, 1)", ahead)
	var node, toast int64
	if err := pool.QueryRow(ctx, `SELECT relfilenode, COALESCE((SELECT relfilenode
		FROM pg_class t WHERE t.oid = c.reltoastrelid), 0) FROM pg_class c
		WHERE oid = to_regclass($1)`, "sage."+tbl.Name).Scan(&node, &toast); err != nil {
		t.Fatal(err)
	}
	res, err := Convert(ctx, pool, tbl)
	if err != nil || !res.Converted {
		t.Fatalf("Convert = %+v, %v", res, err)
	}
	if want := DayStart(ahead).Add(2 * day); !res.Cut.Equal(want) {
		t.Fatalf("cut = %s, want %s (two days past the newest row)", res.Cut, want)
	}
	var histNode, histToast int64
	if err := pool.QueryRow(ctx, `SELECT relfilenode, COALESCE((SELECT relfilenode
		FROM pg_class t WHERE t.oid = c.reltoastrelid), 0) FROM pg_class c
		WHERE oid = to_regclass($1)`, "sage."+tbl.HistoryName()).
		Scan(&histNode, &histToast); err != nil {
		t.Fatal(err)
	}
	if histNode != node || histToast != toast {
		t.Fatalf("history relfilenode %d/%d, plain table was %d/%d: the table was rewritten",
			histNode, histToast, node, toast)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM ONLY sage."+tbl.DefaultName()); n != 0 {
		t.Fatalf("%d rows copied into the default partition, want none", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM ONLY sage."+tbl.HistoryName()); n != 5001 {
		t.Fatalf("history holds %d rows, want all 5001", n)
	}
}

// Rows written between VALIDATE and the exclusive lock must satisfy the
// CHECK: the cutover is the second UTC midnight after now, at least a day
// away.
func TestConvert_CutLeavesADayOfMargin(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	fill(t, ctx, pool, tbl, 100)
	res, err := Convert(ctx, pool, tbl)
	if err != nil || !res.Converted {
		t.Fatalf("Convert = %+v, %v", res, err)
	}
	var now time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	if want := DayStart(now).Add(2 * day); !res.Cut.Equal(want) {
		t.Fatalf("cut = %s, want %s", res.Cut, want)
	}
	if res.LockHeld <= 0 || res.LockHeld > LockTimeout {
		t.Fatalf("lock held %s, want a brief positive hold", res.LockHeld)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM pg_constraint WHERE conname LIKE $1",
		tbl.Name+"%cutover%"); n != 0 {
		t.Fatalf("%d cutover constraints left on the partitioned table", n)
	}
}

// A row dated more than a week ahead is a broken clock; partitioning
// around it would keep every new row in the history partition.
func TestConvert_RowsFarAheadAreRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	fill(t, ctx, pool, tbl, 100)
	exec(t, ctx, pool, "INSERT INTO sage."+tbl.Name+" (at, v) VALUES (now() + interval '10 days', 1)")
	_, err := Convert(ctx, pool, tbl)
	if !errors.Is(err, ErrFutureRows) {
		t.Fatalf("Convert = %v, want ErrFutureRows", err)
	}
	if h := Hint(err); !strings.Contains(h, "clock") {
		t.Fatalf("hint %q does not mention the clock", h)
	}
	exec(t, ctx, pool, "DELETE FROM sage."+tbl.Name+" WHERE at > now() + interval '7 days'")
	assertPlainAndClean(t, ctx, pool, tbl)
}

// holdShareLock keeps ACCESS SHARE on rel (a reader in a long transaction)
// until the returned func is called.
func holdShareLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rel string) func() {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM "+rel+" LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback(context.Background()) }) }
	t.Cleanup(release)
	return release
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestConvert_LockTimeoutAtCutoverLeavesPlainTable(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, []string{"id"})
	fill(t, ctx, pool, tbl, 2000)
	var release func()
	withHook(t, func(ctx context.Context, _ DB, phase string) error {
		if phase == "exclusive" {
			// A reader that finishes after the lock timeout, before cleanup gives up.
			release = holdShareLock(t, ctx, pool, "sage."+tbl.Name)
			time.AfterFunc(LockTimeout+500*time.Millisecond, release)
		}
		return nil
	})
	start := time.Now()
	_, err := Convert(ctx, pool, tbl)
	release()
	if pgCode(err) != "55P03" {
		t.Fatalf("Convert = %v, want lock_not_available", err)
	}
	if el := time.Since(start); el > LockTimeout+10*time.Second {
		t.Fatalf("gave up after %s, want about the %s lock timeout", el, LockTimeout)
	}
	if h := Hint(err); !strings.Contains(h, "lock") {
		t.Fatalf("hint %q does not mention the lock", h)
	}
	assertPlainAndClean(t, ctx, pool, tbl)
}

func TestConvert_LockTimeoutAddingCheckLeavesPlainTable(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	fill(t, ctx, pool, tbl, 2000)
	release := holdShareLock(t, ctx, pool, "sage."+tbl.Name)
	_, err := Convert(ctx, pool, tbl)
	release()
	if pgCode(err) != "55P03" {
		t.Fatalf("Convert = %v, want lock_not_available", err)
	}
	assertPlainAndClean(t, ctx, pool, tbl)
}

func TestConvert_StatementTimeoutValidatingLeavesPlainTable(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, []string{"id"})
	fill(t, ctx, pool, tbl, 200000)
	withHook(t, func(ctx context.Context, db DB, phase string) error {
		if phase != "validate" {
			return nil
		}
		_, err := db.Exec(ctx, "SET statement_timeout = '1ms'")
		return err
	})
	_, err := Convert(ctx, pool, tbl)
	if pgCode(err) != "57014" {
		t.Fatalf("Convert = %v, want a statement timeout", err)
	}
	if h := Hint(err); !strings.Contains(h, "took longer") {
		t.Fatalf("hint %q does not explain the timeout", h)
	}
	assertPlainAndClean(t, ctx, pool, tbl)
}

// A full disk (or any error) inside the exclusive transaction rolls it
// back; the CHECK and the pre-built key made before it are removed.
func TestConvert_DiskFullInsideTransactionLeavesPlainTable(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, []string{"id"})
	fill(t, ctx, pool, tbl, 2000)
	withHook(t, func(_ context.Context, _ DB, phase string) error {
		if phase == "attach" {
			return &pgconn.PgError{Code: "53100",
				Message: "could not extend file: No space left on device"}
		}
		return nil
	})
	_, err := Convert(ctx, pool, tbl)
	if pgCode(err) != "53100" {
		t.Fatalf("Convert = %v, want the injected disk-full error", err)
	}
	if h := Hint(err); !strings.Contains(h, "disk") {
		t.Fatalf("hint %q does not mention disk", h)
	}
	assertPlainAndClean(t, ctx, pool, tbl)
}

// Bootstrap runs under a deadline: a cancelled context mid-conversion
// still removes the CHECK (cleanup runs on its own bounded context).
func TestConvert_CancelledContextStillCleansUp(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, []string{"id"})
	fill(t, ctx, pool, tbl, 2000)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	withHook(t, func(_ context.Context, _ DB, phase string) error {
		if phase == "index" {
			cancel()
		}
		return nil
	})
	if _, err := Convert(cctx, pool, tbl); err == nil {
		t.Fatal("Convert succeeded on a cancelled context")
	}
	assertPlainAndClean(t, ctx, pool, tbl)
}

// A process that died mid-conversion leaves its CHECK (which would refuse
// every row once the clock passes the cutover) and maybe an invalid key.
func TestCleanup_RemovesLeftoversOfACrashedConversion(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, []string{"id"})
	fill(t, ctx, pool, tbl, 100)
	leftovers := func() {
		exec(t, ctx, pool, fmt.Sprintf(`ALTER TABLE sage.%[1]s ADD CONSTRAINT %[1]s_cutover_check
			CHECK (at IS NOT NULL AND at < now() + interval '1 day') NOT VALID;
			CREATE UNIQUE INDEX %[1]s_cutover_key ON sage.%[1]s (id, at)`, tbl.Name))
	}
	leftovers()
	// Another session converting the table owns them: leave them alone.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))",
		"sage.partition.convert."+tbl.Name); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(ctx, pool, tbl); err != nil {
		t.Fatalf("Cleanup while another session converts: %v", err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM pg_constraint WHERE conname = $1",
		tbl.Name+"_cutover_check"); n != 1 {
		t.Fatal("Cleanup removed the CHECK of a conversion in progress")
	}
	if _, err := Convert(ctx, pool, tbl); !errors.Is(err, ErrBusy) {
		t.Fatalf("Convert while another session converts = %v, want ErrBusy", err)
	}
	_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock_all()")
	conn.Release()
	if err := Cleanup(ctx, pool, tbl); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM pg_constraint WHERE conname LIKE $1",
		tbl.Name+"%cutover%"); n != 0 {
		t.Fatal("Cleanup left the CHECK")
	}
	// Convert also clears leftovers before it starts.
	leftovers()
	converted(t, ctx, pool, tbl)
}

// pg_sage's writers keep inserting while the table is converted; none of
// their statements fails, none waits longer than the lock timeout, and
// every row is there afterwards.
func TestConvert_ConcurrentWriterNeverFails(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, []string{"id"})
	fill(t, ctx, pool, tbl, 60000)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var inserted int64
	var worst time.Duration
	var werr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			if _, err := pool.Exec(ctx, "INSERT INTO sage."+tbl.Name+" (v) VALUES (-1)"); err != nil {
				werr = err
				return
			}
			inserted++
			worst = max(worst, time.Since(start))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	converted(t, ctx, pool, tbl)
	close(stop)
	wg.Wait()
	if werr != nil {
		t.Fatalf("a writer failed during the conversion: %v", werr)
	}
	if worst > LockTimeout {
		t.Fatalf("a writer waited %s, longer than the %s lock timeout", worst, LockTimeout)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sage."+tbl.Name+" WHERE v = -1"); n != inserted {
		t.Fatalf("%d writer rows present, %d inserted", n, inserted)
	}
}

// Bootstrap hands Convert its own session: the session's timeouts come
// back as they were and no advisory lock stays held.
func TestConvert_RestoresSessionSettings(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET lock_timeout = '7s'; SET statement_timeout = '9s'"); err != nil {
		t.Fatal(err)
	}
	res, err := Convert(ctx, conn, tbl)
	if err != nil || !res.Converted {
		t.Fatalf("Convert = %+v, %v", res, err)
	}
	var lock, stmt string
	var held int64
	if err := conn.QueryRow(ctx, `SELECT current_setting('lock_timeout'),
		current_setting('statement_timeout'), (SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND pid = pg_backend_pid())`).
		Scan(&lock, &stmt, &held); err != nil {
		t.Fatal(err)
	}
	if lock != "7s" || stmt != "9s" || held != 0 {
		t.Fatalf("after Convert: lock_timeout %s, statement_timeout %s, %d advisory locks",
			lock, stmt, held)
	}
}
