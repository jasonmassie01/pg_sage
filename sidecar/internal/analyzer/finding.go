package analyzer

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
)

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
			// Existing open finding — bump count and refresh.
			_, err = pool.Exec(ctx,
				`UPDATE sage.findings
				 SET last_seen = now(),
				     occurrence_count = occurrence_count + 1,
				     detail = $1,
				     severity = $2
				 WHERE id = $3`,
				detailJSON, f.Severity, existingID,
			)
			if err != nil {
				return err
			}
			continue
		}

		// Insert new finding.
		_, err = pool.Exec(ctx,
			`INSERT INTO sage.findings
			 (category, severity, object_type, object_identifier,
			  title, detail, recommendation, recommended_sql,
			  rollback_sql, status, last_seen, occurrence_count)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'open',now(),1)`,
			f.Category, f.Severity, f.ObjectType, f.ObjectIdentifier,
			f.Title, detailJSON, f.Recommendation, f.RecommendedSQL,
			f.RollbackSQL,
		)
		if err != nil {
			return err
		}
	}
	return nil
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
