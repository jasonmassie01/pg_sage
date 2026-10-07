package retention

import (
	"strings"
	"testing"
	"time"
)

// Withheld decisions kept past the decisions window (they back an action,
// a dry run, an open deadline) were re-read by every purge pass: 75,000
// rows and 170 ms per pass on CI in the nightly perf gate, deleting
// nothing. A pass now reads only rows created since the last complete
// pass; a full sweep (at start and daily) catches rows whose keep reason
// went away.

func TestSweepPlan(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	floor, full := sweepPlan(sweepState{}, now)
	if !full || !floor.IsZero() {
		t.Fatalf("first pass = floor %v full %t, want a full sweep", floor, full)
	}
	st := sweepState{floor: now.Add(-31 * 24 * time.Hour), fullAt: now.Add(-time.Hour)}
	floor, full = sweepPlan(st, now)
	if full || !floor.Equal(st.floor) {
		t.Fatalf("an hour after a full sweep = floor %v full %t, want incremental from %v",
			floor, full, st.floor)
	}
	for _, since := range []time.Duration{fullSweepEvery, fullSweepEvery + time.Minute} {
		st.fullAt = now.Add(-since)
		if _, full = sweepPlan(st, now); !full {
			t.Fatalf("%s after the last full sweep: not a full sweep", since)
		}
	}
	st.fullAt = now.Add(-fullSweepEvery + time.Second)
	if _, full = sweepPlan(st, now); full {
		t.Fatal("a full sweep before the day is over")
	}
	st.fullAt = now.Add(time.Hour) // the clock went back: sweep fully
	if _, full = sweepPlan(st, now); !full {
		t.Fatal("a last full sweep in the future was trusted")
	}
}

func TestSweepDone(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	prev := sweepState{floor: now.Add(-40 * 24 * time.Hour), fullAt: now.Add(-2 * time.Hour)}
	wantFloor := now.Add(-30*24*time.Hour - sweepOverlap)
	got := sweepDone(prev, now, 30, true, true)
	if !got.floor.Equal(wantFloor) || !got.fullAt.Equal(now) {
		t.Fatalf("after a full sweep = %+v, want floor %v and fullAt now", got, wantFloor)
	}
	got = sweepDone(prev, now, 30, false, true)
	if !got.floor.Equal(wantFloor) || !got.fullAt.Equal(prev.fullAt) {
		t.Fatalf("after an incremental pass = %+v, want floor %v, fullAt kept", got,
			wantFloor)
	}
	for _, full := range []bool{true, false} {
		if got := sweepDone(prev, now, 30, full, false); got != prev {
			t.Fatalf("an unfinished pass (full %t) moved the state: %+v", full, got)
		}
	}
	// The floor never moves back (a shorter window after a config change
	// still reads from the earlier floor; a later full sweep covers it).
	ahead := sweepState{floor: now, fullAt: now.Add(-time.Hour)}
	if got := sweepDone(ahead, now, 30, false, true); !got.floor.Equal(now) {
		t.Fatalf("floor moved back to %v", got.floor)
	}
}

func TestWithheldDecisionRuleSweepsByCreation(t *testing.T) {
	for _, rule := range purgeRules(decisionRetention(30, 365)) {
		if rule.table != "decision" || !strings.Contains(rule.extra, "verdict <> 'execute'") {
			continue
		}
		if rule.sweepCol != "created_at" {
			t.Fatalf("withheld rule sweeps by %q, want created_at", rule.sweepCol)
		}
		sql := purgeSQL(rule, "sage.decision", 1000)
		if !strings.Contains(sql, "created_at >= $2") {
			t.Fatalf("withheld purge has no sweep floor:\n%s", sql)
		}
		return
	}
	t.Fatal("no withheld decision rule")
}

// Every other rule keeps its single-parameter statement.
func TestOnlySweptRulesTakeAFloor(t *testing.T) {
	for _, rule := range purgeRules(decisionRetention(30, 365)) {
		if rule.sweepCol != "" {
			continue
		}
		if sql := purgeSQL(rule, "sage."+rule.table, 1000); strings.Contains(sql, "$2") {
			t.Fatalf("rule %s.%s binds a floor it does not have:\n%s", rule.table,
				rule.timeCol, sql)
		}
	}
}

// A withheld decision kept because it backs an action is not re-read by
// the next pass once the action is gone; the daily full sweep purges it.
func TestWithheldPurgeRereadsKeptRowsOnlyOnTheFullSweep(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("dec_sweep")
	id := insertSeedDecision(t, ctx, tag, decisionSeed{"index", "parked", "60 days", "", ""})
	action := insertID(t, ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome, decision_id) VALUES ($1, 'SELECT 1', 'success', $2) RETURNING id`, tag, id)
	now := time.Now()
	c := New(testPool, decisionRetention(30, 365), noopLog)
	c.now = func() time.Time { return now }
	c.Run(ctx)
	if !decisionExists(t, ctx, id) {
		t.Fatal("a decision backing an action was purged")
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM sage.action_log WHERE id = $1",
		action); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	c.Run(ctx)
	if !decisionExists(t, ctx, id) {
		t.Fatal("the incremental pass re-read a row below its floor")
	}
	now = now.Add(fullSweepEvery)
	c.Run(ctx)
	if decisionExists(t, ctx, id) {
		t.Fatal("the daily full sweep kept a decision nothing backs any more")
	}
}

// A row that aged into the window since the last pass is purged by the
// next incremental pass; one below the floor waits for the full sweep.
func TestWithheldPurgeIncrementalPassAdmitsNewlyAgedRows(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("dec_aged")
	// The first (full) pass happens "two days ago": its floor is 32 days
	// and an hour back.
	now := time.Now().Add(-48 * time.Hour)
	c := New(testPool, decisionRetention(30, 365), noopLog)
	c.now = func() time.Time { return now }
	c.Run(ctx)
	aged := insertSeedDecision(t, ctx, tag, decisionSeed{"index", "parked", "31 days", "", ""})
	below := insertSeedDecision(t, ctx, tag, decisionSeed{"index", "parked", "40 days", "", ""})
	now = now.Add(time.Minute)
	c.Run(ctx)
	if decisionExists(t, ctx, aged) {
		t.Error("the incremental pass kept a decision that aged into the window")
	}
	if !decisionExists(t, ctx, below) {
		t.Error("the incremental pass read below its floor")
	}
	now = now.Add(fullSweepEvery)
	c.Run(ctx)
	if decisionExists(t, ctx, below) {
		t.Error("the full sweep kept an expired decision")
	}
}
