package analyzer

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const actionResolutionReopenGrace = "2 minutes"

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

// UpsertFindings persists a batch of findings, incrementing occurrence_count
// for existing open findings and inserting new ones.
func UpsertFindings(ctx context.Context, pool *pgxpool.Pool, findings []Finding) error {
	for _, f := range findings {
		detailJSON, err := json.Marshal(f.Detail)
		if err != nil {
			return err
		}

		var existingID string
		var count int
		err = pool.QueryRow(ctx,
			`SELECT id, occurrence_count FROM sage.findings
			 WHERE category = $1 AND object_identifier = $2 AND status = 'open'`,
			f.Category, f.ObjectIdentifier,
		).Scan(&existingID, &count)

		if err == nil {
			// Existing open finding — bump count and refresh every
			// user-visible field. Titles, recommendations, and SQL
			// snippets can legitimately change between scans (the lint
			// subsystem re-computes impact estimates; tier-1 rules
			// re-compute recommendation text), so overwriting is
			// correct. rule_id / impact_score are only refreshed when
			// the caller supplied a non-empty / non-zero value.
			_, err = pool.Exec(ctx,
				`UPDATE sage.findings
				 SET last_seen = now(),
				     occurrence_count = occurrence_count + 1,
				     detail = $1,
				     severity = $2,
				     title = $3,
				     recommendation = $4,
				     recommended_sql = $5,
				     rule_id = COALESCE(NULLIF($6, ''), rule_id),
				     impact_score = CASE
				         WHEN $7 <> 0 THEN $7::real
				         ELSE impact_score END
				 WHERE id = $8`,
				detailJSON, f.Severity, f.Title,
				f.Recommendation, f.RecommendedSQL,
				f.RuleID, f.ImpactScore, existingID,
			)
			if err != nil {
				return err
			}
			continue
		}
		if err != pgx.ErrNoRows {
			return err
		}
		recentlyResolved, err := recentlyResolvedByAction(
			ctx, pool, f.Category, f.ObjectIdentifier)
		if err != nil {
			return err
		}
		if recentlyResolved {
			continue
		}

		// Insert new finding. Pass rule_id/impact_score as NULL when
		// unset so non-lint findings (the common case) don't populate
		// these columns.
		var ruleID any
		if f.RuleID != "" {
			ruleID = f.RuleID
		}
		var impactScore any
		if f.ImpactScore != 0 {
			impactScore = f.ImpactScore
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO sage.findings
			 (category, severity, object_type, object_identifier,
			  title, detail, recommendation, recommended_sql,
			  rollback_sql, status, last_seen, occurrence_count,
			  rule_id, impact_score)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'open',now(),1,
			         $10,$11)`,
			f.Category, f.Severity, f.ObjectType, f.ObjectIdentifier,
			f.Title, detailJSON, f.Recommendation, f.RecommendedSQL,
			f.RollbackSQL, ruleID, impactScore,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func recentlyResolvedByAction(
	ctx context.Context,
	pool *pgxpool.Pool,
	category, objectIdentifier string,
) (bool, error) {
	var one int
	err := pool.QueryRow(ctx,
		`SELECT 1 FROM sage.findings
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
		`SELECT id, object_identifier FROM sage.findings
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
			`UPDATE sage.findings
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
