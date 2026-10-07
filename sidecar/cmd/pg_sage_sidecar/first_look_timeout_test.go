package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// The first look runs on its own budget unless the operator set a lower
// safety.query_timeout_ms (YAML or API override); pg_sage's default never
// clamps it. Checks that degrade with a transient error are retried once
// and the retry is recorded on the same report.

func TestFirstLookOptionsOwnBudgetUnlessOperatorSet(t *testing.T) {
	own := firstlook.DefaultStatementTimeout
	for _, tc := range []struct {
		name    string
		queryMS int
		set     map[string]bool
		want    time.Duration
		setBy   string
	}{
		{"pg_sage default", config.DefaultQueryTimeoutMs, map[string]bool{}, own, ""},
		{"derived, not the operator's", 2000, map[string]bool{"collector.interval_seconds": true},
			own, ""},
		{"operator lower", 300, map[string]bool{queryTimeoutKey: true}, 300 * time.Millisecond,
			queryTimeoutKey},
		{"operator equal", 5000, map[string]bool{queryTimeoutKey: true}, own, ""},
		{"operator higher", 30000, map[string]bool{queryTimeoutKey: true}, own, ""},
		{"operator unknown fails closed", config.DefaultQueryTimeoutMs, nil,
			500 * time.Millisecond, queryTimeoutKey},
		{"unknown and unset value", 0, nil, own, ""},
		{"operator set zero", 0, map[string]bool{queryTimeoutKey: true}, own, ""},
	} {
		c := config.DefaultConfig()
		c.Safety.QueryTimeoutMs = tc.queryMS
		got := firstLookOptions(c, tc.set)
		if got.StatementTimeout != tc.want || got.TimeoutSetBy != tc.setBy {
			t.Errorf("%s: timeout %v set by %q, want %v by %q", tc.name, got.StatementTimeout,
				got.TimeoutSetBy, tc.want, tc.setBy)
		}
	}
	c := config.DefaultConfig()
	c.Analyzer.XIDWraparoundWarning, c.Analyzer.XIDWraparoundCritical = 100, 200
	got := firstLookOptions(c, map[string]bool{})
	if got.Thresholds.XIDWarnFraction != firstlook.XIDFraction(100) ||
		got.Thresholds.XIDCriticalFraction != firstlook.XIDFraction(200) {
		t.Fatalf("analyzer thresholds lost: %+v", got.Thresholds)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The runtime reads who set the timeout from the YAML file; an unreadable
// file keeps the configured value (fail closed).
func TestRuntimeFirstLookOptionsReadTheConfigFile(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		queryMS    int
		want       time.Duration
	}{
		{"default, no file", "", config.DefaultQueryTimeoutMs, firstlook.DefaultStatementTimeout},
		{"file without the key", writeConfig(t, "collector:\n  interval_seconds: 60\n"),
			config.DefaultQueryTimeoutMs, firstlook.DefaultStatementTimeout},
		{"file sets it", writeConfig(t, "safety:\n  query_timeout_ms: 300\n"), 300,
			300 * time.Millisecond},
		{"unreadable file", filepath.Join(t.TempDir(), "missing.yaml"),
			config.DefaultQueryTimeoutMs, 500 * time.Millisecond},
	} {
		c := config.DefaultConfig()
		c.ConfigPath, c.Safety.QueryTimeoutMs = tc.path, tc.queryMS
		rt := &databaseRuntime{cfg: c, spec: databaseRuntimeSpec{Name: "orders"}}
		if got := rt.resolveFirstLookOptions(t.Context()); got.StatementTimeout != tc.want {
			t.Errorf("%s: timeout %v, want %v", tc.name, got.StatementTimeout, tc.want)
		}
	}
}

func TestOperatorSetKeysReadsAPIOverrides(t *testing.T) {
	pool := selfConfigPool(t)
	clean := func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.config WHERE key = $1",
			queryTimeoutKey)
	}
	clean()
	t.Cleanup(clean)
	set, err := operatorSetKeys(t.Context(), "", "orders", pool, 0)
	if err != nil || set == nil || set[queryTimeoutKey] {
		t.Fatalf("before the override: %v err %v", set, err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO sage.config (key, value)
		VALUES ($1, '300')`, queryTimeoutKey); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	set, err = operatorSetKeys(t.Context(), "", "orders", pool, 0)
	if err != nil || !set[queryTimeoutKey] {
		t.Fatalf("after the override: %v err %v", set, err)
	}
	c := config.DefaultConfig()
	c.Safety.QueryTimeoutMs = 300
	if got := firstLookOptions(c, set); got.StatementTimeout != 300*time.Millisecond {
		t.Fatalf("override not applied: %v", got.StatementTimeout)
	}
}

// lockCatalogs holds the named pg_catalog tables exclusively until release,
// on a connection of its own: a new backend cannot start while pg_index is
// locked, so p must keep its warm connection free.
func lockCatalogs(t *testing.T, ctx context.Context, p *pgxpool.Pool,
	tables ...string) func() {
	t.Helper()
	conn, err := pgx.ConnectConfig(ctx, p.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect lock session: %v", err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() { _ = conn.Close(context.Background()) }) // ends the lock
	}
	t.Cleanup(release)
	if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
		t.Fatalf("begin lock transaction: %v", err)
	}
	for _, tbl := range tables {
		if _, err := conn.Exec(ctx, "LOCK TABLE pg_catalog."+tbl+
			" IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatalf("lock %s: %v", tbl, err)
		}
	}
	return release
}

// releaseAtFinish is a first-look clock that releases the catalog locks
// when firstlook.Run stamps FinishedAt (its second call from Run): every
// check has run by then, whatever the load, and the save can proceed. A
// fixed timer raced the client-side step deadline on a busy CI runner and
// let the extension read through (PR #130).
func releaseAtFinish(release func()) func() time.Time {
	var mu sync.Mutex
	runCalls := 0
	return func() time.Time {
		pc, _, _, _ := runtime.Caller(1)
		if fn := runtime.FuncForPC(pc); fn != nil && strings.HasSuffix(fn.Name(), "firstlook.Run") {
			mu.Lock()
			runCalls++
			if runCalls == 2 {
				release()
			}
			mu.Unlock()
		}
		return time.Now()
	}
}

type levelLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *levelLog) logf(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *levelLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// A first look whose index and extension reads time out finds nothing;
// the retry completes them, updates the stored report, the metrics and the
// first finding.
func TestFirstLookRetryRecordsTheResult(t *testing.T) {
	_, p, ctx := firstLookDB(t)
	seedDuplicateIndex(t, ctx, p)
	if _, err := onboarding.Init(ctx, p, "app"); err != nil {
		t.Fatalf("onboarding init: %v", err)
	}
	opts := firstlook.Options{StatementTimeout: 300 * time.Millisecond}
	if _, err := firstlook.Run(ctx, p, opts); err != nil { // warm the catalog caches
		t.Fatalf("warm-up: %v", err)
	}
	tr := onboarding.NewTracker()
	started := time.Now()
	tr.Start("app", started)
	logs := &levelLog{}
	run := &firstLookRun{name: "app", pool: p, provider: "self-managed", started: started,
		tracker: tr, opts: opts, logf: logs.logf}
	release := lockCatalogs(t, ctx, p, "pg_index", "pg_extension")
	run.opts.Now = releaseAtFinish(release)
	report, err := run.execute(ctx)
	release()
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(report.Items) != 0 || len(report.Retryable) == 0 {
		t.Fatalf("first attempt items %+v retryable %v: want none found, some retryable",
			report.Items, report.Retryable)
	}
	if st, _, _ := onboarding.Get(ctx, p, "app"); st.FirstFindingAt != nil {
		t.Fatalf("first finding recorded before the retry: %+v", st)
	}
	retried := run.retryDegraded(ctx, report, 10*time.Millisecond)
	assertRetryRecorded(t, ctx, p, report, retried)
	if m := tr.Snapshot()[0]; m.FirstLookItems != len(retried.Items) || !m.HasTTFF {
		t.Fatalf("tracker = %+v, want %d items and a first finding", m, len(retried.Items))
	}
	st, _, err := onboarding.Get(ctx, p, "app")
	if err != nil || st.FirstFindingAt == nil || st.FirstFindingSource != "first_look" {
		t.Fatalf("onboarding = %+v err %v, want the retry's first finding", st, err)
	}
	if !strings.Contains(logs.text(), "retry") {
		t.Fatalf("logs do not mention the retry:\n%s", logs.text())
	}
}

func assertRetryRecorded(t *testing.T, ctx context.Context, p *pgxpool.Pool,
	report, retried firstlook.Report) {
	t.Helper()
	if retried.ID != report.ID || len(retried.Items) == 0 || len(retried.Retryable) != 0 {
		t.Fatalf("retried = id %d items %d retryable %v", retried.ID, len(retried.Items),
			retried.Retryable)
	}
	saved, found, err := firstlook.NewStore(p).Latest(ctx, "app")
	if err != nil || !found || saved.ID != report.ID || len(saved.Items) != len(retried.Items) {
		t.Fatalf("stored = %+v found %v err %v", saved, found, err)
	}
	dup := false
	for _, it := range saved.Items {
		dup = dup || (it.Rule == firstlook.RuleDuplicateIndex &&
			strings.HasPrefix(it.Object, "public.fl_orders_c"))
	}
	marked := 0
	for _, c := range saved.Checks {
		if c.Retried {
			marked++
		}
	}
	if !dup || marked == 0 {
		t.Fatalf("stored report lacks the retry: duplicate %v, %d retried checks: %+v",
			dup, marked, saved.Checks)
	}
}

func TestFirstLookRetrySkipsAndFailures(t *testing.T) {
	logs := &levelLog{}
	run := &firstLookRun{name: "app", tracker: onboarding.NewTracker(), logf: logs.logf}
	clean := firstlook.Report{ID: 3, Database: "app"}
	if got := run.retryDegraded(t.Context(), clean, time.Hour); got.ID != 3 ||
		logs.text() != "" {
		t.Fatalf("nothing to retry: %+v logs %q", got, logs.text())
	}
	degraded := firstlook.Report{ID: 4, Database: "app",
		Retryable: []string{firstlook.RuleDuplicateIndex},
		Checks: []firstlook.Check{{Rule: firstlook.RuleDuplicateIndex,
			Status: firstlook.CheckDegraded}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := run.retryDegraded(ctx, degraded, time.Hour); got.ID != 4 ||
		got.Checks[0].Retried || strings.Contains(logs.text(), "WARN") {
		t.Fatalf("cancelled wait: %+v logs %q", got, logs.text())
	}
	// No pool: the retry fails, is logged, and the first attempt stands.
	if got := run.retryDegraded(t.Context(), degraded, time.Millisecond); got.ID != 4 ||
		got.Checks[0].Retried || !strings.Contains(logs.text(), "WARN") {
		t.Fatalf("failed retry: %+v logs %q", got, logs.text())
	}
}
