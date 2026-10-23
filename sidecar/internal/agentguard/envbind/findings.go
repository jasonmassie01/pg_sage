package envbind

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// FindingCategory is the critical finding of a binding that no longer
// holds (G1-04b): a re-pointed DSN, a physical database under two labels,
// a branch without its receipt.
const FindingCategory = "agent_environment_binding"

const raiseFindingSQL = `/* pg_sage agent_env_finding v1 */
INSERT INTO sage.findings AS f (category, severity, object_type, object_identifier, title,
    detail, recommendation, recommended_sql, rollback_sql, status, last_seen,
    occurrence_count)
VALUES ($1, 'critical', 'database', $2, $3, $4, $5, '', '', 'open', now(), 1)
ON CONFLICT (category, object_identifier) WHERE status = 'open' DO UPDATE SET
    title = EXCLUDED.title, detail = EXCLUDED.detail,
    recommendation = EXCLUDED.recommendation, last_seen = now(),
    occurrence_count = f.occurrence_count + 1
WHERE f.detail IS DISTINCT FROM EXCLUDED.detail OR f.last_seen < now() - interval '1 hour'`

const resolveFindingSQL = `/* pg_sage agent_env_finding v1 */
UPDATE sage.findings SET status = 'resolved', resolved_at = now()
WHERE category = $1 AND object_identifier = $2 AND status = 'open'`

type findingDetail struct {
	Database  string    `json:"database"`
	Label     Env       `json:"label"`
	Effective Env       `json:"effective"`
	Reasons   []string  `json:"reasons"`
	Changed   []string  `json:"changed,omitempty"`
	Live      Identity  `json:"live"`
	Snapshot  *Identity `json:"snapshot,omitempty"`
	Conflicts []Peer    `json:"conflicts,omitempty"`
}

// raiseFinding opens (or refreshes) the database's critical finding in its
// own sage schema, keyed by database_id.
func raiseFinding(ctx context.Context, db Database, ev Evidence) error {
	detail, err := json.Marshal(findingDetail{Database: db.Name, Label: ev.Label,
		Effective: ev.Effective, Reasons: ev.Reasons, Changed: ev.Changed, Live: ev.Live,
		Snapshot: ev.Snapshot, Conflicts: ev.Conflicts})
	if err != nil {
		return fmt.Errorf("encode binding finding of %s: %w", db.Name, err)
	}
	title := fmt.Sprintf("Agent environment of %s is evaluated as prod: %s", db.Name,
		strings.Join(ev.Reasons, ", "))
	rec := "Agents are held to prod rules on this database until an admin confirms its " +
		"identity. If the connection was re-pointed or this is a copy or standby of " +
		"another database, keep it prod; otherwise set its label again " +
		"(PUT /api/v1/agent-environments/" + db.Name + ")."
	if _, err := db.Pool.Exec(ctx, raiseFindingSQL, FindingCategory, db.ID, title, detail,
		rec); err != nil {
		return fmt.Errorf("raise binding finding of %s: %w", db.Name, err)
	}
	return nil
}

func resolveFinding(ctx context.Context, db Database) error {
	if _, err := db.Pool.Exec(ctx, resolveFindingSQL, FindingCategory, db.ID); err != nil {
		return fmt.Errorf("resolve binding finding of %s: %w", db.Name, err)
	}
	return nil
}
