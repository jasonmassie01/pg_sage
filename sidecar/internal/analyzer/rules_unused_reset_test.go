package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Unused-index evidence drives an autonomous DROP INDEX, so a window that
// contains a statistics reset is broken evidence, not zero scans: the
// unused clock restarts at the reset, and only a full clean window after
// it can produce a finding. Resets are seen through the relation stats
// epoch each snapshot records (pg_stat_database.stats_reset, which every
// pg_stat_reset* moves, and the postmaster start), through the live epoch,
// through a counter that went down, and through a new index object (oid)
// under an old name.

var resetT = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

func resetSnap(scans int64, oid uint32, epoch time.Time) *collector.Snapshot {
	s := unusedIndexSnap(scans)
	s.Indexes[0].IndexRelID = oid
	s.System.RelationStatsEpoch = epoch
	return s
}

func resetExtras(firstSeen time.Time, now *time.Time) *RuleExtras {
	e := &RuleExtras{FirstSeen: map[string]time.Time{}, RecentlyCreated: map[string]time.Time{},
		Now: func() time.Time { return *now }}
	if !firstSeen.IsZero() {
		e.FirstSeen["public.idx_x"] = firstSeen
	}
	return e
}

// A reset between two samples hides any scans before it: an index unused
// since 6 days before the reset is not 8 days unused two days after it.
// The clock restarts at the reset; exactly one window after it the index
// is unused (boundary inclusive), one minute earlier it is not.
func TestUnusedIndex_ResetBetweenSamplesRestartsClockAtReset(t *testing.T) {
	now := resetT.Add(-6 * day)
	extras := resetExtras(time.Time{}, &now)
	cfg := unusedCfg()
	if got := ruleUnusedIndexes(resetSnap(0, 100, resetT.Add(-30*day)), nil, cfg,
		extras); len(got) != 0 {
		t.Fatalf("first sight: %+v", got)
	}
	cases := []struct {
		at   time.Time
		want int
	}{
		{resetT.Add(2 * day), 0},
		{resetT.Add(7*day - time.Minute), 0},
		{resetT.Add(7 * day), 1},
	}
	for _, tc := range cases {
		now = tc.at
		got := ruleUnusedIndexes(resetSnap(0, 100, resetT), nil, cfg, extras)
		if len(got) != tc.want {
			t.Fatalf("at reset+%s: %d findings, want %d", tc.at.Sub(resetT), len(got),
				tc.want)
		}
	}
}

// The coordinator's scenario: idx_scan > 0 is observed, the counters are
// reset between samples, zero scans follow. No finding until a full
// window after the first zero sample following the reset.
func TestUnusedIndex_ObservedScansThenResetThenZero(t *testing.T) {
	now := resetT.Add(-time.Hour)
	extras := resetExtras(resetT.Add(-30*day), &now)
	cfg := unusedCfg()
	ruleUnusedIndexes(resetSnap(42, 100, resetT.Add(-30*day)), nil, cfg, extras)
	if _, ok := extras.FirstSeen["public.idx_x"]; ok {
		t.Fatal("a scanned index keeps its unused clock")
	}
	now = resetT.Add(time.Minute)
	prev := resetSnap(42, 100, resetT.Add(-30*day))
	for _, at := range []time.Duration{time.Minute, 3 * day, 7 * day} {
		now = resetT.Add(at)
		if got := ruleUnusedIndexes(resetSnap(0, 100, resetT), prev, cfg, extras); len(got) != 0 {
			t.Fatalf("reset+%s: %+v, want none within the window", at, got)
		}
		prev = resetSnap(0, 100, resetT)
	}
	now = resetT.Add(7*day + time.Minute)
	if got := ruleUnusedIndexes(resetSnap(0, 100, resetT), prev, cfg, extras); len(got) != 1 {
		t.Fatalf("after a full clean window: %d findings, want 1", len(got))
	}
}

// A counter that went down between the previous and the current sample is
// a reset even without epoch evidence (epoch unknown on both sides): the
// clock restarts at the current sample.
func TestUnusedIndex_CounterDecreaseRestartsClock(t *testing.T) {
	now := resetT
	extras := resetExtras(resetT.Add(-30*day), &now)
	cfg := unusedCfg()
	prev := resetSnap(3, 100, time.Time{})
	if got := ruleUnusedIndexes(resetSnap(0, 100, time.Time{}), prev, cfg, extras); len(got) != 0 {
		t.Fatalf("right after a decrease: %+v", got)
	}
	if !extras.FirstSeen["public.idx_x"].Equal(resetT) {
		t.Fatalf("clock = %s, want restarted at %s", extras.FirstSeen["public.idx_x"], resetT)
	}
	now = resetT.Add(7 * day)
	same := resetSnap(0, 100, time.Time{})
	if got := ruleUnusedIndexes(resetSnap(0, 100, time.Time{}), same, cfg, extras); len(got) != 1 {
		t.Fatalf("a window after the decrease: %d findings, want 1", len(got))
	}
}

// A previous sample of a different object (dropped and recreated) is not a
// counter decrease of this one, and a new oid under an old name restarts
// the clock: the new index has no history.
func TestUnusedIndex_NewObjectUnderOldNameRestartsClock(t *testing.T) {
	now := resetT
	extras := resetExtras(time.Time{}, &now)
	cfg := unusedCfg()
	now = resetT.Add(-30 * day)
	ruleUnusedIndexes(resetSnap(0, 100, resetT.Add(-60*day)), nil, cfg, extras)
	now = resetT
	if got := ruleUnusedIndexes(resetSnap(0, 200, resetT.Add(-60*day)), nil, cfg,
		extras); len(got) != 0 {
		t.Fatalf("recreated index flagged with its predecessor's history: %+v", got)
	}
	if !extras.FirstSeen["public.idx_x"].Equal(resetT) || extras.IndexOID["public.idx_x"] != 200 {
		t.Fatalf("clock = %s oid = %d, want restarted for oid 200",
			extras.FirstSeen["public.idx_x"], extras.IndexOID["public.idx_x"])
	}
	// Unknown oids (legacy snapshots, tests) never count as a new object.
	now = resetT.Add(7 * day)
	if got := ruleUnusedIndexes(resetSnap(0, 0, resetT.Add(-60*day)), nil, cfg,
		extras); len(got) != 1 {
		t.Fatalf("unknown oid: %d findings, want 1", len(got))
	}
}

// The later of the snapshot's recorded epoch and the live epoch counts:
// either one alone proves the reset.
func TestUnusedIndex_LaterOfSnapshotAndLiveEpoch(t *testing.T) {
	cfg := unusedCfg()
	for name, tc := range map[string]struct{ snapshot, live time.Time }{
		"snapshot only":  {resetT, time.Time{}},
		"live only":      {time.Time{}, resetT},
		"live later":     {resetT.Add(-30 * day), resetT},
		"snapshot later": {resetT, resetT.Add(-30 * day)},
	} {
		now := resetT.Add(2 * day)
		extras := resetExtras(resetT.Add(-30*day), &now)
		extras.StatsEpoch = tc.live
		got := ruleUnusedIndexes(resetSnap(0, 100, tc.snapshot), nil, cfg, extras)
		if len(got) != 0 {
			t.Errorf("%s: %d findings two days after a reset, want 0", name, len(got))
		}
	}
}

// A finding states its evidence: since when the index is unused and the
// statistics epoch it was checked against.
func TestUnusedIndex_FindingCarriesEvidence(t *testing.T) {
	now := resetT
	extras := resetExtras(resetT.Add(-10*day), &now)
	extras.IndexOID = map[string]uint32{"public.idx_x": 100}
	epoch := resetT.Add(-20 * day)
	got := ruleUnusedIndexes(resetSnap(0, 100, epoch), nil, unusedCfg(), extras)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	d := got[0].Detail
	if d["unused_since"] != resetT.Add(-10*day).Format(time.RFC3339) ||
		d["stats_epoch"] != epoch.Format(time.RFC3339) {
		t.Fatalf("detail = %v, want unused_since and stats_epoch", d)
	}
}

// Nil state maps (rule extras built as literals) are tolerated.
func TestUnusedIndex_NilOIDMap(t *testing.T) {
	now := resetT
	extras := &RuleExtras{FirstSeen: map[string]time.Time{"public.idx_x": resetT.Add(-30 * day)},
		RecentlyCreated: map[string]time.Time{}, Now: func() time.Time { return now }}
	if got := ruleUnusedIndexes(resetSnap(0, 100, time.Time{}), nil, unusedCfg(),
		extras); len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	if extras.IndexOID["public.idx_x"] != 100 {
		t.Fatal("the object identity was not recorded")
	}
}
