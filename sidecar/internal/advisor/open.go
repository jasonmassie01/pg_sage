package advisor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
)

// advisorCategories are the finding categories the sub-advisors own.
var advisorCategories = []string{
	"wal_tuning", "connection_tuning", "query_rewrite", "bloat_remediation",
}

// openFindingStore reads the open findings of the given categories.
type openFindingStore interface {
	OpenFindings(ctx context.Context, categories []string) ([]analyzer.Finding, error)
}

// noOpenFindings is the store without a database: nothing is persisted,
// so nothing is open.
type noOpenFindings struct{}

func (noOpenFindings) OpenFindings(context.Context, []string) ([]analyzer.Finding, error) {
	return nil, nil
}

type pgOpenFindings struct{ pool *pgxpool.Pool }

// openFindingsSQL reads every open row, acted on or not: each is
// re-checked, and a row this cycle does not re-emit would be resolved.
const openFindingsSQL = `/* pg_sage */
SELECT category, severity, COALESCE(object_type, ''), COALESCE(object_identifier, ''),
       title, detail, COALESCE(recommendation, ''), COALESCE(recommended_sql, ''),
       COALESCE(rollback_sql, '')
FROM sage.findings
WHERE status = 'open' AND category = ANY($1)
ORDER BY id`

func (s pgOpenFindings) OpenFindings(ctx context.Context, categories []string) (
	[]analyzer.Finding, error) {
	rows, err := s.pool.Query(ctx, openFindingsSQL, categories)
	if err != nil {
		return nil, fmt.Errorf("read open advisor findings: %w", err)
	}
	defer rows.Close()
	var out []analyzer.Finding
	for rows.Next() {
		var f analyzer.Finding
		if err := rows.Scan(&f.Category, &f.Severity, &f.ObjectType, &f.ObjectIdentifier,
			&f.Title, &f.Detail, &f.Recommendation, &f.RecommendedSQL,
			&f.RollbackSQL); err != nil {
			return nil, fmt.Errorf("scan open advisor finding: %w", err)
		}
		f.ActionRisk = reemitRisk(f.RecommendedSQL)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read open advisor findings: %w", err)
	}
	return out, nil
}

// reemitRisk is the action risk a re-emitted open finding carries. Risk is
// not stored; every advisor statement is a setting change (ALTER SYSTEM,
// or ALTER DATABASE after the managed rewrite), which the advisor rates
// moderate. Re-emitting it never lowers that rating.
func reemitRisk(sql string) string {
	if sql == "" {
		return ""
	}
	return "moderate"
}
