package upgradecheck

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Findings turns each check that does not pass into an upgrade_readiness
// finding. A block is a warning (the upgrade would fail, nothing is down);
// warn and unknown are info. The identifier names the source, target and
// check, so the same check's next run updates or resolves its finding.
// No finding carries SQL: the case is advisory, pg_sage never acts on it.
func Findings(r Report) []analyzer.Finding {
	var out []analyzer.Finding
	for _, c := range r.Checks {
		if c.Status == Pass {
			continue
		}
		out = append(out, finding(r, c))
	}
	return out
}

func finding(r Report, c Check) analyzer.Finding {
	sev := "info"
	if c.Status == Block {
		sev = "warning"
	}
	upgrade := fmt.Sprintf("PostgreSQL %d to %d via %s", r.SourceMajor, r.Target.Major,
		r.Target.Method)
	rows := make([]any, len(c.Rows))
	for i, row := range c.Rows {
		m := make(map[string]any, len(row))
		for k, v := range row {
			m[k] = v
		}
		rows[i] = m
	}
	detail := map[string]any{"check": c.ID, "status": string(c.Status), "reason": c.Reason,
		"rows": rows, "total": c.Total, "source_major": r.SourceMajor,
		"target_major": r.Target.Major, "method": r.Target.Method,
		"provider": r.Target.Provider}
	if c.SQLState != "" {
		detail["sqlstate"] = c.SQLState
	}
	if len(c.Skipped) > 0 {
		detail["skipped"] = c.Skipped
	}
	return analyzer.Finding{Category: Category, Severity: sev, ObjectType: "server",
		ObjectIdentifier: upgrade + ": " + c.ID,
		Title: fmt.Sprintf("Upgrade to PostgreSQL %d: %s (%s)", r.Target.Major, c.Title,
			c.Status),
		Detail: detail, Recommendation: c.Reason}
}
