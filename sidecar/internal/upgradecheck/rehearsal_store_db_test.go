package upgradecheck

import (
	"testing"
	"time"
)

// drill-02b: the rehearsal is stored on the brief and carried onto the
// next stored brief of the same database and target.

func storedRehearsal(t *testing.T, d liveDB, id int64) (string, int) {
	t.Helper()
	var db string
	var changed int
	if err := d.pool.QueryRow(d.ctx, `SELECT brief->'rehearsal'->>'database',
		(brief->'rehearsal'->>'plans_changed')::int FROM sage.upgrade_briefs WHERE id = $1`,
		id).Scan(&db, &changed); err != nil {
		t.Fatalf("read the stored rehearsal of brief %d: %v", id, err)
	}
	return db, changed
}

func sampleBrief(database string, major int) Brief {
	b := Brief{Database: database, Source: BriefVersion{Major: 14},
		Target: BriefVersion{Major: major}, Method: PGUpgrade, Provider: "self-managed"}
	b.Hash = briefHash(b)
	return b
}

func sampleRehearsal(database string) RehearsalReport {
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	return RehearsalReport{Database: database, SourceMajor: 14, TargetMajor: 17,
		State: RehearsalDestroyed, StartedAt: at, FinishedAt: at.Add(time.Hour),
		PlansChanged: 2, RecoveredAfterAnalyze: 1,
		Changed: []RehearsedQuery{{QueryID: 1, Status: QueryCompared, ChangedAsCutOver: true},
			{QueryID: 2, Status: QueryCompared, ChangedAfterAnalyze: true}}}
}

func TestRehearsalStoreDB_StoredOnTheBriefAndCarriedForward(t *testing.T) {
	d := bootstrapped(t)
	b := sampleBrief(d.name, 17)
	got, err := StoreRehearsal(d.ctx, d.pool, b, sampleRehearsal(d.name))
	if err != nil {
		t.Fatalf("StoreRehearsal: %v", err)
	}
	if db, changed := storedRehearsal(t, d, got.ID); db != d.name || changed != 2 {
		t.Fatalf("stored rehearsal database %q changed %d, want %s 2", db, changed, d.name)
	}
	if got.Rehearsal == nil || got.Rehearsal.PlansChanged != 2 {
		t.Fatalf("StoredBrief.Rehearsal = %+v", got.Rehearsal)
	}
	same, err := StoreBrief(d.ctx, d.pool, b)
	if err != nil || same.Stored || same.ID != got.ID {
		t.Fatalf("unchanged brief stored again: %+v %v", same, err)
	}
	if same.Rehearsal == nil || same.Rehearsal.Database != d.name {
		t.Fatalf("unchanged brief lost its rehearsal: %+v", same.Rehearsal)
	}
	next := b
	next.Method = Logical
	next.Hash = briefHash(next)
	moved, err := StoreBrief(d.ctx, d.pool, next)
	if err != nil || !moved.Stored || moved.ID == got.ID {
		t.Fatalf("changed brief not stored: %+v %v", moved, err)
	}
	if db, changed := storedRehearsal(t, d, moved.ID); db != d.name || changed != 2 {
		t.Fatalf("new brief row rehearsal %q %d, want it carried forward", db, changed)
	}
	if briefRows(t, d) != 2 {
		t.Fatalf("brief rows = %d, want 2", briefRows(t, d))
	}
}

func TestRehearsalStoreDB_InvalidReportRefused(t *testing.T) {
	d := bootstrapped(t)
	b := sampleBrief(d.name, 17)
	bad := sampleRehearsal(d.name)
	bad.Database = "other"
	if _, err := StoreRehearsal(d.ctx, d.pool, b, bad); err == nil {
		t.Fatal("a rehearsal of another database was stored on the brief")
	}
	bad = sampleRehearsal(d.name)
	bad.TargetMajor = 16
	if _, err := StoreRehearsal(d.ctx, d.pool, b, bad); err == nil {
		t.Fatal("a rehearsal to another target major was stored on the brief")
	}
	if briefRows(t, d) != 0 {
		t.Fatalf("refused rehearsals left %d brief rows", briefRows(t, d))
	}
	if _, err := StoreRehearsal(d.ctx, nil, b, sampleRehearsal(d.name)); err == nil {
		t.Fatal("nil pool accepted")
	}
}
