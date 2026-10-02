package slo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Transition is one change of an SLO's state; From is empty for the
// first evaluation.
type Transition struct {
	From State     `json:"from,omitempty"`
	To   State     `json:"to"`
	At   time.Time `json:"at"`
}

type prevState struct {
	state     State
	since     time.Time
	burnStart *time.Time
}

// Save records a status: the state's since-time moves only when the
// state changes (a transition is appended), and a burn (page or ticket)
// keeps its start time until the SLO stops burning. It returns the
// status as stored and whether the state changed.
func (s *Store) Save(ctx context.Context, scope sre.Scope, st Status) (Status, bool, error) {
	if err := scope.Validate(); err != nil {
		return Status{}, false, fmt.Errorf("%w: %v", ErrInvalidObjective, err)
	}
	var changed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		prev, found, err := lockStatus(ctx, tx, scope, st.Name)
		if err != nil {
			return err
		}
		changed = !found || prev.state != st.State
		applyHistory(&st, prev, found, changed)
		if err := upsertStatus(ctx, tx, scope, st); err != nil {
			return err
		}
		if !changed {
			return nil
		}
		var from *string
		if found {
			v := string(prev.state)
			from = &v
		}
		_, err = tx.Exec(ctx, `INSERT INTO sage.sre_slo_transitions
			(deployment_id, database_id, name, from_state, to_state, at)
			VALUES ($1, $2, $3, $4, $5, $6)`, string(scope.DeploymentID),
			string(scope.DatabaseID), st.Name, from, string(st.State), st.EvaluatedAt)
		return err
	})
	if err != nil {
		return Status{}, false, fmt.Errorf("save SLO status of %s: %w", st.Name, err)
	}
	return st, changed, nil
}

func applyHistory(st *Status, prev prevState, found, changed bool) {
	st.StateSince = st.EvaluatedAt
	if !changed {
		st.StateSince = prev.since
	}
	st.BurnStartedAt = nil
	if !st.Burning() {
		return
	}
	start := st.EvaluatedAt
	wasBurning := found && (prev.state == StatePage || prev.state == StateTicket)
	if wasBurning && prev.burnStart != nil {
		start = *prev.burnStart
	}
	st.BurnStartedAt = &start
}

func lockStatus(ctx context.Context, tx pgx.Tx, scope sre.Scope, name string) (prevState,
	bool, error) {
	var p prevState
	var state string
	err := tx.QueryRow(ctx, `SELECT state, state_since, burn_started_at
		FROM sage.sre_service_slos
		WHERE deployment_id = $1 AND database_id = $2 AND name = $3 FOR UPDATE`,
		string(scope.DeploymentID), string(scope.DatabaseID), name).Scan(&state, &p.since,
		&p.burnStart)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	p.state = State(state)
	return p, err == nil, err
}

func upsertStatus(ctx context.Context, tx pgx.Tx, scope sre.Scope, st Status) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode status: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO sage.sre_service_slos
		(deployment_id, database_id, name, kind, source, target, definition_hash, state,
		 fast_burning, status, evaluated_at, state_since, burn_started_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13)
		ON CONFLICT (deployment_id, database_id, name) DO UPDATE SET
		    kind = EXCLUDED.kind, source = EXCLUDED.source, target = EXCLUDED.target,
		    definition_hash = EXCLUDED.definition_hash, state = EXCLUDED.state,
		    fast_burning = EXCLUDED.fast_burning, status = EXCLUDED.status,
		    evaluated_at = EXCLUDED.evaluated_at, state_since = EXCLUDED.state_since,
		    burn_started_at = EXCLUDED.burn_started_at,
		    version = sage.sre_service_slos.version + 1`,
		string(scope.DeploymentID), string(scope.DatabaseID), st.Name, string(st.Kind),
		string(st.Source), st.Target, st.DefinitionHash, string(st.State), st.FastBurning,
		string(raw), st.EvaluatedAt, st.StateSince, st.BurnStartedAt)
	return err
}

// Statuses reads the scope's stored statuses, by name.
func (s *Store) Statuses(ctx context.Context, scope sre.Scope) ([]Status, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidObjective, err)
	}
	rows, err := s.pool.Query(ctx, `SELECT status::text FROM sage.sre_service_slos
		WHERE deployment_id = $1 AND database_id = $2 ORDER BY name`,
		string(scope.DeploymentID), string(scope.DatabaseID))
	if err != nil {
		return nil, fmt.Errorf("read SLO statuses: %w", err)
	}
	defer rows.Close()
	out := []Status{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan SLO status: %w", err)
		}
		var st Status
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return nil, fmt.Errorf("decode SLO status: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Transitions reads an SLO's state history, newest first.
func (s *Store) Transitions(ctx context.Context, scope sre.Scope, name string,
	limit int) ([]Transition, error) {
	rows, err := s.pool.Query(ctx, `SELECT COALESCE(from_state, ''), to_state, at
		FROM sage.sre_slo_transitions
		WHERE deployment_id = $1 AND database_id = $2 AND name = $3
		ORDER BY at DESC, id DESC LIMIT $4`, string(scope.DeploymentID),
		string(scope.DatabaseID), name, limit)
	if err != nil {
		return nil, fmt.Errorf("read SLO transitions of %s: %w", name, err)
	}
	defer rows.Close()
	out := []Transition{}
	for rows.Next() {
		var t Transition
		var from, to string
		if err := rows.Scan(&from, &to, &t.At); err != nil {
			return nil, fmt.Errorf("scan SLO transition: %w", err)
		}
		t.From, t.To = State(from), State(to)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Prune deletes the scope's statuses (and their history) of SLOs that
// are no longer configured.
func (s *Store) Prune(ctx context.Context, scope sre.Scope, keep []string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sage.sre_service_slos
		WHERE deployment_id = $1 AND database_id = $2 AND NOT (name = ANY($3))`,
		string(scope.DeploymentID), string(scope.DatabaseID), keep)
	if err != nil {
		return fmt.Errorf("prune SLO statuses: %w", err)
	}
	return nil
}

// Purge ages out one SLO's samples (shared by every database of the
// deployment that has the SLO) and this database's transitions of it.
func (s *Store) Purge(ctx context.Context, scope sre.Scope, slo string, samplesBefore,
	transitionsBefore time.Time) (int64, error) {
	a, err := s.pool.Exec(ctx, `DELETE FROM sage.sre_sli_samples
		WHERE deployment_id = $1 AND slo_name = $2 AND observed_at < $3`,
		string(scope.DeploymentID), slo, samplesBefore)
	if err != nil {
		return 0, fmt.Errorf("purge SLI samples of %s: %w", slo, err)
	}
	b, err := s.pool.Exec(ctx, `DELETE FROM sage.sre_slo_transitions
		WHERE deployment_id = $1 AND database_id = $2 AND name = $3 AND at < $4`,
		string(scope.DeploymentID), string(scope.DatabaseID), slo, transitionsBefore)
	if err != nil {
		return 0, fmt.Errorf("purge SLO transitions of %s: %w", slo, err)
	}
	return a.RowsAffected() + b.RowsAffected(), nil
}
