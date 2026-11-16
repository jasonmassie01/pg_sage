package rollout

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-03b on real SQL: three fixture databases on one server, each with
// its own sage schema. The treated one holds the action's v2 outcome; the
// untreated ones run the same statement under other queryids (texts that
// differ only in case, spacing and a semicolon), which the fleet control
// matches by normalized text.

// poolMember executes through a fake and reads effects from its database.
type poolMember struct {
	*fakeTarget
	PoolEffects
}

// effectWindow is the verdict's window: yesterday's 24 hours, on the
// server clock.
func effectWindow(t *testing.T, pool *pgxpool.Pool) (from, to time.Time) {
	t.Helper()
	to = testdb.MustDBNow(t, pool).UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	return to.Add(-24 * time.Hour), to
}

// seedQuery records queryid as text in a queries snapshot at `at`, with
// blocks per call before and after `from` over 24 hourly rows each.
func seedQuery(t *testing.T, pool *pgxpool.Pool, queryid int64, text string, from,
	at time.Time, beforeBlocks, afterBlocks int64) {
	t.Helper()
	ctx := context.Background()
	doc, err := json.Marshal([]map[string]any{{"queryid": queryid, "query": text,
		"calls": 4800, "total_exec_time": 100.0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES ($1, 'queries', $2::jsonb)`, at, doc); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	for h := -24; h < 24; h++ {
		blocks := beforeBlocks
		if h >= 0 {
			blocks = afterBlocks
		}
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_hourly (queryid, hour,
			calls, exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read,
			temp_blks_written, epoch_break, pairs)
			VALUES ($1, $2, 100, 50, 100, $3, 0, 0, 0, false, 2)`, queryid,
			from.Add(time.Duration(h)*time.Hour), blocks*100); err != nil {
			t.Fatalf("seed rollup: %v", err)
		}
	}
}

// seedOutcome records action 101 with evidence and its target queryids.
func seedOutcome(t *testing.T, pool *pgxpool.Pool, from, to time.Time, evidence,
	otherEvidence string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (id, executed_at, action_type,
		sql_executed, outcome, before_state) VALUES (101, $1, 'create_index', $2, 'success',
		'{"target_queryids": [11]}')`, from, canarySQL); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_outcome (action_log_id,
		action_class, predicted, prediction_method, verdict, tolerance, evidence,
		window_start, window_end, decided_at, verifier, other_evidence)
		VALUES (101, 'index_create', '{"class": "index_create", "method": "none",
		"target_queryids": [11]}', 'none', 'improved', 'no_prediction', $1::jsonb, $2, $3,
		$3, 'v2', $4::jsonb)`, evidence, from, to, otherEvidence); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
}

const v2Evidence = `{"v2": {"verifier": "v2", "measured": true, "effect_pct": -28,
	"ci_low_pct": -35, "ci_high_pct": -20, "basis": "pre_window"}}`

// Integration: the stored rollout_instance rows carry effect_pct and
// effect_method, measured from the three databases' own history.
func TestEffectPoolReadsAcrossThreeFixtureDatabases(t *testing.T) {
	d1, _ := freshRunPool(t, "effect member one")
	d2, _ := freshRunPool(t, "effect member two")
	d3, _ := freshRunPool(t, "effect member three")
	from, to := effectWindow(t, d1)
	seedOutcome(t, d1, from, to, v2Evidence, "null")
	seedQuery(t, d1, 11, "SELECT * FROM orders  WHERE status = $1;", from,
		to.Add(-time.Hour), 10, 7)
	seedQuery(t, d2, 22, "select *\n from orders where status = $1", from,
		to.Add(-time.Hour), 10, 10)
	seedQuery(t, d3, 33, "SELECT * FROM orders WHERE status = $1", from,
		to.Add(-time.Hour), 20, 20)
	seedQuery(t, d3, 34, "SELECT * FROM items WHERE id = $1", from, to.Add(-time.Hour),
		1, 90)
	// d3's newest snapshot holds both statements (two snapshots at one
	// instant would leave which one is newest to the sort).
	if _, err := d3.Exec(context.Background(), `INSERT INTO sage.snapshots (collected_at,
		category, data) VALUES ($1, 'queries', '[{"queryid": 33, "query":
		"SELECT * FROM orders WHERE status = $1"}, {"queryid": 34, "query":
		"SELECT * FROM items WHERE id = $1"}]'::jsonb)`, to.Add(-30*time.Minute)); err != nil {
		t.Fatalf("seed d3 snapshot: %v", err)
	}
	queueID := seedQueueRow(t, d1)

	eff, err := NewPoolEffects(d1).ActionEffect(context.Background(), 101)
	if err != nil || !eff.Measured || eff.EffectPct != -28 || eff.CILowPct != -35 ||
		eff.CIHighPct != -20 || len(eff.Queries) != 1 || eff.Queries[0] != effectQuery ||
		!eff.From.Equal(from) || !eff.To.Equal(to) {
		t.Fatalf("action effect = %+v (%v)", eff, err)
	}
	change, ran, err := NewPoolEffects(d3).QueryChange(context.Background(), eff.Queries,
		from, to)
	if err != nil || !ran || change != 0 {
		t.Fatalf("d3 change = %v %v (%v), want 0 (items is not the same query)", change,
			ran, err)
	}

	control := bootstrappedPool(t)
	members := map[string]CanaryTarget{}
	for name, pool := range map[string]*pgxpool.Pool{"d1": d1, "d2": d2, "d3": d3} {
		ft := newTarget(name, canarySQL)
		ft.settleAfter = 0
		members[name] = poolMember{fakeTarget: ft, PoolEffects: NewPoolEffects(pool)}
	}
	run := runPoolCanary(t, control, members)
	r := instanceByName(t, control, run, "d1")
	if r.EffectMethod != EffectFleetControl || !near(r.EffectPct, -30) ||
		!near(r.ControlPct, 0) || r.RegressionPct != -30 {
		t.Fatalf("d1 = %+v (effect %s control %s)", r, fmtPtr(r.EffectPct),
			fmtPtr(r.ControlPct))
	}
	// d2 and d3 have no outcome row for their action: flag only.
	if r := instanceByName(t, control, run, "d2"); r.EffectMethod != EffectFlagOnly {
		t.Fatalf("d2 = %+v", r)
	}
	assertFleetLadder(t, d1, queueID)
}

func runPoolCanary(t *testing.T, control *pgxpool.Pool,
	members map[string]CanaryTarget) string {
	t.Helper()
	src := newTarget("source", "")
	src.source[42] = SourceAction{SQL: canarySQL, RollbackSQL: "DROP INDEX x",
		ActionType: "create_index_concurrently", Outcome: "success", Verification: "success"}
	members["source"] = src
	svc, err := NewCanaryService(func(name string) (CanaryTarget, error) {
		return members[name], nil
	}, NewPostgresRunStore(control), CanaryOptions{CanaryInstances: 1,
		RegressionLimitPct: 10, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	run, err := svc.Run(context.Background(), canaryRequest("d1", "d2", "d3"))
	if err != nil || run.State != "complete" {
		t.Fatalf("run = %+v (%v)", run, err)
	}
	return run.EvidenceID
}

func instanceByName(t *testing.T, pool *pgxpool.Pool, runID, name string) InstanceRecord {
	t.Helper()
	list, err := NewPostgresRunStore(pool).Instances(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list {
		if r.InstanceID == name {
			return r
		}
	}
	t.Fatalf("no instance %s in %+v", name, list)
	return InstanceRecord{}
}

// seedQueueRow is a queue item that ran action 101.
func seedQueueRow(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	ctx := context.Background()
	var findingID, queueID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status)
		VALUES ('missing_index', 'info', 'table', 'public.orders', 'fleet', '{}', 'open')
		RETURNING id`).Scan(&findingID); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_queue (finding_id, proposed_sql,
		rollback_sql, action_risk, status, action_log_id)
		VALUES ($1, $2, '', 'moderate', 'approved', 101) RETURNING id`, findingID,
		canarySQL).Scan(&queueID); err != nil {
		t.Fatalf("seed queue row: %v", err)
	}
	return queueID
}

func assertFleetLadder(t *testing.T, pool *pgxpool.Pool, queueID int) {
	t.Helper()
	raw, err := verify.ReadLadder(context.Background(), pool, queueID)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := verify.ParseLadder(raw)
	if err != nil || len(recs) != 1 || recs[0].Tier != verify.TierFleet ||
		recs[0].Result != verify.LadderPassed || !near(recs[0].Predicted, -30) {
		t.Fatalf("ladder = %s (%v), want one passed tier-4 record of -30", raw, err)
	}
}

// Invalid input on real rows: a v2 record that is not an object is
// malformed; a shadow v2 record in other_evidence is read; no row is no
// effect, not an error.
func TestEffectPoolDecodesStoredV2Records(t *testing.T) {
	for _, c := range []struct {
		name, evidence, other string
		measured, malformed   bool
		reason                string
	}{
		{"string", `{"v2": "oops"}`, "null", false, true, "not a JSON object"},
		{"shadow", `{}`, v2Evidence, true, false, ""},
		{"unmeasured", `{"v2": {"measured": false, "attribution_reason": "too few calls"}}`,
			"null", false, false, "too few calls"},
		{"no number", `{"v2": {"measured": true, "effect_pct": null}}`, "null", false,
			true, "effect_pct"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, _ := freshRunPool(t, "effect decode "+c.name)
			from, to := effectWindow(t, pool)
			seedOutcome(t, pool, from, to, c.evidence, c.other)
			eff, err := NewPoolEffects(pool).ActionEffect(context.Background(), 101)
			if err != nil || eff.Measured != c.measured || eff.Malformed != c.malformed ||
				!strings.Contains(eff.Reason, c.reason) {
				t.Fatalf("effect = %+v (%v)", eff, err)
			}
		})
	}
	pool, _ := freshRunPool(t, "effect decode none")
	eff, err := NewPoolEffects(pool).ActionEffect(context.Background(), 999)
	if err != nil || eff.Measured || eff.Malformed || !strings.Contains(eff.Reason,
		"no recorded outcome") {
		t.Fatalf("missing outcome = %+v (%v)", eff, err)
	}
}

// Nil/empty and error propagation on real SQL: no queries ran none; a
// closed pool is an error naming what was read.
func TestEffectPoolQueryChangeEdges(t *testing.T) {
	pool, dsn := freshRunPool(t, "effect change edges")
	from, to := effectWindow(t, pool)
	change, ran, err := NewPoolEffects(pool).QueryChange(context.Background(), nil, from, to)
	if err != nil || ran || change != 0 {
		t.Fatalf("no queries = %v %v (%v)", change, ran, err)
	}
	change, ran, err = NewPoolEffects(pool).QueryChange(context.Background(),
		[]string{effectQuery}, from, to)
	if err != nil || ran {
		t.Fatalf("no history = %v %v (%v), want not ran", change, ran, err)
	}
	if _, _, err := NewPoolEffects(pool).QueryChange(context.Background(),
		[]string{effectQuery}, to, from); err == nil {
		t.Fatal("an empty window was accepted")
	}
	closed := openPool(t, dsn)
	closed.Close()
	_, _, err = NewPoolEffects(closed).QueryChange(context.Background(),
		[]string{effectQuery}, from, to)
	if err == nil || !strings.Contains(err.Error(), "query history") {
		t.Fatalf("closed pool = %v, want an error naming the query history", err)
	}
	if _, err := NewPoolEffects(closed).ActionEffect(context.Background(), 101); err == nil ||
		!strings.Contains(err.Error(), "action 101") {
		t.Fatalf("closed pool effect = %v", err)
	}
}
