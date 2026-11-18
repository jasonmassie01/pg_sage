package reviewtest

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Seeding finished reviews for the scorecard's and the model gate's tests
// (review-05a): a done review run, its model runs, its plan with steps in
// the state the executor would leave them, each step's origin, and the
// review's plan row, written directly so a test names every outcome.

// Hex64 is a valid sha256-shaped hash for rows that need one.
const Hex64 = "0000000000000000000000000000000000000000000000000000000000000001"

// Outcome is one plan step of a seeded review.
type Outcome struct {
	Origin     string // generator, model or person
	Ref        string // the generator's name, the model id or the person
	Verdict    string // "" (pending) or a sage.plan_step verdict
	Accepted   bool   // started (attempt 1, executed)
	Reverted   bool   // state reverted
	ActionType string // "" is create_index
	SQL        string // "" is a SQL text unique to the step
}

// Review is one seeded review run.
type Review struct {
	Database string
	Status   string   // "" is done
	Models   []string // one done rank run per model
	Steps    []Outcome
}

// DB is a pool, a connection or a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SeedReview writes r and returns its run id.
func SeedReview(ctx context.Context, db DB, r Review) (int64, error) {
	status := r.Status
	if status == "" {
		status = "done"
	}
	var run int64
	err := db.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		rubric_version, status, finished_at) VALUES ($1, 'review', 'pg_sage-rubric/1', $2,
		CASE WHEN $2 = 'running' THEN NULL ELSE clock_timestamp() END) RETURNING id`,
		r.Database, status).Scan(&run)
	if err != nil {
		return 0, fmt.Errorf("seed a review run: %w", err)
	}
	for i, m := range r.Models {
		_, err := db.Exec(ctx, `INSERT INTO sage.review_model_run (run_id, attempt, role,
			model, prompt_version, pack_hash, output_sha256, status) VALUES ($1, $2, 'rank',
			$3, 'review-03a/1', $4, $4, 'done')`, run, i+1, m, Hex64)
		if err != nil {
			return 0, fmt.Errorf("seed a model run: %w", err)
		}
	}
	if len(r.Steps) == 0 {
		return run, nil
	}
	return run, seedPlan(ctx, db, run, r.Steps)
}

func seedPlan(ctx context.Context, db DB, run int64, steps []Outcome) error {
	origin := "generator"
	for _, s := range steps {
		if s.Origin == "model" {
			origin = "model"
		}
	}
	var plan int64
	err := db.QueryRow(ctx, `INSERT INTO sage.plan (goal, origin, status, plan_hash,
		created_by) VALUES ('seeded review plan', $1, 'running', $2, 'review')
		RETURNING id`, origin, Hex64).Scan(&plan)
	if err != nil {
		return fmt.Errorf("seed a plan: %w", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO sage.review_plan (run_id, plan_id, plan_hash)
		VALUES ($1, $2, $3)`, run, plan, Hex64); err != nil {
		return fmt.Errorf("seed a review plan: %w", err)
	}
	for i, s := range steps {
		if err := seedStep(ctx, db, run, plan, i+1, s); err != nil {
			return err
		}
	}
	return nil
}

func seedStep(ctx context.Context, db DB, run, plan int64, ordinal int, s Outcome) error {
	action, sql := s.ActionType, s.SQL
	if action == "" {
		action = "create_index"
	}
	if sql == "" {
		sql = fmt.Sprintf("CREATE INDEX CONCURRENTLY seeded_%d_%d ON t (a)", run, ordinal)
	}
	state, attempt := "pending", 0
	switch {
	case s.Reverted:
		state, attempt = "reverted", 1
	case s.Accepted && s.Verdict != "":
		state, attempt = "kept", 1
	case s.Accepted:
		state, attempt = "judging", 1
	}
	var verdict any
	if s.Verdict != "" {
		verdict = s.Verdict
	}
	var id int64
	err := db.QueryRow(ctx, `INSERT INTO sage.plan_step (plan_id, ordinal, kind,
		action_type, proposed_sql, metric, expected_direction, expected_source,
		deadline_minutes, on_improved, on_no_change, on_regressed, on_inconclusive,
		on_expired, state, attempt, executed_at, verdict) VALUES ($1, $2, 'action', $3, $4,
		'time_per_call', 'down', 'none', 60, 'keep', 'keep', 'revert_step', 'keep', 'keep',
		$5, $6, CASE WHEN $6 > 0 THEN now() END, $7) RETURNING id`, plan, ordinal, action,
		sql, state, attempt, verdict).Scan(&id)
	if err != nil {
		return fmt.Errorf("seed plan step %d: %w", ordinal, err)
	}
	_, err = db.Exec(ctx, `INSERT INTO sage.plan_step_origin (step_id, plan_id, run_id,
		origin, origin_ref) VALUES ($1, $2, $3, $4, $5)`, id, plan, run, s.Origin,
		strings.TrimSpace(s.Ref))
	if err != nil {
		return fmt.Errorf("seed the origin of step %d: %w", ordinal, err)
	}
	return nil
}
