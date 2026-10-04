package firstlook

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// seedProblems creates a schema with one of each catalog problem the first
// look reports and returns its name.
func seedProblems(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	s := uniqueSchema("fl_")
	q := pgx.Identifier{s}.Sanitize()
	execAll(t, ctx, pool,
		"CREATE SCHEMA "+q,
		"CREATE TABLE "+q+".parent (id int PRIMARY KEY)",
		"INSERT INTO "+q+".parent SELECT g FROM generate_series(1, 10) g",
		"CREATE TABLE "+q+".child (id int PRIMARY KEY, parent_id int REFERENCES "+q+
			".parent (id), a int)",
		"INSERT INTO "+q+".child SELECT g, 1 + g % 10, g FROM generate_series(1, 2000) g",
		"CREATE INDEX child_a_one ON "+q+".child (a)",
		"CREATE INDEX child_a_two ON "+q+".child (a)",
		"CREATE SEQUENCE "+q+".near_seq AS integer",
		"SELECT setval('"+s+".near_seq', 2000000000)",
		"CREATE TABLE "+q+".dupvals (v int)",
		"INSERT INTO "+q+".dupvals VALUES (1), (1)",
		"CREATE TABLE "+q+".churn (id int PRIMARY KEY, pad text)",
		"INSERT INTO "+q+".churn SELECT g, repeat('x', 200) FROM generate_series(1, 2000) g",
		"DELETE FROM "+q+".churn WHERE id <= 1500",
		// The bloat estimate reads the table size from pg_class.relpages (no
		// lock, unlike pg_relation_size), which only VACUUM or ANALYZE set.
		"ANALYZE "+q+".churn",
		"ANALYZE "+q+".child",
	)
	// A failed concurrent build leaves an invalid index behind.
	if _, err := pool.Exec(ctx, "CREATE UNIQUE INDEX CONCURRENTLY dupvals_v ON "+q+
		".dupvals (v)"); err == nil {
		t.Fatal("unique build over duplicate values unexpectedly succeeded")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE")
	})
	if err := testdb.FlushIdleSessions(ctx, pool); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
	return s
}

func testOptions(database string) Options {
	th := DefaultThresholds()
	th.BloatMinBytes = 8192
	th.FKMinRows = 0
	return Options{Database: database, Provider: "self-managed", Thresholds: th}
}

func findItem(r Report, rule, objectPrefix string) *Item {
	for i := range r.Items {
		if r.Items[i].Rule == rule && strings.HasPrefix(r.Items[i].Object, objectPrefix) {
			return &r.Items[i]
		}
	}
	return nil
}

func checkStatus(r Report, rule string) Check {
	for _, c := range r.Checks {
		if c.Rule == rule {
			return c
		}
	}
	return Check{}
}

func TestRunFindsSeededProblems(t *testing.T) {
	pool, ctx := livePool(t)
	s := seedProblems(t, ctx, pool)
	r, err := Run(ctx, pool, testOptions("app"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []struct{ rule, object string }{
		{RuleDuplicateIndex, s + ".child_a_two"},
		{RuleUnindexedFK, s + ".child."},
		{RuleSequenceRunway, s + ".near_seq"},
		{RuleInvalidIndex, s + ".dupvals_v"},
		{RuleTableBloat, s + ".churn"},
	} {
		it := findItem(r, want.rule, want.object)
		if it == nil {
			t.Fatalf("no %s item for %s in %+v", want.rule, want.object, r.Items)
		}
		requireEvidence(t, *it)
	}
	if r.Database != "app" || r.Relations <= 0 || r.DurationMS < 0 ||
		r.FinishedAt.Before(r.StartedAt) {
		t.Fatalf("report header = %+v", r)
	}
	for _, rule := range []string{RuleInvalidIndex, RuleDuplicateIndex,
		RuleNeverScannedIndex, RuleUnindexedFK, RuleXIDRunway, RuleSequenceRunway,
		RuleTableBloat, RuleTestSchema, RuleMissingExtension} {
		if c := checkStatus(r, rule); c.Status == "" {
			t.Fatalf("check %s missing from %+v", rule, r.Checks)
		}
	}
	if c := checkStatus(r, RuleDuplicateIndex); c.Status != CheckFinding {
		t.Fatalf("duplicate check = %+v, want finding", c)
	}
	if len(r.Capabilities) < 3 {
		t.Fatalf("capabilities = %+v, want pg_stat_statements, hypopg, auto_explain",
			r.Capabilities)
	}
}

func TestRunNeverTouchesUserTables(t *testing.T) {
	pool, ctx := livePool(t)
	s := seedProblems(t, ctx, pool)
	scans := func() (int64, int64, int) {
		var seq, idx int64
		var rels int
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(seq_scan), 0)::bigint,
			COALESCE(sum(COALESCE(idx_scan, 0)), 0)::bigint FROM pg_stat_user_tables
			WHERE schemaname = $1`, s).Scan(&seq, &idx); err != nil {
			t.Fatalf("read scans: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname <> 'sage'`).
			Scan(&rels); err != nil {
			t.Fatalf("count relations: %v", err)
		}
		return seq, idx, rels
	}
	seq0, idx0, rels0 := scans()
	if _, err := Run(ctx, pool, testOptions("app")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := testdb.FlushIdleSessions(ctx, pool); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
	seq1, idx1, rels1 := scans()
	if seq1 != seq0 || idx1 != idx0 {
		t.Fatalf("user table scans changed: seq %d->%d idx %d->%d", seq0, seq1, idx0, idx1)
	}
	if rels1 != rels0 {
		t.Fatalf("relations outside sage changed %d -> %d: the first look must not write",
			rels0, rels1)
	}
}

func TestRunRespectsStatementTimeout(t *testing.T) {
	pool, ctx := livePool(t)
	r, err := Run(ctx, pool, testOptions("app"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.StatementTimeoutMS != int(DefaultStatementTimeout/time.Millisecond) {
		t.Fatalf("default timeout = %d ms", r.StatementTimeoutMS)
	}

	// A lower timeout already set on the session is kept.
	cfg := pool.Config().Copy()
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "1500"
	low, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect with timeout: %v", err)
	}
	defer low.Close()
	r, err = Run(ctx, low, testOptions("app"))
	if err != nil || r.StatementTimeoutMS != 1500 {
		t.Fatalf("session timeout: %d ms err %v, want 1500", r.StatementTimeoutMS, err)
	}

	// A timeout too short for any catalog query degrades every check with
	// the reason stated; the run itself still returns a report.
	opts := testOptions("app")
	opts.StatementTimeout = time.Millisecond
	r, err = Run(ctx, pool, opts)
	if err != nil {
		t.Fatalf("1 ms timeout: %v", err)
	}
	degraded := 0
	for _, c := range r.Checks {
		if c.Status == CheckDegraded {
			degraded++
			if !strings.Contains(c.Note, "timeout") {
				t.Fatalf("degraded check %+v does not state the timeout", c)
			}
		}
	}
	if degraded == 0 {
		t.Fatalf("checks %+v: none degraded under a 1 ms timeout", r.Checks)
	}
}

func TestRunErrors(t *testing.T) {
	if _, err := Run(context.Background(), nil, Options{Database: "app"}); !errors.Is(err,
		ErrNoPool) {
		t.Fatalf("nil pool err = %v, want ErrNoPool", err)
	}
	pool, _ := livePool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, pool, Options{Database: "app"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx err = %v, want context.Canceled", err)
	}
}

func TestRunIsDeterministicUnderConcurrency(t *testing.T) {
	pool, ctx := livePool(t)
	seedProblems(t, ctx, pool)
	var wg sync.WaitGroup
	reports := make([]Report, 4)
	errs := make([]error, 4)
	for i := range reports {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reports[i], errs[i] = Run(ctx, pool, testOptions("app"))
		}(i)
	}
	wg.Wait()
	key := func(r Report) string {
		var b strings.Builder
		for _, it := range r.Items {
			if it.Rule == RuleNeverScannedIndex {
				continue // the other runs' catalog reads do not scan indexes, but stay safe
			}
			fmt.Fprintf(&b, "%s|%s|%s\n", it.Rule, it.Severity, it.Object)
		}
		return b.String()
	}
	for i := range reports {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		if key(reports[i]) != key(reports[0]) {
			t.Fatalf("run %d differs:\n%s\nvs\n%s", i, key(reports[i]), key(reports[0]))
		}
	}
}

// monitorRole creates a login role with only pg_monitor and returns a pool
// connected as it.
func monitorRole(t *testing.T, ctx context.Context, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	role := uniqueSchema("fl_monitor_")
	password := "pw_" + role
	var db string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("current database: %v", err)
	}
	rq := pgx.Identifier{role}.Sanitize()
	execAll(t, ctx, admin,
		"CREATE ROLE "+rq+" LOGIN PASSWORD '"+password+"'",
		"GRANT pg_monitor TO "+rq,
		"GRANT CONNECT ON DATABASE "+pgx.Identifier{db}.Sanitize()+" TO "+rq)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "REASSIGN OWNED BY "+rq+" TO CURRENT_USER")
		_, _ = admin.Exec(context.Background(), "DROP OWNED BY "+rq)
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+rq)
	})
	u, err := url.Parse(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(role, password)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as monitor: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRunWithLeastPrivilegeDegradesWithReason(t *testing.T) {
	admin, ctx := livePool(t)
	s := seedProblems(t, ctx, admin)
	mon := monitorRole(t, ctx, admin)
	var super bool
	if err := mon.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").
		Scan(&super); err != nil || super {
		t.Fatalf("monitor role superuser=%v err=%v", super, err)
	}
	r, err := Run(ctx, mon, testOptions("app"))
	if err != nil {
		t.Fatalf("run as monitor: %v", err)
	}
	if findItem(r, RuleDuplicateIndex, s+".child_a_two") == nil ||
		findItem(r, RuleUnindexedFK, s+".child.") == nil {
		t.Fatalf("catalog findings missing under pg_monitor: %+v", r.Items)
	}
	c := checkStatus(r, RuleSequenceRunway)
	if c.Status != CheckDegraded || !strings.Contains(c.Note, "GRANT SELECT") {
		t.Fatalf("sequence check = %+v, want degraded with the grant that fixes it", c)
	}
}
