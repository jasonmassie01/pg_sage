package analyzer

import (
	"context"
	"fmt"
	"strings"
)

// checkWorkMemPromotion emits a role-level work_mem promotion
// advisory finding per v0.8.5 plan §5 (Feature 3).
//
// Signal: for each role whose active query_hints contain a
// `Set(work_mem "NMB")` directive attached to five or more
// queries, emit a single `ALTER ROLE ... SET work_mem = '<max>MB'`
// recommendation. The threshold is
// cfg.Analyzer.WorkMemPromotionThreshold (default 5).
//
// Design choices from plan §5.2:
//   - Joins sage.query_hints to pg_stat_statements on queryid, and
//     pg_roles on s.userid. Using pg_roles (not pg_user) so NOLOGIN
//     roles inheriting from group roles are reachable (CHECK-W08).
//   - Groups on role only, selects max(hint_mb) as the target value,
//     since downgrading would re-trigger the per-query hints
//     (CHECK-W04 tiebreak by max).
//   - Regex ~ with capture form survives hint_text containing other
//     Set directives around the work_mem one.
//   - Role name passed through QuoteIdentifier so reserved words
//     and mixed-case names quote correctly (CHECK-W07, W09).
//
// This is advisory-only: the executor never applies ALTER ROLE
// autonomously in v0.8.5 because role-level changes have a broader
// blast radius than the tuner's per-query surface.
func (a *Analyzer) checkWorkMemPromotion(ctx context.Context) []Finding {
	if a.pool == nil {
		return nil
	}
	threshold := a.cfg.Analyzer.WorkMemPromotionThreshold
	if threshold <= 0 {
		return nil
	}
	rows, err := a.pool.Query(ctx, `/* pg_sage */ 
SELECT
    r.rolname AS role_name,
    count(*)  AS hint_count,
    max((regexp_match(h.hint_text, 'Set\(work_mem "(\d+)MB"\)'))[1]::int) AS max_mb
FROM sage.query_hints h
JOIN pg_stat_statements s USING (queryid)
JOIN pg_roles r ON r.oid = s.userid
WHERE h.status = 'active'
  AND h.hint_text ~ 'Set\(work_mem "\d+MB"\)'
GROUP BY r.rolname
HAVING count(*) >= $1
ORDER BY r.rolname`, threshold)
	if err != nil {
		a.logFn("WARN", "analyzer",
			"work_mem promotion query: %v", err)
		return nil
	}
	defer rows.Close()

	var findings []Finding
	for rows.Next() {
		var role string
		var hintCount, maxMB int
		if err := rows.Scan(&role, &hintCount, &maxMB); err != nil {
			a.logFn("WARN", "analyzer",
				"work_mem promotion scan: %v", err)
			continue
		}
		findings = append(findings,
			buildWorkMemPromotionFinding(role, hintCount, maxMB, threshold))
	}
	if err := rows.Err(); err != nil {
		a.logFn("WARN", "analyzer",
			"work_mem promotion rows: %v", err)
	}
	return findings
}

// buildWorkMemPromotionFinding assembles the advisory finding for
// one (role, max_mb, hint_count) tuple. Pure function, no I/O, so
// it can be unit-tested without a database.
func buildWorkMemPromotionFinding(
	role string, hintCount, maxMB, threshold int,
) Finding {
	quoted := quoteRoleIdentifier(role)
	title := fmt.Sprintf(
		"Role %s: %d queries carry work_mem hints — consider role-level promotion",
		role, hintCount,
	)
	rec := fmt.Sprintf(
		"%d active queries running as role %q currently carry a "+
			"Set(work_mem \"NMB\") hint. Promoting work_mem at the "+
			"role level via ALTER ROLE ... SET work_mem = '%dMB' "+
			"avoids per-query hint maintenance and makes the larger "+
			"buffer available to every future query for this role. "+
			"The recommended value is the MAX of all observed hint "+
			"sizes because downgrading would cause the tuner to "+
			"re-install the hints on next cycle. Review the affected "+
			"queries in sage.query_hints before applying — this is "+
			"advisory only and is not auto-executed.",
		hintCount, role, maxMB,
	)
	sql := fmt.Sprintf(
		"ALTER ROLE %s SET work_mem = '%dMB'", quoted, maxMB,
	)
	rollback := fmt.Sprintf(
		"ALTER ROLE %s RESET work_mem", quoted,
	)
	return Finding{
		Category:         "work_mem_promotion",
		Severity:         "info",
		ObjectType:       "role",
		ObjectIdentifier: role,
		Title:            title,
		Detail: map[string]any{
			"role_name":          role,
			"hint_count":         hintCount,
			"threshold":          threshold,
			"suggested_work_mem": fmt.Sprintf("%dMB", maxMB),
			"max_mb":             maxMB,
		},
		Recommendation: rec,
		RecommendedSQL: sql,
		RollbackSQL:    rollback,
		ActionRisk:     "moderate",
	}
}

// quoteRoleIdentifier wraps a role name in double quotes and
// doubles any embedded double quote. Kept local to the analyzer
// package to avoid pulling the tuner package just for a one-line
// helper (the tuner's QuoteIdentifier has the same semantics).
func quoteRoleIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
