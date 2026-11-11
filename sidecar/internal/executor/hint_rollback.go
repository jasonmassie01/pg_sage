package executor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/tuner"
)

// ErrHintTextUnknown refuses a legacy hint rollback whose pg_sage hint text
// cannot be established: running it unscoped could delete an operator's
// own hint for the same query (tuning-02a).
var ErrHintTextUnknown = errors.New("hint_text_unknown")

// legacyHintDeleteRe matches the rollback SQL releases before tuning-02a
// recorded for an applied hint: scoped to the query, not to the hint text.
var legacyHintDeleteRe = regexp.MustCompile(`(?is)^\s*(?:/\*\s*pg_sage\s*\*/\s*)?` +
	`DELETE\s+FROM\s+hint_plan\.hints\s+WHERE\s+query_id\s*=\s*(-?\d+)\s+` +
	`AND\s+application_name\s*=\s*''\s*;?\s*$`)

// legacyHintDeleteQueryID returns the query id of a legacy unscoped hint
// DELETE, or false for any other SQL.
func legacyHintDeleteQueryID(sql string) (int64, bool) {
	m := legacyHintDeleteRe.FindStringSubmatch(sql)
	if m == nil {
		return 0, false
	}
	qid, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return qid, true
}

// scopeHintRollback rewrites a legacy unscoped hint DELETE to the form
// scoped to the hint text pg_sage recorded for the action. Any other SQL is
// returned unchanged. When the text cannot be established the rollback is
// refused with ErrHintTextUnknown instead of deleting blindly.
func scopeHintRollback(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, rollbackSQL string,
) (string, error) {
	qid, ok := legacyHintDeleteQueryID(rollbackSQL)
	if !ok {
		return rollbackSQL, nil
	}
	if pool == nil {
		return "", fmt.Errorf("%w: no database to read the recorded hint for "+
			"queryid %d (action %d)", ErrHintTextUnknown, qid, actionID)
	}
	var executed string
	var recorded []string
	err := pool.QueryRow(ctx, `/* pg_sage executor */ SELECT a.sql_executed,
       COALESCE((SELECT array_agg(DISTINCT q.hint_text) FROM sage.query_hints q
                  WHERE q.queryid = $2), '{}')
  FROM sage.action_log a WHERE a.id = $1`, actionID, qid).Scan(&executed, &recorded)
	if err != nil {
		return "", fmt.Errorf("%w: reading the recorded hint for queryid %d "+
			"(action %d): %v", ErrHintTextUnknown, qid, actionID, err)
	}
	hint, ok := chooseRecordedHint(executed, qid, recorded)
	if !ok {
		return "", fmt.Errorf("%w: pg_sage recorded no unambiguous hint text for "+
			"queryid %d (action %d); the unscoped rollback could delete an "+
			"operator's hint, so remove the row from hint_plan.hints by hand "+
			"if it is pg_sage's", ErrHintTextUnknown, qid, actionID)
	}
	scoped := tuner.BuildDeleteSQL(qid, hint)
	if err := ValidateExecutorSQL(scoped); err != nil {
		return "", fmt.Errorf("scoped hint rollback for action %d: %w", actionID, err)
	}
	return scoped, nil
}

// chooseRecordedHint picks the hint text a legacy rollback is scoped to. It
// must be one sage.query_hints recorded for the query: the text the
// action's own INSERT installed when that INSERT parses, otherwise the
// query's single recorded text. Anything else is ambiguous.
func chooseRecordedHint(executed string, qid int64, recorded []string) (string, bool) {
	if own, ok := tuner.InsertedHintText(executed, qid); ok {
		for _, r := range recorded {
			if r == own {
				return own, true
			}
		}
		return "", false
	}
	if len(recorded) == 1 {
		return recorded[0], true
	}
	return "", false
}
