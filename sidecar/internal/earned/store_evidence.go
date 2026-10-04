package earned

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Outcome results and sources.
const (
	ResultVerifiedRecovery = "verified_recovery"
	ResultNotRecovered     = "not_recovered"
	ResultHarmful          = "harmful"
	ResultSafetyViolation  = "safety_violation"
	// ResultUnverified is an action that succeeded but whose effect no
	// verification confirmed (P0-6): recorded, never promotion credit.
	ResultUnverified = "unverified"
	// ResultRejected is an operator's rejection of a proposed action, or
	// an operator's rollback of an executed one (roadmap 1.2): a demerit.
	ResultRejected = "rejected"

	SourceExecutor = "executor"
	SourceOperator = "operator"
	SourceRollout  = "rollout"
	// SourceRollback records an executed action an operator rolled back.
	SourceRollback = "rollback"
)

// Outcome is one live (or game-day) result of a family action.
type Outcome struct {
	Database    string      `json:"database"`
	ActionLogID int64       `json:"action_log_id,omitempty"`
	Family      Family      `json:"family"`
	Class       ActionClass `json:"class"`
	Level       Level       `json:"level"`
	Result      string      `json:"result"`
	Source      string      `json:"source"`
	Actor       string      `json:"actor"`
	Detail      string      `json:"detail,omitempty"`
	At          time.Time   `json:"at"`
	// Verdict is the raw verdict behind a reconciled outcome (a
	// sage.action_outcome verdict, rolled_back or rejected); ObservedAt is
	// when the monitored database observed it; QueueID names a rejected
	// approval item. A demerit observed before the pair's level was set
	// was already known and does not demote it again.
	Verdict    string     `json:"verdict,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	QueueID    int64      `json:"queue_id,omitempty"`
}

// Review verdicts.
const (
	VerdictAccepted = "accepted"
	VerdictRejected = "rejected"
)

// Review is an operator's verdict on one investigation packet: the
// shadow record behind L2.
type Review struct {
	Database        string    `json:"database"`
	InvestigationID string    `json:"investigation_id"`
	Family          Family    `json:"family"`
	Verdict         string    `json:"verdict"`
	Reviewer        string    `json:"reviewer"`
	Note            string    `json:"note,omitempty"`
	At              time.Time `json:"at"`
}

// upsertReview records (or replaces) the verdict on a packet. Only a
// person's review counts as evidence, and a review that does not count
// never replaces one that does (ErrConflict).
func (s *PostgresStore) upsertReview(ctx context.Context, r Review) error {
	if err := s.checkDatabase(r.Database); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO sage.sre_packet_reviews
		(deployment_id, database_name, investigation_id, family, verdict, reviewer, note,
		 reviewed_at, counts_as_evidence)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9)
		ON CONFLICT (deployment_id, database_name, investigation_id) DO UPDATE
		SET family = EXCLUDED.family, verdict = EXCLUDED.verdict,
		    reviewer = EXCLUDED.reviewer, note = EXCLUDED.note,
		    reviewed_at = EXCLUDED.reviewed_at,
		    counts_as_evidence = EXCLUDED.counts_as_evidence
		WHERE EXCLUDED.counts_as_evidence OR NOT sre_packet_reviews.counts_as_evidence`,
		s.deployment, r.Database, r.InvestigationID, string(r.Family), r.Verdict,
		r.Reviewer, r.Note, r.At, ReviewCountsAsEvidence(r.Reviewer))
	if err != nil {
		return storeErr("record packet review", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: a person already reviewed investigation %s; a review "+
			"through MCP cannot replace it", ErrConflict, r.InvestigationID)
	}
	return nil
}

// insertOutcome records an outcome; false when that action's outcome
// from that source is already recorded.
func (s *PostgresStore) insertOutcome(ctx context.Context, o Outcome) (bool, error) {
	if err := s.checkDatabase(o.Database); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_outcomes
		(deployment_id, database_name, action_log_id, family, action_class, level, result,
		 source, actor, detail, recorded_at, verdict, observed_at, queue_id)
		VALUES ($1, $2, NULLIF($3::bigint, 0), $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11,
		        NULLIF($12, ''), $13, NULLIF($14::bigint, 0))
		ON CONFLICT DO NOTHING`,
		s.deployment, o.Database, o.ActionLogID, string(o.Family), string(o.Class),
		int16(o.Level), o.Result, o.Source, o.Actor, o.Detail, o.At, o.Verdict,
		o.ObservedAt, o.QueueID)
	if err != nil {
		return false, storeErr("record autonomy outcome", err)
	}
	return tag.RowsAffected() == 1, nil
}

// insertEvalRun stores a parsed report with its provenance; a report
// already stored (same hash) is returned with Duplicate set, and marked
// signed when it arrives signed now.
func (s *PostgresStore) insertEvalRun(ctx context.Context, run EvalRun) (EvalRun, error) {
	if run.Source == SourceGameDay {
		if err := s.checkDatabase(run.Database); err != nil {
			return EvalRun{}, err
		}
	}
	args, err := evalRunArgs(run)
	if err != nil {
		return EvalRun{}, err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO sage.sre_eval_runs
		(deployment_id, id, source, schema_version, generated_at, ingested_at, ingested_by,
		 database_name, report_sha256, gated_arms, cells, origin, pg_sage_version,
		 pg_sage_commit, signature, model_lift)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10, $11, $12, $13, $14,
		        $15, $16)
		ON CONFLICT (deployment_id, report_sha256) DO NOTHING`,
		append([]any{s.deployment}, args...)...)
	if err != nil {
		return EvalRun{}, storeErr("store eval run", err)
	}
	if tag.RowsAffected() == 1 {
		return run, nil
	}
	return s.duplicateEvalRun(ctx, run, args[7], args[13])
}

// evalRunArgs are the stored columns of run after the deployment.
func evalRunArgs(run EvalRun) ([]any, error) {
	cells, err := json.Marshal(run.Cells)
	if err != nil {
		return nil, fmt.Errorf("%w: encode cells: %v", ErrInvalidReport, err)
	}
	gated, err := json.Marshal(run.Gated)
	if err != nil {
		return nil, fmt.Errorf("%w: encode gated arms: %v", ErrInvalidReport, err)
	}
	sum, err := hex.DecodeString(run.SHA256)
	if err != nil || len(sum) != 32 {
		return nil, fmt.Errorf("%w: report hash", ErrInvalidReport)
	}
	var signature, lift []byte
	if run.Signature != nil {
		if signature, err = json.Marshal(run.Signature); err != nil {
			return nil, fmt.Errorf("%w: encode signature: %v", ErrInvalidReport, err)
		}
	}
	if run.ModelLift != nil {
		if lift, err = json.Marshal(run.ModelLift); err != nil {
			return nil, fmt.Errorf("%w: encode model lift: %v", ErrInvalidReport, err)
		}
	}
	return []any{run.ID, run.Source, run.Schema, run.GeneratedAt, run.IngestedAt,
		run.IngestedBy, run.Database, sum, gated, cells, run.Origin, run.Build.Version,
		run.Build.Commit, signature, lift}, nil
}

// duplicateEvalRun reads the stored copy of run; an unsigned copy is
// marked signed when run is (never the reverse).
func (s *PostgresStore) duplicateEvalRun(ctx context.Context, run EvalRun, sum,
	signature any) (EvalRun, error) {
	if run.Signature != nil {
		if _, err := s.pool.Exec(ctx, `UPDATE sage.sre_eval_runs
			SET origin = 'signed_release', signature = $3
			WHERE deployment_id = $1 AND report_sha256 = $2 AND source = 'bench'
			  AND signature IS NULL`, s.deployment, sum, signature); err != nil {
			return EvalRun{}, storeErr("mark eval run signed", err)
		}
	}
	existing, err := s.scanEvalRun(s.pool.QueryRow(ctx, evalRunSelect+
		` WHERE deployment_id = $1 AND report_sha256 = $2`, s.deployment, sum))
	existing.Duplicate = true
	return existing, storeErr("read duplicate eval run", err)
}

const evalRunSelect = `SELECT id::text, source, schema_version, generated_at, ingested_at,
	ingested_by, COALESCE(database_name, ''), encode(report_sha256, 'hex'), gated_arms,
	cells, origin, pg_sage_version, pg_sage_commit, signature, model_lift
	FROM sage.sre_eval_runs`

func (s *PostgresStore) scanEvalRun(row pgx.Row) (EvalRun, error) {
	var run EvalRun
	var gated, cells, signature, lift []byte
	err := row.Scan(&run.ID, &run.Source, &run.Schema, &run.GeneratedAt, &run.IngestedAt,
		&run.IngestedBy, &run.Database, &run.SHA256, &gated, &cells, &run.Origin,
		&run.Build.Version, &run.Build.Commit, &signature, &lift)
	if err != nil {
		return EvalRun{}, err
	}
	if err := json.Unmarshal(gated, &run.Gated); err != nil {
		return EvalRun{}, fmt.Errorf("decode gated arms: %w", err)
	}
	if err := json.Unmarshal(cells, &run.Cells); err != nil {
		return EvalRun{}, fmt.Errorf("decode cells: %w", err)
	}
	if signature != nil {
		run.Signature = &ReportSignature{}
		if err := json.Unmarshal(signature, run.Signature); err != nil {
			return EvalRun{}, fmt.Errorf("decode signature: %w", err)
		}
	}
	if lift != nil {
		run.ModelLift = &ModelLift{}
		if err := json.Unmarshal(lift, run.ModelLift); err != nil {
			return EvalRun{}, fmt.Errorf("decode model lift: %w", err)
		}
	}
	return run.WithProvenance(), nil
}

// runningBuild is the build bench reports must score (none: only
// unstamped reports count).
func (s *PostgresStore) runningBuild() Build {
	if s.build == nil {
		return Build{}
	}
	return s.build().Normalized()
}

// GameDayRuns lists the database's game-day reports generated since
// since, newest first.
func (s *PostgresStore) GameDayRuns(ctx context.Context, since time.Time) ([]EvalRun,
	error) {
	rows, err := s.pool.Query(ctx, evalRunSelect+` WHERE deployment_id = $1
		AND source = 'game_day' AND database_name = $3 AND generated_at >= $2
		ORDER BY generated_at DESC LIMIT 200`, s.deployment, since, s.database)
	if err != nil {
		return nil, storeErr("list game-day reports", err)
	}
	defer rows.Close()
	var out []EvalRun
	for rows.Next() {
		run, err := s.scanEvalRun(rows)
		if err != nil {
			return nil, storeErr("scan game-day report", err)
		}
		out = append(out, run)
	}
	return out, storeErr("list game-day reports", rows.Err())
}
