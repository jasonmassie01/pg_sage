package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Change reviews waiting for a DBA verdict are cases (source
// change_review). They are read from each database's own
// sage.change_review through the DBA-queue partial index
// (change_review_dba_queue), newest completion first.

const changeReviewCasesLimit = 200

const changeReviewCasesSQL = `/* pg_sage change_review_cases v1 */
SELECT id::text, COALESCE(repo, ''), COALESCE(pr_url, ''), COALESCE(pr_number, 0),
       COALESCE(pr_title, ''), state, verdict, COALESCE(dba_verdict, ''), completed_at,
       findings
FROM sage.change_review
WHERE state = 'complete' AND dba_verdict IS NULL AND verdict IN ('review', 'risky')
ORDER BY completed_at DESC
LIMIT $1`

// pgUndefinedTable is SQLSTATE 42P01: a schema bootstrapped before
// Change Review has no sage.change_review, and so no review cases.
const pgUndefinedTable = "42P01"

// queryChangeReviewCases is the database's change reviews waiting for a
// DBA, as cases named after the database's fleet name.
func queryChangeReviewCases(ctx context.Context, selected namedPool, now time.Time) (
	[]cases.Case, error) {
	rows, err := selected.pool.Query(ctx, changeReviewCasesSQL, changeReviewCasesLimit)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
			return nil, nil
		}
		return nil, fmt.Errorf("read change review cases of %s: %w", selected.name, err)
	}
	defer rows.Close()
	out := make([]cases.Case, 0)
	for rows.Next() {
		src, err := scanChangeReviewCase(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("read change review cases of %s: %w", selected.name, err)
		}
		src.DatabaseName = selected.name
		if c, ok := cases.ProjectChangeReview(src, now); ok {
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read change review cases of %s: %w", selected.name, err)
	}
	return out, nil
}

func scanChangeReviewCase(scan func(...any) error) (cases.SourceChangeReview, error) {
	var src cases.SourceChangeReview
	var verdict *string
	var completed *time.Time
	var findings []byte
	if err := scan(&src.ID, &src.Repo, &src.PRURL, &src.PRNumber, &src.PRTitle, &src.State,
		&verdict, &src.DBAVerdict, &completed, &findings); err != nil {
		return src, err
	}
	if verdict != nil {
		src.Verdict = *verdict
	}
	if completed != nil {
		src.CompletedAt = completed.UTC()
	}
	var stored []struct {
		RuleID   string `json:"rule_id"`
		Severity string `json:"severity"`
		Title    string `json:"title"`
	}
	if err := json.Unmarshal(findings, &stored); err != nil {
		return src, fmt.Errorf("review %s findings: %w", src.ID, err)
	}
	for _, f := range stored {
		src.Findings = append(src.Findings, cases.ChangeReviewFinding{RuleID: f.RuleID,
			Severity: f.Severity, Title: f.Title})
	}
	return src, nil
}

// queryNonFindingCases is one database's incident, query-hint and
// change-review cases.
func queryNonFindingCases(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	selected namedPool,
) ([]cases.Case, error) {
	out, err := queryIncidentCases(ctx, mgr, selected)
	if err != nil {
		return nil, err
	}
	hints, err := queryHintCases(ctx, selected)
	if err != nil {
		return nil, err
	}
	reviews, err := queryChangeReviewCases(ctx, selected, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return append(append(out, hints...), reviews...), nil
}
