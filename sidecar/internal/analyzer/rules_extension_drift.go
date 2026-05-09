package analyzer

import (
	"context"
	"fmt"
)

// checkExtensionDrift emits one finding per installed extension
// whose installed_version lags behind default_version. This is
// commonly a pg_stat_statements / pgvector / postgis minor bump
// after a PostgreSQL point release, which won't take effect
// until an operator runs ALTER EXTENSION ... UPDATE.
//
// The finding is advisory-only; executor does not auto-apply
// extension updates because they can require connection restarts
// and function signature changes.
func (a *Analyzer) checkExtensionDrift(ctx context.Context) []Finding {
	rows, err := a.pool.Query(ctx, `
SELECT e.extname,
       e.extversion        AS installed_version,
       ae.default_version  AS default_version
FROM pg_extension e
JOIN pg_available_extensions ae ON ae.name = e.extname
WHERE ae.default_version IS NOT NULL
  AND e.extversion IS DISTINCT FROM ae.default_version
ORDER BY e.extname`)
	if err != nil {
		a.logFn("WARN", "analyzer", "extension drift query: %v", err)
		return nil
	}
	defer rows.Close()

	var findings []Finding
	for rows.Next() {
		var name, installed, defaultVer string
		if err := rows.Scan(&name, &installed, &defaultVer); err != nil {
			a.logFn("WARN",
				"analyzer", "extension drift scan: %v", err)
			continue
		}
		findings = append(findings, Finding{
			Category:         "extension_drift",
			Severity:         "warning",
			ObjectType:       "extension",
			ObjectIdentifier: name,
			Title: fmt.Sprintf(
				"Extension %s can be updated from %s to %s",
				name, installed, defaultVer,
			),
			Detail: map[string]any{
				"extension":         name,
				"installed_version": installed,
				"default_version":   defaultVer,
			},
			Recommendation: "Review extension release notes, then run ALTER EXTENSION during a maintenance window if appropriate.",
		})
	}
	if err := rows.Err(); err != nil {
		a.logFn("WARN",
			"analyzer", "extension drift rows: %v", err)
	}
	return findings
}
