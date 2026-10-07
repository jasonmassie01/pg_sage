package probes

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// A query-scoped plan_regressions run on the meta history store binds both
// the statement ($3) and this database's id (appended after it): it reads
// only that statement of only this database, in both placements.
func TestPlanRegressionsScopedInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	const target, other = 883001, 883002
	now := time.Now()
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		st := histstore.Resolve(p.Monitored)
		for _, qid := range []int64{target, other} {
			for _, s := range []sample{{30 * time.Minute, 0, 0, "v1:a"},
				{20 * time.Minute, 10, 5, "v1:a"}, {10 * time.Minute, 20, 55, "v1:b"}} {
				if _, err := st.Exec(ctx, `INSERT INTO sage.query_store (captured_at,
					queryid, calls, total_exec_time, mean_exec_time, plan_hash{dbcol})
					VALUES ($1, $2, $3, $4, 0, $5{dbval})`,
					now.Add(-s.ago), qid, s.calls, s.total, s.hash); err != nil {
					t.Fatalf("%s seed %d: %v", mode, qid, err)
				}
			}
		}
		// Another database flips the target the other way, later.
		p.Noise(t, now.Add(-5*time.Minute), target)
		p.Noise(t, now.Add(-2*time.Minute), target)
		r := NewRunner(p.Monitored, Catalog(), NewLimiter(1))
		shifts, err := PlanShifts(r.Run(ctx, PlanRegressions, Args{QueryID: target}))
		if err != nil {
			t.Fatalf("%s scoped run: %v", mode, err)
		}
		if len(shifts) != 1 || shifts[0].QueryID != target ||
			shifts[0].PreviousHash != "v1:a" || shifts[0].CurrentHash != "v1:b" {
			t.Fatalf("%s: scoped shifts = %+v, want this database's one flip of %d",
				mode, shifts, target)
		}
		all, err := PlanShifts(r.Run(ctx, PlanRegressions, Args{}))
		if err != nil || len(all) != 2 {
			t.Fatalf("%s: unscoped shifts = %+v (%v), want both statements", mode, all, err)
		}
	}
}
