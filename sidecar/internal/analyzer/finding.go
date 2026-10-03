package analyzer

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

const actionResolutionReopenGrace = "2 minutes"

// DetailApprovalRequired is the Detail key a producer sets (to a
// human-readable reason) when a finding's SQL may run only with operator
// approval, never unattended: e.g. an index drop while standby usage is
// unknown, or a restart-required setting. The executor enforces it.
const DetailApprovalRequired = "approval_required"

// Finding represents a single diagnostic finding from the rules engine.
type Finding struct {
	Category         string
	Severity         string // "info", "warning", "critical"
	ObjectType       string
	ObjectIdentifier string
	Title            string
	Detail           map[string]any
	Recommendation   string
	RecommendedSQL   string
	RollbackSQL      string
	ActionRisk       string // "safe", "moderate", "high_risk"
	DatabaseName     string // populated by fleet manager, not persisted
	// RuleID and ImpactScore are optional and only populated by the
	// schema lint subsystem (category = "schema_lint:<rule_id>"). They
	// persist to sage.findings.rule_id / impact_score. Zero values are
	// written as SQL NULL.
	RuleID      string
	ImpactScore float64
}

func isSelfMonitoringFinding(f Finding) bool {
	return selfmonitor.IsFinding(selfmonitor.FindingFields{
		ObjectIdentifier: f.ObjectIdentifier,
		Title:            f.Title,
		Detail:           f.Detail,
		RecommendedSQL:   f.RecommendedSQL,
		RollbackSQL:      f.RollbackSQL,
	})
}

func recentlyResolvedByAction(
	ctx context.Context,
	pool *pgxpool.Pool,
	category, objectIdentifier string,
) (bool, error) {
	var one int
	err := pool.QueryRow(ctx,
		`/* pg_sage */ SELECT 1 FROM sage.findings
		  WHERE category = $1
		    AND object_identifier = $2
		    AND status = 'resolved'
		    AND action_log_id IS NOT NULL
		    AND resolved_at > now() - ($3::text)::interval
		  LIMIT 1`,
		category, objectIdentifier, actionResolutionReopenGrace,
	).Scan(&one)
	if err == nil {
		return true, nil
	}
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return false, err
}

// ResolveCleared marks open findings as resolved when they are no longer
// present in the active set for a given category.
func ResolveCleared(
	ctx context.Context,
	pool *pgxpool.Pool,
	activeIdentifiers map[string]bool,
	category string,
) error {
	rows, err := pool.Query(ctx,
		`/* pg_sage */ SELECT id, object_identifier FROM sage.findings
		 WHERE category = $1 AND status = 'open'`,
		category,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	var toResolve []string
	for rows.Next() {
		var id, ident string
		if err := rows.Scan(&id, &ident); err != nil {
			return err
		}
		if !activeIdentifiers[ident] {
			toResolve = append(toResolve, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range toResolve {
		_, err := pool.Exec(ctx,
			`/* pg_sage */ UPDATE sage.findings
			 SET status = 'resolved', resolved_at = now()
			 WHERE id = $1`,
			id,
		)
		if err != nil {
			return err
		}
	}
	return nil
}
