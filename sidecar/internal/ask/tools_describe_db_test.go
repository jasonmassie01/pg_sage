package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// describe_table on partitioned parents, never-analyzed tables, unquoted
// mixed-case names and bounded reads (mcp-03). A partitioned parent owns
// no storage and no vacuum history, so its own numbers ("0 bytes total",
// "about 0 rows", "never") are citable but wrong; the tool must report
// the leaves instead.

var (
	bytesIn    = regexp.MustCompile(`(\d+) bytes in partitions`)
	zeroBytes  = regexp.MustCompile(`(^|[^0-9])0 bytes total`)
	plainShape = regexp.MustCompile(`^table public\.ask_plain: \d+ bytes total, ` +
		`about 50 rows, 2 columns\ncolumns: id bigint, note text\n` +
		`last vacuum [^,]+, last analyze [^,]+$`)
)

// partitioned creates public.<name> partitioned by RANGE (created) with
// n monthly partitions holding rows rows each, dropped at cleanup.
func (f *fixture) partitioned(name string, n, rows int) {
	f.t.Helper()
	f.exec(`DROP TABLE IF EXISTS public.` + name + ` CASCADE`)
	f.exec(`CREATE TABLE public.` + name + ` (id bigint, created date NOT NULL, note text)
		PARTITION BY RANGE (created)`)
	f.t.Cleanup(func() { f.exec(`DROP TABLE IF EXISTS public.` + name + ` CASCADE`) })
	for i := 1; i <= n; i++ {
		f.exec(fmt.Sprintf(`CREATE TABLE public.%s_p%d PARTITION OF public.%s
			FOR VALUES FROM ('2026-%02d-01') TO ('2026-%02d-01')`, name, i, name, i, i+1))
		if rows > 0 {
			f.exec(fmt.Sprintf(`INSERT INTO public.%s SELECT g, '2026-%02d-15',
				repeat('x', 100) FROM generate_series(1, %d) g`, name, i, rows))
		}
	}
}

// waitVacuumed waits until every leaf of public.<name> shows a vacuum and
// an analyze time (PG 14 reports statistics asynchronously).
func (f *fixture) waitVacuumed(name string, leaves int64) {
	f.t.Helper()
	f.waitLeafStats(name, leaves, "vacuum", true)
}

// waitAnalyzed waits until every leaf of public.<name> shows an analyze
// time. On PG 14 an ANALYZE reaches pg_stat_user_tables asynchronously:
// right after it, a read showed "never" in 10 of 20 tries (PG 17: 0 of
// 20), so concurrent readers started at once saw different texts (ci-17).
func (f *fixture) waitAnalyzed(name string, leaves int64) {
	f.t.Helper()
	f.waitLeafStats(name, leaves, "analyze", false)
}

// waitLeafStats waits until every leaf of public.<name> shows an analyze
// time, and a vacuum time too when vacuumed is set.
func (f *fixture) waitLeafStats(name string, leaves int64, what string, vacuumed bool) {
	f.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f.count(`SELECT count(*) FROM pg_stat_user_tables WHERE relname LIKE $1
			AND (last_vacuum IS NOT NULL OR NOT $2) AND last_analyze IS NOT NULL`,
			name+`\_p%`, vacuumed) >= leaves {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Fatalf("leaves of %s never showed %s statistics", name, what)
}

// leafBytes sums pg_total_relation_size over the first limit leaves of
// public.<name> in oid order, computed independently of the tool.
func (f *fixture) leafBytes(name string, limit int) int64 {
	f.t.Helper()
	return f.count(`SELECT COALESCE(sum(pg_total_relation_size(relid)), 0)::bigint FROM
		(SELECT relid FROM pg_partition_tree($1::regclass) WHERE isleaf
		 ORDER BY relid LIMIT $2) l`, "public."+name, limit)
}

func describedBytes(t *testing.T, text string) int64 {
	t.Helper()
	m := bytesIn.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no partition size in:\n%s", text)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("partition size %q: %v", m[1], err)
	}
	return n
}

func setPartitionCap(t *testing.T, n int) {
	t.Helper()
	old := maxDescribePartitions
	maxDescribePartitions = n
	t.Cleanup(func() { maxDescribePartitions = old })
}

func TestDescribeTable_PartitionedSumsLeaves(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	f.partitioned("ask_events", 3, 1000)
	f.exec(`VACUUM ANALYZE public.ask_events`)
	f.waitVacuumed("ask_events", 3)
	out := mustTool(t, s, viewer, "describe_table", `{"table":"public.ask_events"}`)
	for _, want := range []string{"partitioned table public.ask_events", "3 partitions",
		"RANGE (created)", "about 3000 rows", "3 columns", "partition vacuum oldest 2",
		"partition analyze oldest 2"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, out.Text)
		}
	}
	got, want := describedBytes(t, out.Text), f.leafBytes("ask_events", 3)
	if got <= 0 || got != want {
		t.Errorf("size = %d, want the leaves' %d (> 0)", got, want)
	}
	// CHECK-18: none of the parent's own empty numbers survive.
	for _, bad := range []string{"never", "about 0 rows", "unavailable", "first "} {
		if strings.Contains(out.Text, bad) {
			t.Errorf("text has %q:\n%s", bad, out.Text)
		}
	}
	if zeroBytes.MatchString(out.Text) {
		t.Errorf("text reports 0 bytes total:\n%s", out.Text)
	}
	if out.Evidence.ID != "table:public.ask_events" {
		t.Errorf("id = %s", out.Evidence.ID)
	}
}

func TestDescribeTable_PartitionedWithoutPartitions(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	f.partitioned("ask_empty_parent", 0, 0)
	out := mustTool(t, s, viewer, "describe_table", `{"table":"public.ask_empty_parent"}`)
	for _, want := range []string{"0 partitions", "0 bytes in partitions",
		"row estimate unavailable (never analyzed)",
		"partition vacuum oldest never, newest never"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, out.Text)
		}
	}
	if strings.Contains(out.Text, "about") || zeroBytes.MatchString(out.Text) {
		t.Errorf("text claims parent numbers:\n%s", out.Text)
	}
}

func TestDescribeTable_PartitionCap(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	f.partitioned("ask_capped", 3, 10)
	f.exec(`ANALYZE public.ask_capped`)
	setPartitionCap(t, 2)
	out := mustTool(t, s, viewer, "describe_table", `{"table":"public.ask_capped"}`)
	if !strings.Contains(out.Text, "3 partitions") ||
		!strings.Contains(out.Text, "first 2 of 3 partitions") ||
		!strings.Contains(out.Text, "about 20 rows") {
		t.Errorf("capped text:\n%s", out.Text)
	}
	if got, want := describedBytes(t, out.Text), f.leafBytes("ask_capped", 2); got != want {
		t.Errorf("capped size = %d, want first-two sum %d", got, want)
	}
	// Boundary: a cap equal to the leaf count covers everything.
	setPartitionCap(t, 3)
	full := mustTool(t, s, viewer, "describe_table", `{"table":"public.ask_capped"}`)
	if strings.Contains(full.Text, "first ") || !strings.Contains(full.Text, "about 30 rows") {
		t.Errorf("uncapped text:\n%s", full.Text)
	}
}

func TestDescribeTable_SubPartitionsCountLeavesOnly(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	f.exec(`DROP TABLE IF EXISTS public.ask_tenants CASCADE`)
	t.Cleanup(func() { f.exec(`DROP TABLE IF EXISTS public.ask_tenants CASCADE`) })
	f.exec(`CREATE TABLE public.ask_tenants (tenant text NOT NULL, created date NOT NULL)
		PARTITION BY LIST (tenant)`)
	f.exec(`CREATE TABLE public.ask_tenants_a PARTITION OF public.ask_tenants
		FOR VALUES IN ('a') PARTITION BY RANGE (created)`)
	f.exec(`CREATE TABLE public.ask_tenants_a1 PARTITION OF public.ask_tenants_a
		FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`)
	f.exec(`CREATE TABLE public.ask_tenants_b PARTITION OF public.ask_tenants
		FOR VALUES IN ('b')`)
	out := mustTool(t, s, viewer, "describe_table", `{"table":"public.ask_tenants"}`)
	if !strings.Contains(out.Text, "2 partitions") || !strings.Contains(out.Text, "LIST (tenant)") {
		t.Errorf("sub-partitioned text:\n%s", out.Text)
	}
}

func TestDescribeTable_UnquotedNamesFoldToLowerCase(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	f.exec(`DROP TABLE IF EXISTS public.ask_lower, public."Ask_Mixed", public.ask_mixed,
		public."OnlyQuoted"`)
	t.Cleanup(func() {
		f.exec(`DROP TABLE IF EXISTS public.ask_lower, public."Ask_Mixed", public.ask_mixed,
			public."OnlyQuoted"`)
	})
	f.exec(`CREATE TABLE public.ask_lower (id int)`)
	f.exec(`CREATE TABLE public."Ask_Mixed" (id int)`)
	f.exec(`COMMENT ON TABLE public."Ask_Mixed" IS 'the quoted one'`)
	f.exec(`CREATE TABLE public.ask_mixed (id int)`)
	f.exec(`COMMENT ON TABLE public.ask_mixed IS 'the folded one'`)
	f.exec(`CREATE TABLE public."OnlyQuoted" (id int)`)
	for _, in := range []string{"public.Ask_Lower", "PUBLIC.ask_lower", "Public.ASK_LOWER"} {
		out := mustTool(t, s, viewer, "describe_table", `{"table":"`+in+`"}`)
		if out.Status != "ok" || !strings.HasPrefix(out.Text, "table public.ask_lower:") {
			t.Errorf("%s = %s %q", in, out.Status, out.Text)
		}
		if out.Evidence.ID != "table:"+in {
			t.Errorf("%s: id = %s, want the input unchanged", in, out.Evidence.ID)
		}
	}
	// An exact match wins over the folded name.
	exact := mustTool(t, s, viewer, "describe_table", `{"table":"public.Ask_Mixed"}`)
	if !strings.Contains(exact.Text, "the quoted one") {
		t.Errorf("exact match lost to folding:\n%s", exact.Text)
	}
	// Folding only lowers: a quoted upper-case table is not found by a
	// lower-case name, and a quoted argument is refused by the argument rule.
	if nf := mustTool(t, s, viewer, "describe_table", `{"table":"public.onlyquoted"}`); nf.Status !=
		"not_found" {
		t.Errorf("public.onlyquoted = %+v", nf)
	}
	wantInvalid(t, s, viewer, "describe_table", `{"table":"public.\"Ask_Lower\""}`)
}

func TestDescribeTable_NeverAnalyzedPlainTable(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	f.exec(`DROP TABLE IF EXISTS public.ask_plain`)
	t.Cleanup(func() { f.exec(`DROP TABLE IF EXISTS public.ask_plain`) })
	f.exec(`CREATE TABLE public.ask_plain (id bigint, note text)`)
	f.exec(`INSERT INTO public.ask_plain SELECT g, 'n' FROM generate_series(1, 50) g`)
	before := mustTool(t, s, viewer, "describe_table", `{"table":"public.ask_plain"}`)
	if !strings.Contains(before.Text, "row estimate unavailable (never analyzed)") ||
		strings.Contains(before.Text, "about 0 rows") {
		t.Errorf("never-analyzed text:\n%s", before.Text)
	}
	f.exec(`ANALYZE public.ask_plain`)
	after := mustTool(t, s, viewer, "describe_table", `{"table":"public.ask_plain"}`)
	if !plainShape.MatchString(after.Text) {
		t.Errorf("plain table format changed:\n%s", after.Text)
	}
}

// runToolCtx runs a tool under ctx (runTool uses an unbounded context).
func runToolCtx(ctx context.Context, t *testing.T, s *Service, name, args string) (
	agentloop.Output, error) {
	t.Helper()
	for _, tool := range s.newSession(viewer).tools() {
		if tool.Name == name {
			return tool.Run(ctx, json.RawMessage(args))
		}
	}
	t.Fatalf("tool %s is not offered", name)
	return agentloop.Output{}, nil
}

// lockTable holds ACCESS EXCLUSIVE on public.<name> until the test ends.
func (f *fixture) lockTable(name string) {
	f.t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	f.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(f.ctx, `LOCK TABLE public.`+name+` IN ACCESS EXCLUSIVE MODE`); err !=
		nil {
		f.t.Fatalf("lock %s: %v", name, err)
	}
}

func setDescribeTimeouts(t *testing.T, statement, lock time.Duration) {
	t.Helper()
	oldS, oldL := describeStatementTimeout, describeLockTimeout
	describeStatementTimeout, describeLockTimeout = statement, lock
	t.Cleanup(func() { describeStatementTimeout, describeLockTimeout = oldS, oldL })
}

func TestDescribeTable_BoundedReadNamesTheTable(t *testing.T) {
	cases := []struct {
		name          string
		statement     time.Duration
		lock          time.Duration
		wantSQLSTATEs string
	}{
		{"statement timeout", 300 * time.Millisecond, 0, "57014"},
		{"lock timeout", 2 * time.Second, 200 * time.Millisecond, "55P03"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			s := f.service(f.deps(nil))
			f.exec(`DROP TABLE IF EXISTS public.ask_locked`)
			f.exec(`CREATE TABLE public.ask_locked (id int)`)
			t.Cleanup(func() { f.exec(`DROP TABLE IF EXISTS public.ask_locked`) })
			setDescribeTimeouts(t, tc.statement, tc.lock)
			f.lockTable("ask_locked")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			out, err := runToolCtx(ctx, t, s, "describe_table", `{"table":"public.ask_locked"}`)
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("describe waited %s; the read is not bounded", took)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tc.wantSQLSTATEs {
				t.Fatalf("err = %v (out %+v), want SQLSTATE %s", err, out, tc.wantSQLSTATEs)
			}
			msg := err.Error()
			if errors.Is(err, pgx.ErrNoRows) || !strings.Contains(msg, "public.ask_locked") ||
				strings.Contains(msg, "does not exist") {
				t.Errorf("err = %v, want one naming public.ask_locked", err)
			}
		})
	}
}

func TestDescribeTable_ConcurrentPartitionedReads(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	f.partitioned("ask_shared", 2, 5)
	f.exec(`ANALYZE public.ask_shared`)
	f.waitAnalyzed("ask_shared", 2)
	const workers = 8
	texts := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := runToolCtx(context.Background(), t, s, "describe_table",
				`{"table":"public.ask_shared"}`)
			texts[i], errs[i] = out.Text, err
		}(i)
	}
	wg.Wait()
	for i := range workers {
		if errs[i] != nil || texts[i] != texts[0] || !strings.Contains(texts[i], "about 10 rows") {
			t.Errorf("worker %d: err %v text:\n%s", i, errs[i], texts[i])
		}
	}
}

// A partition past the cap is never opened, so DDL holding it does not
// block or fail the describe; listing the leaves must not lock them all
// (pg_partition_tree would hold AccessShareLock on every leaf until commit).
func TestDescribeTable_LockedPartitionPastCapDoesNotBlock(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	f.partitioned("ask_wide", 3, 5)
	f.exec(`ANALYZE public.ask_wide`)
	setPartitionCap(t, 2)
	setDescribeTimeouts(t, 2*time.Second, 200*time.Millisecond)
	f.lockTable("ask_wide_p3")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := runToolCtx(ctx, t, s, "describe_table", `{"table":"public.ask_wide"}`)
	if err != nil {
		t.Fatalf("describe blocked by a lock on an uncovered partition: %v", err)
	}
	for _, want := range []string{"3 partitions", "first 2 of 3 partitions", "about 10 rows"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, out.Text)
		}
	}
}
