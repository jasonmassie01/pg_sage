package scorecard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/modellift"
)

// The scorecard reads the database's own sage schema: its review runs
// (sage.review_run, sage.review_model_run), each review's plan
// (sage.review_plan, review-03b) and the plan's steps with their origin
// (sage.plan_step, sage.plan_step_origin) and their executor outcome
// (plan-02's link: the step's verdict and state, its live step approval,
// and sage.action_log's rollback). Read-only.

// ErrNoStore is a scorecard read without a database.
var ErrNoStore = errors.New("scorecard: no database to read")

// MaxReviews bounds one read: the newest reviews first.
const MaxReviews = 2000

// Querier is a pool, a connection or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Store reads scorecards.
type Store struct{ db Querier }

// NewStore reads through db.
func NewStore(db Querier) *Store { return &Store{db: db} }

// Read is database's scorecard over its done and partial reviews that
// finished at or after since (the zero time: all). Reviews of a database in
// fixtures, or named by a clone receipt (sage.clone_instances), are marked
// Fixture. An error names the query that failed.
func (s *Store) Read(ctx context.Context, database string, since time.Time,
	fixtures []string) (Scorecard, error) {
	if s == nil || s.db == nil {
		return Scorecard{}, ErrNoStore
	}
	if strings.TrimSpace(database) == "" {
		return Scorecard{}, fmt.Errorf("%w: a scorecard names its database", ErrInvalid)
	}
	runs, err := s.runs(ctx, database, since, fixtures)
	if err != nil {
		return Scorecard{}, err
	}
	ids := make([]int64, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	steps, err := s.steps(ctx, ids)
	if err != nil {
		return Scorecard{}, err
	}
	return Build(database, since, runs, steps)
}

const runsSQL = `/* pg_sage */ SELECT r.id, r.database_name, r.status, r.finished_at,
	COALESCE((SELECT array_agg(DISTINCT m.model ORDER BY m.model)
		FROM sage.review_model_run m
		WHERE m.run_id = r.id AND m.status = 'done' AND m.model <> ''), '{}'::text[]),
	(r.database_name = ANY ($3::text[]) OR EXISTS (SELECT 1 FROM sage.clone_instances c
		WHERE c.name = r.database_name))
	FROM sage.review_run r
	WHERE r.kind = 'review' AND r.status IN ('done', 'partial') AND r.database_name = $1
	  AND r.finished_at >= $2
	ORDER BY r.finished_at DESC, r.id DESC LIMIT $4`

func (s *Store) runs(ctx context.Context, database string, since time.Time,
	fixtures []string) ([]Run, error) {
	names := make([]string, 0, len(fixtures))
	for _, f := range fixtures {
		names = append(names, strings.TrimSpace(f))
	}
	rows, err := s.db.Query(ctx, runsSQL, database, since, names, MaxReviews)
	if err != nil {
		return nil, queryErr("review runs", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var models []string
		if err := rows.Scan(&r.ID, &r.Database, &r.Status, &r.FinishedAt, &models,
			&r.Fixture); err != nil {
			return nil, queryErr("review runs", err)
		}
		r.Models = normalizeModels(models)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, queryErr("review runs", err)
	}
	return out, nil
}

// A step of a model plan without an origin row counts as the model's, as
// changeplan's origin hold does (fail closed). Any rollback the executor
// started counts as a revert, whether it finished, is still running or
// failed: a failed rollback is the worst outcome, never a better one.
const stepsSQL = `/* pg_sage */ SELECT rp.run_id,
	COALESCE(o.origin, CASE p.origin WHEN 'generator' THEN 'generator'
		WHEN 'person' THEN 'person' ELSE 'model' END),
	COALESCE(NULLIF(o.origin_ref, ''), NULLIF(p.model, ''), NULLIF(p.origin_ref, ''),
		p.created_by),
	s.action_type, s.proposed_sql,
	(s.attempt > 0 OR s.executed_at IS NOT NULL OR EXISTS (SELECT 1
		FROM sage.plan_step_approval a WHERE a.step_id = s.id AND a.voided_at IS NULL)),
	COALESCE(s.verdict, ''),
	(s.state IN ('reverting', 'reverted') OR EXISTS (SELECT 1 FROM sage.action_log al
		WHERE al.id = s.action_log_id AND al.outcome IN ('rolling_back', 'rolled_back',
			'rollback_failed', 'reverted')))
	FROM sage.review_plan rp
	JOIN sage.plan p ON p.id = rp.plan_id
	JOIN sage.plan_step s ON s.plan_id = rp.plan_id
	LEFT JOIN sage.plan_step_origin o ON o.step_id = s.id
	WHERE rp.run_id = ANY ($1::bigint[]) AND s.kind <> 'check'
	ORDER BY rp.run_id, s.ordinal`

func (s *Store) steps(ctx context.Context, runs []int64) ([]Step, error) {
	if len(runs) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, stepsSQL, runs)
	if err != nil {
		return nil, queryErr("review steps", err)
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var st Step
		if err := rows.Scan(&st.RunID, &st.Origin, &st.OriginRef, &st.ActionType, &st.SQL,
			&st.Accepted, &st.Verdict, &st.Reverted); err != nil {
			return nil, queryErr("review steps", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, queryErr("review steps", err)
	}
	return out, nil
}

func queryErr(name string, err error) error {
	return fmt.Errorf("scorecard query %q: %w", name, err)
}

// normalizeModels is the distinct normalized model ids, sorted.
func normalizeModels(models []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range models {
		if n := modellift.NormalizeModel(m); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
