package retention

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/config"
)

// planTerminalStatuses are the plan statuses retention ages out; a draft,
// proposed, approved, running or halted plan is open and kept however old
// (plan-01a).
const planTerminalStatuses = `('completed', 'reverted', 'rejected', 'voided', 'cancelled')`

// planPurgeRule ages a terminal plan out with retention.actions_days on
// updated_at; its steps, edges and approvals cascade.
func planPurgeRule(r config.RetentionConfig) purgeRule {
	return purgeRule{table: "plan", timeCol: "updated_at", days: r.ActionsDays,
		extra: "AND status IN " + planTerminalStatuses}
}

// keepCitedPacks keeps a workload pack (evid-02a) that a plan cites: an
// open plan, or a terminal one younger than actions_days (a validated
// integer, never text; 0 keeps actions forever, so every citing plan
// keeps its pack). Probed through sage.plan's pack_hash index.
func keepCitedPacks(actionsDays int) string {
	young := ""
	if actionsDays > 0 {
		young = fmt.Sprintf(`
	                    OR p.updated_at >= now() - make_interval(days => %d)`, actionsDays)
	}
	return `AND NOT EXISTS (SELECT 1 FROM sage.plan p
	                WHERE p.pack_hash = workload_packs.content_sha256
	                  AND (p.status NOT IN ` + planTerminalStatuses + young + `))`
}
