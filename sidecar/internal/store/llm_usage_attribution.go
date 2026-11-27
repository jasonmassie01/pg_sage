package store

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-04: the attributed LLM usage ledger shares sage.llm_usage with
// fleet-10's budget counters. Budget rows keep purpose '_total' (and an empty
// model); attribution rows name a purpose and model and only ever add deltas,
// so a restart mid-day keeps the day's totals.

// Sources of an attribution row's token split.
const (
	LLMSourceProvider = "provider"
	LLMSourceEstimate = "estimate"
)

// LLMAttributionRow is one (day, database key, role, purpose, model) row.
// CostUSD is nil when any call in it had no operator price.
type LLMAttributionRow struct {
	Day                                             time.Time
	DatabaseKey, Role, Purpose, Model               string
	PromptTokens, CompletionTokens, ReasoningTokens int64
	Calls, Failures                                 int64
	CostUSD                                         *float64
	Source                                          string
}

var attributionPurpose = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

const addAttributionSQL = `
INSERT INTO sage.llm_usage AS u (day, database_key, role, purpose, model, tokens,
       prompt_tokens, completion_tokens, reasoning_tokens, calls, failures,
       est_cost_usd, source, updated_at)
SELECT r.day, r.database_key, r.role, r.purpose, r.model,
       r.prompt + r.completion, r.prompt, r.completion, r.reasoning, r.calls,
       r.failures, r.cost::numeric, r.source, now()
  FROM unnest($1::date[], $2::text[], $3::text[], $4::text[], $5::text[],
              $6::bigint[], $7::bigint[], $8::bigint[], $9::bigint[], $10::bigint[],
              $11::float8[], $12::text[])
       AS r(day, database_key, role, purpose, model, prompt, completion, reasoning,
            calls, failures, cost, source)
ON CONFLICT (day, database_key, role, purpose, model) DO UPDATE
   SET tokens = u.tokens + EXCLUDED.tokens,
       prompt_tokens = u.prompt_tokens + EXCLUDED.prompt_tokens,
       completion_tokens = u.completion_tokens + EXCLUDED.completion_tokens,
       reasoning_tokens = u.reasoning_tokens + EXCLUDED.reasoning_tokens,
       calls = u.calls + EXCLUDED.calls,
       failures = u.failures + EXCLUDED.failures,
       est_cost_usd = u.est_cost_usd + EXCLUDED.est_cost_usd,
       source = CASE WHEN EXCLUDED.source = 'estimate' THEN 'estimate' ELSE u.source END,
       updated_at = now()`

// AddAttribution adds rows' deltas in one multi-row upsert. A NULL cost on
// either side keeps the row's cost NULL. Rows are validated before any SQL.
func (s *LLMUsageStore) AddAttribution(ctx context.Context, rows []LLMAttributionRow) error {
	for _, row := range rows {
		if err := validateAttribution(row); err != nil {
			return err
		}
	}
	if len(rows) == 0 {
		return nil
	}
	if s.pool == nil {
		return errNoControlPool
	}
	args := attributionArgs(mergeAttribution(rows))
	qctx, cancel := context.WithTimeout(ctx, LLMUsageTimeout)
	defer cancel()
	if _, err := s.pool.Exec(qctx, addAttributionSQL, args...); err != nil {
		return fmt.Errorf("add %d LLM attribution rows to sage.llm_usage: %w", len(rows), err)
	}
	return nil
}

func attributionArgs(rows []LLMAttributionRow) []any {
	n := len(rows)
	days := make([]time.Time, n)
	keys, roles, purposes := make([]string, n), make([]string, n), make([]string, n)
	models, sources := make([]string, n), make([]string, n)
	prompt, completion, reasoning := make([]int64, n), make([]int64, n), make([]int64, n)
	calls, failures, costs := make([]int64, n), make([]int64, n), make([]*float64, n)
	for i, r := range rows {
		days[i], keys[i], roles[i], purposes[i] = utcDate(r.Day), r.DatabaseKey, r.Role,
			r.Purpose
		models[i], sources[i], costs[i] = r.Model, r.Source, r.CostUSD
		prompt[i], completion[i], reasoning[i] = r.PromptTokens, r.CompletionTokens,
			r.ReasoningTokens
		calls[i], failures[i] = r.Calls, r.Failures
	}
	return []any{days, keys, roles, purposes, models, prompt, completion, reasoning,
		calls, failures, costs, sources}
}

// Attribution returns every attribution row from since's UTC day on, by
// day, key, role, purpose and model.
func (s *LLMUsageStore) Attribution(ctx context.Context, since time.Time) (
	[]LLMAttributionRow, error) {
	if s.pool == nil {
		return nil, errNoControlPool
	}
	qctx, cancel := context.WithTimeout(ctx, LLMUsageTimeout)
	defer cancel()
	rows, err := s.pool.Query(qctx, `SELECT day, database_key, role, purpose, model,
		prompt_tokens, completion_tokens, reasoning_tokens, calls, failures,
		est_cost_usd::float8, source
		FROM sage.llm_usage WHERE day >= $1::date AND purpose <> '_total'
		ORDER BY day, database_key, role, purpose, model`, utcDate(since))
	if err != nil {
		return nil, fmt.Errorf("read LLM attribution since %s: %w",
			utcDate(since).Format(time.DateOnly), err)
	}
	defer rows.Close()
	var out []LLMAttributionRow
	for rows.Next() {
		var r LLMAttributionRow
		if err := rows.Scan(&r.Day, &r.DatabaseKey, &r.Role, &r.Purpose, &r.Model,
			&r.PromptTokens, &r.CompletionTokens, &r.ReasoningTokens, &r.Calls,
			&r.Failures, &r.CostUSD, &r.Source); err != nil {
			return nil, fmt.Errorf("scan LLM attribution row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read LLM attribution rows: %w", err)
	}
	return out, nil
}

// PruneAttribution deletes the attribution rows of days before cutoff.
func (s *LLMUsageStore) PruneAttribution(ctx context.Context, cutoff time.Time) (int64, error) {
	if s.pool == nil {
		return 0, errNoControlPool
	}
	qctx, cancel := context.WithTimeout(ctx, LLMUsageTimeout)
	defer cancel()
	tag, err := s.pool.Exec(qctx, `DELETE FROM sage.llm_usage
		WHERE day < $1::date AND purpose <> '_total'`, utcDate(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune LLM attribution before %s: %w",
			utcDate(cutoff).Format(time.DateOnly), err)
	}
	return tag.RowsAffected(), nil
}

func validateAttribution(r LLMAttributionRow) error {
	switch {
	case r.Day.IsZero():
		return fmt.Errorf("LLM attribution row %+v: zero day", r)
	case r.DatabaseKey == "":
		return fmt.Errorf("LLM attribution row %+v: empty database key", r)
	case r.Role != "general" && r.Role != "optimizer":
		return fmt.Errorf("LLM attribution row %+v: role %q (want general or optimizer)",
			r, r.Role)
	case !attributionPurpose.MatchString(r.Purpose):
		return fmt.Errorf("LLM attribution row %+v: purpose %q is not a lower-case "+
			"identifier", r, r.Purpose)
	case r.PromptTokens < 0 || r.CompletionTokens < 0 || r.ReasoningTokens < 0 ||
		r.Calls < 0 || r.Failures < 0:
		return fmt.Errorf("LLM attribution row %+v: negative count", r)
	case r.CostUSD != nil && (*r.CostUSD < 0 || math.IsNaN(*r.CostUSD) ||
		math.IsInf(*r.CostUSD, 0)):
		return fmt.Errorf("LLM attribution row %+v: cost %v is not a finite amount >= 0",
			r, *r.CostUSD)
	case r.Source != LLMSourceProvider && r.Source != LLMSourceEstimate:
		return fmt.Errorf("LLM attribution row %+v: source %q (want provider or estimate)",
			r, r.Source)
	}
	return nil
}

type attributionKey struct {
	day                            int64
	database, role, purpose, model string
}

// mergeAttribution sums rows of one key: one upsert cannot touch a row
// twice. The result is sorted, so concurrent flushes lock rows in order.
func mergeAttribution(rows []LLMAttributionRow) []LLMAttributionRow {
	index := map[attributionKey]int{}
	out := make([]LLMAttributionRow, 0, len(rows))
	for _, r := range rows {
		k := attributionKey{utcDate(r.Day).Unix(), r.DatabaseKey, r.Role, r.Purpose, r.Model}
		i, ok := index[k]
		if !ok {
			index[k] = len(out)
			out = append(out, r)
			continue
		}
		out[i] = sumAttribution(out[i], r)
	}
	sort.Slice(out, func(i, j int) bool { return attributionLess(out[i], out[j]) })
	return out
}

func sumAttribution(a, b LLMAttributionRow) LLMAttributionRow {
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.ReasoningTokens += b.ReasoningTokens
	a.Calls += b.Calls
	a.Failures += b.Failures
	if a.CostUSD == nil || b.CostUSD == nil {
		a.CostUSD = nil
	} else {
		sum := *a.CostUSD + *b.CostUSD
		a.CostUSD = &sum
	}
	if b.Source == LLMSourceEstimate {
		a.Source = LLMSourceEstimate
	}
	return a
}

func attributionLess(a, b LLMAttributionRow) bool {
	ka := []string{utcDate(a.Day).Format(time.DateOnly), a.DatabaseKey, a.Role, a.Purpose,
		a.Model}
	kb := []string{utcDate(b.Day).Format(time.DateOnly), b.DatabaseKey, b.Role, b.Purpose,
		b.Model}
	for i := range ka {
		if ka[i] != kb[i] {
			return ka[i] < kb[i]
		}
	}
	return false
}

// AttributionRows converts an accumulator flush to rows, keying each
// database with keyOf (the fleet-10 ledger key) and merging names that
// share a key.
func AttributionRows(deltas map[llm.UsageKey]llm.UsageCounts,
	keyOf func(database string) string) []LLMAttributionRow {
	rows := make([]LLMAttributionRow, 0, len(deltas))
	for k, c := range deltas {
		row := LLMAttributionRow{Day: k.Day, DatabaseKey: keyOf(k.Database), Role: k.Role,
			Purpose: k.Purpose, Model: k.Model, PromptTokens: c.PromptTokens,
			CompletionTokens: c.CompletionTokens, ReasoningTokens: c.ReasoningTokens,
			Calls: c.Calls, Failures: c.Failures, Source: LLMSourceProvider}
		if c.Priced {
			cost := c.CostUSD
			row.CostUSD = &cost
		}
		if c.Estimated {
			row.Source = LLMSourceEstimate
		}
		rows = append(rows, row)
	}
	return mergeAttribution(rows)
}

// LLMAttributionSink writes an accumulator's flushes to s.
func LLMAttributionSink(s *LLMUsageStore, keyOf func(database string) string) llm.UsageSink {
	return func(ctx context.Context, deltas map[llm.UsageKey]llm.UsageCounts) error {
		return s.AddAttribution(ctx, AttributionRows(deltas, keyOf))
	}
}
