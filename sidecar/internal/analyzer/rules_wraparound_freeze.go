package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// criticalFreezeXIDAge is the absolute transaction age at which a table's
// wraparound risk is critical. It matches PostgreSQL's default
// autovacuum_freeze_max_age, past which autovacuum force-runs anyway.
const criticalFreezeXIDAge = 200_000_000

// ruleWraparoundFreeze recommends VACUUM (FREEZE) on tables whose oldest
// transaction age is approaching XID-wraparound territory. Freezing is a
// SAFE maintenance action (no rollback) and prevents the single most
// catastrophic Postgres failure mode. This is the auto-fireable half of
// A6; dropping abandoned replication slots (the other wraparound cause)
// stays manual because it is irreversible.
func ruleWraparoundFreeze(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	if current == nil {
		return nil
	}
	threshold := int64(cfg.Analyzer.WraparoundFreezeXIDAge)
	if threshold <= 0 {
		threshold = 150_000_000
	}

	var findings []Finding
	for _, t := range current.Tables {
		if t.XIDAge < threshold {
			continue
		}
		severity := "warning"
		if t.XIDAge >= criticalFreezeXIDAge {
			severity = "critical"
		}
		ident := t.SchemaName + "." + t.RelName
		q := sanitize.QuoteQualifiedName(t.SchemaName, t.RelName)
		findings = append(findings, Finding{
			Category:         "wraparound_freeze",
			Severity:         severity,
			ObjectType:       "table",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Table %s transaction age is %d — freeze to avoid wraparound",
				ident, t.XIDAge),
			Detail: map[string]any{
				"xid_age":      t.XIDAge,
				"threshold":    threshold,
				"critical_age": criticalFreezeXIDAge,
			},
			Recommendation: fmt.Sprintf(
				"Run VACUUM (FREEZE) on %s to reset its transaction age and "+
					"prevent XID wraparound. VACUUM is safe maintenance.",
				ident),
			RecommendedSQL: fmt.Sprintf("VACUUM (FREEZE) %s;", q),
			ActionRisk:     "safe",
		})
	}
	return findings
}
