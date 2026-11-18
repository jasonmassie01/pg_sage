package rollout

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runLockNamespace keys the session advisory lock a running canary holds
// on the control database (with hashtext of its run id): a sidecar that
// stops ends its session, and with it the lock, so the startup sweep can
// tell a run nobody runs any more from one another sidecar still runs.
const runLockNamespace int32 = 0x53524f4c // "SROL"

// releaseTimeout bounds unlocking a run and returning its session.
const releaseTimeout = 5 * time.Second

// interruptedDetail is added to an instance the run stopped in flight.
const interruptedDetail = "a sidecar restart stopped the canary before this " +
	"database's verdict was recorded, and the canary was not resumed; any change it " +
	"made stays with the database's own post-action monitor: check its action log"

// runLock is a running canary's hold on its run.
type runLock struct {
	conn *pgxpool.Conn
	id   string
}

// lockRun takes the run's advisory lock on a session it keeps until
// release. The run id is new, so another holder is an error.
func (store *PostgresRunStore) lockRun(ctx context.Context, id string) (*runLock, error) {
	if store == nil || store.pool == nil {
		return nil, errors.New("rollout run store is unavailable")
	}
	conn, err := store.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("lock canary run %s: acquire a session: %w", id, err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2))`,
		runLockNamespace, id).Scan(&got); err != nil {
		conn.Release()
		return nil, fmt.Errorf("lock canary run %s: %w", id, err)
	}
	if !got {
		conn.Release()
		return nil, fmt.Errorf("lock canary run %s: another session holds it", id)
	}
	return &runLock{conn: conn, id: id}, nil
}

// release unlocks the run; a session that cannot unlock is closed, which
// ends the lock with it, rather than returned to the pool still holding it.
func (l *runLock) release() {
	if l == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	var unlocked bool
	if err := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1, hashtext($2))`,
		runLockNamespace, l.id).Scan(&unlocked); err != nil || !unlocked {
		_ = l.conn.Conn().Close(ctx)
	}
	l.conn.Release()
}

// SaveProgress records one instance's part in a running canary. It
// writes nothing for a run that is no longer in canary and returns
// ErrRunInterrupted, so a canary another sidecar interrupted stops.
func (store *PostgresRunStore) SaveProgress(ctx context.Context, runID string,
	r InstanceRecord) error {
	if store == nil || store.pool == nil {
		return errors.New("rollout run store is unavailable")
	}
	var actionLogID any
	if r.ActionLogID > 0 {
		actionLogID = r.ActionLogID
	}
	tag, err := store.pool.Exec(ctx, `INSERT INTO sage.rollout_instance
		(run_evidence_id, instance_id, ordinal, status, evidence_id, action_log_id,
		 regression_pct, detail, updated_at, effect_pct, control_pct, effect_method)
		SELECT $1, $2, $3, $4, NULLIF($5, ''), $6, $7, NULLIF($8, ''), $9, $10, $11,
		       NULLIF($12, '')
		WHERE EXISTS (SELECT 1 FROM sage.rollout_run
		              WHERE evidence_id = $1 AND state = 'canary')
		ON CONFLICT (run_evidence_id, instance_id) DO UPDATE SET `+instanceUpsertSet,
		runID, r.InstanceID, r.Ordinal, r.Status, r.EvidenceID, actionLogID,
		r.RegressionPct, truncateDetail(r.Detail), r.UpdatedAt, r.EffectPct, r.ControlPct,
		r.EffectMethod)
	if err != nil {
		return fmt.Errorf("record canary progress of %s on %s: %w", runID, r.InstanceID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("record canary progress of %s on %s: %w", runID, r.InstanceID,
			ErrRunInterrupted)
	}
	return nil
}

// InterruptOrphaned marks every run left in canary by a sidecar that no
// longer runs it (nobody holds its lock) as interrupted, and its
// instances still being changed or awaiting a verdict as interrupted. A
// run another live sidecar holds is left alone; two sidecars sweeping at
// once interrupt each run once (row locks and the run's lock).
func (store *PostgresRunStore) InterruptOrphaned(ctx context.Context,
	at time.Time) ([]InterruptedRun, error) {
	if store == nil || store.pool == nil {
		return nil, errors.New("rollout run store is unavailable")
	}
	out := []InterruptedRun{}
	err := pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
		for _, bound := range []string{`SET LOCAL statement_timeout = '30s'`,
			`SET LOCAL lock_timeout = '5s'`} {
			if _, err := tx.Exec(ctx, bound); err != nil {
				return fmt.Errorf("bound the sweep: %w", err)
			}
		}
		ids, err := canaryRunIDs(ctx, tx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			run, ok, err := interruptRun(ctx, tx, id, at)
			if err != nil {
				return err
			}
			if ok {
				out = append(out, run)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("interrupt orphaned canary runs: %w", err)
	}
	return out, nil
}

// canaryRunIDs locks the runs in canary no other sweep holds.
func canaryRunIDs(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT evidence_id FROM sage.rollout_run
		WHERE state = 'canary' ORDER BY id FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return nil, fmt.Errorf("list canary runs: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list canary runs: %w", err)
	}
	return ids, nil
}

// interruptRun interrupts one run when no session holds its lock.
func interruptRun(ctx context.Context, tx pgx.Tx, id string, at time.Time) (
	InterruptedRun, bool, error) {
	var free bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1, hashtext($2))`,
		runLockNamespace, id).Scan(&free); err != nil {
		return InterruptedRun{}, false, fmt.Errorf("probe the lock of run %s: %w", id, err)
	}
	if !free {
		return InterruptedRun{}, false, nil
	}
	run, err := scanRunRecord(tx.QueryRow(ctx, `UPDATE sage.rollout_run
		SET state = 'interrupted', halt_reason = $2, updated_at = $3
		WHERE evidence_id = $1 AND state = 'canary'
		RETURNING `+rolloutRunColumns, id, HaltSidecarRestart, at))
	if errors.Is(err, pgx.ErrNoRows) {
		return InterruptedRun{}, false, nil
	}
	if err != nil {
		return InterruptedRun{}, false, fmt.Errorf("interrupt run %s: %w", id, err)
	}
	changing, err := interruptInstances(ctx, tx, id, at)
	if err != nil {
		return InterruptedRun{}, false, err
	}
	kept, err := keptInstances(ctx, tx, id)
	if err != nil {
		return InterruptedRun{}, false, err
	}
	return InterruptedRun{Run: run, Changing: changing, Kept: kept}, true, nil
}

// instanceColumns are the rollout_instance columns scanInstances reads.
const instanceColumns = `instance_id, ordinal, status, COALESCE(evidence_id, ''),
	COALESCE(action_log_id, 0), COALESCE(regression_pct, 0),
	COALESCE(detail, ''), updated_at, effect_pct, control_pct, COALESCE(effect_method, '')`

// interruptInstances marks a run's in-flight instances interrupted.
func interruptInstances(ctx context.Context, tx pgx.Tx, id string, at time.Time) (
	[]InstanceRecord, error) {
	rows, err := tx.Query(ctx, `UPDATE sage.rollout_instance
		SET status = 'interrupted', updated_at = $2,
		    detail = left(concat_ws('; ', detail, $3::text), 2000)
		WHERE run_evidence_id = $1 AND status IN ('applying', 'awaiting_verdict')
		RETURNING `+instanceColumns, id, at, interruptedDetail)
	if err != nil {
		return nil, fmt.Errorf("interrupt the instances of run %s: %w", id, err)
	}
	out, err := scanInstances(rows)
	if err != nil {
		return nil, fmt.Errorf("interrupt the instances of run %s: %w", id, err)
	}
	return out, nil
}

// keptInstances lists the databases an interrupted run changed and whose
// verdict it recorded: nothing rolls them back, so they keep the change.
func keptInstances(ctx context.Context, tx pgx.Tx, id string) ([]InstanceRecord, error) {
	rows, err := tx.Query(ctx, `SELECT `+instanceColumns+`
		FROM sage.rollout_instance
		WHERE run_evidence_id = $1 AND status IN ('applied', 'verification_pending')`, id)
	if err != nil {
		return nil, fmt.Errorf("list the changed instances of run %s: %w", id, err)
	}
	out, err := scanInstances(rows)
	if err != nil {
		return nil, fmt.Errorf("list the changed instances of run %s: %w", id, err)
	}
	return out, nil
}

// scanInstances collects instance rows in rollout order.
func scanInstances(rows pgx.Rows) ([]InstanceRecord, error) {
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (InstanceRecord, error) {
		var r InstanceRecord
		err := row.Scan(&r.InstanceID, &r.Ordinal, &r.Status, &r.EvidenceID,
			&r.ActionLogID, &r.RegressionPct, &r.Detail, &r.UpdatedAt, &r.EffectPct,
			&r.ControlPct, &r.EffectMethod)
		return r, err
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b InstanceRecord) int { return cmp.Compare(a.Ordinal, b.Ordinal) })
	return out, nil
}
