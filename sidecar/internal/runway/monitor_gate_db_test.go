package runway

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-01 on real PostgreSQL: a low-age cluster burning XIDs fast
// opens no wraparound finding (freezing bounds its runway); the same
// trend past the freeze maximum warns.
//
// No concurrency test: one monitor goroutine ticks per runtime.

// xidRewriter runs the real xid_runway probe and edits its row: the
// tests drop its counters (so the tick writes no xid or mxid sample) or
// move its age or freeze maximum, keeping every other column real.
type xidRewriter struct {
	inner ProbeRunner
	edit  map[string]any
}

func (x xidRewriter) Run(ctx context.Context, id probes.ID, a probes.Args) probes.Result {
	res := x.inner.Run(ctx, id, a)
	if id != probes.XIDRunwayProbe || len(res.Rows) != 1 {
		return res
	}
	row := probes.Row{}
	for k, v := range res.Rows[0] {
		row[k] = v
	}
	for k, v := range x.edit {
		row[k] = v
	}
	res.Rows = []probes.Row{row}
	return res
}

func (x xidRewriter) RunBackground(ctx context.Context, id probes.ID,
	a probes.Args) probes.Result {
	if id == probes.XIDRunwayProbe {
		return x.Run(ctx, id, a)
	}
	return x.inner.RunBackground(ctx, id, a)
}

// unsampledXID reads the real xid_runway without its counters, so a
// tick writes no xid or mxid sample, with edit applied on top.
func unsampledXID(inner ProbeRunner, edit map[string]any) xidRewriter {
	e := map[string]any{"next_xid": nil, "mxid_counter": nil}
	for k, v := range edit {
		e[k] = v
	}
	return xidRewriter{inner: inner, edit: e}
}

// pastGateXID is the reading the forecast-02 tests tick with: no counters
// (only the seeded samples exist) and a cluster age (2.02e9, the seeded
// trend's) past the 200M freeze maximum, so the gate projects the trend.
func pastGateXID(inner ProbeRunner) xidRewriter {
	return unsampledXID(inner, map[string]any{"cluster_xid_age": int64(2020000000),
		"freeze_max_age": int64(200000000)})
}

// mustTick ticks and fails on any problem.
func mustTick(t *testing.T, ctx context.Context, m *Monitor) TickResult {
	t.Helper()
	res, err := m.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	return res
}

// burnXIDs consumes perStep transaction IDs (one transaction calling
// pg_current_xact_id() each) before each of steps samples, then spaces
// the xid and mxid samples step apart ending now: a compressed timescale.
func burnXIDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, m *Monitor,
	steps, perStep int, step time.Duration) {
	t.Helper()
	for s := 0; s < steps; s++ {
		for i := 0; i < perStep; i++ {
			if _, err := pool.Exec(ctx, `SELECT pg_catalog.pg_current_xact_id()`); err != nil {
				t.Fatalf("burn step %d: %v", s, err)
			}
		}
		if _, err := m.Sample(ctx); err != nil {
			t.Fatalf("sample step %d: %v", s, err)
		}
	}
	if _, err := pool.Exec(ctx, `WITH o AS (
		    SELECT id, row_number() OVER (PARTITION BY kind ORDER BY sampled_at DESC) - 1 AS k
		    FROM sage.runway_samples WHERE kind IN ('xid', 'mxid') AND subject = 'cluster')
		UPDATE sage.runway_samples r
		SET sampled_at = now() - o.k * make_interval(secs => $1)
		FROM o WHERE r.id = o.id`, step.Seconds()); err != nil {
		t.Fatalf("compress the samples: %v", err)
	}
}

func countCategory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cat string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.findings WHERE category = $1`,
		cat).Scan(&n); err != nil {
		t.Fatalf("count %s findings: %v", cat, err)
	}
	return n
}

// CHECK-02: 5,004 transactions on a fresh cluster, sampled 100 ms apart,
// measure more than 4,000 XIDs/s: ungated, the warning limit is under 14
// days away. Below autovacuum_freeze_max_age the tick opens no
// forecast_wraparound_runway finding. Read with a freeze maximum below
// the real age (1,000), the same trend warns, projected with the net age
// slope (template0 is never vacuumed, so the cluster age rises with
// every XID).
func TestMonitorTick_LowAgeBurnOpensNoWraparoundFinding(t *testing.T) {
	pool, ctx := exclusivePool(t, "the cluster-wide XID slope of a low-age burn")
	resetXID(t, ctx, pool)
	sampler := newTestMonitor(t, pool, liveRunner(pool), nil)
	burnXIDs(t, ctx, pool, sampler, 12, 417, 100*time.Millisecond)
	set, err := probes.RunwayTrendsOf(liveRunner(pool).Run(ctx, probes.RunwayTrendsProbe,
		probes.Args{Window: 6 * time.Hour}))
	if err != nil {
		t.Fatalf("trends: %v", err)
	}
	tr, ok := probes.FindTrend(set.Trends, probes.RunwayXID, probes.SubjectCluster)
	horizon := testOptions().WraparoundHorizon
	if !ok || tr.Samples != 12 || !within(tr.SecondsToLimit(), horizon) {
		t.Fatalf("premise: xid trend %+v (%v) does not project inside %s", tr, ok, horizon)
	}
	m := newTestMonitor(t, pool, unsampledXID(liveRunner(pool), nil), nil)
	m.opts.MinSpan = time.Second
	res := mustTick(t, ctx, m)
	var wrap []Runway // other tests' series (sequences) may project too
	for _, r := range res.Runways {
		if r.Category == CategoryWraparound {
			wrap = append(wrap, r)
		}
	}
	if n := countCategory(t, ctx, pool, CategoryWraparound); n != 0 || len(wrap) != 0 {
		t.Fatalf("a low-age burn opened %d wraparound findings (runways %+v)", n, wrap)
	}
	past := newTestMonitor(t, pool, unsampledXID(liveRunner(pool),
		map[string]any{"freeze_max_age": int64(1000)}), nil)
	past.opts.MinSpan = time.Second
	mustTick(t, ctx, past)
	var sev, basis string
	var freezeMax float64
	if err := pool.QueryRow(ctx, `SELECT severity, detail->>'projection_basis',
		(detail->>'freeze_max_age')::float8 FROM sage.findings
		WHERE category = $1 AND object_identifier = 'xid' AND status = 'open'`,
		CategoryWraparound).Scan(&sev, &basis, &freezeMax); err != nil {
		t.Fatalf("past the gate: no open xid finding: %v", err)
	}
	if sev != SeverityWarning || basis != BasisNetAgeSlope || freezeMax != 1000 {
		t.Fatalf("past the gate: severity %s basis %q freeze_max_age %v", sev, basis,
			freezeMax)
	}
}
