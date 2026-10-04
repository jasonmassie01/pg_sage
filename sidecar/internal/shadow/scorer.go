package shadow

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Scorer scores one monitored database's pending shadow decisions from
// what happened since they were recorded (judge). A score is written
// once: pending -> scored, guarded by the status, so concurrent scorers
// score a decision once.
type Scorer struct {
	pool *pgxpool.Pool
	opts Options
	logf func(string, ...any)
}

// Result counts one pass: decisions examined, still waiting, and scored
// by source.
type Result struct {
	Examined int            `json:"examined"`
	Waiting  int            `json:"waiting"`
	Scored   map[string]int `json:"scored"`
}

// NewScorer scores the shadow ledger behind pool.
func NewScorer(pool *pgxpool.Pool, opts Options, logf func(string, ...any)) *Scorer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Scorer{pool: pool, opts: opts.normalized(), logf: logf}
}

// pendingDecision is a pending decision with its age and the database's
// clock when it was read.
type pendingDecision struct {
	Decision
	age time.Duration
	now time.Time
}

// RunOnce scores what it can of the oldest pending decisions.
func (s *Scorer) RunOnce(ctx context.Context) (Result, error) {
	res := Result{Scored: map[string]int{}}
	if s == nil || s.pool == nil {
		return res, ErrUnavailable
	}
	pending, err := s.pending(ctx)
	if err != nil || len(pending) == 0 {
		return res, err
	}
	since := pending[0].RecordedAt
	queue, err := s.queueFacts(ctx, since)
	if err != nil {
		return res, err
	}
	actions, err := s.actionFacts(ctx, since)
	if err != nil {
		return res, err
	}
	w := newWhatIf(s, s.opts.HypoPGBudget)
	for _, p := range pending {
		res.Examined++
		ev, err := s.evidence(ctx, p, queue, actions, w)
		if err != nil {
			return res, err
		}
		j := judge(p.Decision, ev, p.age, s.opts)
		if j.Wait {
			res.Waiting++
			continue
		}
		written, err := s.write(ctx, p.Decision, j)
		if err != nil {
			return res, err
		}
		if written {
			res.Scored[j.Source]++
			countScore(s.opts.Database, p.Class, j.Score, j.Source)
		}
	}
	return res, nil
}

// evidence gathers every source for p; the costlier ones (catalog and
// verification reads, what-if) only when no cheaper one decided.
func (s *Scorer) evidence(ctx context.Context, p pendingDecision, queue []queueRow,
	actions []actionRow, w *whatIf) (facts, error) {
	ev := facts{queue: matchQueue(queue, p.Decision), action: matchAction(actions, p.Decision)}
	if ev.queue != nil || ev.action != nil {
		return ev, nil
	}
	ext, err := s.external(ctx, p)
	if err != nil || ext != nil {
		ev.external = ext
		return ev, err
	}
	if p.Class == "index_create" && p.age >= s.opts.ScoreAfter {
		ev.hypo = w.evaluate(ctx, p)
	}
	return ev, nil
}

const pendingSQL = `/* pg_sage */ SELECT ` + decisionColumns + `,
	extract(epoch FROM now() - recorded_at)::float8, now()
	FROM sage.shadow_decision WHERE status = 'pending'
	ORDER BY recorded_at, id LIMIT $1`

func (s *Scorer) pending(ctx context.Context) ([]pendingDecision, error) {
	rows, err := s.pool.Query(ctx, pendingSQL, s.opts.Batch)
	if err != nil {
		return nil, fmt.Errorf("shadow: read pending decisions: %w", err)
	}
	defer rows.Close()
	var out []pendingDecision
	for rows.Next() {
		var p pendingDecision
		var ageSeconds float64
		d, err := scanDecision(rows, &ageSeconds, &p.now)
		if err != nil {
			return nil, fmt.Errorf("shadow: scan pending decision: %w", err)
		}
		p.Decision = d
		p.age = time.Duration(ageSeconds * float64(time.Second))
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shadow: read pending decisions: %w", err)
	}
	return out, nil
}

const writeScoreSQL = `/* pg_sage */ UPDATE sage.shadow_decision
	SET status = 'scored', score = $2, score_source = $3, counted = $4,
	    score_reason = $5, score_detail = $6, ref_action_log_id = NULLIF($7::bigint, 0),
	    ref_queue_id = NULLIF($8::bigint, 0), scored_at = now()
	WHERE id = $1 AND status = 'pending'`

// write records j's score; false when another scorer scored it first.
func (s *Scorer) write(ctx context.Context, d Decision, j judgement) (bool, error) {
	detail := j.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	tag, err := s.pool.Exec(ctx, writeScoreSQL, d.ID, j.Score, j.Source, j.Counted,
		truncate(j.Reason, 2000), detail, j.RefActionLogID, j.RefQueueID)
	if err != nil {
		return false, fmt.Errorf("shadow: score decision %d: %w", d.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// whatIf runs the what-if of index creates within a per-pass budget.
type whatIf struct {
	s      *Scorer
	budget int
	hypo   *optimizer.HypoPG
}

func newWhatIf(s *Scorer, budget int) *whatIf {
	return &whatIf{s: s, budget: budget,
		hypo: optimizer.NewHypoPG(s.pool, func(component, format string, args ...any) {
			s.logf(component+": "+format, args...)
		})}
}
