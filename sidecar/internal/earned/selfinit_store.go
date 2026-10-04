package earned

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// classRecordSQL counts a pair's outcomes on the database, and its
// credited and uncredited decided outcomes since its last demerit. Rows
// recorded before outcomes kept their verdict count by result.
const classRecordSQL = `WITH pair AS (
	SELECT verdict, result, COALESCE(observed_at, recorded_at) AS at
	  FROM sage.sre_autonomy_outcomes
	 WHERE deployment_id = $1 AND database_name = $2 AND family = $3
	   AND action_class = $4
), last AS (
	SELECT at, COALESCE(verdict, result) AS cause FROM pair
	 WHERE result IN ('harmful', 'safety_violation', 'rejected')
	 ORDER BY at DESC LIMIT 1
)
SELECT
	count(*) FILTER (WHERE verdict = 'improved'
	                    OR (verdict IS NULL AND result = 'verified_recovery')),
	count(*) FILTER (WHERE verdict = 'neutral'),
	count(*) FILTER (WHERE verdict = 'regressed'
	                    OR (verdict IS NULL AND result IN ('harmful', 'safety_violation'))),
	count(*) FILTER (WHERE verdict = 'rolled_back'),
	count(*) FILTER (WHERE verdict = 'rejected'
	                    OR (verdict IS NULL AND result = 'rejected')),
	count(*) FILTER (WHERE verdict = 'insufficient_evidence'),
	count(*) FILTER (WHERE verdict = 'unverifiable'
	                    OR (verdict IS NULL AND result = 'unverified')),
	count(*) FILTER (WHERE result = 'verified_recovery'
	                   AND (l.at IS NULL OR pair.at > l.at)),
	count(*) FILTER (WHERE result = 'unverified' AND verdict = 'neutral'
	                   AND (l.at IS NULL OR pair.at > l.at)),
	max(l.at), max(l.cause)
FROM pair LEFT JOIN last l ON true`

// ClassRecord reads a pair's verdict record on the database.
func (s *PostgresStore) ClassRecord(ctx context.Context, f Family, c ActionClass) (
	ClassRecord, error) {
	var r ClassRecord
	var cause *string
	err := s.pool.QueryRow(ctx, classRecordSQL, s.deployment, s.database, string(f),
		string(c)).Scan(&r.Improved, &r.Neutral, &r.Regressed, &r.RolledBack, &r.Rejected,
		&r.Insufficient, &r.Unverifiable, &r.Successes, &r.Uncredited, &r.LastDemeritAt,
		&cause)
	if err != nil {
		return ClassRecord{}, storeErr("read class record", err)
	}
	if r.LastDemeritAt != nil {
		at := r.LastDemeritAt.UTC()
		r.LastDemeritAt = &at
	}
	if cause != nil {
		r.LastDemerit = demeritName(*cause)
	}
	return r, nil
}

// demeritName names a stored demerit (a verdict or, for rows without
// one, a result).
func demeritName(stored string) string {
	switch stored {
	case CauseRegressed, CauseRolledBack, CauseRejected:
		return stored
	}
	return CauseHarmful
}

// cursors reads the database's reconcile cursors (none before the first
// pass).
func (s *PostgresStore) cursors(ctx context.Context) (selfCursors, error) {
	var c selfCursors
	err := s.pool.QueryRow(ctx, `SELECT verdict_cursor, rollback_cursor, rejection_cursor
		FROM sage.trust_ledger_state WHERE deployment_id = $1 AND database_name = $2`,
		s.deployment, s.database).Scan(&c.verdict, &c.rollback, &c.rejection)
	if errors.Is(err, pgx.ErrNoRows) {
		return selfCursors{}, nil
	}
	return c, storeErr("read reconcile cursors", err)
}

// saveCursors moves the database's cursors forward (never back).
func (s *PostgresStore) saveCursors(ctx context.Context, c selfCursors) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sage.trust_ledger_state
		(deployment_id, database_name, verdict_cursor, rollback_cursor, rejection_cursor)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (deployment_id, database_name) DO UPDATE SET
		  verdict_cursor = GREATEST(trust_ledger_state.verdict_cursor,
		                            EXCLUDED.verdict_cursor),
		  rollback_cursor = GREATEST(trust_ledger_state.rollback_cursor,
		                             EXCLUDED.rollback_cursor),
		  rejection_cursor = GREATEST(trust_ledger_state.rejection_cursor,
		                              EXCLUDED.rejection_cursor),
		  updated_at = clock_timestamp()`,
		s.deployment, s.database, c.verdict, c.rollback, c.rejection)
	return storeErr("save reconcile cursors", err)
}

// lastChanges reads the newest level-changing event of every pair of the
// database.
func (s *PostgresStore) lastChanges(ctx context.Context) (map[pairKey]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (family, action_class) id, family,
		action_class, event_type, from_level, to_level, actor, reason,
		COALESCE(database_name, ''), COALESCE(proposal_id::text, ''),
		COALESCE(action_log_id, 0), evidence, created_at
		FROM sage.sre_autonomy_events
		WHERE deployment_id = $1 AND database_name = $2
		  AND event_type IN ('promotion_approved', 'downgraded', 'auto_downgraded',
		                     'carried_over', 'grandfathered', 'database_scoped')
		ORDER BY family, action_class, created_at DESC, id DESC`, s.deployment, s.database)
	if err != nil {
		return nil, storeErr("read level changes", err)
	}
	defer rows.Close()
	out := map[pairKey]Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, storeErr("scan level change", err)
		}
		out[pairKey{e.Family, e.Class}] = e
	}
	return out, storeErr("read level changes", rows.Err())
}

// stamp is a nullable time for an API row.
func stamp(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
