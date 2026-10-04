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
	case CauseRegressed, CauseRolledBack, CauseRejected, CauseShadowIncorrect:
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

// lastChangesSQL reads each pair's newest level-changing event through the
// pair's index.
const lastChangesSQL = `/* pg_sage */ SELECT e.id, e.family, e.action_class, e.event_type,
	e.from_level, e.to_level, e.actor, e.reason, e.database_name, e.proposal_id,
	e.action_log_id, e.evidence, e.created_at
	FROM unnest($3::text[], $4::text[]) AS p(family, action_class)
	CROSS JOIN LATERAL (
		SELECT id, family, action_class, event_type, from_level, to_level, actor, reason,
		       COALESCE(database_name, '') AS database_name,
		       COALESCE(proposal_id::text, '') AS proposal_id,
		       COALESCE(action_log_id, 0) AS action_log_id, evidence, created_at
		  FROM sage.sre_autonomy_events ev
		 WHERE ev.deployment_id = $1 AND ev.database_name = $2
		   AND ev.family = p.family AND ev.action_class = p.action_class
		   AND ev.event_type IN ('promotion_approved', 'downgraded', 'auto_downgraded',
		                         'carried_over', 'grandfathered', 'database_scoped')
		 ORDER BY ev.created_at DESC, ev.id DESC LIMIT 1) e`

// lastChanges reads the newest level-changing event of each pair.
func (s *PostgresStore) lastChanges(ctx context.Context, pairs []pairKey) (
	map[pairKey]Event, error) {
	families, classes := pairArrays(pairs)
	out := map[pairKey]Event{}
	err := s.queryEach(ctx, "read level changes", lastChangesSQL,
		[]any{s.deployment, s.database, families, classes}, func(rows pgx.Rows) error {
			e, err := scanEvent(rows)
			if err != nil {
				return err
			}
			out[pairKey{e.Family, e.Class}] = e
			return nil
		})
	return out, err
}

// stamp is a nullable time for an API row.
func stamp(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
