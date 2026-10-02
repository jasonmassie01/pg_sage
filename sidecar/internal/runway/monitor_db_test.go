package runway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The runway monitor on the collector tick against real PostgreSQL: it
// samples the series into sage.runway_samples, opens a forecast finding
// when a runway crosses its horizon, starts one pre-incident
// investigation per finding and severity, and resolves the finding once
// the runway clears. It never resolves what it could not evaluate.

type fakeStarter struct {
	mu       sync.Mutex
	triggers []sre.Trigger
	err      error
}

func (f *fakeStarter) Start(_ context.Context, t sre.Trigger) (sre.Investigation, bool,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggers = append(f.triggers, t)
	return sre.Investigation{}, true, f.err
}

func (f *fakeStarter) started() []sre.Trigger {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sre.Trigger(nil), f.triggers...)
}

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

// seedSequenceTrend writes 12 samples over an hour of a sequence that
// reaches its limit in about 10 days.
func seedSequenceTrend(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.runway_samples WHERE subject = $1", name)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value, counter, limit_value)
		SELECT 'sequence', $1, 'seed', now() - make_interval(mins => 60 - i * 5),
		       2e9 + i * 5 * 60 * 170, 2e9 + i * 5 * 60 * 170, 2147483647
		FROM generate_series(0, 11) i`, name); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func newTestMonitor(t *testing.T, pool *pgxpool.Pool, runner ProbeRunner,
	st Starter) *Monitor {
	t.Helper()
	opts := testOptions()
	opts.Retention = 48 * time.Hour
	opts.Interval = time.Minute
	m, err := NewMonitor(pool, runner, st, opts, func(string, string, ...any) {})
	if err != nil {
		t.Fatalf("NewMonitor: %v", err)
	}
	return m
}

func openFinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cat,
	ident string) (int64, string, bool) {
	t.Helper()
	var id int64
	var sev string
	err := pool.QueryRow(ctx, `SELECT id, severity FROM sage.findings
		WHERE category = $1 AND object_identifier = $2 AND status = 'open'`, cat, ident).
		Scan(&id, &sev)
	return id, sev, err == nil
}

func TestMonitorTick_OpensAFindingAndAnInvestigation(t *testing.T) {
	pool, ctx := livePool(t)
	name := fmt.Sprintf("public.mon_%d_seq", time.Now().UnixNano())
	seedSequenceTrend(t, ctx, pool, name)
	st := &fakeStarter{}
	m := newTestMonitor(t, pool, probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(1)), st)
	res, err := m.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	id, sev, ok := openFinding(t, ctx, pool, CategorySequence, name)
	if !ok || sev != SeverityWarning {
		t.Fatalf("finding open=%v severity %q (tick %+v)", ok, sev, res)
	}
	var want *sre.Trigger
	for _, tr := range st.started() {
		if tr.Subject == "sequence "+name {
			tr := tr
			want = &tr
		}
	}
	if want == nil || want.Kind != sre.TriggerSequence ||
		want.IdempotencyKey != fmt.Sprintf("runway:%d:warning", id) ||
		want.CaseID != "finding:orders:"+CategorySequence+":sequence:"+name {
		t.Fatalf("started = %+v, want the sequence investigation of finding %d",
			st.started(), id)
	}
	if res.Sampled == 0 {
		t.Fatalf("tick sampled nothing: %+v", res)
	}
	var xid int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.runway_samples
		WHERE kind = 'xid' AND subject = 'cluster'`).Scan(&xid); err != nil || xid == 0 {
		t.Fatalf("xid samples = %d (%v)", xid, err)
	}
	// The runway clears: its samples age out of the lookback; the finding
	// resolves on the next tick and no investigation starts.
	if _, err := pool.Exec(ctx, `DELETE FROM sage.runway_samples WHERE subject = $1`,
		name); err != nil {
		t.Fatalf("clear: %v", err)
	}
	before := len(st.started())
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if _, _, ok := openFinding(t, ctx, pool, CategorySequence, name); ok {
		t.Fatal("a cleared runway's finding stayed open")
	}
	for _, tr := range st.started()[before:] {
		if strings.Contains(tr.Subject, name) {
			t.Fatalf("a cleared runway started %+v", tr)
		}
	}
}

func TestMonitorTick_InvestigateOffOnlyFindings(t *testing.T) {
	pool, ctx := livePool(t)
	name := fmt.Sprintf("public.off_%d_seq", time.Now().UnixNano())
	seedSequenceTrend(t, ctx, pool, name)
	st := &fakeStarter{}
	m := newTestMonitor(t, pool, probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(1)), st)
	m.opts.Investigate = false
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, _, ok := openFinding(t, ctx, pool, CategorySequence, name); !ok {
		t.Fatal("no finding with investigations off")
	}
	if n := len(st.started()); n != 0 {
		t.Fatalf("started %d investigations with investigate off", n)
	}
}

// failingRunner fails one probe and delegates the rest.
type failingRunner struct {
	inner ProbeRunner
	fail  probes.ID
}

func (f failingRunner) Run(ctx context.Context, id probes.ID, a probes.Args) probes.Result {
	if id == f.fail {
		return probes.Result{ProbeID: id, Status: probes.StatusError,
			Reason: "statement_timeout", ObservedAt: time.Now()}
	}
	return f.inner.Run(ctx, id, a)
}

// Error propagation: without trends nothing is evaluated, so open
// findings stay open; a starter failure is logged, not fatal.
func TestMonitorTick_UnreadableTrendsResolveNothing(t *testing.T) {
	pool, ctx := livePool(t)
	name := fmt.Sprintf("public.keep_%d_seq", time.Now().UnixNano())
	seedSequenceTrend(t, ctx, pool, name)
	real := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
	st := &fakeStarter{err: errors.New("coordinator not bound")}
	m := newTestMonitor(t, pool, real, st)
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("tick with a failing starter: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.runway_samples WHERE subject = $1`,
		name); err != nil {
		t.Fatalf("clear: %v", err)
	}
	m.runner = failingRunner{inner: real, fail: probes.RunwayTrendsProbe}
	res, err := m.Tick(ctx)
	if err == nil || !strings.Contains(err.Error(), "runway_trends") {
		t.Fatalf("tick error = %v, want the unreadable trends named", err)
	}
	if _, _, ok := openFinding(t, ctx, pool, CategorySequence, name); !ok {
		t.Fatalf("a finding was resolved without evaluating it (%+v)", res)
	}
}

// Old samples are pruned past the retention.
func TestMonitorTick_PrunesOldSamples(t *testing.T) {
	pool, ctx := livePool(t)
	name := fmt.Sprintf("old_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value)
		VALUES ('wal_slot', $1, 'e', now() - interval '3 days', 1),
		       ('wal_slot', $1, 'e', now() - interval '1 hour', 2)`, name); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := newTestMonitor(t, pool, probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(1)), nil)
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM sage.runway_samples WHERE subject = $1",
		name).Scan(&n)
	if n != 1 {
		t.Fatalf("%d samples left, want only the one inside the retention", n)
	}
}

// standbyRunner reports a server in recovery.
type standbyRunner struct{ inner ProbeRunner }

func (s standbyRunner) Run(ctx context.Context, id probes.ID, a probes.Args) probes.Result {
	res := s.inner.Run(ctx, id, a)
	if id == probes.WALRunwayProbe && len(res.Rows) == 1 {
		res.Rows[0]["in_recovery"] = true
	}
	return res
}

// A standby is read-only: the monitor samples nothing there.
func TestMonitorTick_StandbyWritesNoSamples(t *testing.T) {
	pool, ctx := livePool(t)
	var before, after int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM sage.runway_samples").Scan(&before)
	m := newTestMonitor(t, pool, standbyRunner{inner: probes.NewRunner(pool,
		probes.Catalog(), probes.NewLimiter(1))}, nil)
	res, err := m.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM sage.runway_samples").Scan(&after)
	if res.Sampled != 0 || after > before {
		t.Fatalf("standby sampled %d (rows %d -> %d)", res.Sampled, before, after)
	}
}

// A restarted sequence starts a new epoch, so its trend never spans the
// reset.
func TestMonitorTick_SequenceRestartStartsANewEpoch(t *testing.T) {
	pool, ctx := livePool(t)
	seq := fmt.Sprintf("mon_epoch_%d_seq", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SEQUENCE %[1]s AS integer;
		SELECT setval('%[1]s', 2000000000)`, seq)); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SEQUENCE "+seq) })
	m := newTestMonitor(t, pool, probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(1)), nil)
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, err := pool.Exec(ctx, "ALTER SEQUENCE "+seq+" RESTART WITH 1500000000; SELECT nextval('"+
		seq+"')"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	var epochs int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT epoch) FROM sage.runway_samples
		WHERE kind = 'sequence' AND subject = $1`, "public."+seq).Scan(&epochs); err != nil ||
		epochs != 2 {
		t.Fatalf("epochs = %d (%v), want 2 across the restart", epochs, err)
	}
}

// State transition: a restarted monitor continues each series in its
// epoch (it reads the last samples) instead of starting new ones.
func TestMonitorTick_RestartContinuesTheSeries(t *testing.T) {
	pool, ctx := livePool(t)
	runner := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
	if _, err := newTestMonitor(t, pool, runner, nil).Tick(ctx); err != nil {
		t.Fatalf("first monitor: %v", err)
	}
	if _, err := newTestMonitor(t, pool, runner, nil).Tick(ctx); err != nil {
		t.Fatalf("restarted monitor: %v", err)
	}
	var epochs, samples int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT epoch), count(*)
		FROM sage.runway_samples WHERE kind = 'wal_position' AND subject = 'cluster'
		  AND sampled_at > now() - interval '1 minute'`).Scan(&epochs, &samples); err != nil {
		t.Fatalf("count: %v", err)
	}
	if samples < 2 || epochs != 1 {
		t.Fatalf("%d samples in %d epochs, want one epoch across the restart", samples,
			epochs)
	}
}
