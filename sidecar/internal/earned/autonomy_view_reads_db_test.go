package earned

import (
	"testing"
	"time"
)

// earned-08 review: the batched reads of View and Evaluate keep what the
// per-pair reads gave them, where the golden fixture has nothing to show:
// a composite class bounded by its components, and the game days and
// bench report of the view.

// Evaluate reads every pair's level from one batch; a composite's level
// is still bounded by its components' levels, as Granted bounds it.
func TestAutonomyViewEvaluateBoundsACompositeByItsComponents(t *testing.T) {
	cases := []struct {
		name              string
		create, drop, own Level
		hasOwn            bool
		granted           Level
	}{
		{"components at L2, no own row", L2, L2, 0, false, L2},
		{"drop at its default", L3, 0, 0, false, L1},
		{"own row demoted", L3, L3, L1, true, L1},
		{"own row above its components", L2, L2, L3, true, L2},
	}
	for _, c := range cases {
		lf := newLimiterFixture(t)
		if c.create > 0 {
			lf.seedLevel(FamilyTuning, ClassIndexCreate, c.create)
		}
		if c.drop > 0 {
			lf.seedLevel(FamilyHygiene, ClassIndexDrop, c.drop)
		}
		if c.hasOwn {
			lf.seedLevel(FamilyTuning, ClassIndexReplace, c.own)
		}
		e, err := lf.svc.Evaluate(lf.ctx)
		if err != nil {
			t.Fatalf("%s: evaluate: %v", c.name, err)
		}
		levels := evaluatedLevels(e)
		if got, ok := levels[pairKey{FamilyTuning, ClassIndexReplace}]; !ok || got != c.granted {
			t.Fatalf("%s: evaluate saw index_replace at %v (listed %v), want %v", c.name, got,
				ok, c.granted)
		}
		for key, got := range levels {
			st, err := lf.svc.Granted(lf.ctx, key.family, key.class)
			if err != nil || st.Level != got {
				t.Fatalf("%s: evaluate saw %s/%s at %v, Granted reads %v (%v)", c.name,
					key.family, key.class, got, st.Level, err)
			}
		}
		if len(levels) != applicablePairs() {
			t.Fatalf("%s: evaluate covered %d pairs, want %d", c.name, len(levels),
				applicablePairs())
		}
	}
}

// evaluatedLevels is the level Evaluate saw for each pair it covered.
func evaluatedLevels(e Evaluation) map[pairKey]Level {
	out := map[pairKey]Level{}
	for _, np := range e.NotProposed {
		out[pairKey{np.Family, np.Class}] = np.Granted
	}
	for _, p := range e.Created {
		out[pairKey{p.Family, p.Class}] = p.From
	}
	return out
}

// The view carries the database's recent game days and the newest bench
// report from the same batch, the reports GameDayRuns and LatestBench
// read; an empty ledger shows an empty (not null) game-day list.
func TestAutonomyViewShowsTheGameDaysAndTheBench(t *testing.T) {
	f := newReconFixture(t)
	v, err := f.svc.View(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v.GameDays == nil || len(v.GameDays) != 0 || v.Bench != nil {
		t.Fatalf("empty ledger: game days %v, bench %v; want [] and nil", v.GameDays,
			v.Bench)
	}
	now := f.clock.Now()
	for i, fam := range []Family{FamilyWraparound, FamilyWAL} {
		at := now.Add(-time.Duration(i+1) * time.Hour)
		if _, err := f.svc.IngestEvalRun(f.ctx, benchReport(at, fam), SourceGameDay,
			ActorPgSage, f.db); err != nil {
			t.Fatal(err)
		}
	}
	bench, err := f.svc.IngestEvalRun(f.ctx, benchReport(now.Add(-30*time.Minute),
		FamilyWraparound), SourceBench, ActorPgSage, "")
	if err != nil {
		t.Fatal(err)
	}
	if v, err = f.svc.View(f.ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := f.store.GameDayRuns(f.ctx, now.Add(-f.svc.cfg.Thresholds.BenchMaxAge))
	if err != nil || len(runs) != 2 {
		t.Fatalf("game day runs = %d (%v), want 2", len(runs), err)
	}
	if len(v.GameDays) != len(runs) || v.GameDays[0].ID != runs[0].ID ||
		v.GameDays[1].ID != runs[1].ID {
		t.Fatalf("view game days = %+v, want %+v (newest first)", v.GameDays, runs)
	}
	if v.Bench == nil || v.Bench.ID != bench.ID {
		t.Fatalf("view bench = %+v, want the ingested report %s", v.Bench, bench.ID)
	}
}
