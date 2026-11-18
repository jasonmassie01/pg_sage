package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// guard-02c on a real server: the plan of a query on a 99.9% NULL column
// flips when an ANALYZE samples a different distribution, and the rule
// names the analyzed relation with both plan hashes.

const flipQueryBase = 918_000_000

type flipFixture struct {
	pool  *pgxpool.Pool
	a     *Analyzer
	table string
	qid   int64
}

func newFlipFixture(t *testing.T, n int64) *flipFixture {
	t.Helper()
	pool := phase2Pool(t)
	a, _ := recordingAnalyzer(t, pool, 7)
	ctx := context.Background()
	f := &flipFixture{pool: pool, a: a, qid: flipQueryBase + n,
		table: fmt.Sprintf("flip_%d_%d", n, time.Now().UnixNano())}
	clean := func() {
		c := context.Background()
		_, _ = pool.Exec(c, "DELETE FROM sage.explain_cache WHERE queryid = $1", f.qid)
		_, _ = pool.Exec(c, `DELETE FROM sage.action_log WHERE finding_id IN (SELECT id
			FROM sage.findings WHERE category = $1 AND object_identifier = $2)`,
			categoryPlanFlip, f.ident())
		_, _ = pool.Exec(c, "DELETE FROM sage.findings WHERE category = $1 AND "+
			"object_identifier = $2", categoryPlanFlip, f.ident())
		_, _ = pool.Exec(c, "DROP TABLE IF EXISTS public."+f.table)
	}
	clean()
	t.Cleanup(clean)
	if _, err := pool.Exec(ctx, `CREATE TABLE public.`+f.table+` AS
		SELECT g AS id, CASE WHEN g % 1000 = 0 THEN g END AS v
		  FROM generate_series(1, 100000) g;
		CREATE INDEX ON public.`+f.table+` (v)`); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	return f
}

func (f *flipFixture) ident() string { return fmt.Sprintf("queryid:%d", f.qid) }

// analyze runs ANALYZE and waits until pg_stat_user_tables shows it
// (PostgreSQL 14 reports through the statistics collector).
func (f *flipFixture) analyze(t *testing.T) time.Time {
	t.Helper()
	ctx := context.Background()
	var before *time.Time
	_ = f.pool.QueryRow(ctx, `SELECT last_analyze FROM pg_stat_user_tables
		WHERE relid = to_regclass($1)`, "public."+f.table).Scan(&before)
	if _, err := f.pool.Exec(ctx, "ANALYZE public."+f.table); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var at *time.Time
		err := f.pool.QueryRow(ctx, `SELECT pg_stat_clear_snapshot(), last_analyze
			FROM pg_stat_user_tables WHERE relid = to_regclass($1)`, "public."+f.table).
			Scan(nil, &at)
		if err == nil && at != nil && (before == nil || at.After(*before)) {
			return *at
		}
		if time.Now().After(deadline) {
			t.Fatalf("last_analyze of %s never advanced (%v)", f.table, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// capture stores the query's current EXPLAIN as a plan capture.
func (f *flipFixture) capture(t *testing.T) []byte {
	t.Helper()
	ctx := context.Background()
	q := "SELECT * FROM " + f.table + " WHERE v IS NOT NULL"
	var plan []byte
	if err := f.pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+q).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO sage.explain_cache (captured_at, queryid,
		query_text, plan_json, source, total_cost) VALUES (clock_timestamp(), $1, $2,
		$3::jsonb, 'explain_estimate', ($3::jsonb->0->'Plan'->>'Total Cost')::float8)`,
		f.qid, q, plan); err != nil {
		t.Fatalf("store capture: %v", err)
	}
	return plan
}

func (f *flipFixture) run(t *testing.T) ([]Finding, []Finding) {
	t.Helper()
	ctx := context.Background()
	f.a.eval = newCycleEval()
	pairs, ok := f.a.loadPlanPairs(ctx)
	if !ok {
		t.Fatal("plan pairs could not be read")
	}
	return pick(rulePlanRegression(pairs), f.ident()),
		pick(f.a.checkPlanFlips(ctx, pairs, ok), f.ident())
}

func pick(fs []Finding, ident string) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.ObjectIdentifier == ident {
			out = append(out, f)
		}
	}
	return out
}

// flip makes the fixture's plan flip: a plan with 0.1% non-NULL v, then
// every row set and an ANALYZE that sees it.
func (f *flipFixture) flip(t *testing.T) time.Time {
	t.Helper()
	f.analyze(t)
	f.capture(t)
	if _, err := f.pool.Exec(context.Background(),
		"UPDATE public."+f.table+" SET v = id"); err != nil {
		t.Fatalf("update: %v", err)
	}
	at := f.analyze(t)
	f.capture(t)
	return at
}

func TestPlanFlipDB_NullColumnAnalyzeFlipsThePlan(t *testing.T) {
	f := newFlipFixture(t, 1)
	at := f.flip(t)
	regressions, flips := f.run(t)
	if len(regressions) != 1 {
		t.Fatalf("plan_regression findings = %+v, want one", regressions)
	}
	if len(flips) != 1 {
		t.Fatalf("flip findings = %+v, want one", flips)
	}
	d := flips[0].Detail
	prev, cur := d["previous_plan_hash"], d["current_plan_hash"]
	if prev == nil || cur == nil || prev == cur {
		t.Fatalf("hashes %v -> %v, want two different fingerprints", prev, cur)
	}
	if d["relation"] != "public."+f.table || d["analyzed_by"] != analyzedByManual {
		t.Fatalf("detail = %v", d)
	}
	if d["analyzed_at"] != at.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("analyzed_at = %v, want %v", d["analyzed_at"], at.UTC())
	}
	if flips[0].RecommendedSQL != `ANALYZE "public"."`+f.table+`";` ||
		flips[0].ActionRisk != "safe" {
		t.Fatalf("sql %q risk %q", flips[0].RecommendedSQL, flips[0].ActionRisk)
	}
}

func TestPlanFlipDB_NoAnalyzeBetweenCapturesGivesNoFlip(t *testing.T) {
	f := newFlipFixture(t, 2)
	f.analyze(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO sage.explain_cache (captured_at, queryid,
		plan_json, source, total_cost) VALUES
		(clock_timestamp(), $1, $2::jsonb, 'collector', 100),
		(clock_timestamp() + interval '1 second', $1, $3::jsonb, 'collector', 400)`,
		f.qid, planJSONOn(t, f.table, "Index Scan"),
		planJSONOn(t, f.table, "Seq Scan")); err != nil {
		t.Fatalf("seed plans: %v", err)
	}
	regressions, flips := f.run(t)
	if len(regressions) != 1 || len(flips) != 0 {
		t.Fatalf("regressions %d flips %+v, want one regression and no flip",
			len(regressions), flips)
	}
	if !f.a.eval.ok[categoryPlanFlip] || f.a.eval.hasFailed(categoryPlanFlip) {
		t.Fatal("the flip category was not evaluated")
	}
}

func planJSONOn(t *testing.T, table, node string) string {
	t.Helper()
	raw, err := json.Marshal([]map[string]any{{"Plan": map[string]any{
		"Node Type": node, "Relation Name": table, "Plan Rows": 100}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// reanalyzed stores the flip as a finding the executor resolved with an
// ANALYZE action, as markFindingActioned leaves it.
func (f *flipFixture) reanalyzed(t *testing.T, flip Finding) int64 {
	t.Helper()
	ctx := context.Background()
	raw, _ := json.Marshal(flip.Detail)
	var id, action int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql)
		VALUES ($1, $2, 'query', $3, $4, $5, $6, $7) RETURNING id`, categoryPlanFlip,
		flip.Severity, flip.ObjectIdentifier, flip.Title, raw, flip.Recommendation,
		flip.RecommendedSQL).Scan(&id); err != nil {
		t.Fatalf("store flip: %v", err)
	}
	if _, err := f.pool.Exec(ctx, flip.RecommendedSQL); err != nil {
		t.Fatalf("re-analyze: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, finding_id,
		sql_executed, outcome) VALUES ('analyze', $1, $2, 'success') RETURNING id`, id,
		flip.RecommendedSQL).Scan(&action); err != nil {
		t.Fatalf("store action: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE sage.findings SET status = 'resolved',
		resolved_at = now(), acted_on_at = now(), action_log_id = $1 WHERE id = $2`,
		action, id); err != nil {
		t.Fatalf("resolve flip: %v", err)
	}
	return id
}

func (f *flipFixture) storedDetail(t *testing.T, id int64) map[string]any {
	t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT detail FROM sage.findings
		WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read finding: %v", err)
	}
	var d map[string]any
	_ = json.Unmarshal(raw, &d)
	return d
}

// open -> re-analyzed -> no change: kept open, handed on, and no second
// ANALYZE is proposed.
func TestPlanFlipDB_ReanalyzeWithNoChangeIsHandedOn(t *testing.T) {
	f := newFlipFixture(t, 3)
	f.flip(t)
	_, flips := f.run(t)
	if len(flips) != 1 {
		t.Fatalf("flips = %+v", flips)
	}
	id := f.reanalyzed(t, flips[0])
	f.capture(t) // the data did not change: the same (flipped) plan again
	_, flips = f.run(t)
	if len(flips) != 1 || flips[0].Detail[detailReanalyzeVerdict] != verdictNoChange ||
		flips[0].RecommendedSQL != "" || flips[0].Detail["handed_to"] != handedToPlanStability {
		t.Fatalf("after the re-analyze: %+v, want one handed-on finding without SQL", flips)
	}
	// The cycle's evidence is collected after the action resolved the row.
	var collected time.Time
	if err := f.pool.QueryRow(context.Background(), "SELECT clock_timestamp()").
		Scan(&collected); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertFindingsObserved(context.Background(), f.pool, flips,
		Evidence{CollectedAt: collected}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var status, sql string
	if err := f.pool.QueryRow(context.Background(), `SELECT status,
		COALESCE(recommended_sql, '') FROM sage.findings WHERE id = $1`, id).
		Scan(&status, &sql); err != nil {
		t.Fatal(err)
	}
	if status != "open" || sql != "" {
		t.Fatalf("finding %d = %s / %q, want reopened without a second ANALYZE", id, status, sql)
	}
}

// open -> re-analyzed -> improved: the verdict is recorded and the
// finding stays resolved.
func TestPlanFlipDB_ReanalyzeThatRestoresThePlanIsRecorded(t *testing.T) {
	f := newFlipFixture(t, 4)
	f.flip(t)
	_, flips := f.run(t)
	if len(flips) != 1 {
		t.Fatalf("flips = %+v", flips)
	}
	if _, err := f.pool.Exec(context.Background(), "UPDATE public."+f.table+
		" SET v = CASE WHEN id % 1000 = 0 THEN id END"); err != nil {
		t.Fatal(err)
	}
	id := f.reanalyzed(t, flips[0])
	f.capture(t)
	if _, flips = f.run(t); len(flips) != 0 {
		t.Fatalf("after a restoring re-analyze: %+v, want none", flips)
	}
	d := f.storedDetail(t, id)
	if d[detailReanalyzeVerdict] != verdictImproved || d["after_plan_hash"] !=
		d["previous_plan_hash"] {
		t.Fatalf("stored detail = %v, want improved back to the earlier plan", d)
	}
}

// Two cycles that see the same flip at once open one finding.
func TestPlanFlipDB_TwoCyclesOpenOneFinding(t *testing.T) {
	f := newFlipFixture(t, 5)
	f.flip(t)
	_, flips := f.run(t)
	if len(flips) != 1 {
		t.Fatalf("flips = %+v", flips)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = UpsertFindings(context.Background(), f.pool, flips)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	var open, count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*),
		COALESCE(max(occurrence_count), 0) FROM sage.findings
		WHERE category = $1 AND object_identifier = $2 AND status = 'open'`,
		categoryPlanFlip, f.ident()).Scan(&open, &count); err != nil {
		t.Fatal(err)
	}
	if open != 1 || count != 2 {
		t.Fatalf("open rows %d (occurrences %d), want one row seen twice", open, count)
	}
}

// Blocks per call come from the hourly rollup: the 24 hours before the
// analyze's hour, and from that hour on; an hour outside is not counted.
func TestPlanFlipDB_BlocksPerCallFromTheRollup(t *testing.T) {
	f := newFlipFixture(t, 6)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			"DELETE FROM sage.query_rollup_hourly WHERE queryid = $1", f.qid)
	})
	pivot := time.Now().UTC().Truncate(time.Hour).Add(-3*time.Hour + 20*time.Minute)
	base := pivot.Truncate(time.Hour)
	for _, r := range []struct {
		at         time.Duration
		calls, hit int64
	}{{-30 * time.Hour, 10, 999999}, {-2 * time.Hour, 10, 100}, {-time.Hour, 10, 140},
		{0, 5, 24000}, {time.Hour, 5, 24000}} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO sage.query_rollup_hourly (queryid, hour,
			calls, exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read,
			temp_blks_written, epoch_break, pairs)
			VALUES ($1, $2, $3, 1, 1, $4, 0, 0, 0, false, 2)`, f.qid, base.Add(r.at), r.calls,
			r.hit); err != nil {
			t.Fatalf("seed rollup: %v", err)
		}
	}
	b, err := pgPlanFlipDB{pool: f.pool}.blocksPerCall(ctx, f.qid, pivot)
	if err != nil {
		t.Fatalf("blocks per call: %v", err)
	}
	if b.Before == nil || *b.Before != 12 || b.After == nil || *b.After != 4800 {
		t.Fatalf("blocks per call = %v / %v, want 12 before and 4800 after", b.Before, b.After)
	}
	none, err := pgPlanFlipDB{pool: f.pool}.blocksPerCall(ctx, f.qid+500, pivot)
	if err != nil || none.Before != nil || none.After != nil {
		t.Fatalf("a query without rollup rows = %+v, %v; want unknown", none, err)
	}
}
