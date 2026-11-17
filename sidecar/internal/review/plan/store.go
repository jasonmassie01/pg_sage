package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changeplan"
)

// Store keeps review plans: the plan itself through changeplan's store
// (validation against the resolver, caps, hash, created event), and the
// review's own rows (sage.review_plan, sage.plan_step_origin,
// sage.review_advice) beside it.
type Store struct {
	pool     *pgxpool.Pool
	plans    *changeplan.Store
	resolver changeplan.Resolver
}

// NewStore stores review plans on pool (the monitored database's sage
// schema), creating plans with plans and validating them with resolver.
func NewStore(pool *pgxpool.Pool, plans *changeplan.Store, resolver changeplan.Resolver) *Store {
	return &Store{pool: pool, plans: plans, resolver: resolver}
}

// Saved is a stored review plan. PlanID is 0 when the review proposed no
// change; Reused is set when the run's plan was already stored.
type Saved struct {
	RunID          int64  `json:"run_id"`
	PlanID         int64  `json:"plan_id,omitempty"`
	PlanHash       string `json:"plan_hash,omitempty"`
	GeneratorSteps int    `json:"generator_steps"`
	ModelSteps     int    `json:"model_steps"`
	Advice         int    `json:"advice"`
	Message        string `json:"message,omitempty"`
	Reused         bool   `json:"reused"`
}

// Save stores res once per review run. It locks the run's row, so a
// re-run (or a concurrent save) of the same run finds the stored plan and
// returns it with Reused set. A plan the store refuses fails the save with
// an error naming sage.plan; if the review rows cannot be written after
// the plan was created, the plan is voided so no orphan waits for an
// approval.
func (s *Store) Save(ctx context.Context, res Result) (Saved, error) {
	if res.RunID <= 0 {
		return Saved{}, fmt.Errorf("%w: run id %d", ErrInvalidInput, res.RunID)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Saved{}, fmt.Errorf("save the review plan of run %d: begin: %w", res.RunID, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if saved, found, err := lockRun(ctx, tx, res.RunID); err != nil || found {
		return saved, err
	}
	var stored changeplan.Plan
	if res.Plan != nil {
		stored, err = s.plans.Create(ctx, *res.Plan, s.resolver)
		if err != nil {
			return Saved{}, fmt.Errorf("save the review plan of run %d: insert into "+
				"sage.plan: %w", res.RunID, err)
		}
	}
	saved, err := record(ctx, tx, res, stored)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		if stored.ID > 0 {
			_ = s.plans.Void(context.WithoutCancel(ctx), stored.ID,
				"the review could not record its plan: "+err.Error())
		}
		return Saved{}, fmt.Errorf("save the review plan of run %d: %w", res.RunID, err)
	}
	return saved, nil
}

// lockRun locks run's row for the save and returns its stored review plan
// when there is one.
func lockRun(ctx context.Context, tx pgx.Tx, run int64) (Saved, bool, error) {
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '30s'`); err != nil {
		return Saved{}, false, fmt.Errorf("save the review plan of run %d: %w", run, err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '30s'`); err != nil {
		return Saved{}, false, fmt.Errorf("save the review plan of run %d: %w", run, err)
	}
	var id int64
	err := tx.QueryRow(ctx, `/* pg_sage */ SELECT id FROM sage.review_run WHERE id = $1
		FOR UPDATE`, run).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Saved{}, false, fmt.Errorf("save the review plan of run %d: the review run "+
			"does not exist", run)
	}
	if err != nil {
		return Saved{}, false, fmt.Errorf("save the review plan of run %d: lock the run: %w",
			run, err)
	}
	sv := Saved{RunID: run, Reused: true}
	var planID *int64
	var hash *string
	err = tx.QueryRow(ctx, `/* pg_sage */ SELECT plan_id, plan_hash, generator_steps,
		model_steps, advice, message FROM sage.review_plan WHERE run_id = $1`, run).
		Scan(&planID, &hash, &sv.GeneratorSteps, &sv.ModelSteps, &sv.Advice, &sv.Message)
	if errors.Is(err, pgx.ErrNoRows) {
		return Saved{}, false, nil
	}
	if err != nil {
		return Saved{}, false, fmt.Errorf("save the review plan of run %d: read "+
			"sage.review_plan: %w", run, err)
	}
	if planID != nil {
		sv.PlanID = *planID
	}
	if hash != nil {
		sv.PlanHash = *hash
	}
	return sv, true, nil
}

// record writes the review rows of res, whose plan is stored (zero when
// the review has no plan).
func record(ctx context.Context, tx pgx.Tx, res Result, stored changeplan.Plan) (Saved,
	error) {
	sv := Saved{RunID: res.RunID, Advice: len(res.Advice), Message: res.Message}
	var planID *int64
	var hash *string
	if stored.ID > 0 {
		planID, hash = &stored.ID, &stored.PlanHash
		sv.PlanID, sv.PlanHash = stored.ID, stored.PlanHash
	}
	for _, st := range res.Steps {
		if st.Origin == OriginModel {
			sv.ModelSteps++
		} else {
			sv.GeneratorSteps++
		}
	}
	_, err := tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.review_plan (run_id, plan_id,
		plan_hash, generator_steps, model_steps, hypotheses, advice, message)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, res.RunID, planID, hash, sv.GeneratorSteps,
		sv.ModelSteps, res.Hypotheses, sv.Advice, clip(res.Message, 1000))
	if err != nil {
		return Saved{}, fmt.Errorf("insert into sage.review_plan: %w", err)
	}
	if err := recordOrigins(ctx, tx, res, stored); err != nil {
		return Saved{}, err
	}
	return sv, recordAdvice(ctx, tx, res)
}

func recordOrigins(ctx context.Context, tx pgx.Tx, res Result, stored changeplan.Plan) error {
	byOrdinal := map[int]int64{}
	for _, s := range stored.Steps {
		byOrdinal[s.Ordinal] = s.ID
	}
	for _, st := range res.Steps {
		id, ok := byOrdinal[st.Ordinal]
		if !ok {
			return fmt.Errorf("the stored plan %d has no step %d", stored.ID, st.Ordinal)
		}
		prediction, err := json.Marshal(st.Prediction)
		if err != nil {
			return fmt.Errorf("encode step %d's prediction: %w", st.Ordinal, err)
		}
		measurement, err := json.Marshal(st.Measurement)
		if err != nil {
			return fmt.Errorf("encode step %d's measurement: %w", st.Ordinal, err)
		}
		_, err = tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.plan_step_origin (step_id,
			plan_id, run_id, origin, origin_ref, evidence_ids, prediction, measurement)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, stored.ID, res.RunID, st.Origin,
			clip(st.OriginRef, 200), nonNil(st.EvidenceIDs), prediction, measurement)
		if err != nil {
			return fmt.Errorf("insert into sage.plan_step_origin (step %d): %w", st.Ordinal,
				err)
		}
	}
	return nil
}

func recordAdvice(ctx context.Context, tx pgx.Tx, res Result) error {
	for i, a := range res.Advice {
		_, err := tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.review_advice (run_id,
			ordinal, origin, origin_ref, kind, target, title, reason, detail, evidence_ids)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, res.RunID, i+1, a.Origin,
			clip(a.OriginRef, 200), clip(a.Kind, 100), clip(a.Target, 1000),
			clip(a.Title, 1000), a.Reason, clip(a.Detail, 2000), nonNil(a.EvidenceIDs))
		if err != nil {
			return fmt.Errorf("insert into sage.review_advice (row %d): %w", i+1, err)
		}
	}
	return nil
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
