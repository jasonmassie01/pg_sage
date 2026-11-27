package analyzer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// SidecarCPUFinding is the one sage_self_budget finding the process watch
// raises when the sidecar process uses more CPU per collector cycle than
// self_budget.cpu_ms_per_cycle allows (footprint-07). The budget covers
// every monitored database together, so the finding is about the sidecar
// object, not a database; it names the busiest loops of local work.
func SidecarCPUFinding(b selfbudget.Breach, loops []selfbudget.LoopCost) Finding {
	return Finding{
		Category: categorySelfBudget, Severity: "warning",
		ObjectType: SidecarObject, ObjectIdentifier: SidecarObject,
		Title: "pg_sage process exceeded its own cpu budget " +
			"(all monitored databases together)",
		Detail: map[string]any{"breaches": []map[string]any{{
			"resource": string(b.Resource), "used": b.Used, "limit": b.Limit,
			"unit": b.Unit, "share": b.Share()}},
			"top_statements": statementRows(nil), "top_loops": loopRows(loops)},
		Recommendation: sidecarCPUAdvice(loops),
	}
}

func sidecarCPUAdvice(loops []selfbudget.LoopCost) string {
	busiest := ""
	if len(loops) > 0 {
		busiest = fmt.Sprintf("The busiest loop was %s (%.0f ms over %d runs, LLM "+
			"waits excluded). ", loops[0].Name, loops[0].BusyMs, loops[0].Runs)
	}
	return busiest + "top_loops shows which component on which database worked most. " +
		"Lengthen collector.interval_seconds or analyzer.interval_seconds, monitor " +
		"fewer databases from this sidecar, turn off components you do not use, or " +
		"raise self_budget.cpu_ms_per_cycle if this cost is expected."
}

// ResolveSidecarSelfBudget resolves pool's open process CPU finding and
// returns how many rows it resolved (0 when none was open).
func ResolveSidecarSelfBudget(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx,
		`/* pg_sage analyzer */ UPDATE sage.findings
		 SET status = 'resolved', resolved_at = now()
		 WHERE category = $1 AND object_type = $2 AND object_identifier = $2
		   AND status = 'open'`,
		categorySelfBudget, SidecarObject)
	if err != nil {
		return 0, fmt.Errorf("resolve the sidecar %s finding: %w", categorySelfBudget, err)
	}
	return tag.RowsAffected(), nil
}
