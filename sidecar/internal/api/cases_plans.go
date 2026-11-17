package api

import (
	"context"
	"log/slog"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/changeplan"
)

// casePlanReader is the plan store's read of the plans findings are steps
// of (*changeplan.Store).
type casePlanReader interface {
	PlansForFindings(ctx context.Context, findingIDs []int64) (
		map[int64][]changeplan.PlanRef, error)
}

// enrichCasePlans attaches each finding case's plans ("Part of plan",
// ux-03), read through the database's plan store.
func enrichCasePlans(ctx context.Context, projected []cases.Case, selected namedPool,
	findingIDs []int) {
	if len(findingIDs) == 0 || selected.pool == nil {
		return
	}
	attachCasePlans(ctx, projected, selected.name, changeplan.NewStore(selected.pool),
		findingIDs)
}

// attachCasePlans reads the plans of findingIDs in one query and attaches
// them; a failed read is logged and marks every finding case
// plans_unavailable, never "part of no plan".
func attachCasePlans(ctx context.Context, projected []cases.Case, database string,
	reader casePlanReader, findingIDs []int) {
	if len(findingIDs) == 0 || reader == nil {
		return
	}
	ids := make([]int64, len(findingIDs))
	for i, id := range findingIDs {
		ids[i] = int64(id)
	}
	byFinding, err := reader.PlansForFindings(ctx, ids)
	if err != nil {
		slog.Warn("case plans read failed; marked plans_unavailable",
			"database", database, "findings", len(ids), "error", err)
	}
	for i := range projected {
		findingID, ok := caseFindingID(projected[i])
		switch {
		case !ok:
		case err != nil:
			cases.MarkPlansUnavailable(&projected[i])
		case len(byFinding[int64(findingID)]) > 0:
			cases.AttachPlans(&projected[i], casePlanLinks(byFinding[int64(findingID)]))
		}
	}
}

func casePlanLinks(refs []changeplan.PlanRef) []cases.PlanLink {
	out := make([]cases.PlanLink, 0, len(refs))
	for _, r := range refs {
		out = append(out, cases.PlanLink{ID: r.ID, Goal: r.Goal})
	}
	return out
}
