package rca

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// Dogfood round 2 (lifeos 2026-10-04): incident a9f1afd4 "Autovacuum
// falling behind" was escalated to critical on public.collector_manifests
// (30 live / 27 dead rows, 328 KB) and public.delivery_queue (3 / 3, 1.2
// MB). Autovacuum does not even consider those tables (threshold 50 + 20%),
// and their size cannot matter. A dead-tuple ratio counts only on a table
// with at least rca.vacuum_min_dead_tuples dead tuples and a heap of at
// least rca.vacuum_min_table_mb.

const mib = int64(1 << 20)

func floorCfg() *config.Config {
	cfg := defaultCfg()
	cfg.RCA.VacuumMinDeadTuples = 1000
	cfg.RCA.VacuumMinTableMB = 8
	return cfg
}

func tableAt(name string, live, dead, bytes int64) collector.TableStats {
	return collector.TableStats{SchemaName: "public", RelName: name, NLiveTup: live,
		NDeadTup: dead, TableBytes: bytes}
}

func blocked(t *testing.T, sig *Signal) []string {
	t.Helper()
	if sig == nil {
		return nil
	}
	tables, ok := sig.Metrics["blocked_tables"].([]string)
	if !ok {
		t.Fatalf("blocked_tables = %#v", sig.Metrics["blocked_tables"])
	}
	return tables
}

func TestVacuumBlocked_LifeosTinyTablesNeverFire(t *testing.T) {
	snap := &collector.Snapshot{CollectedAt: time.Now(), Tables: []collector.TableStats{
		tableAt("collector_manifests", 30, 27, 335872),
		tableAt("delivery_queue", 3, 3, 1236992),
	}}
	if sig := newTestEngine().detectVacuumBlocked(snap, floorCfg()); sig != nil {
		t.Fatalf("tiny tables fired vacuum_blocked: %+v", sig)
	}
}

func TestVacuumBlocked_SizeFloorBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		table collector.TableStats
		fires bool
	}{
		{"both floors met exactly", tableAt("t", 4000, 1000, 8*mib), true},
		{"one dead tuple short", tableAt("t", 3996, 999, 8*mib), false},
		{"one byte short", tableAt("t", 4000, 1000, 8*mib-1), false},
		{"big but few dead tuples", tableAt("t", 100, 900, 64*mib), false},
		{"many dead tuples, tiny heap", tableAt("t", 2000, 50000, 2*mib), false},
		{"large and bloated", tableAt("t", 100000, 50000, 512*mib), true},
		{"above floors, ratio below pct", tableAt("t", 1000000, 1000, 512*mib), false},
		{"zero live tuples", tableAt("t", 0, 5000, 64*mib), false},
		{"unknown size (0 bytes)", tableAt("t", 4000, 1000, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &collector.Snapshot{CollectedAt: time.Now(),
				Tables: []collector.TableStats{tc.table}}
			sig := newTestEngine().detectVacuumBlocked(snap, floorCfg())
			if (sig != nil) != tc.fires {
				t.Fatalf("fired = %v, want %v (%+v)", sig != nil, tc.fires, tc.table)
			}
		})
	}
}

// Only the tables above the floor are named; a tiny bloated table next to
// a large one is not evidence of the large one's incident.
func TestVacuumBlocked_NamesOnlyTablesAboveTheFloor(t *testing.T) {
	snap := &collector.Snapshot{CollectedAt: time.Now(), Tables: []collector.TableStats{
		tableAt("tiny", 3, 3, 16384),
		tableAt("orders", 100000, 40000, 256*mib),
	}}
	got := blocked(t, newTestEngine().detectVacuumBlocked(snap, floorCfg()))
	if len(got) != 1 || got[0] != "public.orders" {
		t.Fatalf("blocked tables = %v, want only public.orders", got)
	}
}

// A zero-value config (no floor keys) still gets the defaults: the
// default-masking guard of the user's rules.
func TestVacuumBlocked_ZeroConfigUsesDefaultFloors(t *testing.T) {
	cfg := defaultCfg()
	snap := &collector.Snapshot{CollectedAt: time.Now(), Tables: []collector.TableStats{
		tableAt("collector_manifests", 30, 27, 335872),
	}}
	if sig := newTestEngine().detectVacuumBlocked(snap, cfg); sig != nil {
		t.Fatalf("zero-value floors let a 57-row table fire: %+v", sig)
	}
	// Each default floor on its own: a large heap with 999 dead tuples, and
	// many dead tuples in a heap one byte under 8 MB.
	for _, tbl := range []collector.TableStats{tableAt("big_few", 1000, 999, 512*mib),
		tableAt("small_many", 4000, 5000, 8*mib-1)} {
		snap.Tables = []collector.TableStats{tbl}
		if sig := newTestEngine().detectVacuumBlocked(snap, cfg); sig != nil {
			t.Fatalf("zero-value floors let %s fire: %+v", tbl.RelName, sig)
		}
	}
	snap.Tables = []collector.TableStats{tableAt("orders", 4000, 1000, 8*mib)}
	if sig := newTestEngine().detectVacuumBlocked(snap, cfg); sig == nil {
		t.Fatal("the default floors suppressed a table exactly at the floor")
	}
}

// A configured floor is honoured both ways.
func TestVacuumBlocked_ConfiguredFloors(t *testing.T) {
	cfg := defaultCfg()
	cfg.RCA.VacuumMinDeadTuples = 10
	cfg.RCA.VacuumMinTableMB = 1
	snap := &collector.Snapshot{CollectedAt: time.Now(), Tables: []collector.TableStats{
		tableAt("queue", 40, 10, mib),
	}}
	if sig := newTestEngine().detectVacuumBlocked(snap, cfg); sig == nil {
		t.Fatal("a lowered floor must let a small hot table count")
	}
	cfg.RCA.VacuumMinDeadTuples = 11
	if sig := newTestEngine().detectVacuumBlocked(snap, cfg); sig != nil {
		t.Fatal("raised dead-tuple floor ignored")
	}
}

// End to end through the engine: tiny tables never open, let alone
// escalate, an autovacuum incident however many cycles they persist.
func TestVacuumBlocked_TinyTablesNeverEscalateToCritical(t *testing.T) {
	eng := newTestEngine()
	cfg := floorCfg()
	snap := &collector.Snapshot{CollectedAt: time.Now(), Tables: []collector.TableStats{
		tableAt("collector_manifests", 30, 27, 335872),
		tableAt("delivery_queue", 3, 3, 1236992),
	}}
	for i := 0; i < 10; i++ {
		snap.CollectedAt = time.Now()
		eng.Analyze(snap, nil, cfg, nil)
	}
	for _, inc := range eng.ActiveIncidents() {
		for _, id := range inc.SignalIDs {
			if id == "vacuum_blocked" {
				t.Fatalf("tiny tables opened a %s vacuum incident: %s", inc.Severity,
					inc.RootCause)
			}
		}
	}
	// The same snapshot with a table above the floor does open and, after
	// the escalation cycles, escalate: the floor is not a blanket mute.
	snap.Tables = append(snap.Tables, tableAt("orders", 100000, 40000, 256*mib))
	for i := 0; i < 10; i++ {
		snap.CollectedAt = time.Now()
		eng.Analyze(snap, nil, cfg, nil)
	}
	critical := false
	for _, inc := range eng.ActiveIncidents() {
		for _, id := range inc.SignalIDs {
			critical = critical || id == "vacuum_blocked" && inc.Severity == "critical"
		}
	}
	if !critical {
		t.Fatalf("a large bloated table did not escalate: %+v", eng.ActiveIncidents())
	}
}
