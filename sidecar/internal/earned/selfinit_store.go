package earned

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

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
