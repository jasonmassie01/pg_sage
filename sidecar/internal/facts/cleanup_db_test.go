package facts

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The cleanup batch run as an operator runs it: psql autocommit, one
// statement at a time, carrying on past a failing statement (psql's
// default ON_ERROR_STOP off).

// splitScript splits a script into statements like psql: on ';' outside
// quotes, dollar quotes and line comments.
func splitScript(script string) []string {
	var out []string
	start := 0
	for i := 0; i < len(script); i++ {
		switch {
		case strings.HasPrefix(script[i:], "--"):
			i += max(strings.IndexByte(script[i:], '\n'), 0)
		case script[i] == '\'':
			i = quoteEnd(script, i)
		case script[i] == '$':
			i = dollarQuoteEnd(script, i)
		case script[i] == ';':
			if stmt := strings.TrimSpace(script[start : i+1]); stmt != ";" {
				out = append(out, stmt)
			}
			start = i + 1
		}
	}
	return out
}

// quoteEnd is the index of the quote closing the string opened at i.
func quoteEnd(script string, i int) int {
	for j := i + 1; j < len(script); j++ {
		if script[j] != '\'' {
			continue
		}
		if j+1 < len(script) && script[j+1] == '\'' {
			j++
			continue
		}
		return j
	}
	return len(script)
}

// dollarQuoteEnd is the index of the last byte of the dollar quote opened
// at i, or i when no dollar quote opens there.
func dollarQuoteEnd(script string, i int) int {
	end := strings.IndexByte(script[i+1:], '$')
	if end < 0 || strings.ContainsAny(script[i+1:i+1+end], " \n;'") {
		return i
	}
	tag := script[i : i+end+2]
	closing := strings.Index(script[i+len(tag):], tag)
	if closing < 0 {
		return len(script)
	}
	return i + len(tag) + closing + len(tag) - 1
}

type scriptRun struct {
	notices    []string
	errs       []error
	statements int
}

// runScript runs setup then the script's statements on one session.
func runScript(t *testing.T, ctx context.Context, script string, setup ...string) scriptRun {
	t.Helper()
	cfg, err := pgx.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	var run scriptRun
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		run.notices = append(run.notices, n.Message)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	for _, s := range setup {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, stmt := range splitScript(script) {
		run.statements++
		if _, err := conn.Exec(ctx, stmt); err != nil {
			run.errs = append(run.errs, err)
		}
	}
	return run
}

// stableActivity reads the schemas' baselines once every count the pool's
// sessions hold has been applied or lost for good (flushSetupStats), so
// setup traffic reported late is not mistaken for activity. Two equal
// reads prove nothing on PostgreSQL 14: the setup session keeps its last
// 500 ms of counts until its next statement (the second read's ping
// reports them), and that read may use a stats file up to 500 ms old,
// written for another backend before the collector applied the report, so
// both reads miss the same counts.
func stableActivity(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	schemas []string) map[string]int64 {
	t.Helper()
	if err := flushSetupStats(ctx, pool); err != nil {
		t.Fatalf("flush the setup's statistics: %v", err)
	}
	base, err := NewActivityReader(pool).SchemaActivity(ctx, schemas)
	if err != nil {
		t.Fatalf("read activity: %v", err)
	}
	return base
}

func schemaExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace
		WHERE nspname = $1)`, name).Scan(&ok)
	if err != nil {
		t.Fatalf("schema %s: %v", name, err)
	}
	return ok
}

func dropSchemasAfter(t *testing.T, pool *pgxpool.Pool, names ...string) {
	t.Cleanup(func() {
		for _, n := range names {
			_, _ = pool.Exec(context.Background(),
				"DROP SCHEMA IF EXISTS "+pgx.Identifier{n}.Sanitize()+" CASCADE")
		}
	})
}

func createSchemas(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tables int,
	names ...string) {
	t.Helper()
	dropSchemasAfter(t, pool, names...)
	for _, n := range names {
		q := pgx.Identifier{n}.Sanitize()
		stmts := []string{"CREATE SCHEMA " + q}
		for i := 0; i < tables; i++ {
			stmts = append(stmts, fmt.Sprintf(
				"CREATE TABLE %s.t%02d (id bigserial PRIMARY KEY, note text)", q, i))
		}
		if _, err := pool.Exec(ctx, strings.Join(stmts, ";\n")); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
}

// skipNotices are the run's notices that say why a block kept its schema.
func skipNotices(run scriptRun) []string {
	var out []string
	for _, n := range run.notices {
		if strings.HasPrefix(n, "skip ") {
			out = append(out, n)
		}
	}
	return out
}

func hasNotice(run scriptRun, want string) bool {
	for _, n := range run.notices {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

func TestCleanupBatchDropsTwoHundredSchemasWithinDefaultLockLimits(t *testing.T) {
	pool, _ := livePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var locks int
	if err := pool.QueryRow(ctx, `SELECT current_setting('max_locks_per_transaction')::int`).
		Scan(&locks); err != nil || locks > 64 {
		t.Fatalf("max_locks_per_transaction %d (%v): the test needs the default 64", locks, err)
	}
	prefix := uniqueName("test_bulk_") + "_"
	names := make([]string, 200)
	for i := range names {
		names[i] = fmt.Sprintf("%s%03d", prefix, i)
	}
	createSchemas(t, ctx, pool, 20, names...)
	base := stableActivity(t, ctx, pool, names)
	if len(base) != 200 {
		t.Fatalf("baselines for %d of 200 schemas", len(base))
	}
	script := cleanupSQL(1, prefix+"*", names, base, time.Now())
	if got := countLinesWithPrefix(script, "DO "); got != 200 {
		t.Fatalf("%d DO blocks", got)
	}
	run := runScript(t, ctx, script)
	if len(run.errs) != 0 || run.statements != 201 {
		t.Fatalf("%d statements, errors %v", run.statements, run.errs)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_namespace
		WHERE left(nspname, length($1)) = $1`, prefix).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d schemas left (%v); skips %v", left, err, skipNotices(run))
	}
}

func TestCleanupBatchSkipsASchemaWrittenAfterItWasBuilt(t *testing.T) {
	pool, ctx := livePool(t)
	prefix := uniqueName("test_late_") + "_"
	quiet, busy := prefix+"quiet", prefix+"busy"
	createSchemas(t, ctx, pool, 1, quiet, busy)
	base := stableActivity(t, ctx, pool, []string{quiet, busy})
	script := cleanupSQL(2, prefix+"*", []string{busy, quiet}, base, time.Now())
	execAll(t, ctx, pool, "INSERT INTO "+pgx.Identifier{busy, "t00"}.Sanitize()+
		" (note) VALUES ('late write')")
	reader := NewActivityReader(pool)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		cur, err := reader.SchemaActivity(ctx, []string{busy})
		if err != nil {
			t.Fatalf("read activity: %v", err)
		}
		if cur[busy] != base[busy] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the late write never reached the statistics")
		}
	}
	run := runScript(t, ctx, script)
	if len(run.errs) != 0 {
		t.Fatalf("errors %v", run.errs)
	}
	if !hasNotice(run, "skip "+busy+": activity or stats reset since the batch was built") {
		t.Fatalf("no skip notice for %s: %v", busy, run.notices)
	}
	if !schemaExists(t, ctx, pool, busy) || schemaExists(t, ctx, pool, quiet) {
		t.Fatalf("busy kept %v, quiet dropped %v", schemaExists(t, ctx, pool, busy),
			!schemaExists(t, ctx, pool, quiet))
	}
}

func TestCleanupBatchSkipsSchemasLockedByAnotherSession(t *testing.T) {
	pool, ctx := livePool(t)
	prefix := uniqueName("test_lock_") + "_"
	free, locked, refd, last := prefix+"a_free", prefix+"b_locked", prefix+"c_refd",
		prefix+"d_last"
	other := uniqueName("app_ref_")
	createSchemas(t, ctx, pool, 1, free, locked, refd, last, other)
	execAll(t, ctx, pool, fmt.Sprintf("CREATE TABLE %s (id bigint REFERENCES %s (id))",
		pgx.Identifier{other, "r"}.Sanitize(), pgx.Identifier{refd, "t00"}.Sanitize()))
	names := []string{free, locked, refd, last}
	script := cleanupSQL(3, prefix+"*", names, stableActivity(t, ctx, pool, names),
		time.Now())
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	// A lock inside the schema is seen up front; one on a dependent table
	// elsewhere is met by the drop itself and ends at lock_timeout.
	for _, q := range []string{
		"LOCK TABLE " + pgx.Identifier{locked, "t00"}.Sanitize() + " IN ACCESS SHARE MODE",
		"LOCK TABLE " + pgx.Identifier{other, "r"}.Sanitize() + " IN ACCESS EXCLUSIVE MODE",
	} {
		if _, err := holder.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	started := time.Now()
	run := runScript(t, ctx, script)
	if elapsed := time.Since(started); len(run.errs) != 0 || elapsed > 20*time.Second {
		t.Fatalf("errors %v after %s", run.errs, elapsed)
	}
	for _, want := range []string{"skip " + locked + ": another session holds a lock",
		"skip " + refd + ": lock_timeout reached"} {
		if !hasNotice(run, want) {
			t.Fatalf("no notice %q: %v", want, run.notices)
		}
	}
	if !schemaExists(t, ctx, pool, locked) || !schemaExists(t, ctx, pool, refd) ||
		schemaExists(t, ctx, pool, free) || schemaExists(t, ctx, pool, last) {
		t.Fatalf("after the run: notices %v", run.notices)
	}
}

func TestCleanupBatchCarriesOnPastAFailingBlock(t *testing.T) {
	pool, ctx := livePool(t)
	role := uniqueName("sage_cleanup_owner_")
	prefix := uniqueName("test_own_") + "_"
	mine1, theirs, mine2 := prefix+"a", prefix+"b", prefix+`c"x;y'z\w`
	rq := pgx.Identifier{role}.Sanitize()
	execAll(t, ctx, pool, "CREATE ROLE "+rq+" NOLOGIN")
	t.Cleanup(func() {
		for _, q := range []string{"DROP OWNED BY " + rq, "DROP ROLE " + rq} {
			_, _ = pool.Exec(context.Background(), q)
		}
	})
	createSchemas(t, ctx, pool, 2, mine1, theirs, mine2)
	for _, s := range []string{mine1, mine2} {
		execAll(t, ctx, pool, "ALTER SCHEMA "+pgx.Identifier{s}.Sanitize()+" OWNER TO "+rq)
	}
	names := []string{mine1, theirs, mine2}
	script := cleanupSQL(4, prefix+"*", names, stableActivity(t, ctx, pool, names),
		time.Now())
	run := runScript(t, ctx, script, "SET ROLE "+rq)
	if len(run.errs) != 1 || !strings.Contains(run.errs[0].Error(), "must be owner") {
		t.Fatalf("errors %v", run.errs)
	}
	if schemaExists(t, ctx, pool, mine1) || schemaExists(t, ctx, pool, mine2) ||
		!schemaExists(t, ctx, pool, theirs) {
		t.Fatalf("the failing block stopped the batch: notices %v", run.notices)
	}
}

func TestActivityReaderReadsOnlyTheNamedSchemas(t *testing.T) {
	pool, ctx := livePool(t)
	prefix := uniqueName("test_read_") + "_"
	a, b := prefix+"a", prefix+"b"
	createSchemas(t, ctx, pool, 1, a, b)
	got, err := NewActivityReader(pool).SchemaActivity(ctx, []string{a, prefix + "gone"})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v err %v", got, err)
	}
	if _, ok := got[a]; !ok {
		t.Fatalf("no counter for %s: %v", a, got)
	}
	none, err := NewActivityReader(pool).SchemaActivity(ctx, nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("no schemas: %v %v", none, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := NewActivityReader(pool).SchemaActivity(cancelled, []string{a}); err == nil ||
		!strings.Contains(err.Error(), "schema activity") {
		t.Fatalf("cancelled read: %v", err)
	}
}
