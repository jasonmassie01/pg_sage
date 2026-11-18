package rubric

import (
	"testing"
)

// fleetpol-01b: discovery reads a member's grades oldest first to date its
// first grade and the first that meets its set's baseline.
func TestStoreDB_OldestIsOldestFirstWithDimensions(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	db := "oldest_" + t.Name()
	if got, err := s.Oldest(ctx, db, 5); err != nil || len(got) != 0 {
		t.Fatalf("no runs: %+v %v", got, err)
	}
	var ids []int64
	for i, letter := range []Letter{"C", "A", ""} {
		id, err := s.Start(ctx, spec1(db))
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if letter == "" {
			if err := s.Finish(ctx, id, RunFailed, nil, "loader failed"); err != nil {
				t.Fatalf("fail run: %v", err)
			}
			continue
		}
		g := &Grade{RubricVersion: Version, Overall: letter, OverallScore: 80,
			CoveragePct: 100, Dimensions: []DimensionGrade{{Dimension: "safety",
				Title: "Safety", Letter: letter, Score: 80, Coverage: 100}}}
		if err := s.Finish(ctx, id, RunDone, g, ""); err != nil {
			t.Fatalf("finish %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	got, err := s.Oldest(ctx, db, 5)
	if err != nil || len(got) != 2 {
		t.Fatalf("Oldest = %+v %v, want the two graded runs", got, err)
	}
	if got[0].ID != ids[0] || got[1].ID != ids[1] ||
		got[0].Grade.Dimensions[0].Letter != "C" || got[1].Grade.Dimensions[0].Letter != "A" ||
		got[0].FinishedAt == nil {
		t.Fatalf("Oldest order or dimensions wrong: %+v", got)
	}
	if one, err := s.Oldest(ctx, db, 0); err != nil || len(one) != 1 || one[0].ID != ids[0] {
		t.Fatalf("n=0 = %+v %v, want the oldest one", one, err)
	}
}
