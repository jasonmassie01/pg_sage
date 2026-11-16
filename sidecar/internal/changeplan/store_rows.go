package changeplan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const planColumns = `id, database_id, goal, scope, origin, origin_ref,
	COALESCE(pack_hash, ''), model, prompt_version, status, plan_hash, created_at,
	updated_at, created_by, pause_reason`

const stepColumns = `id, ordinal, kind, action_type, setting, proposed_sql, rollback_sql,
	finding_id, evidence_ids, target_queryids, metric, expected_direction,
	expected_pct::float8, expected_source, guardrails, min_evidence, deadline_minutes,
	checkpoint, on_improved, on_no_change, on_regressed, on_inconclusive,
	COALESCE(on_unverifiable, ''), on_expired, state, queue_id, action_log_id, decision_id,
	COALESCE(verdict, ''), verdict_at, state_reason, COALESCE(claimed_by, ''),
	claimed_until, attempt, executed_at, waiting_since`

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	var scope []byte
	var origin, status string
	err := row.Scan(&p.ID, &p.DatabaseID, &p.Goal, &scope, &origin, &p.OriginRef, &p.PackHash,
		&p.Model, &p.PromptVersion, &status, &p.PlanHash, &p.CreatedAt, &p.UpdatedAt,
		&p.CreatedBy, &p.PauseReason)
	if err != nil {
		return Plan{}, err
	}
	p.Origin, p.Status = Origin(origin), Status(status)
	if err := json.Unmarshal(scope, &p.Scope); err != nil {
		return Plan{}, fmt.Errorf("decode the scope of plan %d: %w", p.ID, err)
	}
	return p, nil
}

func scanStep(rows pgx.Rows) (Step, error) {
	var s Step
	var kind, metric, dir, source, state, verdict string
	var acts [6]string
	var guards, minEv []byte
	err := rows.Scan(&s.ID, &s.Ordinal, &kind, &s.ActionType, &s.Setting, &s.ProposedSQL,
		&s.RollbackSQL, &s.FindingID, &s.EvidenceIDs, &s.TargetQueryIDs, &metric, &dir,
		&s.ExpectedPct, &source, &guards, &minEv, &s.DeadlineMinutes, &s.Checkpoint,
		&acts[0], &acts[1], &acts[2], &acts[3], &acts[4], &acts[5], &state,
		&s.Runtime.QueueID, &s.Runtime.ActionLogID, &s.Runtime.DecisionID, &verdict,
		&s.Runtime.VerdictAt, &s.Runtime.StateReason, &s.Runtime.ClaimedBy,
		&s.Runtime.ClaimedUntil, &s.Runtime.Attempt, &s.Runtime.ExecutedAt,
		&s.Runtime.WaitingSince)
	if err != nil {
		return Step{}, err
	}
	s.Kind, s.Metric, s.ExpectedDirection = StepKind(kind), Metric(metric), Direction(dir)
	s.ExpectedSource, s.Runtime.State = ExpectedSource(source), StepState(state)
	s.Runtime.Verdict = Outcome(verdict)
	s.OnImproved, s.OnNoChange = OutcomeAction(acts[0]), OutcomeAction(acts[1])
	s.OnRegressed, s.OnInconclusive = OutcomeAction(acts[2]), OutcomeAction(acts[3])
	s.OnUnverifiable, s.OnExpired = OutcomeAction(acts[4]), OutcomeAction(acts[5])
	if err := json.Unmarshal(guards, &s.Guardrails); err != nil {
		return Step{}, fmt.Errorf("decode the guardrails of step %d: %w", s.Ordinal, err)
	}
	if err := json.Unmarshal(minEv, &s.MinEvidence); err != nil {
		return Step{}, fmt.Errorf("decode the minimum evidence of step %d: %w", s.Ordinal, err)
	}
	if len(s.Guardrails) == 0 {
		s.Guardrails = nil
	}
	return s, nil
}

// loadPlan reads plan id, its steps in ordinal order and its edges.
func loadPlan(ctx context.Context, q querier, id int64) (Plan, error) {
	p, err := scanPlan(q.QueryRow(ctx, `/* pg_sage */ SELECT `+planColumns+`
		  FROM sage.plan WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, fmt.Errorf("%w: plan %d", ErrNotFound, id)
	}
	if err != nil {
		return Plan{}, err
	}
	if p.Steps, err = loadSteps(ctx, q, id); err != nil {
		return Plan{}, err
	}
	if p.Edges, err = loadEdges(ctx, q, id); err != nil {
		return Plan{}, err
	}
	return p, nil
}

func loadSteps(ctx context.Context, q querier, id int64) ([]Step, error) {
	rows, err := q.Query(ctx, `/* pg_sage */ SELECT `+stepColumns+`
		  FROM sage.plan_step WHERE plan_id = $1 ORDER BY ordinal`, id)
	if err != nil {
		return nil, fmt.Errorf("read the steps: %w", err)
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		s, err := scanStep(rows)
		if err != nil {
			return nil, fmt.Errorf("read the steps: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func loadEdges(ctx context.Context, q querier, id int64) ([]Edge, error) {
	rows, err := q.Query(ctx, `/* pg_sage */ SELECT f.ordinal, t.ordinal
		  FROM sage.plan_step_edge e
		  JOIN sage.plan_step f ON f.id = e.from_step_id
		  JOIN sage.plan_step t ON t.id = e.to_step_id
		 WHERE e.plan_id = $1 ORDER BY f.ordinal, t.ordinal`, id)
	if err != nil {
		return nil, fmt.Errorf("read the edges: %w", err)
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.From, &e.To); err != nil {
			return nil, fmt.Errorf("read the edges: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func insertPlan(ctx context.Context, tx pgx.Tx, p Plan) (int64, error) {
	scope, err := json.Marshal(p.Scope)
	if err != nil {
		return 0, fmt.Errorf("encode the scope: %w", err)
	}
	var id int64
	err = tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.plan (database_id, goal, scope,
		origin, origin_ref, pack_hash, model, prompt_version, status, plan_hash, created_by)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11) RETURNING id`,
		p.DatabaseID, p.Goal, scope, string(p.Origin), p.OriginRef, p.PackHash, p.Model,
		p.PromptVersion, string(p.Status), p.PlanHash, p.CreatedBy).Scan(&id)
	if err != nil {
		return 0, err
	}
	for _, s := range p.Steps {
		if err := insertStep(ctx, tx, id, s); err != nil {
			return 0, err
		}
	}
	return id, insertEdges(ctx, tx, id, p.Edges)
}

const insertStepSQL = `/* pg_sage */ INSERT INTO sage.plan_step (plan_id, ordinal, kind,
	action_type, setting, proposed_sql, rollback_sql, finding_id, evidence_ids,
	target_queryids, metric, expected_direction, expected_pct, expected_source, guardrails,
	min_evidence, deadline_minutes, checkpoint, on_improved, on_no_change, on_regressed,
	on_inconclusive, on_unverifiable, on_expired)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
	        $19, $20, $21, $22, NULLIF($23, ''), $24)`

func insertStep(ctx context.Context, tx pgx.Tx, planID int64, s Step) error {
	args, err := stepArgs(s)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, insertStepSQL, append([]any{planID}, args...)...)
	if err != nil {
		return fmt.Errorf("store step %d: %w", s.Ordinal, err)
	}
	return nil
}

// stepArgs are the hashed columns of s, ordinal first, in insertStepSQL's
// order from $2.
func stepArgs(s Step) ([]any, error) {
	guards := s.Guardrails
	if guards == nil {
		guards = []Guardrail{}
	}
	g, err := json.Marshal(guards)
	if err != nil {
		return nil, fmt.Errorf("encode the guardrails of step %d: %w", s.Ordinal, err)
	}
	m, err := json.Marshal(s.MinEvidence)
	if err != nil {
		return nil, fmt.Errorf("encode the minimum evidence of step %d: %w", s.Ordinal, err)
	}
	return []any{s.Ordinal, string(s.Kind), s.ActionType, s.Setting, s.ProposedSQL,
		s.RollbackSQL, s.FindingID, nilIfEmpty(s.EvidenceIDs), nilIfEmpty(s.TargetQueryIDs),
		string(s.Metric), string(s.ExpectedDirection), s.ExpectedPct,
		string(s.ExpectedSource), g, m, s.DeadlineMinutes, s.Checkpoint,
		string(s.OnImproved), string(s.OnNoChange), string(s.OnRegressed),
		string(s.OnInconclusive), string(s.OnUnverifiable), string(s.OnExpired)}, nil
}

// nilIfEmpty stores an empty list as NULL, so it reads back as nil.
func nilIfEmpty[T any](list []T) []T {
	if len(list) == 0 {
		return nil
	}
	return list
}

func insertEdges(ctx context.Context, tx pgx.Tx, planID int64, edges []Edge) error {
	for _, e := range edges {
		tag, err := tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.plan_step_edge (plan_id,
			from_step_id, to_step_id)
			SELECT $1, f.id, t.id FROM sage.plan_step f, sage.plan_step t
			 WHERE f.plan_id = $1 AND f.ordinal = $2 AND t.plan_id = $1 AND t.ordinal = $3`,
			planID, e.From, e.To)
		if err != nil {
			return fmt.Errorf("store edge %d->%d: %w", e.From, e.To, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: edge %d->%d names a step the plan lacks", ErrInvalid,
				e.From, e.To)
		}
	}
	return nil
}
