package agentposture

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// fleetpol-02a on real PostgreSQL: the exposure sweep.

// supabaseShaped creates the cluster-wide Supabase roles anon,
// authenticated and service_role under the Supabase roles lock; they are
// dropped when the test ends.
func supabaseShaped(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	testdb.LockAgentRoles(t)
	testdb.HoldSupabaseRoles(t, testdb.SkipUnlessLive(t))
	for _, r := range []string{"anon", "authenticated"} {
		execAll(t, ctx, pool, "CREATE ROLE "+r+" NOLOGIN")
	}
	execAll(t, ctx, pool, "CREATE ROLE service_role NOLOGIN BYPASSRLS")
}

func sweepMonitor(pool *pgxpool.Pool, now *atomic.Pointer[time.Time],
	reg *Registry) *Monitor {
	return NewMonitor(pool, MonitorOptions{Registry: reg,
		Config: DefaultConfig, Now: func() time.Time { return *now.Load() }})
}

func nowAt(t time.Time) *atomic.Pointer[time.Time] {
	p := &atomic.Pointer[time.Time]{}
	p.Store(&t)
	return p
}

func findingOn(run analyzer.OutOfCycle, category, object string) *analyzer.Finding {
	for i := range run.Findings {
		f := &run.Findings[i]
		if f.Category == category && f.ObjectIdentifier == object {
			return f
		}
	}
	return nil
}

// CHECK-15, happy path: on a Supabase-shaped database a table created
// for anon without row-level security is flagged by AP-03 within one
// sweep tick, outside the analyzer cycle.
func TestSweep_SupabaseExposedTableFlaggedWithinOneTick(t *testing.T) {
	pool, ctx := livePool(t)
	supabaseShaped(t, ctx, pool)
	m := sweepMonitor(pool, nowAt(time.Now()), nil)
	pub, clock := newRecordingPublisher(), newFakeClock()
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		m.RunSweeps(sctx, SweepOptions{Publisher: pub, After: clock.after})
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	pub.wait(t) // enrollment sweep
	if _, ran := pub.last(); !ran {
		t.Fatal("the enrollment sweep did not run the exposure detectors")
	}
	table := "public.leads_" + suffix(t)
	execAll(t, ctx, pool, "CREATE TABLE "+table+" (id int, email text)",
		"GRANT SELECT ON "+table+" TO anon")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+table) })
	clock.ticks <- time.Now()
	pub.wait(t)
	run, ran := pub.last()
	if !ran {
		t.Fatal("the sweep after the exposing DDL did not run")
	}
	f := findingOn(run, "agent_posture:AP-03", table)
	if f == nil || f.Severity != "critical" {
		t.Fatalf("AP-03 on %s = %+v, want a critical finding; findings %d", table, f,
			len(run.Findings))
	}
	if f.Detail["mode"] != "shadow" || f.Detail["would_be_action"] != "enable_row_level_security" {
		t.Fatalf("AP-03 detail = %v, want mode shadow and enable_row_level_security", f.Detail)
	}
	if !slices.Contains(run.Evaluated, "agent_posture:AP-03") ||
		slices.Contains(run.Evaluated, "agent_posture:AP-06") {
		t.Fatalf("evaluated = %v, want the exposure subset only", run.Evaluated)
	}
}

// Nil/empty: no exposed role and no PUBLIC grant gives no AP-03 or AP-15
// finding, and both categories are still evaluated (their old findings
// may resolve).
func TestSweep_NothingExposedStillEvaluates(t *testing.T) {
	pool, ctx := livePool(t)
	testdb.LockAgentRoles(t)
	m := sweepMonitor(pool, nowAt(time.Now()), nil)
	run, ran, err := m.Sweep(ctx)
	if err != nil || !ran {
		t.Fatalf("Sweep = ran %v, %v", ran, err)
	}
	for _, f := range run.Findings {
		if f.Category == "agent_posture:AP-03" || f.Category == "agent_posture:AP-15" {
			t.Fatalf("finding on an unexposed database: %+v", f)
		}
	}
	for _, c := range []string{"agent_posture:AP-03", "agent_posture:AP-15"} {
		if !slices.Contains(run.Evaluated, c) {
			t.Fatalf("evaluated = %v, want %s", run.Evaluated, c)
		}
	}
}

// Boundary: an unchanged fingerprint for 23 h 59 m runs nothing in the
// sweeper; the daily run stays the analyzer's, which runs every detector.
// It runs on the exclusive server: the fingerprint hashes cluster-wide
// roles, which another package may change on the shared one.
func TestSweep_UnchangedFingerprintRunsNothingAndDailyStaysTheAnalyzers(t *testing.T) {
	pool, ctx := livePoolOn(t, testdb.Exclusive(t,
		"an unchanged cluster catalog must run no sweep"))
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.Local)
	now := nowAt(start)
	exp, rest := &countingDetector{id: "AP-03"}, &countingDetector{id: "AP-06"}
	m := newTestMonitorAt(t, pool, now, exp, rest)
	if _, err := m.Detect(ctx); err != nil || exp.count() != 1 || rest.count() != 1 {
		t.Fatalf("first cycle = %v, runs %d, %d; want every detector once", err,
			exp.count(), rest.count())
	}
	for _, d := range []time.Duration{0, time.Minute, 23*time.Hour + 59*time.Minute} {
		at := start.Add(d)
		now.Store(&at)
		if _, ran, err := m.Sweep(ctx); err != nil || ran {
			t.Fatalf("sweep after %s = ran %v, %v; want nothing", d, ran, err)
		}
	}
	if exp.count() != 1 || rest.count() != 1 {
		t.Fatalf("runs = %d, %d; want 1, 1", exp.count(), rest.count())
	}
	// The analyzer's daily run (03:00 passed) runs every detector again.
	if _, err := m.Detect(ctx); err != nil {
		t.Fatal(err)
	}
	if exp.count() != 2 || rest.count() != 2 {
		t.Fatalf("after the daily cycle runs = %d, %d; want 2, 2", exp.count(), rest.count())
	}
}

// newTestMonitorAt is newTestMonitor on an atomic clock.
func newTestMonitorAt(t *testing.T, pool *pgxpool.Pool, now *atomic.Pointer[time.Time],
	dets ...Detector) *Monitor {
	t.Helper()
	reg := NewRegistry()
	for _, d := range dets {
		if err := reg.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	return sweepMonitor(pool, now, reg)
}

// serialDetector counts its runs and the most that ran at once.
type serialDetector struct {
	countingDetector
	inFlight, most atomic.Int32
}

func (d *serialDetector) Detect(ctx context.Context, in Input) ([]Finding, error) {
	n := d.inFlight.Add(1)
	defer d.inFlight.Add(-1)
	for {
		m := d.most.Load()
		if n <= m || d.most.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	return d.countingDetector.Detect(ctx, in)
}

// Concurrency: a sweep and an analyzer cycle started together run each
// detector once, never at the same time, and report one finding per
// detector between them. The catalog fingerprint hashes cluster-wide rows
// (pg_roles), so it runs on the exclusive server: on the shared one another
// package creating a role between the cycle and the sweep changes the
// catalog, and the sweep then rightly runs the exposure detector again.
func TestSweep_AndCycleTogetherRunEachDetectorOnceSerially(t *testing.T) {
	pool, ctx := livePoolOn(t, testdb.Exclusive(t,
		"the posture fingerprint hashes cluster-wide roles"))
	for i := 0; i < 5; i++ {
		exp := &serialDetector{countingDetector: countingDetector{id: "AP-03"}}
		rest := &serialDetector{countingDetector: countingDetector{id: "AP-06"}}
		m := newTestMonitorAt(t, pool, nowAt(time.Now()), exp, rest)
		var wg sync.WaitGroup
		var cycle []analyzer.Finding
		var swept analyzer.OutOfCycle
		var cerr, serr error
		wg.Add(2)
		go func() { defer wg.Done(); cycle, cerr = m.Detect(ctx) }()
		go func() { defer wg.Done(); swept, _, serr = m.Sweep(ctx) }()
		wg.Wait()
		if cerr != nil || serr != nil {
			t.Fatalf("round %d: cycle %v, sweep %v", i, cerr, serr)
		}
		if exp.count() != 1 || rest.count() != 1 {
			t.Fatalf("round %d: runs AP-03 %d, AP-06 %d; want 1 each", i, exp.count(),
				rest.count())
		}
		if exp.most.Load() != 1 || rest.most.Load() != 1 {
			t.Fatalf("round %d: detectors ran concurrently", i)
		}
		if n := len(cycle) + len(swept.Findings); n != 2 {
			t.Fatalf("round %d: %d findings between cycle and sweep, want 2", i, n)
		}
	}
}

// State transitions: exposed (open) -> RLS enabled (gone, category
// evaluated, so the open finding resolves) -> RLS disabled (back).
func TestSweep_ExposedResolvedReexposed(t *testing.T) {
	f := newFixture(t)
	m := sweepMonitor(f.pool, nowAt(time.Now()), nil)
	m.opts.Config = func() Config {
		c := DefaultConfig()
		c.ExposedRoles = []string{f.exposed}
		return c
	}
	table := f.q("leads")
	f.exec("CREATE TABLE "+table+" (id int)", "GRANT SELECT ON "+table+" TO "+f.exposed)
	step := func(name string, want bool) {
		t.Helper()
		run, ran, err := m.Sweep(f.ctx)
		if err != nil || !ran {
			t.Fatalf("%s: Sweep = ran %v, %v", name, ran, err)
		}
		if !slices.Contains(run.Evaluated, "agent_posture:AP-03") {
			t.Fatalf("%s: AP-03 not evaluated: %v", name, run.Evaluated)
		}
		if got := findingOn(run, "agent_posture:AP-03", table) != nil; got != want {
			t.Fatalf("%s: AP-03 finding on %s = %v, want %v", name, table, got, want)
		}
	}
	step("exposed", true)
	f.exec("ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY")
	step("rls enabled", false)
	f.exec("ALTER TABLE " + table + " DISABLE ROW LEVEL SECURITY")
	step("re-exposed", true)
}

// Error propagation: when pg_sage's role cannot read pg_stat_activity,
// AP-17 (and every detector that needs the session list) is incomplete,
// never clean, and the catalog detectors still run.
func TestSweep_StatActivityDeniedMakesAP17Incomplete(t *testing.T) {
	pool, ctx := livePool(t)
	testdb.LockAgentRoles(t)
	role := "pnostat_" + suffix(t)
	secret := "pw-" + suffix(t)
	createRole(t, ctx, pool, role, "LOGIN PASSWORD '"+secret+"'")
	execAll(t, ctx, pool, "REVOKE SELECT ON pg_catalog.pg_stat_activity FROM PUBLIC")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"GRANT SELECT ON pg_catalog.pg_stat_activity TO PUBLIC")
	})
	limited := poolAs(t, ctx, role, secret)
	m := sweepMonitor(limited, nowAt(time.Now()), nil)
	run, ran, err := m.Sweep(ctx)
	if err != nil || !ran {
		t.Fatalf("Sweep = ran %v, %v; want a run with AP-17 incomplete", ran, err)
	}
	for _, c := range []string{"agent_posture:AP-17", "agent_posture:AP-01",
		"agent_posture:AP-02"} {
		if slices.Contains(run.Evaluated, c) || !slices.Contains(run.Incomplete, c) {
			t.Fatalf("%s: evaluated %v, incomplete %v; want it incomplete", c,
				run.Evaluated, run.Incomplete)
		}
	}
	for _, c := range []string{"agent_posture:AP-03", "agent_posture:AP-15"} {
		if !slices.Contains(run.Evaluated, c) {
			t.Fatalf("%s not evaluated: %v", c, run.Evaluated)
		}
	}
	// Permission denied is not transient: like a cycle, the sweep does not
	// repeat on the same catalog, and AP-17 was never reported clean.
	if again, ran, err := m.Sweep(ctx); err != nil || ran || len(again.Evaluated) != 0 {
		t.Fatalf("second Sweep = ran %v, %v, evaluated %v; want nothing", ran, err,
			again.Evaluated)
	}
}

// poolAs is a pool on the fixture database as role.
func poolAs(t *testing.T, ctx context.Context, role, secret string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, secret
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool as %s: %v", role, err)
	}
	t.Cleanup(p.Close)
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("ping as %s: %v", role, err)
	}
	return p
}

// A failed publish forgets the sweep, so neither the next sweep nor the
// next cycle takes the exposure detectors as run on this catalog.
func TestSweepOnce_FailedPublishForgetsTheRun(t *testing.T) {
	pool, ctx := livePool(t)
	testdb.LockAgentRoles(t)
	exp, rest := &countingDetector{id: "AP-03"}, &countingDetector{id: "AP-06"}
	m := newTestMonitorAt(t, pool, nowAt(time.Now()), exp, rest)
	pub := newRecordingPublisher()
	pub.err = fmt.Errorf("persist findings: %w", pgx.ErrTxClosed)
	if err := m.SweepOnce(ctx, pub); err == nil {
		t.Fatal("SweepOnce hid the publish error")
	}
	pub.err = nil
	if err := m.SweepOnce(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if _, ran := pub.last(); !ran || exp.count() != 2 {
		t.Fatalf("retry ran %v, AP-03 runs %d; want a second run", ran, exp.count())
	}
}
