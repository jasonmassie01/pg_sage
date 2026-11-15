package optimizer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// maxGuardedRegressionPct is how much a guarded statement's planned cost
// may rise once the index it used is hidden (a replacement's old index, a
// dropped index). More rejects the change, naming the statement.
const maxGuardedRegressionPct = 10

// hideUnsupportedReason is why a replacement was measured the old way:
// hypopg_hide_index arrived in HypoPG 1.4, and providers ship older ones.
const hideUnsupportedReason = "HypoPG without hypopg_hide_index (1.4+): measured with " +
	"the old index present"

// dropUnsupportedReason is why a drop could not be measured at all.
const dropUnsupportedReason = "HypoPG without hypopg_hide_index (1.4+): the drop was " +
	"not measured"

// QueryRegression is a guarded statement whose planned cost rose by Pct
// percent once the index it used was hidden.
type QueryRegression struct {
	QueryID int64   `json:"queryid"`
	Pct     float64 `json:"pct"`
}

// measurement is one planning pass over the workload: each planned
// statement's cost, the statements that could not be planned and, when the
// pass guards an index, the statements whose plan used it.
type measurement struct {
	costs     map[int64]float64
	unplanned []int64
	uses      map[int64]bool
}

// hiddenIndex is an existing index a session plans without.
type hiddenIndex struct {
	name string
	oid  uint32
}

// indexToHide resolves ref's current name from its OID, or the zero value
// when there is nothing to hide (no ref, or HypoPG cannot hide). The plans
// name the index, so the guard matches on this name.
func (s *hypopgSession) indexToHide(ctx context.Context, ref *IndexRef) (hiddenIndex, error) {
	if ref == nil || !s.canHide {
		return hiddenIndex{}, nil
	}
	if ref.OID == 0 {
		return hiddenIndex{}, fmt.Errorf("the identity (OID) of index %s is unknown, so "+
			"it cannot be hidden", ref.Name)
	}
	var name string
	err := s.tx.QueryRow(ctx, "SELECT relname FROM pg_catalog.pg_class WHERE oid=$1",
		ref.OID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return hiddenIndex{}, fmt.Errorf("index %s (OID %d) to hide no longer exists",
			ref.Name, ref.OID)
	}
	if err != nil {
		return hiddenIndex{}, fmt.Errorf("look up index %s (OID %d) to hide: %w",
			ref.Name, ref.OID, err)
	}
	return hiddenIndex{name: name, oid: ref.OID}, nil
}

// hide hides an existing index from this backend's planner. The hidden set
// is backend-local and not transactional: a failed hide leaves it in doubt,
// so the session's connection is discarded instead of pooled.
func (s *hypopgSession) hide(ctx context.Context, oid uint32) error {
	var hidden bool
	err := s.tx.QueryRow(ctx, "SELECT "+s.function("hypopg_hide_index")+"($1)", oid).
		Scan(&hidden)
	if err == nil && !hidden {
		err = fmt.Errorf("hypopg_hide_index returned false (not an index of this database)")
	}
	if err != nil {
		s.discard = true
		return fmt.Errorf("hide index OID %d: %w", oid, err)
	}
	return nil
}

// evaluateDrop plans the workload with ix and again with ix hidden.
func (s *hypopgSession) evaluateDrop(ctx context.Context, ix IndexRef, queries []QueryInfo,
) (WhatIfResult, error) {
	old, err := s.indexToHide(ctx, &ix)
	if err != nil {
		return WhatIfResult{}, err
	}
	before, err := s.measureCosts(ctx, queries, old.name)
	if err != nil || len(before.costs) == 0 {
		return WhatIfResult{Failed: len(before.unplanned),
			UnplannedQueryIDs: before.unplanned}, err
	}
	if err := s.hide(ctx, old.oid); err != nil {
		return WhatIfResult{}, err
	}
	after, err := s.measureCosts(ctx, queries, "")
	if err != nil {
		return WhatIfResult{}, err
	}
	res := comparedCosts(queries, before.costs, after.costs, before.unplanned)
	res.HiddenIndex = old.name
	res.Guarded, res.Regressions = guardedRegressions(queries, before, after.costs)
	return res, nil
}

// guardedRegressions counts the statements whose baseline plan used the
// hidden index and returns those whose planned cost rose more than
// maxGuardedRegressionPct without it, in workload order.
func guardedRegressions(queries []QueryInfo, before measurement, after map[int64]float64,
) (int, []QueryRegression) {
	var guarded int
	var regs []QueryRegression
	seen := make(map[int64]bool, len(queries))
	for _, q := range queries {
		if seen[q.QueryID] || !before.uses[q.QueryID] {
			continue
		}
		seen[q.QueryID] = true
		guarded++
		cost, next := before.costs[q.QueryID], after[q.QueryID]
		if _, ok := after[q.QueryID]; !ok || cost <= 0 {
			continue
		}
		if pct := (next - cost) * 100 / cost; pct > maxGuardedRegressionPct {
			regs = append(regs, QueryRegression{QueryID: q.QueryID, Pct: pct})
		}
	}
	return guarded, regs
}

// regressionReason names the regressed statements: "replacing items_a
// raises the planned cost of queryid 7 by 54.2%, 9 by 12.0% (...)".
func regressionReason(action string, res WhatIfResult) string {
	index := res.HiddenIndex
	if index == "" {
		index = "the index"
	}
	shown := res.Regressions[:min(len(res.Regressions), maxReasonQueryIDs)]
	parts := make([]string, len(shown))
	for i, r := range shown {
		parts[i] = fmt.Sprintf("%d by %.1f%%", r.QueryID, r.Pct)
	}
	list := strings.Join(parts, ", ")
	if rest := len(res.Regressions) - len(shown); rest > 0 {
		list += fmt.Sprintf(" and %d more", rest)
	}
	return fmt.Sprintf("%s %s raises the planned cost of queryid %s (a statement that "+
		"used it may rise at most %d%%)", action, index, list, maxGuardedRegressionPct)
}
