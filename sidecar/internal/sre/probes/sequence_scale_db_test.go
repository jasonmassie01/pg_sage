package probes

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Dogfood lifeos-1: a database with 12,038 sequences (leftover test
// schemas) made sequence_runway time out on every pass. At that scale
// (and past the scan cap) the probe must answer inside the incident-time
// budget, report only used sequences nearest their limit first, say
// that it truncated and how much of the catalog it read.

const (
	scaleIntSeqs = 500
	scaleBigSeqs = 20000
	scaleBatch   = 1000
)

// createScaleFixture creates schema sch with scaleIntSeqs integer
// sequences (every 10th used, one at its limit) and scaleBigSeqs bigint
// sequences (every 10th used), then one used bigint sequence "zz_last"
// created last. Statements are batched so no transaction takes more
// than scaleBatch locks.
func createScaleFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	sch string) {
	t.Helper()
	q := pgx.Identifier{sch}.Sanitize()
	t.Cleanup(func() { dropScaleFixture(t, pool, sch) })
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+q); err != nil {
		t.Fatalf("schema: %v", err)
	}
	mk := func(prefix, typ string, from, to int) {
		_, err := pool.Exec(ctx, fmt.Sprintf(`DO $$ BEGIN
			FOR i IN %d..%d LOOP
				EXECUTE format('CREATE SEQUENCE %s.%s_%%s AS %s', i);
				IF i %% 10 = 0 THEN
					EXECUTE format('SELECT nextval(%%L)', '%s.%s_' || i);
				END IF;
			END LOOP; END $$`, from, to, q, prefix, typ, sch, prefix))
		if err != nil {
			t.Fatalf("create %s sequences %d..%d: %v", prefix, from, to, err)
		}
	}
	mk("i", "integer", 1, scaleIntSeqs)
	for from := 1; from <= scaleBigSeqs; from += scaleBatch {
		mk("b", "bigint", from, from+scaleBatch-1)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		SELECT setval('%[1]s.i_250', 2147483646);
		CREATE SEQUENCE %[1]s.zz_last;
		SELECT setval('%[1]s.zz_last', 9000000000000000000)`, q)); err != nil {
		t.Fatalf("near-limit sequences: %v", err)
	}
}

func dropScaleFixture(t *testing.T, pool *pgxpool.Pool, sch string) {
	q := pgx.Identifier{sch}.Sanitize()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for from := 1; from <= scaleBigSeqs; from += scaleBatch {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DO $$ BEGIN
			FOR i IN %d..%d LOOP
				EXECUTE format('DROP SEQUENCE IF EXISTS %s.b_%%s', i);
			END LOOP; END $$`, from, from+scaleBatch-1, q)); err != nil {
			t.Logf("drop bigint sequences from %d: %v", from, err)
		}
	}
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+q+" CASCADE"); err != nil {
		t.Logf("drop schema %s: %v", sch, err)
	}
}

func TestCatalog_SequenceRunwayAtScale(t *testing.T) {
	pool, ctx := livePool(t)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	sch := fmt.Sprintf("sre_seq_scale_%d", time.Now().UnixNano())
	createScaleFixture(t, ctx, pool, sch)
	// Warm the catalog once, as the monitor would. Standalone the probe
	// takes ~190 ms; under a starved shared CPU (the full suite next to
	// other agents) one run can miss the budget, so up to three runs are
	// made and one must answer inside it.
	_ = catalogRun(t, ctx, pool, SequenceRunwayProbe, Args{})
	var res Result
	for i := 0; i < 3; i++ {
		res = catalogRun(t, ctx, pool, SequenceRunwayProbe, Args{})
		if res.Status == StatusOK {
			break
		}
	}
	if res.Status != StatusOK || res.ElapsedMS >= MaxStatementTimeout.Milliseconds() {
		t.Fatalf("incident-budget run = %s/%s in %d ms (%s)", res.Status, res.Reason,
			res.ElapsedMS, res.Error)
	}
	t.Logf("sequence_runway over the scale fixture: %d ms (incident budget %s)",
		res.ElapsedMS, MaxStatementTimeout)
	ss, err := Sequences(res)
	if err != nil || len(ss) != 50 || !res.Truncated {
		t.Fatalf("sequences = %d (truncated %v, %v), want the 50 nearest", len(ss),
			res.Truncated, err)
	}
	for i, s := range ss {
		if math.IsNaN(s.LastValue) || math.IsNaN(s.Fraction) {
			t.Fatalf("row %d %s has no last value: never-used sequences are skipped", i,
				s.Sequence)
		}
		if i > 0 && s.Fraction > ss[i-1].Fraction {
			t.Fatalf("row %d (%v) after row %d (%v): not nearest the limit first", i,
				s.Fraction, i-1, ss[i-1].Fraction)
		}
	}
	checkScaleCoverage(t, pool, res, ss, sch)
	checkLockFootprint(t, ctx, pool)
}

func checkScaleCoverage(t *testing.T, pool *pgxpool.Pool, res Result, ss []SequenceRunway,
	sch string) {
	t.Helper()
	listed := map[string]bool{}
	for _, s := range ss {
		listed[s.Sequence] = true
	}
	if !listed[sch+".i_250"] {
		t.Fatalf("the integer sequence at its limit is not listed: %v", listed)
	}
	if listed[sch+".zz_last"] {
		t.Fatal("zz_last is past the scan cap (default bigint, created last) yet read")
	}
	if listed[sch+".i_251"] {
		t.Fatal("a never-used sequence is listed")
	}
	c, err := SequenceCoverageOf(res)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	wantScanned := min(int64(SequenceScanCap), lockBudget(t, pool))
	if c.Total < scaleIntSeqs+scaleBigSeqs+1 || c.Scanned != wantScanned ||
		!c.ScanCapped() || !c.Truncated || c.Reported != 50 {
		t.Fatalf("coverage = %+v, want the scan capped at %d of at least %d", c,
			wantScanned, scaleIntSeqs+scaleBigSeqs+1)
	}
	if c.Used < scaleIntSeqs/10 || c.Used >= c.Scanned {
		t.Fatalf("used = %d of %d scanned, want the used sequences only", c.Used,
			c.Scanned)
	}
}

// The background budget applies only to RunBackground; the incident-time
// Run keeps the spec's statement timeout.
func TestRunner_RunBackgroundUsesTheBackgroundBudget(t *testing.T) {
	pool, ctx := livePool(t)
	slow := testSpec("bg_slow", "SELECT pg_sleep(0.6) AS s LIMIT $1",
		func(s *Spec) { s.BackgroundTimeout = 1500 * time.Millisecond })
	plain := testSpec("bg_plain", "SELECT pg_sleep(0.6) AS s LIMIT $1")
	settings := testSpec("bg_settings", `SELECT
		current_setting('statement_timeout') AS statement_timeout,
		current_setting('lock_timeout') AS lock_timeout LIMIT $1`,
		func(s *Spec) { s.BackgroundTimeout = 1500 * time.Millisecond })
	r := testRunner(t, pool, slow, plain, settings)
	// Under heavy host load the client deadline (statement timeout + 1 s)
	// can win the race with the server's statement_timeout: both are the
	// incident budget expiring.
	if res := r.Run(ctx, "bg_slow", Args{}); res.Status != StatusError ||
		(res.Reason != "statement_timeout" && res.Reason != "deadline_exceeded") {
		t.Fatalf("incident-time run = %+v, want the budget to expire", res)
	}
	if res := r.RunBackground(ctx, "bg_slow", Args{}); res.Status != StatusOK {
		t.Fatalf("background run = %+v, want ok inside its 1.5 s budget", res)
	}
	if res := r.RunBackground(ctx, "bg_plain", Args{}); res.Reason != "statement_timeout" {
		t.Fatalf("background run without a background budget = %+v, want the "+
			"statement timeout", res)
	}
	res := r.RunBackground(ctx, "bg_settings", Args{})
	if res.Status != StatusOK || res.Rows[0]["statement_timeout"] != "1500ms" ||
		res.Rows[0]["lock_timeout"] != "100ms" {
		t.Fatalf("background settings = %+v", res)
	}
	var nilRunner *Runner
	if res := nilRunner.RunBackground(ctx, "bg_slow", Args{}); res.Status != StatusError ||
		res.Reason != "not_configured" {
		t.Fatalf("nil runner = %+v", res)
	}
	if res := r.RunBackground(ctx, "nope", Args{}); res.Reason != "unknown_probe" {
		t.Fatalf("unknown probe = %+v", res)
	}
}

// Catalog probes run with JIT off: compiling a catalog query costs more
// than it saves (lifeos: 10.6 ms of a 187 ms sequence_runway).
func TestRunner_DisablesJIT(t *testing.T) {
	pool, ctx := livePool(t)
	r := testRunner(t, pool, testSpec("jit", "SELECT current_setting('jit') AS jit LIMIT $1"))
	res := r.Run(ctx, "jit", Args{})
	if res.Status != StatusOK || res.Rows[0]["jit"] != "off" {
		t.Fatalf("jit = %+v, want off inside a probe", res)
	}
	var jit string
	if err := pool.QueryRow(ctx, "SELECT current_setting('jit')").Scan(&jit); err != nil ||
		jit == "off" && !jitOffByDefault(t, ctx, pool) {
		t.Fatalf("probe jit setting leaked into the pool: %q (%v)", jit, err)
	}
}

func jitOffByDefault(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bool {
	t.Helper()
	var boot string
	if err := pool.QueryRow(ctx, `SELECT boot_val FROM pg_settings
		WHERE name = 'jit'`).Scan(&boot); err != nil {
		t.Fatalf("jit boot value: %v", err)
	}
	return boot == "off"
}

// Another session's temporary sequence cannot be read (PostgreSQL
// raises an error); the probe must skip it, not fail.
func TestCatalog_SequenceRunwaySkipsOtherSessionsTempSequences(t *testing.T) {
	pool, ctx := livePool(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	name := fmt.Sprintf("sre_tmp_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE TEMP SEQUENCE "+name+
		" AS integer; SELECT setval('"+name+"', 2147483000)"); err != nil {
		t.Fatalf("temp sequence: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SEQUENCE "+name) }()
	res := catalogRun(t, ctx, pool, SequenceRunwayProbe, Args{})
	if !res.Status.Usable() {
		t.Fatalf("probe with another session's temp sequence = %+v", res)
	}
	ss, _ := Sequences(res)
	for _, s := range ss {
		if strings.HasSuffix(s.Sequence, "."+name) {
			t.Fatalf("another session's temp sequence listed: %+v", s)
		}
	}
}

// A sequence the role may not read is counted as unreadable, never
// listed as unused or failing the probe.
func TestCatalog_SequenceRunwayCountsUnreadableSequences(t *testing.T) {
	pool, ctx := livePool(t)
	restricted := runwayRestrictedPool(t, ctx, pool, "sre_seq_np")
	role := pgx.Identifier{fmt.Sprintf("sre_seq_np_%d", os.Getpid())}.Sanitize()
	sch := fmt.Sprintf("sre_seq_priv_%d", time.Now().UnixNano())
	q := pgx.Identifier{sch}.Sanitize()
	t.Cleanup(func() { // runs before the role is dropped
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+q+" CASCADE")
	})
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %[1]s;
		CREATE SEQUENCE %[1]s.secret_seq AS integer;
		SELECT setval('%[1]s.secret_seq', 2147483000);
		CREATE SEQUENCE %[1]s.open_seq AS integer;
		SELECT setval('%[1]s.open_seq', 2147483001);
		GRANT SELECT ON SEQUENCE %[1]s.open_seq TO %[2]s`, q, role)); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	res := catalogRun(t, ctx, restricted, SequenceRunwayProbe, Args{})
	ss, err := Sequences(res)
	if res.Status != StatusOK || err != nil {
		t.Fatalf("restricted probe = %+v (%v)", res, err)
	}
	listed := map[string]bool{}
	for _, s := range ss {
		listed[s.Sequence] = true
	}
	if listed[sch+".secret_seq"] || !listed[sch+".open_seq"] {
		t.Fatalf("restricted listing = %v, want open_seq only", listed)
	}
	c, err := SequenceCoverageOf(res)
	if err != nil || c.Unreadable < 1 || c.Used < 1 {
		t.Fatalf("coverage = %+v (%v), want secret_seq counted unreadable", c, err)
	}
	admin, err := SequenceCoverageOf(catalogRun(t, ctx, pool, SequenceRunwayProbe, Args{}))
	if err != nil || admin.Unreadable != 0 || admin.Used < 2 {
		t.Fatalf("superuser coverage = %+v (%v), want nothing unreadable", admin, err)
	}
}

// lockBudget is the share of the shared lock table one sequence_runway
// run may take: the nominal table (max_locks_per_transaction x
// (max_connections + max_prepared_transactions)) divided by 4.
func lockBudget(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT
		current_setting('max_locks_per_transaction')::int8 *
		(current_setting('max_connections')::int8 +
		 current_setting('max_prepared_transactions')::int8) / 4`).Scan(&n); err != nil {
		t.Fatalf("lock budget: %v", err)
	}
	return n
}

// Dogfood lifeos-1, found on the PG14/PG18 matrix (max_connections 100):
// reading 20,000 last values took 20,000 locks and other sessions got
// "out of shared memory". The probe's locks stay within its lock budget.
func checkLockFootprint(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, sequenceRunwaySQL, 51)
	if err != nil {
		t.Fatalf("probe sql: %v", err)
	}
	rows.Close()
	if rows.Err() != nil {
		t.Fatalf("probe rows: %v", rows.Err())
	}
	var held int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_locks
		WHERE pid = pg_backend_pid()`).Scan(&held); err != nil {
		t.Fatalf("locks: %v", err)
	}
	if budget := lockBudget(t, pool); held > budget+100 {
		t.Fatalf("probe transaction holds %d locks, budget %d", held, budget)
	}
}
