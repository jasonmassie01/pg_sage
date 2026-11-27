package shadow

import (
	"fmt"
	"testing"
	"time"
)

// Integration (real Postgres), earned-03: one pass reads the oldest half
// of its batch of pending decisions and the newest half, so decisions
// that wait (up to the horizon) never keep a newer one, whose operator
// already decided, from being scored.

// rejectedDecisions records n index creates on public.o, the i-th
// recorded (n-i) hours ago (oldest first), each rejected by an operator
// right after; it returns their ids, oldest first.
func rejectedDecisions(t *testing.T, s *Store, n int, prefix string) []int64 {
	t.Helper()
	ids := make([]int64, n)
	for i := range n {
		sql := fmt.Sprintf(`CREATE INDEX CONCURRENTLY %s_%d ON public.o (%s_%d)`,
			prefix, i, prefix, i)
		d := record(t, s, sample("index_create", sql))
		recorded := time.Duration(n-i) * time.Hour
		age(t, s.pool, d.ID, recorded)
		queueDecision(t, s.pool, sql, "rejected", 0, recorded-time.Minute)
		ids[i] = d.ID
	}
	return ids
}

// Boundary, failed before earned-03: 200 waiting decisions (the default
// batch) are older than one whose proposal an operator rejected; the
// pass reads only the oldest 200 and never scored it.
func TestScorerNewestDecisionIsNotStarvedByWaitingOnes(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	o := fastOptions()
	for i := range o.Batch {
		record(t, s, sample("vacuum", fmt.Sprintf(`VACUUM public.o_%d`, i)))
	}
	mustExec(t, pool, `UPDATE sage.shadow_decision
		SET recorded_at = now() - interval '5 hours' - id * interval '1 second',
		    last_seen_at = now() - interval '5 hours'`)
	newest := rejectedDecisions(t, s, 1, "starved")[0]
	res := runOnce(t, newScorer(pool, o))
	assertScore(t, s, newest, ScoreIncorrect, SourceOperator, false)
	if res.Examined != o.Batch || res.Waiting != o.Batch-1 ||
		res.Scored[SourceOperator] != 1 {
		t.Fatalf("result %+v, want %d examined, %d waiting, 1 operator score", res, o.Batch,
			o.Batch-1)
	}
}

// State transitions: with a batch of 4 and 6 decided decisions, a pass
// scores the 2 oldest and the 2 newest; the next pass scores the middle.
func TestScorerReadsTheOldestAndNewestHalves(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	o := fastOptions()
	o.Batch = 4
	ids := rejectedDecisions(t, s, 6, "halves")
	res := runOnce(t, newScorer(pool, o))
	if res.Examined != 4 || res.Scored[SourceOperator] != 4 {
		t.Fatalf("first pass %+v, want 4 examined and scored", res)
	}
	for _, i := range []int{0, 1, 4, 5} {
		assertScore(t, s, ids[i], ScoreIncorrect, SourceOperator, false)
	}
	assertPending(t, s, ids[2])
	assertPending(t, s, ids[3])
	res = runOnce(t, newScorer(pool, o))
	if res.Examined != 2 || res.Scored[SourceOperator] != 2 {
		t.Fatalf("second pass %+v, want the 2 left", res)
	}
	assertScore(t, s, ids[2], ScoreIncorrect, SourceOperator, false)
	assertScore(t, s, ids[3], ScoreIncorrect, SourceOperator, false)
}

// Boundary: fewer pending decisions than the batch are each examined
// once (the halves overlap), oldest first; a batch of 1 reads the oldest.
func TestScorerHalvesNeverExamineADecisionTwice(t *testing.T) {
	for _, tc := range []struct {
		name            string
		batch, pending  int
		examined        int
		scoredOldestOne bool
	}{
		{"overlap", 4, 3, 3, false},
		{"exact", 4, 4, 4, false},
		{"odd batch", 3, 5, 3, false},
		{"batch of one", 1, 2, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _ := testPool(t)
			s := NewStore(pool)
			o := fastOptions()
			o.Batch = tc.batch
			ids := rejectedDecisions(t, s, tc.pending, "dup")
			res := runOnce(t, newScorer(pool, o))
			if res.Examined != tc.examined || res.Scored[SourceOperator] != tc.examined {
				t.Fatalf("result %+v, want %d examined and scored", res, tc.examined)
			}
			if tc.scoredOldestOne {
				assertScore(t, s, ids[0], ScoreIncorrect, SourceOperator, false)
				assertPending(t, s, ids[1])
			}
		})
	}
}
