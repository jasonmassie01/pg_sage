package planfresh

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// Plan sources.
const (
	SourceCache   = "explain_cache"
	SourceGeneric = "generic_plan"
	SourceExplain = "explain"
	SourceNone    = "none"
)

// Plan is a statement's plan and where it came from.
type Plan struct {
	Source     string // SourceCache, SourceGeneric, SourceExplain or SourceNone
	Capture    string // the cache row's source when Source is SourceCache
	JSON       []byte
	CapturedAt time.Time
	HasActuals bool
	// StaleReason says why the newest cached plan was not used ("" when
	// it was, or when there was none).
	StaleReason string
	// StaleJSON is the newest cached plan when none was fresh. Only the
	// relations it names may be read (the tuner's deferral gate: a
	// statement reads the same tables after an ANALYZE); its shape never
	// drives a symptom, a hint or the agent.
	StaleJSON []byte
}

// Provenance is the plan's source, capture time and age at now, for tool
// results and finding detail.
func (p Plan) Provenance(now time.Time) map[string]any {
	out := map[string]any{"source": p.Source}
	if p.Capture != "" {
		out["capture"] = p.Capture
	}
	if p.StaleReason != "" {
		out["stale_reason"] = p.StaleReason
	}
	if p.Source == SourceNone || p.CapturedAt.IsZero() {
		return out
	}
	out["captured_at"] = p.CapturedAt.UTC().Format(time.RFC3339)
	out["age_seconds"] = max(int64(now.Sub(p.CapturedAt)/time.Second), 0)
	return out
}

// ExplainFunc plans text afresh with a bounded plain EXPLAIN (never
// ANALYZE). It returns SourceNone when the statement cannot be planned.
type ExplainFunc func(ctx context.Context, text string) (Plan, error)

// Reader reads a statement's newest fresh plan.
type Reader struct {
	// DB runs the bounded reads (a catalogread.Reader).
	DB     catalogread.Querier
	MaxAge time.Duration
	// Explain replaces a stale cached plan; nil means no fallback.
	Explain ExplainFunc
	// ExplainUncached also plans a statement that has no cached plan.
	ExplainUncached bool
	Now             func() time.Time
}

// maxCaptures bounds how many of a statement's newest cached plans are
// judged before falling back to EXPLAIN.
const maxCaptures = 3

const capturesSQL = `/* pg_sage */
SELECT plan_json::text, source, captured_at, analyze_enabled
FROM sage.explain_cache
WHERE queryid = $1 AND source <> 'lakebase_plan_history'
ORDER BY captured_at DESC
LIMIT $2`

// Read returns the newest fresh cached plan of queryID; when every cached
// plan is stale, one fresh plan of text from Explain. A failing Explain is
// an error: the stale plan is never returned in its place.
func (r Reader) Read(ctx context.Context, queryID int64, text string) (Plan, error) {
	caps, err := r.captures(ctx, queryID)
	if err != nil {
		return Plan{}, fmt.Errorf("read explain cache for queryid %d: %w", queryID, err)
	}
	now := r.now()
	reason := ""
	for _, c := range caps {
		v := r.judge(ctx, c, now)
		if v.Fresh {
			return Plan{Source: SourceCache, Capture: c.Source, JSON: c.JSON,
				CapturedAt: c.CapturedAt, HasActuals: HasActuals(c)}, nil
		}
		if reason == "" {
			reason = v.Reason
		}
	}
	var staleJSON []byte
	if len(caps) > 0 {
		staleJSON = caps[0].JSON
	}
	if r.Explain == nil || (len(caps) == 0 && !r.ExplainUncached) {
		return Plan{Source: SourceNone, StaleReason: reason, StaleJSON: staleJSON}, nil
	}
	p, err := r.Explain(ctx, text)
	if err != nil {
		return Plan{}, fmt.Errorf("plan queryid %d afresh: %w", queryID, err)
	}
	// A plain EXPLAIN never carries actuals, whatever its JSON holds.
	p.HasActuals, p.StaleReason, p.StaleJSON = false, reason, staleJSON
	if p.Source != SourceNone && p.CapturedAt.IsZero() {
		p.CapturedAt = now
	}
	return p, nil
}

func (r Reader) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r Reader) captures(ctx context.Context, queryID int64) ([]Capture, error) {
	rows, err := r.DB.Query(ctx, capturesSQL, queryID, maxCaptures)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Capture
	for rows.Next() {
		var c Capture
		var plan string
		if err := rows.Scan(&plan, &c.Source, &c.CapturedAt, &c.AnalyzeEnabled); err != nil {
			return nil, err
		}
		c.JSON = []byte(plan)
		out = append(out, c)
	}
	return out, rows.Err()
}

// judge applies Fresh to c, reading the events of the relations c names
// only when the age rule passes. An unreadable plan or an unreadable
// catalog makes c stale.
func (r Reader) judge(ctx context.Context, c Capture, now time.Time) Verdict {
	if v := Fresh(c, Events{}, now, r.MaxAge); !v.Fresh {
		return v
	}
	names, err := PlanNames(c.JSON)
	if err != nil {
		return stale("the cached plan is unreadable: %v", err)
	}
	if names.Empty() {
		return Verdict{Fresh: true}
	}
	ev, err := r.events(ctx, names, c.CapturedAt)
	if err != nil {
		return stale("the plan's relation history could not be read: %v", err)
	}
	return Fresh(c, ev, now, r.MaxAge)
}
