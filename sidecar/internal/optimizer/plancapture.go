package optimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// PlanCapture obtains execution plans for queries via multiple strategies.
type PlanCapture struct {
	pool                 *pgxpool.Pool
	pgVersionNum         int
	autoExplainAvailable bool
	providerScope        func() string
	providerRetention    time.Duration
	preferredSource      string
	logFn                func(string, string, ...any)
}

// NewPlanCapture creates a PlanCapture.
func NewPlanCapture(
	pool *pgxpool.Pool,
	pgVersionNum int,
	autoExplainAvailable bool,
	preferredSource string,
	logFn func(string, string, ...any),
) *PlanCapture {
	return &PlanCapture{
		pool:                 pool,
		pgVersionNum:         pgVersionNum,
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

	// Resource-bound provider plans remain useful when session auto_explain is
	// unavailable. Keep their source and capture flags distinct from estimates.
	if p.providerScope != nil {
		if scope := p.providerScope(); scope != "" {
			if plans := p.fromLakebase(ctx, queries, scope); len(plans) > 0 {
				return plans, "lakebase_plan_history"
			}
		}
	}

	// Strategy 1: auto_explain cached plans.
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

func (p *PlanCapture) fromAutoExplain(
	ctx context.Context,
	queries []collector.QueryStats,
) []PlanSummary {
	var plans []PlanSummary
	for _, q := range queries {
		var planJSON []byte
		err := p.pool.QueryRow(ctx,
			`/* pg_sage */ SELECT plan_json FROM sage.explain_cache
			 WHERE source IN ('auto_explain_log', 'auto_explain') AND queryid = $1
			 ORDER BY (source = 'auto_explain_log') DESC, captured_at DESC LIMIT 1`,
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
			if err := sanitize.RejectMultiStatement(query); err != nil {
				continue
			}
			// GENERIC_PLAN works with parameterized queries.
			planJSON, err := p.genericPlan(ctx, query)
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

func (p *PlanCapture) genericPlan(ctx context.Context, query string) ([]byte, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	// pgx parameter interpolation rejects intentionally unbound $n placeholders. Send the
	// validated single EXPLAIN directly and drain every result before returning the session.
	results, err := conn.Conn().PgConn().Exec(ctx,
		"EXPLAIN (GENERIC_PLAN, FORMAT JSON) "+query).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 1 {
		return nil, fmt.Errorf("generic EXPLAIN returned no single JSON plan")
	}
	return results[0].Rows[0][0], nil
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
		NodeType      string   `json:"Node Type"`
		TotalCost     float64  `json:"Total Cost"`
		PlanRows      int64    `json:"Plan Rows"`
		ActualRows    *float64 `json:"Actual Rows"` // PG18: 2 decimals
		HeapFetches   *int64   `json:"Heap Fetches"`
		SortMethod    *string  `json:"Sort Method"`
		SortSpaceUsed *int64   `json:"Sort Space Used"`
		SortSpaceType *string  `json:"Sort Space Type"`
		RowsRemoved   *int64   `json:"Rows Removed by Filter"`
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

func (p *PlanCapture) fromLakebase(ctx context.Context, queries []collector.QueryStats, scope string) []PlanSummary {
	var plans []PlanSummary
	for _, q := range queries {
		var raw []byte
		var captured time.Time
		var compute, durationSource string
		var analyzed, buffers, timing *bool
		err := p.pool.QueryRow(ctx, `/* pg_sage */ SELECT plan_json,captured_at,provider_compute_id,analyze_enabled,buffers_enabled,timing_enabled,duration_source
 FROM sage.explain_cache WHERE source='lakebase_plan_history' AND provider_scope_hash=$1 AND queryid=$2 AND captured_at>now()-make_interval(secs=>$3)
 ORDER BY captured_at DESC,id DESC LIMIT 1`, scope, q.QueryID, p.lakebasePlanRetention().Seconds()).Scan(&raw, &captured, &compute, &analyzed, &buffers, &timing, &durationSource)
		if err != nil {
			continue
		}
		ps := summarizePlan(raw, q.QueryID)
		if ps.ScanType == "" {
			continue
		}
		ps.Source = "lakebase_plan_history"
		ps.CapturedAt = captured
		ps.ComputeID = compute
		ps.AnalyzeEnabled = analyzed
		ps.BuffersEnabled = buffers
		ps.TimingEnabled = timing
		ps.DurationSource = durationSource
		plans = append(plans, ps)
	}
	return plans
}

func (p *PlanCapture) lakebasePlanRetention() time.Duration {
	if p.providerRetention > 0 && p.providerRetention < 7*24*time.Hour {
		return p.providerRetention
	}
	return 7 * 24 * time.Hour
}
