package changeplan

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/verify"
)

// plan-02c against the real executor: real indexes are created by the
// plan and reverted by RollbackAction (DROP INDEX CONCURRENTLY under the
// DDL slot and the lock timeout), and the catalog after the revert matches
// the pre-plan state for every reverted step. Verdicts are recorded
// through verify.OutcomeStore as the verifier records them.

// indexPlan creates n tables <prefix>_t1..tn (2,000 rows each, no index),
// an open finding per step and an approved plan of n index creates
// (idx_<prefix>_<i> on <prefix>_t<i>), each depending on the last; mutate
// edits the plan before it is created. One table per step keeps the plan
// under the default cap of 2 new indexes per table.
func (r *realPlan) indexPlan(t *testing.T, prefix string, n int, mutate func(*Plan)) Plan {
	t.Helper()
	p := threeStepPlan()
	p.Steps, p.Edges, p.Scope = nil, nil, Scope{QueryIDs: []int64{101}}
	res := newFakeResolver()
	for i := 1; i <= n; i++ {
		table := fmt.Sprintf("public.%s_t%d", prefix, i)
		index := fmt.Sprintf("idx_%s_%d", prefix, i)
		if _, err := r.pool.Exec(r.ctx, `CREATE TABLE `+table+` AS SELECT g AS id,
			g % 97 AS c FROM generate_series(1, 2000) g`); err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
		s := indexStep(i, index, "c", 0, 101)
		s.ProposedSQL = "CREATE INDEX CONCURRENTLY " + index + " ON " + table + " (c)"
		s.RollbackSQL = "DROP INDEX CONCURRENTLY public." + index
		s.DeadlineMinutes = 60
		var id int64
		if err := r.pool.QueryRow(r.ctx, `INSERT INTO sage.findings (category, severity,
			object_type, object_identifier, title, detail, recommendation, recommended_sql)
			VALUES ('plan_real_' || md5($1), 'warning', 'index', $2, 'f', '{}', 'rec', $1)
			RETURNING id`, s.ProposedSQL, table).Scan(&id); err != nil {
			t.Fatalf("finding: %v", err)
		}
		s.FindingID, res.findings[id] = ptr(id), true
		p.Steps = append(p.Steps, s)
		p.Scope.Objects = append(p.Scope.Objects, table)
		if i > 1 {
			p.Edges = append(p.Edges, Edge{From: i - 1, To: i})
		}
	}
	if mutate != nil {
		mutate(&p)
	}
	created, err := r.store.Create(r.ctx, p, res)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.store.Approve(r.ctx, created.ID, created.PlanHash, 7); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return created
}

// runVerdicts ticks plan id until it is halted or terminal, or until stop
// says so, recording each judging step's verdict (improved unless named)
// through the outcome store once.
func (r *realPlan) runVerdicts(t *testing.T, id int64, verdicts map[int]string, maxTicks int,
	stop func(Plan) bool) Plan {
	t.Helper()
	judged := map[int64]bool{}
	for range maxTicks {
		p := mustGet(t, r.store, id)
		if p.Status.Terminal() || p.Status == StatusHalted || (stop != nil && stop(p)) {
			return p
		}
		for _, s := range p.Steps {
			if s.Runtime.State != StateJudging || judged[*s.Runtime.ActionLogID] {
				continue
			}
			v := verdicts[s.Ordinal]
			if v == "" {
				v = "improved"
			}
			o := verify.Outcome{ActionLogID: *s.Runtime.ActionLogID, Verdict: v,
				Reason: "test verdict"}
			if err := verify.NewOutcomeStore(r.pool).RecordVerdict(r.ctx, o); err != nil {
				t.Fatalf("record the verdict of step %d: %v", s.Ordinal, err)
			}
			judged[*s.Runtime.ActionLogID] = true
		}
		r.tick(t)
	}
	return mustGet(t, r.store, id)
}

// planIndexes are the indexes on the plan's tables (<prefix>_t*), by name.
func (r *realPlan) planIndexes(t *testing.T, prefix string) []string {
	t.Helper()
	rows, err := r.pool.Query(r.ctx, `SELECT indexname FROM pg_indexes
		WHERE schemaname = 'public' AND tablename LIKE $1 ORDER BY indexname`,
		prefix+`\_t%`)
	if err != nil {
		t.Fatalf("read the indexes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("read the indexes: %v", err)
		}
		out = append(out, n)
	}
	return out
}

func (r *realPlan) actionOutcome(t *testing.T, id int64) string {
	t.Helper()
	var o string
	if err := r.pool.QueryRow(r.ctx, `SELECT COALESCE(outcome, '') FROM sage.action_log
		WHERE id = $1`, id).Scan(&o); err != nil {
		t.Fatalf("read action %d: %v", id, err)
	}
	return o
}

// CHECK-16: a regression on step 3 of 5 reverts step 3, keeps 1 and 2, and
// never starts 4; the catalog shows the pre-step state for step 3.
func TestRevertReal_RegressionOnStep3Of5(t *testing.T) {
	r := newRealPlanOn(t, "rollback_real_step")
	p := r.indexPlan(t, "rs", 5, nil)
	if got := r.planIndexes(t, "rs"); len(got) != 0 {
		t.Fatalf("pre-plan indexes %v, want none", got)
	}
	got := r.runVerdicts(t, p.ID, map[int]string{3: "regressed"}, 40, nil)
	want := []StepState{StateKept, StateKept, StateReverted, StatePending, StatePending}
	if got.Status != StatusHalted || !slices.Equal(states(got), want) {
		t.Fatalf("plan %s steps %v, want halted with %v", got.Status, states(got), want)
	}
	if idx := r.planIndexes(t, "rs"); !slices.Equal(idx, []string{"idx_rs_1", "idx_rs_2"}) {
		t.Fatalf("CHECK-16: indexes %v, want only idx_rs_1 and idx_rs_2", idx)
	}
	for _, s := range got.Steps[3:] {
		if s.Runtime.QueueID != nil || s.Runtime.ActionLogID != nil {
			t.Fatalf("CHECK-16: step %d has a queue item or an action: %+v", s.Ordinal,
				s.Runtime)
		}
	}
	if o := r.actionOutcome(t, *got.Steps[2].Runtime.ActionLogID); o != "rolled_back" {
		t.Fatalf("step 3's action outcome %q, want rolled_back", o)
	}
	t.Log("CHECK-16: step 3 reverted, 1 and 2 kept, 4 and 5 never started")
}

// revert_to_checkpoint on real indexes: checkpoint on 1, step 3 regresses:
// 3 then 2 are dropped, 1 stays valid, and 4 never starts.
func TestRevertReal_WalkBackToTheCheckpoint(t *testing.T) {
	r := newRealPlanOn(t, "rollback_real_walk")
	p := r.indexPlan(t, "rw", 4, func(p *Plan) {
		p.Steps[0].Checkpoint = true
		p.Steps[2].OnRegressed = ActionRevertToCheckpoint
	})
	got := r.runVerdicts(t, p.ID, map[int]string{3: "regressed"}, 40, nil)
	want := []StepState{StateKept, StateReverted, StateReverted, StatePending}
	if got.Status != StatusHalted || !slices.Equal(states(got), want) {
		t.Fatalf("plan %s steps %v, want halted with %v", got.Status, states(got), want)
	}
	if idx := r.planIndexes(t, "rw"); !slices.Equal(idx, []string{"idx_rw_1"}) {
		t.Fatalf("indexes %v, want only the checkpoint's idx_rw_1", idx)
	}
	s2, s3 := got.Steps[1].Runtime, got.Steps[2].Runtime
	if r.actionOutcome(t, *s2.ActionLogID) != "rolled_back" ||
		r.actionOutcome(t, *s3.ActionLogID) != "rolled_back" {
		t.Fatalf("steps 2 and 3's actions are not both rolled back: %+v %+v", s2, s3)
	}
}

// A setting step's revert is read back against the prior value the
// executor recorded before it ran (before_state), through a reload.
func TestRevertReal_SettingIsReadBackAtItsPrior(t *testing.T) {
	r := newRealPlanOn(t, "rollback_real_setting")
	t.Cleanup(func() {
		_, _ = r.pool.Exec(context.Background(), `ALTER SYSTEM RESET random_page_cost`)
		_, _ = r.pool.Exec(context.Background(), `SELECT pg_reload_conf()`)
	})
	var prior string
	if err := r.pool.QueryRow(r.ctx, `SELECT setting FROM pg_settings
		WHERE name = 'random_page_cost'`).Scan(&prior); err != nil {
		t.Fatalf("read the prior: %v", err)
	}
	s := settingStep(1)
	s.OnRegressed, s.OnUnverifiable = ActionRevertStep, ActionKeep
	var id int64
	if err := r.pool.QueryRow(r.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql)
		VALUES ('plan_real_setting', 'warning', 'setting', 'random_page_cost', 'f', '{}',
		        'rec', $1) RETURNING id`, s.ProposedSQL).Scan(&id); err != nil {
		t.Fatalf("finding: %v", err)
	}
	s.FindingID = ptr(id)
	p := threeStepPlan()
	p.Steps, p.Edges, p.Scope = []Step{s}, nil, Scope{}
	res := newFakeResolver()
	res.findings[id] = true
	created, err := r.store.Create(r.ctx, p, res)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.store.Approve(r.ctx, created.ID, created.PlanHash, 7); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got := r.runVerdicts(t, created.ID, map[int]string{1: "regressed"}, 20, nil)
	st := got.Steps[0].Runtime
	if st.State != StateReverted || !strings.Contains(st.StateReason, "is back at") {
		t.Fatalf("plan %s step %+v, want reverted with the setting read back", got.Status, st)
	}
	var now string
	if err := r.pool.QueryRow(r.ctx, `SELECT setting FROM pg_settings
		WHERE name = 'random_page_cost'`).Scan(&now); err != nil || now != prior {
		t.Fatalf("random_page_cost %q (err %v), want its prior %q", now, err, prior)
	}
}

// countingExec counts the rollbacks that ran (returned nil) per action,
// and who ran the last of them (the rollback's reason).
type countingExec struct {
	*executor.Executor
	mu  sync.Mutex
	ok  map[int64]int
	who map[int64]string
}

func (c *countingExec) RollbackAction(ctx context.Context, id int64, reason string) error {
	err := c.Executor.RollbackAction(ctx, id, reason)
	if err == nil {
		c.mu.Lock()
		c.ok[id]++
		c.who[id] = "plan"
		if strings.HasPrefix(reason, "operator") {
			c.who[id] = "operator"
		}
		c.mu.Unlock()
	}
	return err
}

func (c *countingExec) count(id int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ok[id]
}

// Concurrency: an operator's manual rollback of step 1's action (the
// executor's claimRollback) races the walk back from step 2: exactly one
// rollback runs and the step ends reverted, 50 times.
func TestRevertReal_OperatorRollbackRacesTheWalk(t *testing.T) {
	r := newRealPlanOn(t, "rollback_real_race")
	ce := &countingExec{Executor: r.exec, ok: map[int64]int{},
		who: map[int64]string{}}
	wins := map[string]int{}
	r.engine.cfg.Executor = ce
	rng := rand.New(rand.NewSource(11))
	for run := range 50 {
		// The walk reaches step 1 after step 2's DROP INDEX CONCURRENTLY, so
		// the delays span both orders.
		delay := time.Duration(rng.Intn(300)) * time.Millisecond
		wins[r.raceOnce(t, ce, run, delay)]++
	}
	t.Logf("step 1's one rollback was run by: %v", wins)
}

// raceOnce runs a two-step plan to step 2's regression and races the
// operator's rollback of step 1 (after delay) against the walk; it
// returns who ran step 1's one rollback.
func (r *realPlan) raceOnce(t *testing.T, ce *countingExec, run int,
	delay time.Duration) string {
	t.Helper()
	prefix := fmt.Sprintf("rr%d", run)
	p := r.indexPlan(t, prefix, 2, func(p *Plan) {
		p.Steps[1].OnRegressed = ActionRevertToCheckpoint
	})
	mid := r.runVerdicts(t, p.ID, nil, 20, func(p Plan) bool {
		return p.Steps[1].Runtime.State == StateJudging
	})
	if mid.Steps[0].Runtime.State != StateKept || mid.Steps[1].Runtime.State != StateJudging {
		t.Fatalf("run %d: steps %v, want kept and judging", run, states(mid))
	}
	a1 := *mid.Steps[0].Runtime.ActionLogID
	if err := verify.NewOutcomeStore(r.pool).RecordVerdict(r.ctx, verify.Outcome{
		ActionLogID: *mid.Steps[1].Runtime.ActionLogID, Verdict: "regressed",
		Reason: "test verdict"}); err != nil {
		t.Fatalf("run %d: record the regression: %v", run, err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(delay)
		_ = ce.RollbackAction(r.ctx, a1, "operator: manual rollback")
	}()
	r.tick(t)
	wg.Wait()
	got := r.runVerdicts(t, p.ID, nil, 6, nil)
	if n := ce.count(a1); n != 1 {
		t.Fatalf("run %d (delay %v): %d rollbacks of step 1's action ran, want 1", run,
			delay, n)
	}
	if s := got.Steps[0].Runtime; s.State != StateReverted || got.Status != StatusHalted {
		t.Fatalf("run %d (delay %v): plan %s step 1 %+v, want reverted and halted", run,
			delay, got.Status, s)
	}
	if idx := r.planIndexes(t, prefix); len(idx) != 0 ||
		r.actionOutcome(t, a1) != "rolled_back" {
		t.Fatalf("run %d: indexes %v, outcome %q: want none and rolled_back", run, idx,
			r.actionOutcome(t, a1))
	}
	ce.mu.Lock()
	defer ce.mu.Unlock()
	return ce.who[a1]
}
