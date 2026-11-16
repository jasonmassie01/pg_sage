package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
)

type rowQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// PostgresObservationSource reads verification evidence: collector
// history (query_store intervals, system and tables snapshots) from the
// database's history store, and the catalog (pg_index, pg_class) from the
// monitored database.
// With history.store: meta those are two databases; the history store is
// resolved from the monitored pool (histstore.Resolve).
type PostgresObservationSource struct {
	queryer rowQuerier
}

func NewPostgresObservationSource(pool *pgxpool.Pool) *PostgresObservationSource {
	if pool == nil {
		return &PostgresObservationSource{}
	}
	return &PostgresObservationSource{queryer: pool}
}

// historyStore is where the history reads run, resolved from the catalog
// handle on every read (histstore.Resolve): a source built before its
// runtime registered still reaches the right store, and in meta mode an
// unregistered pool is refused rather than read as its own history.
func (s *PostgresObservationSource) historyStore() histstore.Store {
	return histstore.Resolve(s.queryer)
}

func (s *PostgresObservationSource) QueryMeasurements(
	ctx context.Context, ids []int64, from, to time.Time,
) (map[int64]Measurement, error) {
	if s == nil || s.queryer == nil {
		return nil, errors.New("verify observation pool is unavailable")
	}
	result := make(map[int64]Measurement, len(ids))
	for _, id := range ids {
		measurement, err := s.queryMeasurement(ctx, id, from, to)
		if err != nil {
			return nil, fmt.Errorf("query %d measurement: %w", id, err)
		}
		result[id] = measurement
	}
	return result, nil
}

func (s *PostgresObservationSource) IndexValid(
	ctx context.Context, name string,
) (bool, error) {
	if s == nil || s.queryer == nil {
		return false, errors.New("verify observation pool is unavailable")
	}
	var valid bool
	err := s.queryer.QueryRow(ctx, `SELECT COALESCE((
		SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid=to_regclass($1)
	), false)`, name).Scan(&valid)
	return valid, err
}

// LoadEvidence reports no evidence: catalogs expose connection counts, not
// host CPU or disk utilization, and a single active query can saturate
// either resource. The executor composes real evidence from the pg-side IO
// sampler, host CPU readers and the maintenance window.
func (s *PostgresObservationSource) LoadEvidence(
	ctx context.Context,
) (LoadEvidence, error) {
	if err := ctx.Err(); err != nil {
		return LoadEvidence{}, err
	}
	return LoadEvidence{}, ErrLoadTelemetryUnavailable
}

// PostgresStateStore persists watch state in sage.verification.
type PostgresStateStore struct {
	pool       *pgxpool.Pool
	minSamples int
}

func NewPostgresStateStore(pool *pgxpool.Pool, minSamples int) *PostgresStateStore {
	return &PostgresStateStore{pool: pool, minSamples: minSamples}
}

type persistedWatch struct {
	WatchID    string        `json:"watch_id"`
	ExecutedAt time.Time     `json:"executed_at"`
	Table      string        `json:"table"`
	IndexName  string        `json:"index_name"`
	Window     time.Duration `json:"window"`
	// WriteScope is the write signal the watch is judged by: "table" for
	// every watch stored since executor-03b. A row without it is older.
	WriteScope string `json:"write_scope,omitempty"`
	// FrozenWeights are the targets' pre-period call shares (lab-01a); a
	// row without them is older and judged by v1 only.
	FrozenWeights map[int64]float64 `json:"frozen_weights,omitempty"`
	// AlphaSpend is the alpha spent by the watch's looks (lab-01b), kept
	// here so a restart continues the spend instead of starting from zero.
	AlphaSpend AlphaSpend `json:"alpha_spend,omitzero"`
	// Other is the other verifier's latest verdict (lab-01c).
	Other *OtherVerdict `json:"other,omitempty"`
}

// writeScopeTable marks a watch judged by the table-scoped write signal.
const writeScopeTable = "table"

func (s *PostgresStateStore) Create(ctx context.Context, state WatchState) error {
	if s == nil || s.pool == nil {
		return errors.New("verify state pool is unavailable")
	}
	criterion, baseline, err := marshalState(state)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var verificationID int64
	err = tx.QueryRow(ctx, `INSERT INTO sage.verification
		(decision_id, action_log_id, criterion, baseline, minimum_samples,
		 next_evaluation_at, hard_deadline_at, verdict, reason)
		SELECT decision_id, id, $2, $3, $4, $5, $6, 'pending', ''
		FROM sage.action_log WHERE id=$1 AND decision_id IS NOT NULL
		RETURNING id`, state.ActionID, criterion, baseline, s.minSamples,
		state.NextEvaluationAt, state.ExecutedAt.Add(state.Criterion.HardMax)).
		Scan(&verificationID)
	if err != nil {
		return fmt.Errorf("insert verification state: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE sage.action_log SET verification_id=$1
		WHERE id=$2`, verificationID, state.ActionID); err != nil {
		return fmt.Errorf("link verification state: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PostgresStateStore) Update(ctx context.Context, state WatchState) error {
	if s == nil || s.pool == nil {
		return errors.New("verify state pool is unavailable")
	}
	criterion, baseline, err := marshalState(state)
	if err != nil {
		return err
	}
	status := databaseVerdict(state.Status)
	command, err := s.pool.Exec(ctx, `UPDATE sage.verification SET
		criterion=$2, baseline=$3, next_evaluation_at=$4, verdict=$5,
		reason=$6, updated_at=now(),
		completed_at=CASE WHEN $7 THEN now() ELSE NULL END,
		observation_failures=$8
		WHERE action_log_id=$1`, state.ActionID, criterion, baseline,
		state.NextEvaluationAt, status, state.Reason, state.Completed,
		state.ObservationFailures)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("verification state for action %d not found", state.ActionID)
	}
	return nil
}

func (s *PostgresStateStore) Get(ctx context.Context, id string) (WatchState, error) {
	if s == nil || s.pool == nil {
		return WatchState{}, errors.New("verify state pool is unavailable")
	}
	return s.scanState(s.pool.QueryRow(ctx, `SELECT action_log_id, criterion,
		baseline, verdict, COALESCE(reason, ''), completed_at IS NOT NULL,
		next_evaluation_at, observation_failures FROM sage.verification
		WHERE baseline->>'watch_id'=$1`, id))
}

func (s *PostgresStateStore) ListDue(
	ctx context.Context, now time.Time,
) ([]WatchState, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("verify state pool is unavailable")
	}
	rows, err := s.pool.Query(ctx, `SELECT action_log_id, criterion,
		baseline, verdict, COALESCE(reason, ''), completed_at IS NOT NULL,
		next_evaluation_at, observation_failures FROM sage.verification
		WHERE completed_at IS NULL
		  AND verdict IN ('pending', 'extended', 'revert', 'unverifiable')
		  AND next_evaluation_at <= $1
		ORDER BY next_evaluation_at, id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := []WatchState{}
	for rows.Next() {
		state, scanErr := s.scanState(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

// Claim is a conditional UPDATE: only a row that is still open and due at
// now is moved to leaseUntil. Row locking serializes concurrent claimers and
// the loser re-checks the predicate against the winner's committed lease, so
// exactly one claim succeeds per due period, across processes.
func (s *PostgresStateStore) Claim(
	ctx context.Context, listed WatchState, now, leaseUntil time.Time,
) (WatchState, bool, error) {
	if s == nil || s.pool == nil {
		return WatchState{}, false, errors.New("verify state pool is unavailable")
	}
	state, err := s.scanState(s.pool.QueryRow(ctx, `UPDATE sage.verification
		SET next_evaluation_at=$3, updated_at=now()
		WHERE action_log_id=$1 AND completed_at IS NULL
		  AND verdict IN ('pending', 'extended', 'revert', 'unverifiable')
		  AND next_evaluation_at <= $2
		RETURNING action_log_id, criterion, baseline, verdict, COALESCE(reason, ''),
			completed_at IS NOT NULL, next_evaluation_at, observation_failures`,
		listed.ActionID, now, leaseUntil))
	if errors.Is(err, pgx.ErrNoRows) {
		return WatchState{}, false, nil
	}
	if err != nil {
		return WatchState{}, false, err
	}
	return state, true, nil
}

// ReopenObservationRetries reopens watches that a transient error closed
// before observation retries existed: an open unverifiable verdict with
// reason observation_unavailable or context_canceled whose hard deadline is
// still ahead becomes an extended watch the next resume observes again. An
// open write_impact revert that the database-wide signal decided (stored
// before executor-03b, so without write_scope) whose drop has not run is
// reopened too, whatever its deadline: it is judged again on the
// table-scoped signal. A table-scoped write_impact revert is a real verdict
// and is left alone. Its next evaluation (the claim
// lease) is kept. A reopened action that its withheld or failed revert left
// rollback_skipped or rollback_failed is watched again (monitoring), so the
// next verdict can settle it; an operator's rollback is never touched. It
// is idempotent.
func (s *PostgresStateStore) ReopenObservationRetries(ctx context.Context) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("verify state pool is unavailable")
	}
	var reopened int64
	err := s.pool.QueryRow(ctx, `WITH reopened AS (
		UPDATE sage.verification SET verdict='extended',
		  reason='observation_retry: reopened ' || reason || ' verdict', updated_at=now()
		WHERE completed_at IS NULL AND (
		  (verdict='unverifiable'
		    AND reason IN ('observation_unavailable', 'context_canceled')
		    AND hard_deadline_at > now())
		  OR (verdict='revert' AND reason='write_impact'
		    AND baseline->>'write_scope' IS NULL))
		RETURNING action_log_id
	), watched AS (
		UPDATE sage.action_log al SET outcome='monitoring', rollback_reason=NULL
		FROM reopened r
		WHERE al.id=r.action_log_id
		  AND al.outcome IN ('rollback_skipped', 'rollback_failed')
		RETURNING al.id
	)
	SELECT count(*) FROM reopened`).Scan(&reopened)
	if err != nil {
		return 0, fmt.Errorf("reopen transient or write-impact verifications: %w", err)
	}
	return reopened, nil
}

type stateScanner interface {
	Scan(...any) error
}

func (s *PostgresStateStore) scanState(row stateScanner) (WatchState, error) {
	var state WatchState
	var criterionJSON, baselineJSON []byte
	err := row.Scan(&state.ActionID, &criterionJSON, &baselineJSON, &state.Status,
		&state.Reason, &state.Completed, &state.NextEvaluationAt, &state.ObservationFailures)
	if err != nil {
		return WatchState{}, err
	}
	var persisted persistedWatch
	if err := json.Unmarshal(criterionJSON, &state.Criterion); err != nil {
		return WatchState{}, fmt.Errorf("decode criterion: %w", err)
	}
	if err := json.Unmarshal(baselineJSON, &persisted); err != nil {
		return WatchState{}, fmt.Errorf("decode watch identity: %w", err)
	}
	state.ID, state.ExecutedAt = persisted.WatchID, persisted.ExecutedAt
	state.Table, state.IndexName = persisted.Table, persisted.IndexName
	state.Window, state.FrozenWeights = persisted.Window, persisted.FrozenWeights
	state.AlphaSpend, state.Other = persisted.AlphaSpend, persisted.Other
	return state, nil
}

func marshalState(state WatchState) ([]byte, []byte, error) {
	criterion, err := json.Marshal(state.Criterion)
	if err != nil {
		return nil, nil, fmt.Errorf("encode criterion: %w", err)
	}
	baseline, err := json.Marshal(persistedWatch{
		WatchID: state.ID, ExecutedAt: state.ExecutedAt, Table: state.Table,
		IndexName: state.IndexName, Window: state.Window, WriteScope: writeScopeTable,
		FrozenWeights: state.FrozenWeights, AlphaSpend: state.AlphaSpend,
		Other: state.Other,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("encode watch identity: %w", err)
	}
	return criterion, baseline, nil
}

func databaseVerdict(status string) string {
	if status == "reverted" {
		return "revert"
	}
	if status == "" {
		return "pending"
	}
	return status
}

var _ ObservationSource = (*PostgresObservationSource)(nil)
var _ StateStore = (*PostgresStateStore)(nil)
