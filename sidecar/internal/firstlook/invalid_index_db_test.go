package firstlook

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrent-access test of the reads: they keep no state. The live
// builds below are the concurrency that matters: another session's build
// seen from the first look's own transaction.

// The fixture's dupvals_v is a unique build that failed on duplicate
// values: REINDEX would fail again, so the item looks for the duplicates.
func TestInvalidIndexReadUniqueGetsDuplicateQuery(t *testing.T) {
	pool, ctx := livePool(t)
	s := variedIndexes(t, ctx, pool)
	p := capPass(50)
	tx := snapshotTx(t, ctx, pool)
	if _, err := p.indexChecks(ctx, tx); err != nil {
		t.Fatalf("index checks: %v", err)
	}
	it := reportItem(t, p.report, RuleInvalidIndex, s+".dupvals_v")
	if !strings.Contains(it.SuggestedSQL, "GROUP BY v") ||
		strings.HasPrefix(it.SuggestedSQL, "REINDEX") {
		t.Fatalf("dupvals_v SQL %q, want the duplicate-key query", it.SuggestedSQL)
	}
	var key, copies int
	if err := tx.QueryRow(ctx, strings.TrimSuffix(it.SuggestedSQL, ";")).Scan(&key,
		&copies); err != nil {
		t.Fatalf("suggested query %q: %v", it.SuggestedSQL, err)
	}
	if key != 1 || copies != 2 {
		t.Fatalf("suggested query found key %d x%d, want 1 x2", key, copies)
	}
	if it.Caveat != "" {
		t.Fatalf("superuser sees every build, yet caveat %q", it.Caveat)
	}
}

// Integration on the key-column read: every plain key is named in order,
// expression keys are empty, INCLUDE columns are left out.
func TestInvalidIndexReadUniqueKeys(t *testing.T) {
	pool, ctx := livePool(t)
	s := uniqueSchema("uqkeys_")
	q := pgx.Identifier{s}.Sanitize()
	execAll(t, ctx, pool, "CREATE SCHEMA "+q,
		"CREATE TABLE "+q+".t (a int, b text, c int)",
		"INSERT INTO "+q+".t VALUES (1, 'x', 1), (1, 'x', 2)")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+q+" CASCADE") })
	for _, ddl := range []string{"CREATE UNIQUE INDEX CONCURRENTLY uk_ba ON %s.t (b, a) " +
		"INCLUDE (c)", "CREATE UNIQUE INDEX CONCURRENTLY uk_expr ON %s.t (a, lower(b))"} {
		if _, err := pool.Exec(ctx, strings.Replace(ddl, "%s", q, 1)); err == nil {
			t.Fatalf("%s over duplicates unexpectedly succeeded", ddl)
		}
	}
	tx := snapshotTx(t, ctx, pool)
	idx, _, err := readIndexes(ctx, tx)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	if err := readUniqueKeys(ctx, tx, idx); err != nil {
		t.Fatalf("read unique keys: %v", err)
	}
	ba, ex := byName(t, idx, s, "uk_ba"), byName(t, idx, s, "uk_expr")
	if strings.Join(ba.KeyNames, ",") != "b,a" || strings.Join(ex.KeyNames, ",") != "a," {
		t.Fatalf("key names uk_ba %q uk_expr %q, want [b a] and [a \"\"]", ba.KeyNames,
			ex.KeyNames)
	}
	if ba.NullsNotDistinct || ex.NullsNotDistinct {
		t.Fatal("default unique indexes read as NULLS NOT DISTINCT")
	}
	if _, err := tx.Exec(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("division by zero succeeded")
	}
	if err := readUniqueKeys(ctx, tx, idx); err == nil ||
		!strings.Contains(err.Error(), "unique index keys") {
		t.Fatalf("aborted transaction: %v, want an error naming the read", err)
	}
}

// holdSnapshot opens a repeatable-read transaction that has read t, so
// concurrent index builds wait for its snapshot; release ends it.
func holdSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	table string) func() {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin snapshot holder: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM "+table+" LIMIT 1"); err != nil {
		t.Fatalf("snapshot holder read: %v", err)
	}
	done := false
	release := func() {
		if !done {
			done = true
			_ = tx.Rollback(context.Background())
		}
	}
	t.Cleanup(release)
	return release
}

// startBuild runs ddl on its own connection; it returns the backend pid
// once the build waits on old snapshots, and a channel with its result.
func startBuild(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	ddl string) (int32, <-chan error) {
	t.Helper()
	return startBuildAt(t, ctx, pool, ddl, "waiting for old snapshots")
}

// startBuildAt is startBuild for a build that waits in the given phase.
func startBuildAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	ddl, wait string) (int32, <-chan error) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire build connection: %v", err)
	}
	var pid int32
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("build pid: %v", err)
	}
	res := make(chan error, 1)
	go func() {
		_, err := conn.Exec(ctx, ddl)
		conn.Release()
		res <- err
	}()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var phase string
		err := pool.QueryRow(ctx, "SELECT phase FROM pg_stat_progress_create_index "+
			"WHERE pid = $1", pid).Scan(&phase)
		if err == nil && phase == wait {
			return pid, res
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never reached phase %q", ddl, wait)
	return 0, nil
}

func liveBuildSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	s := uniqueSchema("live_build_")
	q := pgx.Identifier{s}.Sanitize()
	execAll(t, ctx, pool, "CREATE SCHEMA "+q, "CREATE TABLE "+q+".t (a int, b int)",
		"INSERT INTO "+q+".t SELECT g, g FROM generate_series(1, 1000) g",
		"CREATE INDEX t_a ON "+q+".t (a)")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+q+" CASCADE") })
	return s, q
}

// A CREATE INDEX CONCURRENTLY waiting on an old snapshot leaves its index
// invalid: the first look reports a build in progress, not a failure.
func TestInvalidIndexLiveBuildIsInfo(t *testing.T) {
	pool, ctx := livePool(t)
	s, q := liveBuildSchema(t, ctx, pool)
	release := holdSnapshot(t, ctx, pool, q+".t")
	_, res := startBuild(t, ctx, pool, "CREATE INDEX CONCURRENTLY t_b ON "+q+".t (b)")
	r, err := Run(ctx, pool, testOptions("app"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	it := findItem(r, RuleInvalidIndex, s+".t_b")
	if it == nil {
		t.Fatalf("no invalid-index item for the live build in %+v", r.Items)
	}
	if it.Severity != SeverityInfo || it.SuggestedSQL != "" ||
		!strings.Contains(it.Title, "in progress") || strings.Contains(it.Detail, "failed") {
		t.Fatalf("live build reported as %+v, want info with no SQL", *it)
	}
	if c := checkStatus(r, RuleInvalidIndex); c.Status == CheckDegraded {
		t.Fatalf("invalid-index check = %+v, want not degraded", c)
	}
	release()
	if err := <-res; err != nil {
		t.Fatalf("build after release: %v", err)
	}
}

// holdWriter opens a transaction that has written to table, so a
// concurrent build waits for writers before it builds; release ends it.
func holdWriter(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	table string) func() {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO "+table+" VALUES (0, 0)"); err != nil {
		t.Fatalf("writer insert: %v", err)
	}
	done := false
	release := func() {
		if !done {
			done = true
			_ = tx.Rollback(context.Background())
		}
	}
	t.Cleanup(release)
	return release
}

// The invalid _ccnew copy of a live REINDEX CONCURRENTLY is a build in
// progress, not a leftover to drop. While it waits for writers, index_relid
// names the original; while it waits on old snapshots, the copy.
func TestInvalidIndexLiveReindexCopyIsInfo(t *testing.T) {
	cases := []struct {
		phase string
		hold  func(*testing.T, context.Context, *pgxpool.Pool, string) func()
	}{
		{"waiting for writers before build", holdWriter},
		{"waiting for old snapshots", holdSnapshot},
	}
	for _, c := range cases {
		t.Run(c.phase, func(t *testing.T) { liveReindexCopyIsInfo(t, c.phase, c.hold) })
	}
}

func liveReindexCopyIsInfo(t *testing.T, phase string,
	hold func(*testing.T, context.Context, *pgxpool.Pool, string) func()) {
	pool, ctx := livePool(t)
	s, q := liveBuildSchema(t, ctx, pool)
	release := hold(t, ctx, pool, q+".t")
	_, res := startBuildAt(t, ctx, pool, "REINDEX INDEX CONCURRENTLY "+q+".t_a", phase)
	r, err := Run(ctx, pool, testOptions("app"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	it := findItem(r, RuleInvalidIndex, s+".t_a_ccnew")
	if it == nil {
		t.Fatalf("no invalid-index item for the live _ccnew copy in %+v", r.Items)
	}
	if it.Severity != SeverityInfo || it.SuggestedSQL != "" ||
		!strings.Contains(it.Title, "in progress") {
		t.Fatalf("live reindex copy reported as %+v, want info with no SQL", *it)
	}
	release()
	if err := <-res; err != nil {
		t.Fatalf("reindex after release: %v", err)
	}
}

// A cancelled REINDEX CONCURRENTLY leaves a _ccnew copy: the first look
// suggests dropping it, and the suggestion works.
func TestInvalidIndexCancelledReindexLeftoverGetsDrop(t *testing.T) {
	pool, ctx := livePool(t)
	s, q := liveBuildSchema(t, ctx, pool)
	release := holdSnapshot(t, ctx, pool, q+".t")
	pid, res := startBuild(t, ctx, pool, "REINDEX INDEX CONCURRENTLY "+q+".t_a")
	if _, err := pool.Exec(ctx, "SELECT pg_cancel_backend($1)", pid); err != nil {
		t.Fatalf("cancel reindex: %v", err)
	}
	if err := <-res; err == nil || !strings.Contains(err.Error(), "canceling statement") {
		t.Fatalf("cancelled reindex returned %v, want a cancellation", err)
	}
	release()
	r, err := Run(ctx, pool, testOptions("app"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	it := findItem(r, RuleInvalidIndex, s+".t_a_ccnew")
	if it == nil {
		t.Fatalf("no item for the _ccnew leftover in %+v", r.Items)
	}
	want := "DROP INDEX CONCURRENTLY " + s + ".t_a_ccnew;"
	if it.SuggestedSQL != want || it.Severity != SeverityWarning ||
		!strings.Contains(it.Recommendation, "REINDEX INDEX CONCURRENTLY "+s+".t_a;") {
		t.Fatalf("leftover item %+v, want %q and a re-run of the original", *it, want)
	}
	if _, err := pool.Exec(ctx, strings.TrimSuffix(it.SuggestedSQL, ";")); err != nil {
		t.Fatalf("suggested drop %q: %v", it.SuggestedSQL, err)
	}
	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_index i JOIN pg_class c ON "+
		"c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace "+
		"WHERE n.nspname = $1 AND NOT i.indisvalid", s).Scan(&left); err != nil {
		t.Fatalf("count invalid: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d invalid indexes left after the suggested drop", left)
	}
}

// Integration on the progress read: a live build is returned with its
// table, index and phase; an aborted transaction gives a reason, not an
// error, and leaves the caller's transaction as it was.
func TestReadIndexBuildsLive(t *testing.T) {
	pool, ctx := livePool(t)
	s, q := liveBuildSchema(t, ctx, pool)
	release := holdSnapshot(t, ctx, pool, q+".t")
	_, res := startBuild(t, ctx, pool, "CREATE INDEX CONCURRENTLY t_b ON "+q+".t (b)")
	tx := snapshotTx(t, ctx, pool)
	b := readIndexBuilds(ctx, tx)
	var table, index uint32
	if err := tx.QueryRow(ctx, "SELECT $1::regclass::oid, $2::regclass::oid", q+".t",
		q+".t_b").Scan(&table, &index); err != nil {
		t.Fatalf("resolve oids: %v", err)
	}
	if b.Unseen != "" || len(b.Builds) != 1 || b.Builds[0].TableOID != table ||
		b.Builds[0].IndexOID != index || b.Builds[0].Phase != "waiting for old snapshots" ||
		b.Builds[0].Command != "CREATE INDEX CONCURRENTLY" {
		t.Fatalf("builds %+v, want one on %s.t_b (%d on %d)", b, s, index, table)
	}
	release()
	if err := <-res; err != nil {
		t.Fatalf("build after release: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("division by zero succeeded")
	}
	if b := readIndexBuilds(ctx, tx); b.Unseen == "" || len(b.Builds) != 0 {
		t.Fatalf("aborted transaction gave %+v, want a reason", b)
	}
}

func reportItem(t *testing.T, r *Report, rule, object string) Item {
	t.Helper()
	for _, it := range r.Items {
		if it.Rule == rule && it.Object == object {
			return it
		}
	}
	t.Fatalf("no %s item for %s in %+v", rule, object, r.Items)
	return Item{}
}

// Another role's build shows NULL relid to a role without
// pg_read_all_stats: the read counts it as unseen instead of guessing.
func TestReadIndexBuildsHiddenFromAnotherRole(t *testing.T) {
	pool, ctx := livePool(t)
	_, q := liveBuildSchema(t, ctx, pool)
	role := uniqueSchema("nostats_")
	execAll(t, ctx, pool, "CREATE ROLE "+role+" NOLOGIN")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP ROLE "+role) })
	release := holdSnapshot(t, ctx, pool, q+".t")
	_, res := startBuild(t, ctx, pool, "CREATE INDEX CONCURRENTLY t_b ON "+q+".t (b)")
	tx := snapshotTx(t, ctx, pool)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
		t.Fatalf("set role: %v", err)
	}
	b := readIndexBuilds(ctx, tx)
	if len(b.Builds) != 0 || !strings.Contains(b.Unseen, "1 index builds by other roles") ||
		!strings.Contains(b.Unseen, "pg_read_all_stats") {
		t.Fatalf("builds as %s: %+v, want none seen and one hidden", role, b)
	}
	release()
	if err := <-res; err != nil {
		t.Fatalf("build after release: %v", err)
	}
}

// PostgreSQL 15+: a NULLS NOT DISTINCT unique index counts NULL keys as
// equal, so the suggested query keeps them and finds the NULL duplicate.
func TestInvalidIndexNullsNotDistinctKeepsNulls(t *testing.T) {
	pool, ctx := livePool(t)
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	if version < 150000 {
		t.Skipf("NULLS NOT DISTINCT needs PostgreSQL 15+, server is %d", version)
	}
	s := uniqueSchema("nnd_")
	execAll(t, ctx, pool, "CREATE SCHEMA "+s, "CREATE TABLE "+s+".t (a int)",
		"INSERT INTO "+s+".t VALUES (NULL), (NULL), (1)")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+s+" CASCADE") })
	if _, err := pool.Exec(ctx, "CREATE UNIQUE INDEX CONCURRENTLY t_a ON "+s+
		".t (a) NULLS NOT DISTINCT"); err == nil {
		t.Fatal("NULLS NOT DISTINCT build over two NULLs unexpectedly succeeded")
	}
	p := capPass(50)
	tx := snapshotTx(t, ctx, pool)
	if _, err := p.indexChecks(ctx, tx); err != nil {
		t.Fatalf("index checks: %v", err)
	}
	it := reportItem(t, p.report, RuleInvalidIndex, s+".t_a")
	if strings.Contains(it.SuggestedSQL, "IS NOT NULL") {
		t.Fatalf("SQL %q filters the NULL keys this index counts as equal", it.SuggestedSQL)
	}
	var key *int
	var copies int
	if err := tx.QueryRow(ctx, strings.TrimSuffix(it.SuggestedSQL, ";")).Scan(&key,
		&copies); err != nil {
		t.Fatalf("suggested query %q: %v", it.SuggestedSQL, err)
	}
	if key != nil || copies != 2 {
		t.Fatalf("suggested query found key %v x%d, want NULL x2", key, copies)
	}
}
