package verify

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/planhash"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The mechanism gate on real PostgreSQL (lab-01b): a real index, the
// targets' EXPLAIN (FORMAT JSON) stored in sage.explain_cache the way
// pg_sage stores it, and query_store samples carrying the fingerprints.

type mechFixture struct {
	table, index       string
	indexed, untouched int64
	step               time.Time
}

func explainJSON(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) []byte {
	t.Helper()
	var plan string
	if err := pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql).Scan(&plan); err != nil {
		t.Fatalf("explain %s: %v", sql, err)
	}
	return []byte(plan)
}

func fingerprint(t *testing.T, plan []byte) string {
	t.Helper()
	h, err := planhash.Compute(plan)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return h
}

func storePlan(t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64,
	at time.Time, plan []byte) {
	t.Helper()
	mustExec(t, ctx, pool, `INSERT INTO sage.explain_cache
		(captured_at, queryid, query_text, plan_json, source, plan_hash)
		VALUES ($1, $2, 'fixture', $3::jsonb, 'explain', $4)`, at, qid, string(plan),
		fingerprint(t, plan))
}

func storeSample(t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64,
	at time.Time, hash string) {
	t.Helper()
	mustExec(t, ctx, pool, `INSERT INTO sage.query_store
		(captured_at, queryid, calls, total_exec_time, mean_exec_time, rows, plan_hash)
		VALUES ($1, $2, 10, 5, 0.5, 10, $3)`, at, qid, hash)
}

// newMechFixture builds a table, plans a status lookup (a seq scan), then
// creates the index the step adds: the lookup's plan flips to the index
// 30 s after the step, while a query on another table keeps its plan.
func newMechFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) mechFixture {
	t.Helper()
	n := time.Now().UnixNano()
	f := mechFixture{table: fmt.Sprintf("public.vmech_%d", n),
		index: fmt.Sprintf("vmech_%d_status_idx", n), indexed: 990_400_000 + n%100_000,
		untouched: 990_500_000 + n%100_000}
	other := f.table + "_other"
	mustExec(t, ctx, pool, "CREATE TABLE "+f.table+" (id int, status int, pad text)")
	mustExec(t, ctx, pool, "CREATE TABLE "+other+" (a int)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+f.table+", "+other)
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.explain_cache "+
			"WHERE queryid = ANY($1)", []int64{f.indexed, f.untouched})
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.query_store "+
			"WHERE queryid = ANY($1)", []int64{f.indexed, f.untouched})
	})
	mustExec(t, ctx, pool, "INSERT INTO "+f.table+" SELECT i, i % 1000, repeat('x', 40) "+
		"FROM generate_series(1, 50000) i")
	mustExec(t, ctx, pool, "INSERT INTO "+other+" SELECT i FROM generate_series(1, 100) i")
	mustExec(t, ctx, pool, "ANALYZE "+f.table+"; ANALYZE "+other)
	lookup := "SELECT * FROM " + f.table + " WHERE status = 7"
	untouched := "SELECT * FROM " + other + " WHERE a = 1"
	before, steady := explainJSON(t, ctx, pool, lookup), explainJSON(t, ctx, pool, untouched)
	f.step = time.Now().UTC().Truncate(time.Second)
	mustExec(t, ctx, pool, "CREATE INDEX "+f.index+" ON "+f.table+" (status)")
	mustExec(t, ctx, pool, "ANALYZE "+f.table)
	after := explainJSON(t, ctx, pool, lookup)
	if !planhash.UsesIndex(after, f.index) || planhash.UsesIndex(before, f.index) {
		t.Fatalf("fixture plans do not flip to %s:\nbefore %s\nafter %s", f.index, before,
			after)
	}
	storePlan(t, ctx, pool, f.indexed, f.step.Add(-time.Minute), before)
	storePlan(t, ctx, pool, f.indexed, f.step.Add(20*time.Second), after)
	storePlan(t, ctx, pool, f.untouched, f.step.Add(20*time.Second), steady)
	hA, hB, hC := fingerprint(t, before), fingerprint(t, after), fingerprint(t, steady)
	for _, s := range []struct {
		qid  int64
		at   time.Duration
		hash string
	}{{f.indexed, -2 * time.Minute, hA}, {f.indexed, -time.Minute, hA},
		{f.indexed, 30 * time.Second, hB}, {f.indexed, 90 * time.Second, hB},
		{f.untouched, -time.Minute, hC}, {f.untouched, 30 * time.Second, hC},
		{f.untouched, 90 * time.Second, hC}} {
		storeSample(t, ctx, pool, s.qid, f.step.Add(s.at), s.hash)
	}
	return f
}

func (f mechFixture) input(targets ...int64) MechanismInput {
	return MechanismInput{Class: MechanismIndexCreate, Index: "public." + f.index,
		Targets: targets, ExecutedAt: f.step, Cycle: time.Minute}
}

// Integration: the indexed lookup passes the gate, the untouched query is
// unexplained; alone, the untouched query is no_mechanism.
func TestMechanismOnRealPlans(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	f := newMechFixture(t, ctx, pool)
	src := NewPostgresObservationSource(pool)
	to := f.step.Add(time.Hour)
	m, err := src.CheckMechanism(ctx, f.input(f.indexed, f.untouched), to)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Passed() || !slices.Equal(m.Counted, []int64{f.indexed}) ||
		!slices.Equal(m.Unexplained, []int64{f.untouched}) {
		t.Fatalf("mechanism %+v, want %d counted and %d unexplained", m, f.indexed,
			f.untouched)
	}
	if got := targetReason(m, f.untouched); got != ReasonMechUnchanged {
		t.Fatalf("untouched query: %q, want %q", got, ReasonMechUnchanged)
	}
	if m.PlanBasis != PlanBasisEstimated {
		t.Fatalf("plan basis %q: EXPLAIN without ANALYZE is an estimated plan", m.PlanBasis)
	}
	m, err = src.CheckMechanism(ctx, f.input(f.untouched), to)
	if err != nil || m.Passed() || m.Reason != ReasonNoMechanism {
		t.Fatalf("untouched only: %+v, %v", m, err)
	}
	ev, err := src.PlanEvidence(ctx, []int64{f.indexed, f.untouched}, f.step, to)
	if err != nil {
		t.Fatal(err)
	}
	if h := ev.Hashes[f.indexed]; len(h) != 2 || h[0].Hash == h[1].Hash ||
		!h[1].At.Equal(f.step.Add(30*time.Second)) {
		t.Fatalf("indexed fingerprints %+v, want the last before and the first change", h)
	}
	if !planhash.UsesIndex(ev.Plans[f.indexed], f.index) {
		t.Fatalf("latest plan of the lookup is the pre-step one: %s", ev.Plans[f.indexed])
	}
}

// A plan captured before the step is not the latest plan after it.
func TestMechanismPlanEvidenceReadsOnlyPlansAfterTheStep(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	f := newMechFixture(t, ctx, pool)
	src := NewPostgresObservationSource(pool)
	ev, err := src.PlanEvidence(ctx, []int64{f.indexed}, f.step.Add(time.Minute),
		f.step.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ev.Plans[f.indexed]; ok {
		t.Fatalf("a plan from before the step was read as the latest: %s",
			ev.Plans[f.indexed])
	}
	if ev, err = src.PlanEvidence(ctx, nil, f.step, f.step.Add(time.Hour)); err != nil ||
		len(ev.Hashes) != 0 || len(ev.Plans) != 0 {
		t.Fatalf("no ids: %+v, %v", ev, err)
	}
}

// Error propagation: an explain_cache read error is mechanism_unavailable
// with its cause in the evidence, insufficient evidence, never a revert.
func TestMechanismExplainCacheErrorIsUnavailable(t *testing.T) {
	pool := verifyPoolOn(t, testdb.CreateDatabase(t, "verify_mech_err"))
	ctx := t.Context()
	mustExec(t, ctx, pool, "DROP TABLE sage.explain_cache CASCADE")
	step := time.Now().UTC()
	in := MechanismInput{Class: MechanismIndexCreate, Index: "x_idx", Targets: []int64{1},
		ExecutedAt: step, Cycle: time.Minute}
	m, err := NewPostgresObservationSource(pool).CheckMechanism(ctx, in, step.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if m.Outcome != OutcomeInsufficient ||
		!strings.HasPrefix(m.Reason, ReasonMechanismUnavailable+": ") ||
		!strings.Contains(m.Reason, "explain_cache") {
		t.Fatalf("mechanism %+v, want mechanism_unavailable naming explain_cache", m)
	}
	v, stop := m.Gate()
	if !stop || v.Revert || v.Outcome != OutcomeInsufficient {
		t.Fatalf("gate %+v: an unreadable cache must not revert", v)
	}
	if e, _ := v.Evidence["mechanism"].(map[string]any); e["reason"] != m.Reason {
		t.Fatalf("evidence %v lacks the cause", v.Evidence)
	}
}

// Error propagation without a pool: unavailable, never a credited target;
// a bad input is still the caller's error.
func TestCheckMechanismWithoutPool(t *testing.T) {
	in := MechanismInput{Class: MechanismIndexCreate, Index: "x_idx", Targets: []int64{1},
		ExecutedAt: mechStep, Cycle: time.Minute}
	var src *PostgresObservationSource
	m, err := src.CheckMechanism(context.Background(), in, mechStep.Add(time.Hour))
	if err != nil || m.Passed() || !strings.HasPrefix(m.Reason, ReasonMechanismUnavailable) {
		t.Fatalf("nil source: %+v, %v", m, err)
	}
	in.Class = "nonsense"
	if _, err := src.CheckMechanism(context.Background(), in, mechStep); err == nil {
		t.Fatal("bad input accepted")
	}
	in.Class, in.Metric = MechanismConfigGUC, "temp_spills"
	if m, err := src.CheckMechanism(context.Background(), in, mechStep); err != nil ||
		!m.Passed() {
		t.Fatalf("a setting needs no plan read: %+v, %v", m, err)
	}
}
