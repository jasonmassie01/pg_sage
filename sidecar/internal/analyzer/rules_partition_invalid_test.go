package analyzer

import (
	"context"
	"strings"
	"testing"
	"time"
)

// rules-03 (review): an invalid index can be attached to a partitioned
// index (a failed CREATE INDEX CONCURRENTLY on a partition, then ALTER
// INDEX ... ATTACH PARTITION). PostgreSQL refuses to drop it on its own
// (2BP01), so invalid_index must not recommend DROP INDEX CONCURRENTLY.

// No concurrent access tests: the rule is a pure function over one
// snapshot, run on the analyzer's single cycle goroutine.

// invalidTwice runs ruleInvalidIndexes for two cycles (the rule waits for
// a second sighting) and returns the second cycle's findings.
func invalidTwice(t *testing.T, e *RuleExtras, idx ...string) []Finding {
	t.Helper()
	snap := partitionedSnap(2)
	for i := range snap.Indexes {
		for _, name := range idx {
			if snap.Indexes[i].IndexRelName == name {
				snap.Indexes[i].IsValid = false
			}
		}
	}
	ruleInvalidIndexes(snap, nil, unusedCfg(), e)
	return ruleInvalidIndexes(snap, nil, unusedCfg(), e)
}

func TestPartitionIndex_InvalidAttachedChildIsAdvisory(t *testing.T) {
	e := partExtras()
	got := invalidTwice(t, e, "events_p1_ts_idx")
	if len(got) != 1 || got[0].ObjectIdentifier != "public.events_p1_ts_idx" {
		t.Fatalf("findings = %v, want the invalid attached child", identsOf(got))
	}
	f := got[0]
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Fatalf("SQL = %q rollback = %q, want none: PostgreSQL refuses the drop",
			f.RecommendedSQL, f.RollbackSQL)
	}
	if f.ActionRisk != "high_risk" {
		t.Errorf("ActionRisk = %q, want high_risk (advisory)", f.ActionRisk)
	}
	if f.Detail["attached_to_partitioned_index"] != true {
		t.Errorf("detail = %v, want attached_to_partitioned_index", f.Detail)
	}
	want := `REINDEX INDEX CONCURRENTLY "public"."events_p1_ts_idx"`
	if !strings.Contains(f.Recommendation, want) {
		t.Errorf("recommendation = %q, want it to name %s", f.Recommendation, want)
	}
}

// A standalone invalid index on a partition keeps its drop (boundary).
func TestPartitionIndex_InvalidStandaloneKeepsDrop(t *testing.T) {
	e := partExtras()
	snap := partitionedSnap(1)
	solo := childIndex("events_p1", "events_p1_solo", "ts", 2001, 0)
	solo.IsValid = false
	snap.Indexes = append(snap.Indexes, solo)
	ruleInvalidIndexes(snap, nil, unusedCfg(), e)
	got := ruleInvalidIndexes(snap, nil, unusedCfg(), e)
	if len(got) != 1 || got[0].RecommendedSQL !=
		`DROP INDEX CONCURRENTLY "public"."events_p1_solo";` {
		t.Fatalf("findings = %+v, want the standalone drop", got)
	}
	if got[0].ActionRisk != "safe" {
		t.Errorf("ActionRisk = %q, want safe as before rules-03", got[0].ActionRisk)
	}
}

// Real PostgreSQL: an invalid index attached to a partitioned index gives
// an invalid_index finding with no SQL, and the DROP the rule used to
// recommend is the one PostgreSQL rejects (control).
func TestPartitionIndex_InvalidAttachedChildOnRealPostgres(t *testing.T) {
	ctx := context.Background()
	pool := partitionDB(t, "rules03_inv",
		"CREATE TABLE public.pt (k int) PARTITION BY RANGE (k)",
		"CREATE TABLE public.pt1 PARTITION OF public.pt FOR VALUES FROM (0) TO (10)",
		"INSERT INTO public.pt1 VALUES (1), (1)")
	const build = "CREATE UNIQUE INDEX CONCURRENTLY pt1_k ON public.pt1 (k)"
	if _, err := pool.Exec(ctx, build); err == nil {
		t.Fatal("setup: the unique build over a duplicate must fail and leave it invalid")
	}
	for _, sql := range []string{
		"CREATE UNIQUE INDEX pt_k ON ONLY public.pt (k)",
		"ALTER INDEX public.pt_k ATTACH PARTITION public.pt1_k",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	snap := collectOneSnapshot(t, pool, wraparoundDBConfig())
	e := defaultExtras()
	e.InvalidFirstSeen = map[string]time.Time{"public.pt1_k": time.Now()}
	got := ruleInvalidIndexes(snap, nil, nil, e)
	if len(got) != 1 || got[0].ObjectIdentifier != "public.pt1_k" ||
		got[0].RecommendedSQL != "" {
		t.Fatalf("findings = %+v, want one advisory finding on public.pt1_k", got)
	}
	_, err := pool.Exec(ctx, `DROP INDEX CONCURRENTLY "public"."pt1_k"`)
	if err == nil || !strings.Contains(err.Error(), "requires it") {
		t.Fatalf("control: drop of the attached child err = %v, want the rejection", err)
	}
}
