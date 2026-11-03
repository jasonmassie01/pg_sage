package sre

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Retention (AI-SRE-SPEC §10): terminal, unpinned investigations lose
// their redacted evidence after EvidenceAge and their whole timeline
// after TimelineAge, measured from their last change. Live and pinned
// investigations are never touched. Maintenance changes restart the age
// window, and in-flight or unresolved maintenance keeps its investigation
// and evidence until the outcome is resolved. Every delete leaves a tombstone.
// Rows are locked with SKIP LOCKED, so concurrent passes delete each
// row once.

// retentionActor is the event actor of retention deletes.
const retentionActor = "retention"

func (p RetentionPolicy) validate() error {
	switch {
	case p.EvidenceAge < 24*time.Hour:
		return fmt.Errorf("%w: evidence retention under one day", ErrInvalidRequest)
	case p.TimelineAge < p.EvidenceAge:
		return fmt.Errorf("%w: timeline retention shorter than evidence retention",
			ErrInvalidRequest)
	case p.BatchSize < 1 || p.BatchSize > 1000:
		return fmt.Errorf("%w: batch size %d outside [1, 1000]", ErrInvalidRequest,
			p.BatchSize)
	}
	return nil
}

// Purge applies the retention policy to one scope: whole timelines
// first, then evidence of the investigations that remain.
func (s *PostgresStore) Purge(ctx context.Context, scope Scope,
	p RetentionPolicy) (PurgeResult, error) {
	if err := scope.Validate(); err != nil {
		return PurgeResult{}, err
	}
	if err := p.validate(); err != nil {
		return PurgeResult{}, err
	}
	var res PurgeResult
	err := s.retentionTx(ctx, "purge timelines", func(tx pgx.Tx) error {
		n, err := purgeTimelines(ctx, tx, scope, p)
		res.InvestigationsPurged = n
		return err
	})
	if err != nil {
		return res, err
	}
	err = s.retentionTx(ctx, "purge evidence", func(tx pgx.Tx) error {
		n, err := purgeEvidence(ctx, tx, scope, p)
		res.EvidencePurged = n
		return err
	})
	return res, err
}

// agedIDsSQL locks up to $4 terminal, unpinned investigations whose last
// change is older than $3 seconds. extra is a constant SQL predicate. No
// index keys updated_at (every progress update moves it, which made those
// updates non-HOT); updated_at >= created_at, so the created_at bound
// makes the read a range of idx_sre_investigations_created, oldest first.
func agedIDsSQL(extra string) string {
	return `SELECT id::text FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND NOT pinned
		  AND state IN ` + terminalStates + `
		  AND created_at < now() - make_interval(secs => $3)
		  AND updated_at < now() - make_interval(secs => $3) ` + keepMaintenance + extra + `
		ORDER BY created_at, id LIMIT $4 FOR UPDATE SKIP LOCKED`
}

// agedIDs runs agedIDsSQL for scope.
func agedIDs(ctx context.Context, tx pgx.Tx, scope Scope, age time.Duration,
	batch int, extra string) ([]string, error) {
	rows, err := tx.Query(ctx, agedIDsSQL(extra),
		string(scope.DeploymentID), string(scope.DatabaseID), age.Seconds(), batch)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) == 0 {
		return ids, err
	}
	// The candidate predicate and LockRows may straddle a maintenance
	// commit that changed only the child row. Recheck on a fresh statement
	// snapshot after locking parents; maintenance cannot now mutate them.
	rows, err = tx.Query(ctx, `SELECT id::text FROM sage.sre_investigations
		WHERE deployment_id=$1 AND database_id=$2 AND id=ANY($4::uuid[])
		`+keepMaintenance+` ORDER BY created_at, id`, string(scope.DeploymentID), string(scope.DatabaseID),
		age.Seconds(), ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// retentionTx requires a fresh snapshot for the post-lock maintenance check,
// even if the connecting role changes its default transaction isolation.
func (s *PostgresStore) retentionTx(ctx context.Context, op string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return storeErr(ctx, op, err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if err := fn(tx); err != nil {
		return storeErr(ctx, op, err)
	}
	return storeErr(ctx, op, tx.Commit(ctx))
}

// keepHeldBudget keeps investigations whose model reservation is still
// held (reserved, in flight or unknown): that is billing evidence.
const keepHeldBudget = `AND NOT EXISTS (SELECT 1 FROM sage.sre_budget_reservations b
	WHERE b.deployment_id = sre_investigations.deployment_id
	  AND b.database_id = sre_investigations.database_id
	  AND b.investigation_id = sre_investigations.id
	  AND b.state IN ('reserved', 'inflight', 'unknown'))`

// keepMaintenance makes the maintenance ledger share the investigation's
// bounded lifecycle. A terminal maintenance change ages from its own last
// update, not the earlier diagnostic conclusion. Expired proposed/requested
// work eventually ages out, while execution, rollback and unresolved outcomes
// cannot lose their evidence or rollback identity underneath the operator.
// Both purges lock the parent investigation; maintenance mutations use the
// same lock before changing the proposal or appending its event. Unknown
// future states fail closed. Proposed/requested work has a hard 15-minute
// expiry, so legitimate age-eligible requests (minimum age one day) are expired.
const keepMaintenance = `AND NOT EXISTS (
    SELECT 1 FROM sage.sre_maintenance_proposals m
    WHERE m.deployment_id = sre_investigations.deployment_id
      AND m.database_id = sre_investigations.database_id
      AND m.investigation_id = sre_investigations.id
      AND (m.created_at >= now() - make_interval(secs => $3)
           OR m.updated_at >= now() - make_interval(secs => $3)
           OR m.state NOT IN ('proposed', 'requested', 'executed', 'failed', 'refused', 'rolled_back'))
    OFFSET 0)
`

func purgeTimelines(ctx context.Context, tx pgx.Tx, scope Scope,
	p RetentionPolicy) (int, error) {
	ids, err := agedIDs(ctx, tx, scope, p.TimelineAge, p.BatchSize, keepHeldBudget)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	args := []any{string(scope.DeploymentID), string(scope.DatabaseID), ids}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_tombstones
		(deployment_id, database_id, investigation_id, kind, reason, row_count, detail)
		SELECT i.deployment_id, i.database_id, i.id, 'investigation', 'retention',
		       (SELECT count(*) FROM sage.sre_events e WHERE e.deployment_id = $1
		          AND e.database_id = $2 AND e.investigation_id = i.id),
		       jsonb_build_object('state', i.state, 'trigger_kind', i.trigger_kind,
		           'created_at', i.created_at, 'concluded_at', i.concluded_at,
		           'root', i.summary->>'root', 'head_hash',
		           (SELECT encode(e.hash, 'hex') FROM sage.sre_events e
		            WHERE e.deployment_id = $1 AND e.database_id = $2
		              AND e.investigation_id = i.id ORDER BY e.sequence DESC LIMIT 1))
		FROM sage.sre_investigations i
		WHERE i.deployment_id = $1 AND i.database_id = $2 AND i.id = ANY($3::uuid[])
		ON CONFLICT DO NOTHING`, args...); err != nil {
		return 0, err
	}
	for _, table := range []string{"sre_budget_reservations", "sre_evidence",
		"sre_steps", "sre_hypotheses", "sre_events"} {
		if _, err := tx.Exec(ctx, `DELETE FROM sage.`+table+`
			WHERE deployment_id = $1 AND database_id = $2
			  AND investigation_id = ANY($3::uuid[])`, args...); err != nil {
			return 0, fmt.Errorf("purge %s: %w", table, err)
		}
	}
	tag, err := tx.Exec(ctx, `DELETE FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND id = ANY($3::uuid[])`, args...)
	return int(tag.RowsAffected()), err
}

func purgeEvidence(ctx context.Context, tx pgx.Tx, scope Scope,
	p RetentionPolicy) (int, error) {
	ids, err := agedIDs(ctx, tx, scope, p.EvidenceAge, p.BatchSize,
		"AND evidence_purged_at IS NULL")
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := purgeOneEvidence(ctx, tx, scope, UUID(id)); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func purgeOneEvidence(ctx context.Context, tx pgx.Tx, scope Scope, id UUID) error {
	args := []any{string(scope.DeploymentID), string(scope.DatabaseID), string(id)}
	tag, err := tx.Exec(ctx, `DELETE FROM sage.sre_evidence
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3`, args...)
	if err != nil {
		return err
	}
	rows := tag.RowsAffected()
	if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_tombstones
		(deployment_id, database_id, investigation_id, kind, reason, row_count)
		VALUES ($1, $2, $3, 'evidence', 'retention', $4) ON CONFLICT DO NOTHING`,
		append(args, rows)...); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sage.sre_investigations
		SET evidence_purged_at = clock_timestamp()
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3`, args...); err != nil {
		return err
	}
	return appendEvent(ctx, tx, scope, id, EventEvidencePurged, retentionActor,
		map[string]any{"rows": rows})
}

// Tombstones lists what retention deleted for one investigation.
func (s *PostgresStore) Tombstones(ctx context.Context, scope Scope,
	id UUID) ([]Tombstone, error) {
	if err := validateIDs(scope, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT kind, deleted_at, reason, row_count,
		    detail::text
		FROM sage.sre_tombstones
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
		ORDER BY deleted_at, kind`, string(scope.DeploymentID),
		string(scope.DatabaseID), string(id))
	if err != nil {
		return nil, storeErr(ctx, "tombstones", err)
	}
	defer rows.Close()
	var out []Tombstone
	for rows.Next() {
		t := Tombstone{InvestigationID: id}
		var detail string
		if err := rows.Scan(&t.Kind, &t.DeletedAt, &t.Reason, &t.RowCount,
			&detail); err != nil {
			return nil, storeErr(ctx, "tombstones", err)
		}
		t.Detail = json.RawMessage(detail)
		out = append(out, t)
	}
	return out, storeErr(ctx, "tombstones", rows.Err())
}
