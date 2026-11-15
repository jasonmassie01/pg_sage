package verify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/querystore"
)

// The mechanism gate's evidence (lab-01b): plan fingerprints from the
// query store samples (querystore.PlanFlips, in the history store) and the
// newest plans from sage.explain_cache on the monitored database. Lakebase
// plan history is left out, as the fingerprint copy leaves it out
// (querystore.latestPlanHashesSQL); on NEON and Lakebase the collector's
// generic EXPLAIN is the evidence.

// latestPlansSQL reads each target's newest plan captured after the step.
const latestPlansSQL = `/* pg_sage */ SELECT q.id, p.plan_json::text
	FROM unnest($1::int8[]) AS q(id)
	JOIN LATERAL (
		SELECT e.plan_json FROM sage.explain_cache e
		WHERE e.queryid = q.id AND e.source <> 'lakebase_plan_history'
		  AND e.captured_at > $2 AND e.captured_at <= $3
		ORDER BY e.captured_at DESC, e.id DESC LIMIT 1) p ON true`

// PlanEvidence is the gate's evidence: per target, the fingerprint at or
// before the step and the first different one after it, and the newest
// plan captured after the step.
type PlanEvidence struct {
	Hashes map[int64][]PlanSample
	Plans  map[int64][]byte
}

// PlanEvidence reads the evidence of ids for a step at executedAt, up to
// to.
func (s *PostgresObservationSource) PlanEvidence(ctx context.Context, ids []int64,
	executedAt, to time.Time) (PlanEvidence, error) {
	ev := PlanEvidence{Hashes: map[int64][]PlanSample{}, Plans: map[int64][]byte{}}
	if s == nil || s.queryer == nil {
		return ev, errors.New("verify observation pool is unavailable")
	}
	if len(ids) == 0 {
		return ev, nil
	}
	if err := s.readPlanFlips(ctx, ids, executedAt, to, ev.Hashes); err != nil {
		return ev, err
	}
	rows, err := s.queryer.Query(ctx, latestPlansSQL, ids, executedAt, to)
	if err != nil {
		return ev, fmt.Errorf("read latest plans from sage.explain_cache: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var plan string
		if err := rows.Scan(&id, &plan); err != nil {
			return ev, fmt.Errorf("scan latest plan from sage.explain_cache: %w", err)
		}
		ev.Plans[id] = []byte(plan)
	}
	if err := rows.Err(); err != nil {
		return ev, fmt.Errorf("read latest plans from sage.explain_cache: %w", err)
	}
	return ev, nil
}

func (s *PostgresObservationSource) readPlanFlips(ctx context.Context, ids []int64,
	executedAt, to time.Time, out map[int64][]PlanSample) error {
	flips, err := querystore.PlanFlips(ctx, s.historyStore(), ids, executedAt, to)
	if err != nil {
		return fmt.Errorf("read plan fingerprints: %w", err)
	}
	for id, f := range flips {
		if f.Before != "" {
			out[id] = append(out[id], PlanSample{At: f.BeforeAt, Hash: f.Before})
		}
		if f.After != "" {
			out[id] = append(out[id], PlanSample{At: f.AfterAt, Hash: f.After})
		}
	}
	return nil
}

// CheckMechanism reads the evidence of an index step (in.ExecutedAt to
// to) and applies the gate. A read that fails is mechanism_unavailable
// with its cause, never an error and never a revert; an invalid input is
// the caller's error. A setting needs no read.
func (s *PostgresObservationSource) CheckMechanism(ctx context.Context, in MechanismInput,
	to time.Time) (Mechanism, error) {
	if err := validateMechanism(in); err != nil {
		return Mechanism{}, err
	}
	if indexClass(in.Class) {
		ev, err := s.PlanEvidence(ctx, in.Targets, in.ExecutedAt, to)
		if err != nil {
			return MechanismUnavailable(err), nil
		}
		in.PlanHashes, in.LatestPlans = ev.Hashes, ev.Plans
	}
	return MechanismCheck(in)
}
