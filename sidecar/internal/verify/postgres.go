package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// PostgresObservationSource reads verification evidence from collector history.
type PostgresObservationSource struct {
	queryer rowQuerier
}

func NewPostgresObservationSource(pool *pgxpool.Pool) *PostgresObservationSource {
	if pool == nil {
		return &PostgresObservationSource{}
	}
	return &PostgresObservationSource{queryer: pool}
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

func (s *PostgresObservationSource) queryMeasurement(
	ctx context.Context, id int64, from, to time.Time,
) (Measurement, error) {
	var calls int64
	var latencyMS float64
	err := s.queryer.QueryRow(ctx, `WITH samples AS (
		SELECT calls, total_exec_time,
			row_number() OVER (ORDER BY captured_at) AS first_row,
			row_number() OVER (ORDER BY captured_at DESC) AS last_row
		FROM sage.query_store
		WHERE queryid=$1 AND captured_at BETWEEN $2 AND $3
	), bounds AS (
		SELECT max(calls) FILTER (WHERE last_row=1) -
			max(calls) FILTER (WHERE first_row=1) AS calls,
			max(total_exec_time) FILTER (WHERE last_row=1) -
			max(total_exec_time) FILTER (WHERE first_row=1) AS elapsed
		FROM samples
	)
	SELECT COALESCE(calls, 0),
		CASE WHEN calls > 0 AND elapsed >= 0 THEN elapsed/calls ELSE 0 END
	FROM bounds`, id, from, to).Scan(&calls, &latencyMS)
	if err != nil {
		return Measurement{}, err
	}
	return Measurement{
		Samples:        int(maxInt64(calls, 0)),
		AverageLatency: time.Duration(latencyMS * float64(time.Millisecond)),
	}, nil
}

func (s *PostgresObservationSource) WriteMeasurements(
	ctx context.Context, _ string, from, to time.Time,
) (Measurement, error) {
	if s == nil || s.queryer == nil {
		return Measurement{}, errors.New("verify observation pool is unavailable")
	}
	var samples int
	var elapsedMS float64
	err := s.queryer.QueryRow(ctx, `WITH samples AS (
		SELECT (data->>'blk_write_time')::float8 AS write_ms,
			row_number() OVER (ORDER BY collected_at) AS first_row,
			row_number() OVER (ORDER BY collected_at DESC) AS last_row
		FROM sage.snapshots
		WHERE category='system' AND collected_at BETWEEN $1 AND $2
	), bounds AS (
		SELECT count(*)::int AS samples,
			max(write_ms) FILTER (WHERE last_row=1) -
			max(write_ms) FILTER (WHERE first_row=1) AS elapsed
		FROM samples
	)
	SELECT samples, COALESCE(GREATEST(elapsed, 0), 0) FROM bounds`,
		from, to).Scan(&samples, &elapsedMS)
	if err != nil {
		return Measurement{}, err
	}
	denominator := samples - 1
	if denominator <= 0 {
		return Measurement{Samples: samples}, nil
	}
	return Measurement{
		Samples: samples,
		AverageLatency: time.Duration(
			elapsedMS / float64(denominator) * float64(time.Millisecond),
		),
	}, nil
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

func (s *PostgresObservationSource) CurrentLoad(
	ctx context.Context,
) (LoadSample, error) {
	if err := ctx.Err(); err != nil {
		return LoadSample{}, err
	}
	// Catalogs expose connection counts, not host CPU or disk utilization. A
	// single active query can saturate either resource; reporting that ratio as
	// three utilization metrics silently defeats every load ceiling.
	return LoadSample{}, ErrLoadTelemetryUnavailable
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
}

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
		completed_at=CASE WHEN $7 THEN now() ELSE NULL END
		WHERE action_log_id=$1`, state.ActionID, criterion, baseline,
		state.NextEvaluationAt, status, state.Reason, state.Completed)
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
		next_evaluation_at FROM sage.verification
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
		next_evaluation_at FROM sage.verification
		WHERE verdict IN ('pending', 'extended') AND next_evaluation_at <= $1
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

type stateScanner interface {
	Scan(...any) error
}

func (s *PostgresStateStore) scanState(row stateScanner) (WatchState, error) {
	var state WatchState
	var criterionJSON, baselineJSON []byte
	err := row.Scan(&state.ActionID, &criterionJSON, &baselineJSON, &state.Status,
		&state.Reason, &state.Completed, &state.NextEvaluationAt)
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
	state.Window = persisted.Window
	return state, nil
}

func marshalState(state WatchState) ([]byte, []byte, error) {
	criterion, err := json.Marshal(state.Criterion)
	if err != nil {
		return nil, nil, fmt.Errorf("encode criterion: %w", err)
	}
	baseline, err := json.Marshal(persistedWatch{
		WatchID: state.ID, ExecutedAt: state.ExecutedAt, Table: state.Table,
		IndexName: state.IndexName, Window: state.Window,
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

func maxInt64(value, floor int64) int64 {
	if value < floor {
		return floor
	}
	return value
}

var _ ObservationSource = (*PostgresObservationSource)(nil)
var _ StateStore = (*PostgresStateStore)(nil)
