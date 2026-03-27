package optimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
)

// PlanCapture obtains execution plans for queries via multiple strategies.
type PlanCapture struct {
	pool                 *pgxpool.Pool
	pgVersionNum         int
	extensionPresent     bool
	autoExplainAvailable bool
	preferredSource      string
	logFn                func(string, string, ...any)
}

// NewPlanCapture creates a PlanCapture.
func NewPlanCapture(
	pool *pgxpool.Pool,
	pgVersionNum int,
	extensionPresent bool,
	autoExplainAvailable bool,
	preferredSource string,
	logFn func(string, string, ...any),
) *PlanCapture {
	return &PlanCapture{
		pool:                 pool,
		pgVersionNum:         pgVersionNum,
		extensionPresent:     extensionPresent,
		autoExplainAvailable: autoExplainAvailable,
		preferredSource:      preferredSource,
		logFn:                logFn,
	}
}

// CapturePlans tries to get plans for the given queries.
// Returns plans and the source used.
func (p *PlanCapture) CapturePlans(
	ctx context.Context,
	queries []collector.QueryStats,
) ([]PlanSummary, string) {
	if p.preferredSource == "none" {
		return nil, "none"
	}

	// Strategy 1: Extension's explain_cache.
	if p.extensionPresent && p.preferredSource != "generic_plan" {
		plans := p.fromExplainCache(ctx, queries)
		if len(plans) > 0 {
			return plans, "explain_cache"
		}
	}

	// Strategy 1.5: auto_explain cached plans.
	if p.autoExplainAvailable {
		plans := p.fromAutoExplain(ctx, queries)
		if len(plans) > 0 {
			return plans, "auto_explain"
		}
	}

	// Strategy 2: GENERIC_PLAN (PG16+).
	if p.pgVersionNum >= 160000 {
		plans := p.fromGenericPlan(ctx, queries)
		if len(plans) > 0 {
			return plans, "generic_plan"
		}
	}

	// Strategy 3: No plans available.
	return nil, "query_text_only"
}

func (p *PlanCapture) fromExplainCache(
	ctx context.Context,
	queries []collector.QueryStats,
) []PlanSummary {
	var plans []PlanSummary
	for _, q := range queries {
		var planJSON []byte
		err := p.pool.QueryRow(ctx,
			`SELECT plan_json FROM sage.explain_cache
			 WHERE queryid = $1 ORDER BY captured_at DESC LIMIT 1`,
			q.QueryID).Scan(&planJSON)
		if err != nil {
			continue
		}
		ps := summarizePlan(planJSON, q.QueryID)
		if ps.ScanType != "" {
			plans = append(plans, ps)
		}
	}
	return plans
}

func (p *PlanCapture) fromAutoExplain(
	ctx context.Context,
	queries []collector.QueryStats,
) []PlanSummary {
	var plans []PlanSummary
	for _, q := range queries {
		var planJSON []byte
		err := p.pool.QueryRow(ctx,
			`SELECT plan_json FROM sage.explain_cache
			 WHERE source = 'auto_explain' AND queryid = $1
			 ORDER BY captured_at DESC LIMIT 1`,
			q.QueryID).Scan(&planJSON)
		if err != nil {
			continue
		}
		ps := summarizePlan(planJSON, q.QueryID)
		if ps.ScanType != "" {
			plans = append(plans, ps)
		}
	}
	return plans
}

func (p *PlanCapture) fromGenericPlan(
	ctx context.Context,
	queries []collector.QueryStats,
) []PlanSummary {
	var plans []PlanSummary
	for _, q := range queries {
		query := q.Query
		if strings.Contains(query, "$1") {
			// GENERIC_PLAN works with parameterized queries.
			var planJSON []byte
			err := p.pool.QueryRow(ctx,
				fmt.Sprintf(
					"EXPLAIN (GENERIC_PLAN, FORMAT JSON) %s",
					query,
				),
			).Scan(&planJSON)
			if err != nil {
				continue
			}
			ps := summarizePlan(planJSON, q.QueryID)
			if ps.ScanType != "" {
				plans = append(plans, ps)
			}
		}
	}
	return plans
}

// summarizePlan extracts key info from EXPLAIN JSON output.
func summarizePlan(planJSON []byte, queryID int64) PlanSummary {
	ps := PlanSummary{QueryID: queryID}

	// EXPLAIN FORMAT JSON wraps in an array.
	var wrapper []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &wrapper); err != nil || len(wrapper) == 0 {
		return ps
	}

	var plan struct {
		NodeType      string  `json:"Node Type"`
		TotalCost     float64 `json:"Total Cost"`
		PlanRows      int64   `json:"Plan Rows"`
		ActualRows    *int64  `json:"Actual Rows"`
		HeapFetches   *int64  `json:"Heap Fetches"`
		SortMethod    *string `json:"Sort Method"`
		SortSpaceUsed *int64  `json:"Sort Space Used"`
		SortSpaceType *string `json:"Sort Space Type"`
		RowsRemoved   *int64  `json:"Rows Removed by Filter"`
	}
	if err := json.Unmarshal(wrapper[0].Plan, &plan); err != nil {
		return ps
	}

	ps.ScanType = plan.NodeType
	if plan.HeapFetches != nil {
		ps.HeapFetches = *plan.HeapFetches
	}
	if plan.SortSpaceType != nil &&
		*plan.SortSpaceType == "Disk" &&
		plan.SortSpaceUsed != nil {
		ps.SortDisk = *plan.SortSpaceUsed
	}
	if plan.RowsRemoved != nil {
		ps.RowsRemoved = *plan.RowsRemoved
	}

	// Build summary string (~200 bytes max).
	var parts []string
	parts = append(parts, plan.NodeType)
	if plan.RowsRemoved != nil && *plan.RowsRemoved > 0 {
		parts = append(parts,
			fmt.Sprintf("Rows Removed: %d", *plan.RowsRemoved))
	}
	if ps.HeapFetches > 0 {
		parts = append(parts,
			fmt.Sprintf("Heap Fetches: %d", ps.HeapFetches))
	}
	if ps.SortDisk > 0 {
		parts = append(parts,
			fmt.Sprintf("Sort Disk: %dkB", ps.SortDisk))
	}
	ps.Summary = strings.Join(parts, " → ")
	if len(ps.Summary) > 200 {
		ps.Summary = ps.Summary[:197] + "..."
	}

	return ps
}
