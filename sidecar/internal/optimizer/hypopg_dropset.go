package optimizer

import (
	"context"
	"fmt"
	"strings"
)

// MeasureDropSet plans the workload (at most maxWhatIfStatements, the
// hottest first) with every index of set and again with all of them
// hidden, on one owning session (review-02a: an index consolidation set is
// judged as one change). A statement whose baseline plan used any of them
// and whose planned cost rose more than maxRegressionPct is a regression.
// Without hypopg_hide_index nothing is measured and OldIndexPresent says so.
func (h *HypoPG) MeasureDropSet(ctx context.Context, set []IndexRef, queries []QueryInfo,
	maxRegressionPct float64,
) (WhatIfResult, error) {
	if h.pool == nil {
		return WhatIfResult{}, fmt.Errorf("HypoPG requires a database pool")
	}
	if len(set) == 0 {
		return WhatIfResult{}, fmt.Errorf("HypoPG drop set: no index to hide")
	}
	if err := ctx.Err(); err != nil {
		return WhatIfResult{}, err
	}
	queries = whatIfSample(explainableQueries(queries), nil)
	if len(queries) == 0 {
		return WhatIfResult{}, nil
	}
	return h.inSession(ctx, func(s *hypopgSession) (WhatIfResult, error) {
		if !s.canHide {
			return WhatIfResult{OldIndexPresent: true}, nil
		}
		return s.evaluateDropSet(ctx, set, queries, maxRegressionPct)
	})
}

// evaluateDropSet plans the workload with the set and with it hidden.
func (s *hypopgSession) evaluateDropSet(ctx context.Context, set []IndexRef,
	queries []QueryInfo, maxPct float64,
) (WhatIfResult, error) {
	hidden := make([]hiddenIndex, 0, len(set))
	names := make([]string, 0, len(set))
	for i := range set {
		old, err := s.indexToHide(ctx, &set[i])
		if err != nil {
			return WhatIfResult{}, err
		}
		hidden, names = append(hidden, old), append(names, old.name)
	}
	before, err := s.measureCosts(ctx, queries, names...)
	if err != nil || len(before.costs) == 0 {
		return WhatIfResult{Failed: len(before.unplanned),
			UnplannedQueryIDs: before.unplanned}, err
	}
	for _, old := range hidden {
		if err := s.hide(ctx, old.oid); err != nil {
			return WhatIfResult{}, err
		}
	}
	after, err := s.measureCosts(ctx, queries)
	if err != nil {
		return WhatIfResult{}, err
	}
	res := comparedCosts(queries, before.costs, after.costs, before.unplanned)
	res.HiddenIndex = strings.Join(names, ", ")
	res.Guarded, res.Regressions = guardedRegressionsAbove(queries, before, after.costs, maxPct)
	return res, nil
}
