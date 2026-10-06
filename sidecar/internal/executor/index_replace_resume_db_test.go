package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// A crash between (or inside) the two steps resumes or rolls back safely on
// the next cycle; the undo re-creates the old index and drops the new one;
// the verification monitor rolls back a regression before its verdict and
// re-creates only the old index (the soft drop) after it.

var errSimulatedCrash = errors.New("simulated crash")

// restarted is a new executor on the same database: nothing in memory
// survives, only sage.index_replace and the catalog.
func (f *replaceFixture) restarted() *Executor {
	return withTestStandingGate(manualExecutor(f.pool))
}

func (f *replaceFixture) setState(state string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.index_replace SET state = $2
		WHERE table_name = $1`, f.table, state); err != nil {
		f.t.Fatalf("set state %s: %v", state, err)
	}
}

func TestIndexReplaceCrashBetweenStepsResumesTheDrop(t *testing.T) {
	f := newReplaceFixture(t, "b")
	f.exec.replaceHooks = replaceHooks{afterCreate: func(context.Context) error {
		return errSimulatedCrash
	}}
	if _, err := f.run(); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("the crash stops the run: %v", err)
	}
	if r := f.row(); r.state != replaceCreated {
		t.Fatalf("after the crash the state is %q, want created", r.state)
	}
	if exists, _ := f.index(f.oldIndex); !exists {
		t.Fatal("nothing was dropped before the crash")
	}
	if err := f.restarted().ResumeIndexReplaces(f.ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	r := f.row()
	if r.state != replaceCompleted || r.actionID <= 0 {
		t.Fatalf("resumed state row = %+v", r)
	}
	f.assertSucceeded(r.actionID)
	if err := f.restarted().ResumeIndexReplaces(f.ctx); err != nil || f.row().state !=
		replaceCompleted {
		t.Fatalf("a second resume changes nothing: %v %q", err, f.row().state)
	}
}

func TestIndexReplaceCrashDuringTheCreateDropsNothing(t *testing.T) {
	f := newReplaceFixture(t, "(b / 0)")
	if _, err := f.run(); !errors.Is(err, ErrReplaceCreateFailed) {
		t.Fatalf("setup: %v", err)
	}
	// As if the process died while the failed build was registered: an
	// INVALID remnant and a row still "creating".
	_, _ = f.pool.Exec(f.ctx, "CREATE INDEX CONCURRENTLY "+f.bare+"_a_b ON "+f.table+
		" (a, (b / 0))")
	if exists, valid := f.index(f.newIndex); !exists || valid {
		t.Fatalf("setup: remnant exists=%v valid=%v", exists, valid)
	}
	f.setState(replaceCreating)
	if err := f.restarted().ResumeIndexReplaces(f.ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if exists, _ := f.index(f.newIndex); exists {
		t.Fatal("the INVALID remnant is dropped on resume")
	}
	if exists, valid := f.index(f.oldIndex); !exists || !valid {
		t.Fatal("the old index is untouched")
	}
	if r := f.row(); r.state != replaceCreateFailed {
		t.Fatalf("state = %q, want create_failed", r.state)
	}
}

func TestIndexReplaceResumeFinishesAnInterruptedDrop(t *testing.T) {
	f := newReplaceFixture(t, "b")
	f.exec.replaceHooks = replaceHooks{beforeDrop: func(context.Context) error {
		return errSimulatedCrash
	}}
	if _, err := f.run(); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("setup: %v", err)
	}
	f.setState(replaceDropping)
	// The DROP INDEX CONCURRENTLY had committed before the crash.
	f.must(t, "DROP INDEX "+f.oldIndex)
	if err := f.restarted().ResumeIndexReplaces(f.ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	r := f.row()
	if r.state != replaceCompleted {
		t.Fatalf("state = %q, want completed", r.state)
	}
	f.assertSucceeded(r.actionID)
}

func TestIndexReplaceRollbackRestoresTheOldIndex(t *testing.T) {
	f := newReplaceFixture(t, "b")
	id, err := f.run()
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := f.exec.RollbackAction(f.ctx, id, "operator undo"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	f.assertRolledBack(id)
	if err := f.exec.RollbackIndexReplace(f.ctx, id, "again"); err == nil {
		t.Fatal("a rolled back replace is not rolled back twice")
	}
}

func (f *replaceFixture) assertRolledBack(id int64) {
	f.t.Helper()
	if exists, valid := f.index(f.oldIndex); !exists || !valid {
		f.t.Fatalf("old index restored: exists=%v valid=%v", exists, valid)
	}
	if def, _ := f.indexDef(f.oldIndex); def != f.oldDef {
		f.t.Fatalf("restored definition %q, want %q", def, f.oldDef)
	}
	if exists, _ := f.index(f.newIndex); exists {
		f.t.Fatal("the new index is dropped by the undo")
	}
	if r := f.row(); r.state != replaceRolledBack {
		f.t.Fatalf("state = %q, want rolled_back", r.state)
	}
	if a := f.action(id); a.outcome != "rolled_back" {
		f.t.Fatalf("action outcome = %q", a.outcome)
	}
}

func TestIndexReplaceRollbackResumesAfterACrash(t *testing.T) {
	f := newReplaceFixture(t, "b")
	id, err := f.run()
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	f.setState(replaceRollbackRecreating)
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_log SET outcome = 'rolling_back'
		WHERE id = $1`, id); err != nil {
		t.Fatalf("mark rolling back: %v", err)
	}
	if err := f.restarted().ResumeIndexReplaces(f.ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	f.assertRolledBack(id)
}

// seedVerification gives the action one targeted and one guarded query
// with a frozen baseline of 10 ms.
func (f *replaceFixture) seedVerification(id int64) {
	f.t.Helper()
	base := verify.Measurement{Samples: 1000, AverageLatency: 10 * time.Millisecond,
		StdErr: 100 * time.Microsecond, Buckets: 12}
	patch, _ := json.Marshal(map[string]any{"target_queryids": []int64{1},
		"guarded_queryids": []int64{2},
		"verify_baseline":  map[string]verify.Measurement{"1": base, "2": base}})
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_log
		SET before_state = before_state || $2::jsonb WHERE id = $1`, id, patch); err != nil {
		f.t.Fatalf("seed verification: %v", err)
	}
}

func observed(target, guarded time.Duration) queryObserver {
	return func(_ context.Context, ids []int64, _, _ time.Time) (
		map[int64]verify.Measurement, error) {
		out := map[int64]verify.Measurement{}
		for _, id := range ids {
			lat := target
			if id == 2 {
				lat = guarded
			}
			out[id] = verify.Measurement{Samples: 1000, AverageLatency: lat,
				StdErr: 100 * time.Microsecond, Buckets: 12}
		}
		return out, nil
	}
}

func (f *replaceFixture) monitorConfig(observe queryObserver) RollbackMonitorConfig {
	cfg := f.exec.rollbackMonitorConfig(allowRollback)
	cfg.WindowMinutes, cfg.CapMinutes, cfg.MinCalls = 60, 120, 30
	cfg.DropWindow = 24 * time.Hour
	cfg.observe = observe
	return cfg
}

func TestIndexReplaceMonitorRollsBackARegressionOfTheOldIndexUsers(t *testing.T) {
	f := newReplaceFixture(t, "b")
	id, err := f.run()
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	f.seedVerification(id)
	cfg := f.monitorConfig(observed(5*time.Millisecond, 20*time.Millisecond))
	d, err := f.exec.checkIndexReplace(f.ctx, id, cfg, time.Now().Add(2*time.Hour))
	if err != nil || d.Action != actRollback || d.Verdict != verify.OutcomeRegressed {
		t.Fatalf("decision = %+v, %v", d, err)
	}
	f.assertRolledBack(id)
	if o := storedOutcome(t, f.pool, id); o.Verdict != verify.OutcomeRegressed ||
		o.Class != verify.ClassIndexReplace {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestIndexReplaceMonitorKeepsThenSoftDropRestoresTheOldIndex(t *testing.T) {
	f := newReplaceFixture(t, "b")
	id, err := f.run()
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	f.seedVerification(id)
	at := time.Now().Add(2 * time.Hour)
	keep := f.monitorConfig(observed(5*time.Millisecond, 10*time.Millisecond))
	d, err := f.exec.checkIndexReplace(f.ctx, id, keep, at)
	if err != nil || d.Action != actKeep || d.Verdict != verify.OutcomeImproved {
		t.Fatalf("first verdict = %+v, %v", d, err)
	}
	if a := f.action(id); a.outcome != "success" {
		t.Fatalf("kept action outcome = %q", a.outcome)
	}
	miss := f.monitorConfig(observed(5*time.Millisecond, 20*time.Millisecond))
	d, err = f.exec.checkIndexReplace(f.ctx, id, miss, at.Add(time.Hour))
	if err != nil || d.Action != actRestoreOld {
		t.Fatalf("soft-drop miss = %+v, %v", d, err)
	}
	for _, index := range []string{f.oldIndex, f.newIndex} {
		if exists, valid := f.index(index); !exists || !valid {
			t.Fatalf("%s exists=%v valid=%v: the soft drop re-creates only the old index",
				index, exists, valid)
		}
	}
	if r := f.row(); r.state != replaceOldRestored {
		t.Fatalf("state = %q, want old_restored", r.state)
	}
	if def, _ := f.indexDef(f.oldIndex); def != f.oldDef {
		t.Fatalf("restored definition %q", def)
	}
}

func TestIndexReplaceMonitorEndsAfterTheDropWindow(t *testing.T) {
	f := newReplaceFixture(t, "b")
	id, err := f.run()
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	f.seedVerification(id)
	cfg := f.monitorConfig(observed(5*time.Millisecond, 10*time.Millisecond))
	at := time.Now().Add(2 * time.Hour)
	if d, err := f.exec.checkIndexReplace(f.ctx, id, cfg, at); err != nil ||
		d.Action != actKeep {
		t.Fatalf("keep = %+v, %v", d, err)
	}
	d, err := f.exec.checkIndexReplace(f.ctx, id, cfg, at.Add(48*time.Hour))
	if err != nil || d.Action != actDone {
		t.Fatalf("after the drop window = %+v, %v", d, err)
	}
	if exists, _ := f.index(f.oldIndex); exists {
		t.Fatal("a held soft drop stays dropped")
	}
	if d, err := f.exec.checkIndexReplace(f.ctx, id, cfg, at.Add(49*time.Hour)); err != nil ||
		d.Action != actDone {
		t.Fatalf("a finished watch stays finished: %+v, %v", d, err)
	}
}

// The undo drops only the index this replacement built: a new index of the
// same name created by someone else is kept and the undo fails loudly.
func TestIndexReplaceRollbackKeepsAForeignNewIndex(t *testing.T) {
	f := newReplaceFixture(t, "b")
	id, err := f.run()
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	f.must(t, "DROP INDEX "+f.newIndex)
	f.must(t, "CREATE INDEX "+f.bare+"_a_b ON "+f.table+" (b, a)")
	if err := f.exec.RollbackAction(f.ctx, id, "operator undo"); !errors.Is(err,
		ErrReplaceIdentityChanged) {
		t.Fatalf("rollback over a foreign index = %v", err)
	}
	if exists, _ := f.index(f.newIndex); !exists {
		t.Fatal("the foreign index with the new name is kept")
	}
	if exists, valid := f.index(f.oldIndex); !exists || !valid {
		t.Fatal("the old index is re-created before the drop is refused")
	}
	if r := f.row(); r.state != replaceRollbackFailed {
		t.Fatalf("state = %q, want rollback_failed", r.state)
	}
	if a := f.action(id); a.outcome != "rollback_failed" {
		t.Fatalf("action outcome = %q", a.outcome)
	}
}
