package verify

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// executor-03b on real PostgreSQL: the write signal of a verified index is
// the WAL bytes per tuple its own table wrote, from collector cycles stored
// the way the collector stores them (snapstore, delta-encoded tables rows).
// The workloads run for real; only the cycles' collection times are placed
// around the build (writeFixture).

const writeSignalQueryID = int64(990_301)

// seedQueryGain writes six 10-minute query intervals of 20 calls at about
// 100 ms before executedAt and about 70 ms after: the index helps its
// queries by 30%, so only the write signal can revert it.
func seedQueryGain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, at time.Time) {
	t.Helper()
	before := []float64{2000, 2100, 1900, 2000, 2050, 1950}
	after := []float64{1400, 1450, 1350, 1400, 1420, 1380}
	sample := at.Add(-61 * time.Minute)
	calls, total := int64(1000), 50000.0
	insert := func() {
		mustExec(t, ctx, pool, `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time)
			VALUES ($1, $2, $3, $4, 0)`, sample, writeSignalQueryID, calls, total)
	}
	insert()
	for _, delta := range append(before, after...) {
		sample, calls, total = sample.Add(10*time.Minute), calls+20, total+delta
		insert()
	}
}

// watchWriteFixture runs the durable watch of the index on table at
// executedAt+61m over one-hour windows and returns its verdict.
func watchWriteFixture(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, executedAt time.Time,
	table, index string,
) (Verdict, int64) {
	t.Helper()
	decisionID, actionID := insertVerificationAction(t, ctx, pool)
	t.Cleanup(func() { cleanupVerificationAction(pool, actionID, decisionID) })
	options := DefaultOptions()
	options.Now = func() time.Time { return executedAt.Add(61 * time.Minute) }
	engine, err := NewEngine(NewPostgresObservationSource(pool),
		NewPostgresStateStore(pool, options.MinSamples), options)
	if err != nil {
		t.Fatal(err)
	}
	verdict, err := engine.Watch(ctx, WatchRequest{
		ID:       "write-signal-" + index + executedAt.Format("-20060102"),
		ActionID: actionID, ExecutedAt: executedAt,
		Table: table, IndexName: index,
		Criterion: Criterion{Kind: "per_query_latency",
			TargetIDs: []int64{writeSignalQueryID}, MinGainPct: 20, RegressPct: 15,
			WriteImpactPct: 20, Window: time.Hour, HardMax: 72 * time.Hour},
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	return verdict, actionID
}

func signalSide(t *testing.T, signal map[string]any, side string) map[string]any {
	t.Helper()
	m, ok := signal[side].(map[string]any)
	if !ok {
		t.Fatalf("write_signal = %#v, want a %s side", signal, side)
	}
	return m
}

// The executor-03 scenario on the table-scoped signal: an index is built on
// table A, then a batch writes table B (wide rows, 20,000 per cycle). The
// batch cycles are not A's, so they are unattributed; A's own deletes cost
// what they cost before the build (a delete maintains no index), so the
// index whose queries gained 30% stays.
func TestBatchOnAnotherTableIsNotChargedToTheIndex(t *testing.T) {
	pool := verifyExclusivePool(t, "cluster WAL charged to one table")
	ctx := t.Context()
	executedAt := time.Date(2098, 3, 1, 12, 0, 0, 0, time.UTC)
	f := newWriteFixture(t, ctx, pool, executedAt, "verify_wsig_a", "verify_wsig_b")
	f.write("verify_wsig_a", 120000, `CREATE TABLE public.verify_wsig_a
		WITH (autovacuum_enabled = false) AS
		SELECT g AS id, g % 97 AS customer FROM generate_series(1, 120000) g`)
	f.exec(`CREATE TABLE public.verify_wsig_b(id int, payload text)
		WITH (autovacuum_enabled = false)`)
	deleteA := func(i int) {
		f.write("verify_wsig_a", 10000, `DELETE FROM public.verify_wsig_a
			WHERE id > $1 AND id <= $1 + 10000`, i*10000)
	}
	f.cycle(executedAt.Add(-55 * time.Minute))
	for i := 0; i < 5; i++ {
		deleteA(i)
		f.cycle(executedAt.Add(time.Duration(-45+10*i) * time.Minute))
	}
	f.exec("CREATE INDEX idx_verify_wsig_a ON public.verify_wsig_a(customer)")
	f.cycle(executedAt.Add(time.Minute))
	for i := 0; i < 5; i++ {
		f.write("verify_wsig_b", 20000, `INSERT INTO public.verify_wsig_b
			SELECT g, repeat('x', 200) FROM generate_series(1, 20000) g`)
		f.cycle(executedAt.Add(time.Duration(5+10*i) * time.Minute))
		deleteA(5 + i)
		f.cycle(executedAt.Add(time.Duration(10+10*i) * time.Minute))
	}
	seedQueryGain(t, ctx, pool, executedAt)
	verdict, actionID := watchWriteFixture(t, ctx, pool, executedAt, "public.verify_wsig_a",
		"public.idx_verify_wsig_a")
	signal := writeSignalOf(t, verdict)
	t.Logf("write_signal = %v", signal)
	if !verdict.Retain || verdict.Revert || verdict.Status != "success" {
		t.Fatalf("verdict = %#v (signal %#v), want the index retained", verdict, signal)
	}
	if signal["outcome"] != writeWithinThreshold || signal["decisive"] != false {
		t.Fatalf("write_signal = %#v, want A's measured cost within the threshold", signal)
	}
	before, after := signalSide(t, signal, "before"), signalSide(t, signal, "after")
	if before["intervals"] != 5 || after["intervals"] != 5 || after["unattributed"] != 5 ||
		after["tuples"] != int64(50000) {
		t.Fatalf("sides = %#v / %#v, want 5 A intervals each and 5 B batches unattributed",
			before, after)
	}
	assertStoredVerdict(t, ctx, pool, actionID, "success")
}

// A write-heavy table A: the same inserts before and after the build, and
// after it every tuple also maintains the new index, so the WAL per
// written tuple rises far above 20%: write_impact, though the queries
// gained 30%.
func TestWriteHeavyTableRevertsAsWriteImpact(t *testing.T) {
	pool := verifyExclusivePool(t, "cluster WAL charged to one table")
	ctx := t.Context()
	executedAt := time.Date(2098, 3, 2, 12, 0, 0, 0, time.UTC)
	f := newWriteFixture(t, ctx, pool, executedAt, "verify_wsig_heavy")
	f.exec(`CREATE TABLE public.verify_wsig_heavy(id int, customer int)
		WITH (autovacuum_enabled = false)`)
	insert := func() {
		f.write("verify_wsig_heavy", 20000, `INSERT INTO public.verify_wsig_heavy
			SELECT g, g % 97 FROM generate_series(1, 20000) g`)
	}
	f.cycle(executedAt.Add(-55 * time.Minute))
	for i := 0; i < 5; i++ {
		insert()
		f.cycle(executedAt.Add(time.Duration(-45+10*i) * time.Minute))
	}
	f.exec("CREATE INDEX idx_verify_wsig_heavy ON public.verify_wsig_heavy(customer)")
	f.cycle(executedAt.Add(5 * time.Minute))
	for i := 0; i < 5; i++ {
		insert()
		f.cycle(executedAt.Add(time.Duration(15+10*i) * time.Minute))
	}
	seedQueryGain(t, ctx, pool, executedAt)
	verdict, actionID := watchWriteFixture(t, ctx, pool, executedAt,
		"public.verify_wsig_heavy",
		"public.idx_verify_wsig_heavy")
	signal := writeSignalOf(t, verdict)
	t.Logf("write_signal = %v", signal)
	if !verdict.Revert || verdict.Reason != "write_impact" || verdict.Status != "reverted" {
		t.Fatalf("verdict = %#v (signal %#v), want a write_impact revert", verdict, signal)
	}
	if signal["outcome"] != writeRegressed || signal["decisive"] != true {
		t.Fatalf("write_signal = %#v, want a decisive regression", signal)
	}
	if delta, _ := signal["delta_pct"].(float64); delta <= 20 {
		t.Fatalf("write_signal = %#v, want the per-tuple WAL to rise over 20%%", signal)
	}
	if after := signalSide(t, signal, "after"); after["tuples"] != int64(100000) {
		t.Fatalf("after = %#v, want the 100,000 measured tuples", after)
	}
	assertStoredVerdict(t, ctx, pool, actionID, "revert")
}

// System snapshots with real WAL positions and a database-wide
// blk_write_time surge, but no tables snapshots at all: the table has no
// samples, so the signal is insufficient and the database-wide numbers are
// not used in its place.
func TestNoTableSamplesIsInsufficient(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	executedAt := time.Date(2098, 3, 3, 12, 0, 0, 0, time.UTC)
	f := newWriteFixture(t, ctx, pool, executedAt, "verify_wsig_quiet")
	f.exec("CREATE TABLE public.verify_wsig_quiet(id int)")
	f.exec("CREATE INDEX idx_verify_wsig_quiet ON public.verify_wsig_quiet(id)")
	for i, offset := range []int{-50, -30, -10, 10, 30, 50} {
		f.systemOnly(executedAt.Add(time.Duration(offset)*time.Minute),
			int64(1_000_000+100_000*i), float64(1000*i*i))
	}
	seedQueryGain(t, ctx, pool, executedAt)
	verdict, actionID := watchWriteFixture(t, ctx, pool, executedAt,
		"public.verify_wsig_quiet",
		"public.idx_verify_wsig_quiet")
	signal := writeSignalOf(t, verdict)
	if !verdict.Retain || verdict.Revert || signal["outcome"] != writeInsufficient {
		t.Fatalf("verdict = %#v (signal %#v), want retained, signal insufficient",
			verdict, signal)
	}
	if _, has := signal["delta_pct"]; has {
		t.Fatalf("write_signal = %#v, want no delta", signal)
	}
	if after := signalSide(t, signal, "after"); after["intervals"] != 0 ||
		after["unknown"] != 2 {
		t.Fatalf("after = %#v, want two unknown intervals (no tables rows)", after)
	}
	assertStoredVerdict(t, ctx, pool, actionID, "success")
}

// Stored cycles of exactly +20% WAL per tuple (100 -> 120 bytes) do not
// revert; 100 -> 120.1 bytes does. The cycles are written through
// snapstore, so the tables rows after the first are deltas the read must
// decode.
func TestWriteImpactBoundaryOnStoredCycles(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	for i, tc := range []struct {
		afterBytes int64
		revert     bool
	}{{120_000, false}, {120_100, true}} {
		executedAt := time.Date(2098, 3, 4+i, 12, 0, 0, 0, time.UTC)
		f := newWriteFixture(t, ctx, pool, executedAt, "verify_wsig_edge")
		f.exec("CREATE TABLE IF NOT EXISTS public.verify_wsig_edge(id int)")
		f.exec("CREATE INDEX IF NOT EXISTS idx_verify_wsig_edge ON public.verify_wsig_edge(id)")
		lsn, tuples := int64(1_000_000), int64(0)
		for c := 0; c < 12; c++ {
			at := executedAt.Add(time.Duration(-55+10*c) * time.Minute)
			if c == 6 {
				lsn += 5_000_000 // the build, between the windows
			}
			f.synthetic(at, lsn, tuples)
			step := int64(100_000)
			if c >= 6 {
				step = tc.afterBytes
			}
			lsn, tuples = lsn+step, tuples+1000
		}
		seedQueryGain(t, ctx, pool, executedAt)
		verdict, _ := watchWriteFixture(t, ctx, pool, executedAt, "public.verify_wsig_edge",
			"public.idx_verify_wsig_edge")
		signal := writeSignalOf(t, verdict)
		if verdict.Revert != tc.revert || (tc.revert && verdict.Reason != "write_impact") {
			t.Fatalf("after %d bytes: verdict = %#v (signal %#v), want revert=%v",
				tc.afterBytes, verdict, signal, tc.revert)
		}
		assertSignalFloat(t, signal, "delta_pct", float64(tc.afterBytes-100_000)/1000)
		if b := signalSide(t, signal, "before"); b["intervals"] != 5 ||
			b["bytes_per_tuple"] != 100.0 {
			t.Fatalf("before = %#v, want 5 intervals at 100 bytes per tuple", b)
		}
		f.cleanup()
	}
}

// A window with more collector cycles than maxWriteCycles is read at no
// more than maxWriteCycles of them, evenly, first and last included: the
// whole window's tuples are still measured.
func TestWriteCostReadsAtMostMaxWriteCycles(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	executedAt := time.Date(2098, 3, 8, 12, 0, 0, 0, time.UTC)
	f := newWriteFixture(t, ctx, pool, executedAt, "verify_wsig_dense")
	f.exec("CREATE TABLE public.verify_wsig_dense(id int)")
	from := executedAt.Add(-2 * time.Hour)
	for c := 0; c <= 120; c++ {
		f.synthetic(from.Add(time.Duration(c)*time.Minute), int64(1000+100_000*c),
			int64(1000*c))
	}
	cost, err := NewPostgresObservationSource(pool).WriteCost(ctx,
		"public.verify_wsig_dense", from, executedAt)
	if err != nil {
		t.Fatalf("WriteCost: %v", err)
	}
	if cost.Intervals > maxWriteCycles-1 || cost.Intervals < maxWriteCycles/2 ||
		cost.Tuples != 120_000 || cost.BytesPerTuple != 100 {
		t.Fatalf("cost = %+v, want at most %d intervals covering all 120,000 tuples",
			cost, maxWriteCycles-1)
	}
}

// assertStoredVerdict checks the durable verdict of actionID; a success is
// completed, a revert stays open for the executor's drop.
func assertStoredVerdict(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, actionID int64, want string,
) {
	t.Helper()
	var verdict string
	var completed bool
	err := pool.QueryRow(ctx, `SELECT verdict, completed_at IS NOT NULL
		FROM sage.verification WHERE action_log_id=$1`, actionID).Scan(&verdict, &completed)
	if err != nil || verdict != want || completed != (want == "success") {
		t.Fatalf("stored verification = %q completed=%v, %v; want %q", verdict, completed,
			err, want)
	}
}

// A write_impact revert stored before the upgrade (no write_scope in its
// baseline: the database-wide signal), whose drop has not run yet, is
// reopened by the startup sweep and observed again. A table-scoped
// write_impact revert (executor-03b) is a real verdict and stays.
func TestReopenSweepReopensOnlyLegacyWriteImpactReverts(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	now := time.Now().UTC()
	store, pending := createDBWatch(t, ctx, pool, now.Add(-3*time.Hour))
	_, done := createDBWatch(t, ctx, pool, now.Add(-3*time.Hour))
	_, regressed := createDBWatch(t, ctx, pool, now.Add(-3*time.Hour))
	_, scoped := createDBWatch(t, ctx, pool, now.Add(-3*time.Hour))
	for _, row := range []struct {
		id        int64
		reason    string
		completed bool
	}{{pending.ActionID, "write_impact", false}, {done.ActionID, "write_impact", true},
		{regressed.ActionID, "query_regression", false},
		{scoped.ActionID, "write_impact", false}} {
		mustExec(t, ctx, pool, `UPDATE sage.verification SET verdict='revert', reason=$2,
			hard_deadline_at=now()-interval '1 hour',
			completed_at=CASE WHEN $3 THEN now() END WHERE action_log_id=$1`,
			row.id, row.reason, row.completed)
	}
	for _, legacy := range []int64{pending.ActionID, done.ActionID, regressed.ActionID} {
		mustExec(t, ctx, pool, `UPDATE sage.verification SET baseline = baseline - 'write_scope'
			WHERE action_log_id=$1`, legacy)
	}
	reopened, err := store.ReopenObservationRetries(ctx)
	if err != nil || reopened < 1 {
		t.Fatalf("ReopenObservationRetries = %d, %v; want the pending write_impact row",
			reopened, err)
	}
	for id, want := range map[int64]string{pending.ActionID: "extended",
		done.ActionID: "revert", regressed.ActionID: "revert", scoped.ActionID: "revert"} {
		var verdict, reason string
		err := pool.QueryRow(ctx, `SELECT verdict, reason FROM sage.verification
			WHERE action_log_id=$1`, id).Scan(&verdict, &reason)
		if err != nil || verdict != want {
			t.Fatalf("action %d verdict = %q (%q), %v; want %q", id, verdict, reason, err, want)
		}
		if want == "extended" && reason != "observation_retry: reopened write_impact verdict" {
			t.Fatalf("reopened reason = %q", reason)
		}
	}
	if again, err := store.ReopenObservationRetries(ctx); err != nil || again != 0 {
		t.Fatalf("second sweep = %d, %v; want 0 (idempotent)", again, err)
	}
}
