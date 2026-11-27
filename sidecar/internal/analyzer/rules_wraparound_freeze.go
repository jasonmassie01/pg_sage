package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// wraparoundSkippedSchemas are never diagnosed: pg_sage's own and other
// tool schemas the analyzer excludes everywhere. System catalogs are
// diagnosed (the custodian plans them for an operator).
var wraparoundSkippedSchemas = map[string]bool{"sage": true, "pgsnap": true,
	"google_ml": true}

// ruleWraparoundFreeze diagnoses tables that have used at least
// analyzer.wraparound_freeze_share of their effective freeze maximum (the
// lower of autovacuum_freeze_max_age and the table's reloption, XID or
// multixact), read by the database's freeze sampler: the analyzer, the
// freeze custodian and the SRE probe judge one horizon. It never proposes
// the VACUUM: the custodian is the only producer of freeze actions, and
// the finding links to its decision. A table past its maximum is
// critical (anti-wraparound autovacuum is due). Partitioned parents have
// no relfrozenxid and are never read.
func ruleWraparoundFreeze(
	_ *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	extras *RuleExtras,
) []Finding {
	if extras == nil || len(extras.FreezeHorizons) == 0 {
		return nil
	}
	threshold := cfg.Analyzer.WraparoundFreezeShare
	if !(threshold > 0 && threshold <= 1) {
		threshold = config.DefaultWraparoundFreezeShare
	}
	var findings []Finding
	for _, h := range extras.FreezeHorizons {
		used := h.Share()
		if used < threshold || wraparoundSkippedSchemas[h.Schema] {
			continue
		}
		severity := "warning"
		if used >= 1 {
			severity = "critical"
		}
		ident := h.Schema + "." + h.Table
		findings = append(findings, Finding{
			Category: "wraparound_freeze", Severity: severity,
			ObjectType: "table", ObjectIdentifier: ident,
			Title: fmt.Sprintf("Table %s has used %.0f%% of its freeze maximum",
				ident, used*100),
			Detail: map[string]any{
				"xid_age": h.XIDAge, "mxid_age": h.MultiXactAge,
				"freeze_max_age": h.XIDMaxAge, "mxid_freeze_max_age": h.MultiXactMaxAge,
				"share_used": used, "threshold_share": threshold,
				"catalog": h.Catalog, "via_toast": h.ViaTOAST,
				"custodian_decision": map[string]any{"feature": "freeze", "target": ident},
			},
			Recommendation: fmt.Sprintf("The freeze custodian decides the response for %s "+
				"(a VACUUM, or an operator plan for a catalog or a pinned horizon); see "+
				"its freeze decision for this table. This finding is diagnosis only.", ident),
		})
	}
	return findings
}
