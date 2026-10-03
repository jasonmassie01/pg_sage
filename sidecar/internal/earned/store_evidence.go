package earned

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	SourceExecutor = "executor"
	SourceOperator = "operator"
	SourceRollout  = "rollout"
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

// ShadowStats counts the database's packet reviews of a family by a
// person since since, and the first such review ever. Reviews recorded
// through MCP are kept but are not evidence.
func (s *PostgresStore) ShadowStats(ctx context.Context, f Family, since time.Time) (Shadow,
	error) {
	var sh Shadow
	var first *time.Time
	err := s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE reviewed_at >= $3),
		count(*) FILTER (WHERE reviewed_at >= $3 AND verdict = 'accepted'),
		min(reviewed_at)
		FROM sage.sre_packet_reviews
		WHERE deployment_id = $1 AND database_name = $4 AND family = $2
		  AND counts_as_evidence`,
		s.deployment, string(f), since, s.database).Scan(&sh.Reviewed, &sh.Accepted,
		&first)
	if first != nil {
		sh.FirstReviewAt = first.UTC()
	}
	return sh, storeErr("read shadow record", err)
}

// insertOutcome records an outcome; false when that action's outcome
// from that source is already recorded.
func (s *PostgresStore) insertOutcome(ctx context.Context, o Outcome) (bool, error) {
	if err := s.checkDatabase(o.Database); err != nil {
		return false, err
	}
	var actionLogID any
	if o.ActionLogID > 0 {
		actionLogID = o.ActionLogID
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_outcomes
		(deployment_id, database_name, action_log_id, family, action_class, level, result,
		 source, actor, detail, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11)
		ON CONFLICT DO NOTHING`,
		s.deployment, o.Database, actionLogID, string(o.Family), string(o.Class),
		int16(o.Level), o.Result, o.Source, o.Actor, o.Detail, o.At)
	if err != nil {
		return false, storeErr("record autonomy outcome", err)
	}
	return tag.RowsAffected() == 1, nil
}

// LiveStats counts the database's verified L2 recoveries, unverified and
// harmful outcomes of a pair.
func (s *PostgresStore) LiveStats(ctx context.Context, f Family, c ActionClass) (Live,
	error) {
	var l Live
	err := s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE result = 'verified_recovery' AND level = 2),
		count(*) FILTER (WHERE result IN ('harmful', 'safety_violation')),
		count(*) FILTER (WHERE result = 'unverified')
		FROM sage.sre_autonomy_outcomes
		WHERE deployment_id = $1 AND database_name = $2 AND family = $3
		  AND action_class = $4`,
		s.deployment, s.database, string(f), string(c)).Scan(&l.VerifiedL2,
		&l.HarmfulPair, &l.Unverified)
	return l, storeErr("read live record", err)
}

// FamilyViolations counts the database's harmful or unsafe outcomes of a
// family since since.
func (s *PostgresStore) FamilyViolations(ctx context.Context, f Family,
	since time.Time) (int, error) {
	n, _, err := s.familySafety(ctx, f, since)
	return n, err
}

// familySafety counts the database's harmful or unsafe outcomes of a
// family since since, with the newest one's time (zero without any).
func (s *PostgresStore) familySafety(ctx context.Context, f Family, since time.Time) (int,
	time.Time, error) {
	var n int
	var last *time.Time
	err := s.pool.QueryRow(ctx, `SELECT count(*), max(recorded_at)
		FROM sage.sre_autonomy_outcomes
		WHERE deployment_id = $1 AND database_name = $2 AND family = $3
		  AND recorded_at >= $4 AND result IN ('harmful', 'safety_violation')`,
		s.deployment, s.database, string(f), since).Scan(&n, &last)
	if err != nil || last == nil {
		return n, time.Time{}, storeErr("read family safety record", err)
	}
	return n, last.UTC(), nil
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
		 pg_sage_commit, signature)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10, $11, $12, $13, $14,
		        $15)
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
	var signature []byte
	if run.Signature != nil {
		if signature, err = json.Marshal(run.Signature); err != nil {
			return nil, fmt.Errorf("%w: encode signature: %v", ErrInvalidReport, err)
		}
	}
	return []any{run.ID, run.Source, run.Schema, run.GeneratedAt, run.IngestedAt,
		run.IngestedBy, run.Database, sum, gated, cells, run.Origin, run.Build.Version,
		run.Build.Commit, signature}, nil
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
	cells, origin, pg_sage_version, pg_sage_commit, signature FROM sage.sre_eval_runs`

func (s *PostgresStore) scanEvalRun(row pgx.Row) (EvalRun, error) {
	var run EvalRun
	var gated, cells, signature []byte
	err := row.Scan(&run.ID, &run.Source, &run.Schema, &run.GeneratedAt, &run.IngestedAt,
		&run.IngestedBy, &run.Database, &run.SHA256, &gated, &cells, &run.Origin,
		&run.Build.Version, &run.Build.Commit, &signature)
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

// LatestBench is the deployment's newest bench report that counts for
// the running build, or nil; with a family, the newest such report that
// scored that family. A report stamped for another build never counts;
// an unstamped (operator) report does. Bench evidence is about pg_sage,
// not a database, so every database of the deployment shares it.
func (s *PostgresStore) LatestBench(ctx context.Context, f Family) (*EvalRun, error) {
	b := s.runningBuild()
	run, err := s.scanEvalRun(s.pool.QueryRow(ctx, evalRunSelect+
		` WHERE deployment_id = $1 AND source = 'bench'
		  AND ($2 = '' OR cells @> jsonb_build_array(jsonb_build_object('family', $2::text)))
		  AND ((pg_sage_version = '' AND pg_sage_commit = '')
		    OR ($3 <> '' AND pg_sage_commit = $3)
		    OR (($3 = '' OR pg_sage_commit = '') AND $4 <> '' AND pg_sage_version = $4))
		  ORDER BY generated_at DESC, ingested_at DESC LIMIT 1`, s.deployment, string(f),
		b.Commit, b.Version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, storeErr("read latest bench report", err)
	}
	return &run, nil
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
