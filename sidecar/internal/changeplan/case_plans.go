package changeplan

import (
	"context"
	"fmt"
)

// The plans a case belongs to (ux-03): a case whose finding is a step of a
// plan (sage.plan_step.finding_id) shows "Part of plan: <goal>" and links
// to the plan's review. Dead plans (draft, rejected, voided, cancelled)
// are left out; each finding lists at most MaxCasePlans, newest first.

// MaxCasePlans is how many plans one finding lists.
const MaxCasePlans = 5

// PlanRef is a plan named by id and goal.
type PlanRef struct {
	ID   int64  `json:"id"`
	Goal string `json:"goal"`
}

// PlansForFindings maps each finding id that is a step of a live plan to
// those plans, newest first. No ids reads nothing.
func (s *Store) PlansForFindings(ctx context.Context, findingIDs []int64) (
	map[int64][]PlanRef, error) {
	out := map[int64][]PlanRef{}
	if len(findingIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT finding_id, id, goal FROM (
		  SELECT DISTINCT ps.finding_id, p.id, p.goal
		    FROM sage.plan_step ps JOIN sage.plan p ON p.id = ps.plan_id
		   WHERE ps.finding_id = ANY($1::bigint[])
		     AND p.status NOT IN ('draft', 'rejected', 'voided', 'cancelled')) found
		 ORDER BY finding_id, id DESC`, findingIDs)
	if err != nil {
		return nil, fmt.Errorf("changeplan: read plans for findings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var finding int64
		var ref PlanRef
		if err := rows.Scan(&finding, &ref.ID, &ref.Goal); err != nil {
			return nil, fmt.Errorf("changeplan: read plans for findings: %w", err)
		}
		if len(out[finding]) < MaxCasePlans {
			out[finding] = append(out[finding], ref)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("changeplan: read plans for findings: %w", err)
	}
	return out, nil
}
