package upgradecheck

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/verify"
)

// drill-02c state transitions against the real executor: the proposed
// plan is approved, the engine runs step 1 (ANALYZE) through the executor,
// waits for its verdict, then runs step 2 (the reindex). The verdicts are
// recorded through the outcome store as the verifier records them.

type upgradeEngine struct {
	d      liveDB
	engine *changeplan.Engine
	store  *changeplan.Store
}

func newUpgradeEngine(t *testing.T, d liveDB) *upgradeEngine {
	t.Helper()
	if err := schema.MigrateConfigSchema(d.ctx, d.pool); err != nil {
		t.Fatalf("config schema: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Safety.DDLTimeoutSeconds = 60
	cfg.Verify.WindowMinutes = 5
	ex := executor.New(d.pool, cfg, time.Time{}, func(string, string, ...any) {})
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	ex.EnableStandingPolicyDocument(doc, nil)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = ex.Shutdown(c)
	})
	dbNow := func() time.Time {
		var now time.Time
		if err := d.pool.QueryRow(d.ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			t.Errorf("read the database clock: %v", err)
		}
		return now
	}
	engine, err := changeplan.NewEngine(changeplan.EngineConfig{Pool: d.pool, Executor: ex,
		Waits: ex.VerificationWaits(), Verdicts: changeplan.NewDBVerdicts(d.pool),
		Now: dbNow, Owner: "sidecar-1", Database: d.name})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := ex.AddApprovedActionRunner("plans",
		[]string{executor.PlanStepIdentityPrefix}, engine.Runner(ex)); err != nil {
		t.Fatalf("register the plan runner: %v", err)
	}
	return &upgradeEngine{d: d, engine: engine, store: changeplan.NewStore(d.pool)}
}

func (u *upgradeEngine) ticks(t *testing.T, n int) {
	t.Helper()
	for range n {
		if err := u.engine.Tick(u.d.ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
}

func (u *upgradeEngine) plan(t *testing.T, id int64) changeplan.Plan {
	t.Helper()
	p, err := u.store.Get(u.d.ctx, id)
	if err != nil {
		t.Fatalf("read plan %d: %v", id, err)
	}
	return p
}

// judge records verdict for the plan's judging step, as the verifier does.
func (u *upgradeEngine) judge(t *testing.T, id int64, verdict string) {
	t.Helper()
	for _, s := range u.plan(t, id).Steps {
		if s.Runtime.State == changeplan.StateJudging {
			o := verify.Outcome{ActionLogID: *s.Runtime.ActionLogID, Verdict: verdict,
				Reason: "test verdict"}
			if err := verify.NewOutcomeStore(u.d.pool).RecordVerdict(u.d.ctx, o); err != nil {
				t.Fatalf("record the verdict: %v", err)
			}
			return
		}
	}
	t.Fatalf("plan %d has no judging step: %+v", id, u.plan(t, id).Steps)
}

func analyzed(t *testing.T, d liveDB, table string) bool {
	t.Helper()
	var at *time.Time
	if err := d.pool.QueryRow(d.ctx, `SELECT last_analyze FROM pg_stat_user_tables
		WHERE relid = to_regclass($1)`, table).Scan(&at); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	return at != nil
}

func filenode(t *testing.T, d liveDB, index string) int64 {
	t.Helper()
	var n int64
	if err := d.pool.QueryRow(d.ctx, `SELECT pg_relation_filenode(to_regclass($1))::bigint`,
		index).Scan(&n); err != nil {
		t.Fatalf("read %s: %v", index, err)
	}
	return n
}

func TestPostUpgradeDB_ProposedApprovedStepOneVerifiedThenStepTwo(t *testing.T) {
	d := postUpgradeFixture(t)
	u := newUpgradeEngine(t, d)
	ev := fakeEvents{events: []changefeed.Event{versionEventOf(upgradeEventID)}}
	res, err := newProducer(t, d, ev, nil, nil).Tick(d.ctx)
	if err != nil || len(res.Planned) != 1 {
		t.Fatalf("Tick = %+v, %v", res, err)
	}
	id := res.Planned[0]
	p := u.plan(t, id)
	u.ticks(t, 2) // a proposed plan does not run
	if analyzed(t, d, "public.notes") || u.plan(t, id).Status != changeplan.StatusProposed {
		t.Fatal("a proposed plan ran before its approval")
	}
	if _, err := u.store.Approve(d.ctx, id, p.PlanHash, 7); err != nil {
		t.Fatalf("approve: %v", err)
	}
	before := filenode(t, d, "public.notes_a")
	u.ticks(t, 1) // step 1 runs through the executor, which verifies an ANALYZE at once
	if !analyzed(t, d, "public.notes") || !analyzed(t, d, "public.orders") {
		t.Fatalf("step 1 did not ANALYZE both tables: %+v", u.plan(t, id).Steps)
	}
	step1 := u.plan(t, id).Steps[0]
	o, err := verify.NewOutcomeStore(d.pool).Get(d.ctx, *step1.Runtime.ActionLogID)
	if err != nil {
		t.Fatalf("step 1's verdict: %v", err)
	}
	t.Logf("step 1 verdict from the executor: %s (%s)", o.Verdict, o.Reason)
	u.ticks(t, 3) // step 1 judged and kept; step 2 starts only then
	if filenode(t, d, "public.notes_a") == before {
		t.Fatalf("step 2 did not rebuild notes_a after step 1 was kept: %+v",
			u.plan(t, id).Steps)
	}
	u.assertStepTwoAfterStepOne(t, id)
	for range 3 { // the reindex is monitored: its verdict is the verifier's
		if u.plan(t, id).Steps[1].Runtime.State == changeplan.StateJudging {
			break
		}
		u.ticks(t, 1)
	}
	u.judge(t, id, "improved")
	u.ticks(t, 3)
	got := u.plan(t, id)
	if got.Status != changeplan.StatusCompleted ||
		got.Steps[0].Runtime.State != changeplan.StateKept ||
		got.Steps[1].Runtime.State != changeplan.StateKept {
		t.Fatalf("plan %s steps %+v, want completed with both kept", got.Status, got.Steps)
	}
}

// After pg_upgrade the rows-modified counters start at zero, so the
// executor's ANALYZE verdict is insufficient evidence (inconclusive): the
// ANALYZE step is kept and the plan moves on to the reindex.
func TestPostUpgradeDB_AnalyzeWithResetCountersIsKeptAndThePlanMovesOn(t *testing.T) {
	d := postUpgradeFixture(t)
	waitModified(t, d, "public.notes", 500)          // the inserts reached the statistics
	d.exec(t, "ANALYZE public.notes, public.orders") // the counters as after pg_upgrade
	waitModifiedAtMost(t, d, "public.notes", 0)      // PG14's collector reports it late
	u := newUpgradeEngine(t, d)
	ev := fakeEvents{events: []changefeed.Event{versionEventOf(upgradeEventID)}}
	res, err := newProducer(t, d, ev, nil, nil).Tick(d.ctx)
	if err != nil || len(res.Planned) != 1 {
		t.Fatalf("Tick = %+v, %v", res, err)
	}
	id := res.Planned[0]
	if _, err := u.store.Approve(d.ctx, id, u.plan(t, id).PlanHash, 7); err != nil {
		t.Fatalf("approve: %v", err)
	}
	before := filenode(t, d, "public.notes_a")
	u.ticks(t, 4)
	got := u.plan(t, id)
	s1 := got.Steps[0].Runtime
	if s1.State != changeplan.StateKept || s1.Verdict != changeplan.OutcomeInconclusive {
		t.Fatalf("step 1 %+v, want kept on an inconclusive verdict", s1)
	}
	if got.Status == changeplan.StatusHalted || filenode(t, d, "public.notes_a") == before {
		t.Fatalf("plan %s, notes_a rebuilt %v: want the plan on to the reindex", got.Status,
			filenode(t, d, "public.notes_a") != before)
	}
}

// waitModified waits (up to 30 s) until table's rows modified since its
// last ANALYZE reach want: a backend reports its counts when it idles.
func waitModified(t *testing.T, d liveDB, table string, want int64) {
	t.Helper()
	waitMods(t, d, table, want, func(n int64) bool { return n >= want })
}

// waitModifiedAtMost waits until the count is at most want: before
// PostgreSQL 15 the statistics collector reports an ANALYZE's reset late.
func waitModifiedAtMost(t *testing.T, d liveDB, table string, want int64) {
	t.Helper()
	waitMods(t, d, table, want, func(n int64) bool { return n <= want })
}

func waitMods(t *testing.T, d liveDB, table string, want int64, done func(int64) bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int64
		if err := d.pool.QueryRow(d.ctx, `SELECT n_mod_since_analyze FROM pg_stat_user_tables
			WHERE relid = to_regclass($1)`, table).Scan(&n); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		if done(n) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s has %d rows modified, want %d", table, n, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// assertStepTwoAfterStepOne: step 1 is kept with its verdict, and step 2
// ran no earlier than that verdict.
func (u *upgradeEngine) assertStepTwoAfterStepOne(t *testing.T, id int64) {
	t.Helper()
	p := u.plan(t, id)
	s1, s2 := p.Steps[0].Runtime, p.Steps[1].Runtime
	if s1.State != changeplan.StateKept || s1.VerdictAt == nil || s2.ExecutedAt == nil ||
		s2.ExecutedAt.Before(*s1.VerdictAt) {
		t.Fatalf("step 1 %+v, step 2 %+v: want step 2 run after step 1's verdict", s1, s2)
	}
}
