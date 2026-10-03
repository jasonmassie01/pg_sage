package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrOutcomeNotFound: no outcome (or no action) with that id.
var ErrOutcomeNotFound = errors.New("action outcome not found")

// ErrInvalidOutcome: a verdict or filter the ledger does not accept.
var ErrInvalidOutcome = errors.New("invalid action outcome")

// OutcomeDB is a pool, connection or transaction.
type OutcomeDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// OutcomeStore persists predicted vs observed per action in
// sage.action_outcome, the ledger the trust system reads.
type OutcomeStore struct{ db OutcomeDB }

// NewOutcomeStore returns a store over db.
func NewOutcomeStore(db OutcomeDB) *OutcomeStore { return &OutcomeStore{db: db} }

// RecordPrediction records an action's prediction as a pending outcome.
// A prediction is immutable: a second call for the action is a no-op.
func (s *OutcomeStore) RecordPrediction(ctx context.Context, actionID int64,
	p Prediction) error {
	if s == nil || s.db == nil {
		return errors.New("action outcome store is unavailable")
	}
	raw, err := json.Marshal(normalizedPrediction(p))
	if err != nil {
		return fmt.Errorf("encode prediction for action %d: %w", actionID, err)
	}
	var exists bool
	err = s.db.QueryRow(ctx, `/* pg_sage */ WITH ins AS (
		INSERT INTO sage.action_outcome (action_log_id, database_id, action_class,
			predicted, prediction_method)
		SELECT id, database_id, $2, $3::jsonb, $4 FROM sage.action_log WHERE id = $1
		ON CONFLICT (action_log_id) DO NOTHING RETURNING 1)
		SELECT EXISTS (SELECT 1 FROM sage.action_log WHERE id = $1)`,
		actionID, p.Class, raw, methodOf(p)).Scan(&exists)
	if err != nil {
		return fmt.Errorf("record prediction for action %d: %w", actionID, err)
	}
	if !exists {
		return fmt.Errorf("%w: action %d", ErrOutcomeNotFound, actionID)
	}
	return nil
}

// RecordVerdict records an action's verdict with what was observed. The
// stored prediction is kept (and judged against); without one, the
// outcome's own prediction (or "none") is stored with it.
func (s *OutcomeStore) RecordVerdict(ctx context.Context, o Outcome) error {
	if s == nil || s.db == nil {
		return errors.New("action outcome store is unavailable")
	}
	if !DecidedVerdict(o.Verdict) {
		return fmt.Errorf("%w: verdict %q", ErrInvalidOutcome, o.Verdict)
	}
	stored, ok, err := s.storedPrediction(ctx, o.ActionLogID)
	if err != nil {
		return err
	}
	if ok {
		o.Predicted = stored
	}
	o.Predicted = normalizedPrediction(o.Predicted)
	if o.Class == "" {
		o.Class = o.Predicted.Class
	}
	o.Tolerance = ToleranceVerdict(o.Predicted, o.Verdict, o.Observed.ChangePct)
	return s.upsertVerdict(ctx, o)
}

func (s *OutcomeStore) storedPrediction(ctx context.Context, actionID int64) (
	Prediction, bool, error) {
	var raw []byte
	err := s.db.QueryRow(ctx, `/* pg_sage */ SELECT predicted FROM sage.action_outcome
		WHERE action_log_id = $1`, actionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Prediction{}, false, nil
	}
	if err != nil {
		return Prediction{}, false, fmt.Errorf("read prediction for action %d: %w",
			actionID, err)
	}
	var p Prediction
	if err := json.Unmarshal(raw, &p); err != nil {
		return Prediction{}, false, fmt.Errorf("decode prediction for action %d: %w",
			actionID, err)
	}
	return p, true, nil
}

const upsertVerdictSQL = `/* pg_sage */ INSERT INTO sage.action_outcome
	(action_log_id, database_id, action_class, predicted, prediction_method, verdict,
	 tolerance, observed, evidence, reason, window_start, window_end, decided_at)
	SELECT id, database_id, $2, $3::jsonb, $4, $5, $6, $7::jsonb, $8::jsonb, $9, $10, $11,
	       now()
	  FROM sage.action_log WHERE id = $1
	ON CONFLICT (action_log_id) DO UPDATE SET
	  verdict = EXCLUDED.verdict, tolerance = EXCLUDED.tolerance,
	  observed = EXCLUDED.observed, evidence = EXCLUDED.evidence,
	  reason = EXCLUDED.reason, window_start = EXCLUDED.window_start,
	  window_end = EXCLUDED.window_end, decided_at = now()`

func (s *OutcomeStore) upsertVerdict(ctx context.Context, o Outcome) error {
	predicted, err := json.Marshal(o.Predicted)
	if err != nil {
		return fmt.Errorf("encode prediction for action %d: %w", o.ActionLogID, err)
	}
	observed, err := json.Marshal(o.Observed)
	if err != nil {
		return fmt.Errorf("encode observation for action %d: %w", o.ActionLogID, err)
	}
	evidence := o.Evidence
	if evidence == nil {
		evidence = map[string]any{}
	}
	ev, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("encode evidence for action %d: %w", o.ActionLogID, err)
	}
	tag, err := s.db.Exec(ctx, upsertVerdictSQL, o.ActionLogID, o.Class, predicted,
		methodOf(o.Predicted), o.Verdict, o.Tolerance, observed, ev, o.Reason,
		o.WindowStart, o.WindowEnd)
	if err != nil {
		return fmt.Errorf("record verdict for action %d: %w", o.ActionLogID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: action %d", ErrOutcomeNotFound, o.ActionLogID)
	}
	return nil
}

func normalizedPrediction(p Prediction) Prediction {
	if p.Method == "" {
		p.Method = MethodNone
	}
	return p
}

func methodOf(p Prediction) string {
	switch p.Method {
	case MethodHypoPG, MethodModel, MethodRule:
		return p.Method
	}
	return MethodNone
}

const outcomeColumns = `action_log_id, database_id, action_class, verdict, tolerance,
	predicted, observed, evidence, reason, window_start, window_end, created_at, decided_at`

// Get reads one action's outcome.
func (s *OutcomeStore) Get(ctx context.Context, actionID int64) (Outcome, error) {
	if s == nil || s.db == nil {
		return Outcome{}, errors.New("action outcome store is unavailable")
	}
	o, err := scanOutcome(s.db.QueryRow(ctx, `/* pg_sage */ SELECT `+outcomeColumns+`
		FROM sage.action_outcome WHERE action_log_id = $1`, actionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, fmt.Errorf("%w: action %d", ErrOutcomeNotFound, actionID)
	}
	return o, err
}

// OutcomeFilter selects outcomes: by class and verdict ("" = any), made
// or decided since Since, newest first, at most Limit (1-1000, 0 = 100).
type OutcomeFilter struct {
	Class   string
	Verdict string
	Since   time.Time
	Limit   int
}

// List reads outcomes, newest action first.
func (s *OutcomeStore) List(ctx context.Context, f OutcomeFilter) ([]Outcome, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("action outcome store is unavailable")
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	if f.Limit < 1 || f.Limit > 1000 {
		return nil, fmt.Errorf("%w: limit must be 1-1000, got %d", ErrInvalidOutcome, f.Limit)
	}
	if f.Verdict != "" && f.Verdict != OutcomePending && !DecidedVerdict(f.Verdict) {
		return nil, fmt.Errorf("%w: verdict %q", ErrInvalidOutcome, f.Verdict)
	}
	rows, err := s.db.Query(ctx, `/* pg_sage */ SELECT `+outcomeColumns+`
		FROM sage.action_outcome
		WHERE ($1 = '' OR action_class = $1) AND ($2 = '' OR verdict = $2)
		  AND COALESCE(decided_at, created_at) >= $3
		ORDER BY action_log_id DESC LIMIT $4`, f.Class, f.Verdict, f.Since, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list action outcomes: %w", err)
	}
	defer rows.Close()
	out := []Outcome{}
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read action outcomes: %w", err)
	}
	return out, nil
}

type outcomeScanner interface{ Scan(...any) error }

func scanOutcome(row outcomeScanner) (Outcome, error) {
	var o Outcome
	var predicted, observed, evidence []byte
	err := row.Scan(&o.ActionLogID, &o.DatabaseID, &o.Class, &o.Verdict, &o.Tolerance,
		&predicted, &observed, &evidence, &o.Reason, &o.WindowStart, &o.WindowEnd,
		&o.CreatedAt, &o.DecidedAt)
	if err != nil {
		return Outcome{}, err
	}
	for _, part := range []struct {
		raw  []byte
		into any
		name string
	}{{predicted, &o.Predicted, "prediction"}, {observed, &o.Observed, "observation"},
		{evidence, &o.Evidence, "evidence"}} {
		if err := json.Unmarshal(part.raw, part.into); err != nil {
			return Outcome{}, fmt.Errorf("decode %s of action %d: %w", part.name,
				o.ActionLogID, err)
		}
	}
	return o, nil
}
