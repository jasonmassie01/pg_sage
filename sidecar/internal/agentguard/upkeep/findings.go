package upkeep

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// finding is one open finding this package keeps in step.
type finding struct {
	category, severity, objectType, ident string
	title, recommendation, sql            string
	detail                                any
}

const raiseSQL = `/* pg_sage agent_upkeep_finding v1 */
INSERT INTO sage.findings AS f (category, severity, object_type, object_identifier, title,
    detail, recommendation, recommended_sql, rollback_sql, status, last_seen,
    occurrence_count)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, '', 'open', now(), 1)
ON CONFLICT (category, object_identifier) WHERE status = 'open' DO UPDATE SET
    severity = EXCLUDED.severity, title = EXCLUDED.title, detail = EXCLUDED.detail,
    recommendation = EXCLUDED.recommendation,
    recommended_sql = EXCLUDED.recommended_sql, last_seen = now(),
    occurrence_count = f.occurrence_count + 1
WHERE f.detail IS DISTINCT FROM EXCLUDED.detail OR f.last_seen < now() - interval '1 hour'`

// raise opens or refreshes f in one database's sage schema.
func raise(ctx context.Context, q execer, f finding) error {
	detail, err := json.Marshal(f.detail)
	if err != nil {
		return fmt.Errorf("upkeep: encode finding %s: %w", f.ident, err)
	}
	if _, err := q.Exec(ctx, raiseSQL, f.category, f.severity, f.objectType, f.ident,
		f.title, detail, f.recommendation, f.sql); err != nil {
		return fmt.Errorf("upkeep: raise finding %s: %w", f.ident, err)
	}
	return nil
}

const resolveSQL = `/* pg_sage agent_upkeep_finding v1 */
UPDATE sage.findings SET status = 'resolved', resolved_at = now()
WHERE category = $1 AND object_identifier = $2 AND status = 'open'`

// resolve closes the open finding of category and ident, if any.
func resolve(ctx context.Context, q execer, category, ident string) error {
	if _, err := q.Exec(ctx, resolveSQL, category, ident); err != nil {
		return fmt.Errorf("upkeep: resolve finding %s: %w", ident, err)
	}
	return nil
}

const resolveOthersSQL = `/* pg_sage agent_upkeep_finding v1 */
UPDATE sage.findings SET status = 'resolved', resolved_at = now()
WHERE category = $1 AND status = 'open' AND object_identifier <> ALL($2::text[])`

// resolveOthers closes every open finding of category not in keep.
func resolveOthers(ctx context.Context, q execer, category string, keep []string) error {
	if keep == nil {
		keep = []string{}
	}
	if _, err := q.Exec(ctx, resolveOthersSQL, category, keep); err != nil {
		return fmt.Errorf("upkeep: resolve %s findings: %w", category, err)
	}
	return nil
}
