package earned

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// incidentCursors reads the database's incident reconcile cursors (none
// before the first pass, or before the earned-02 upgrade).
func (s *PostgresStore) incidentCursors(ctx context.Context) (incidentCursors, error) {
	var c incidentCursors
	err := s.pool.QueryRow(ctx, `SELECT handoff_cursor, handoff_pending, auto_cursor,
		auto_pending FROM sage.trust_ledger_state
		WHERE deployment_id = $1 AND database_name = $2`, s.deployment, s.database).
		Scan(&c.handoff.cursor, &c.handoff.pending, &c.auto.cursor, &c.auto.pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return incidentCursors{}, nil
	}
	return c, storeErr("read incident reconcile cursors", err)
}

// saveIncidentCursors replaces the cursors read as loaded with next. It is
// a compare and set: when another pass has replaced loaded since, nothing
// is written (that pass's state is kept and the next pass reads on from
// it; every outcome is de-duplicated by its unique index). An unchanged
// state writes no row version (dogfood round 2). Empty pending sets are
// stored as NULL.
func (s *PostgresStore) saveIncidentCursors(ctx context.Context, loaded,
	next incidentCursors) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sage.trust_ledger_state
		(deployment_id, database_name, handoff_cursor, handoff_pending, auto_cursor,
		 auto_pending)
		VALUES ($1, $2, $3, NULLIF($4::bigint[], '{}'), $5, NULLIF($6::bigint[], '{}'))
		ON CONFLICT (deployment_id, database_name) DO UPDATE SET
		  handoff_cursor = EXCLUDED.handoff_cursor,
		  handoff_pending = EXCLUDED.handoff_pending,
		  auto_cursor = EXCLUDED.auto_cursor,
		  auto_pending = EXCLUDED.auto_pending,
		  updated_at = clock_timestamp()
		WHERE (trust_ledger_state.handoff_cursor,
		       COALESCE(trust_ledger_state.handoff_pending, '{}'),
		       trust_ledger_state.auto_cursor,
		       COALESCE(trust_ledger_state.auto_pending, '{}'))
		      IS NOT DISTINCT FROM ($7::bigint, COALESCE($8::bigint[], '{}'), $9::bigint,
		       COALESCE($10::bigint[], '{}'))
		  AND (EXCLUDED.handoff_cursor, COALESCE(EXCLUDED.handoff_pending, '{}'),
		       EXCLUDED.auto_cursor, COALESCE(EXCLUDED.auto_pending, '{}'))
		      IS DISTINCT FROM (trust_ledger_state.handoff_cursor,
		       COALESCE(trust_ledger_state.handoff_pending, '{}'),
		       trust_ledger_state.auto_cursor,
		       COALESCE(trust_ledger_state.auto_pending, '{}'))`,
		s.deployment, s.database, next.handoff.cursor, pendingArg(next.handoff.pending),
		next.auto.cursor, pendingArg(next.auto.pending), loaded.handoff.cursor,
		pendingArg(loaded.handoff.pending), loaded.auto.cursor,
		pendingArg(loaded.auto.pending))
	return storeErr("save incident reconcile cursors", err)
}

// pendingArg is a pending set as a bigint[] argument ('{}' when empty).
func pendingArg(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// autoExecutedRecordedSet reports which of the L3 executions ids were
// notified, in one read.
func (s *PostgresStore) autoExecutedRecordedSet(ctx context.Context,
	ids []int64) (map[int64]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT action_log_id FROM sage.sre_autonomy_events
		WHERE deployment_id = $1 AND event_type = 'auto_executed'
		  AND database_name = $2 AND action_log_id = ANY ($3::bigint[])`,
		s.deployment, s.database, ids)
	if err != nil {
		return nil, storeErr("read auto-execution notices", err)
	}
	ids, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, storeErr("read auto-execution notices", err)
	}
	done := make(map[int64]bool, len(ids))
	for _, id := range ids {
		done[id] = true
	}
	return done, nil
}

// recordAutoExecuted records an L3 execution's notice once: false when
// another pass recorded it first (the unique sre_autonomy_one_auto_executed).
func (s *PostgresStore) recordAutoExecuted(ctx context.Context, e Event) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_events
		(deployment_id, family, action_class, event_type, actor, reason, database_name,
		 action_log_id, evidence, created_at)
		VALUES ($1, $2, $3, 'auto_executed', $4, $5, $6, $7, '{}', $8)
		ON CONFLICT (deployment_id, database_name, action_log_id)
		  WHERE event_type = 'auto_executed' DO NOTHING`,
		s.deployment, string(e.Family), string(e.Class), e.Actor, e.Reason, s.database,
		e.ActionLogID, e.At)
	if err != nil {
		return false, storeErr("record auto-execution notice", err)
	}
	return tag.RowsAffected() == 1, nil
}
