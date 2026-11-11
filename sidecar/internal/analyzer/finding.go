package analyzer

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// DetailApprovalRequired is the Detail key a producer sets (to a
// human-readable reason) when a finding's SQL may run only with operator
// approval, never unattended: e.g. an index drop while standby usage is
// unknown, or a restart-required setting. The executor enforces it.
const DetailApprovalRequired = "approval_required"

// DetailActionRisk is the Detail key the upsert stores the producer's
// ActionRisk under, so readers of sage.findings can show it (rules-07).
const DetailActionRisk = "action_risk"

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
		Category:         f.Category,
		ObjectType:       f.ObjectType,
		ObjectIdentifier: f.ObjectIdentifier,
		Title:            f.Title,
		Detail:           f.Detail,
		RecommendedSQL:   f.RecommendedSQL,
		RollbackSQL:      f.RollbackSQL,
	})
}

// ResolveCleared marks open findings as resolved when they are no longer
// present in the active set for a given category.
func ResolveCleared(
	ctx context.Context,
	pool *pgxpool.Pool,
	activeIdentifiers map[string]bool,
	category string,
) error {
	_, err := ResolveClearedFindings(ctx, pool, activeIdentifiers, category)
	return err
}

// ClearedFinding identifies a finding that ResolveClearedFindings moved
// from open to resolved, with the severity and title it had then.
type ClearedFinding struct {
	Category         string
	ObjectIdentifier string
	Severity         string
	Title            string
}

// ResolveClearedFindings is ResolveCleared returning the findings it
// resolved, so the analyzer can resolve their pages (alerting-01). Each
// row is resolved only while still open, so concurrent resolvers report
// a finding once. A failed UPDATE does not stop the rest: the findings
// resolved are returned with the joined errors (rules-06). A lost
// connection does stop them.
func ResolveClearedFindings(
	ctx context.Context,
	pool *pgxpool.Pool,
	activeIdentifiers map[string]bool,
	category string,
) ([]ClearedFinding, error) {
	ids, err := clearedFindingIDs(ctx, pool, activeIdentifiers, category)
	if err != nil {
		return nil, fmt.Errorf("list open %s findings: %w", category, err)
	}
	var cleared []ClearedFinding
	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("resolve %s findings stopped: %w",
				category, err))
			break
		}
		var c ClearedFinding
		err := pool.QueryRow(ctx,
			`/* pg_sage analyzer */ UPDATE sage.findings
			 SET status = 'resolved', resolved_at = now()
			 WHERE id = $1 AND status = 'open'
			 RETURNING category, COALESCE(object_identifier, ''),
			           severity, title`,
			id,
		).Scan(&c.Category, &c.ObjectIdentifier, &c.Severity, &c.Title)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // resolved concurrently by another caller
		}
		if err != nil {
			// Keep going: one bad row must not keep the rest open.
			errs = append(errs, fmt.Errorf("resolve %s finding %s: %w",
				category, id, err))
			if connectionLost(err) {
				break // every later row would wait out the same timeout
			}
			continue
		}
		cleared = append(cleared, c)
	}
	return cleared, errors.Join(errs...)
}

// clearedFindingIDs lists the open findings of category whose identity
// is not in activeIdentifiers.
func clearedFindingIDs(
	ctx context.Context,
	pool *pgxpool.Pool,
	activeIdentifiers map[string]bool,
	category string,
) ([]string, error) {
	rows, err := pool.Query(ctx,
		`/* pg_sage analyzer */ SELECT id, object_identifier FROM sage.findings
		 WHERE category = $1 AND status = 'open'
		 ORDER BY id`,
		category,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var toResolve []string
	for rows.Next() {
		var id, ident string
		if err := rows.Scan(&id, &ident); err != nil {
			return nil, err
		}
		if !activeIdentifiers[ident] {
			toResolve = append(toResolve, id)
		}
	}
	return toResolve, rows.Err()
}
