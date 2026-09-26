// Package optimizer is the single source of truth for index
// recommendations (category = "missing_index"). Schema lint intentionally
// does NOT register rules that propose new indexes — those live here,
// where plan capture + HypoPG validation produce confidence-scored
// recommendations instead of raw heuristics. If you find yourself adding
// a "suggest an index" rule in schema/lint, it almost certainly belongs
// in this package instead.
package optimizer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

const defaultMaxNewPerTable = 3
const maxTablesPerCycle = 10

// Optimizer is the v2 index optimizer with plan-aware, HypoPG-validated
// recommendations and confidence scoring.
type Optimizer struct {
	client         *llm.Client
	fallbackClient *llm.Client
	pool           *pgxpool.Pool
	cfg            *config.OptimizerConfig
	validator      *Validator
	planner        *PlanCapture
	hypopg         *HypoPG
	whatIf         whatIfValidator // defaults to hypopg; tests inject fakes
	breaker        *CircuitBreaker
	maxOutput      int
	logFn          func(string, string, ...any)
}

// New creates an Optimizer with all sub-components.
func New(
	client *llm.Client,
	fallbackClient *llm.Client,
	pool *pgxpool.Pool,
	cfg *config.OptimizerConfig,
	pgVersionNum int,
	extensionPresent bool,
	maxOutputTokens int,
	logFn func(string, string, ...any),
	options ...func(*Optimizer),
) *Optimizer {
	if maxOutputTokens <= 0 {
		maxOutputTokens = 8192
	}
	o := &Optimizer{
		client:         client,
		fallbackClient: fallbackClient,
		pool:           pool,
		cfg:            cfg,
		validator:      NewValidator(pool, cfg, logFn),
		planner: NewPlanCapture(
			pool, pgVersionNum, extensionPresent, false,
			cfg.PlanSource, logFn,
		),
		hypopg:    NewHypoPG(pool, cfg.HypoPGMinImprovePct, logFn),
		breaker:   NewCircuitBreaker(),
		maxOutput: maxOutputTokens,
		logFn:     logFn,
	}
	o.whatIf = o.hypopg
	for _, opt := range options {
		opt(o)
	}
	return o
}

// whatIfValidator is the hypothetical-index check (HypoPG).
type whatIfValidator interface {
	IsAvailable(ctx context.Context) bool
	Validate(ctx context.Context, rec Recommendation, queries []QueryInfo,
	) (accepted bool, improvement float64, size int64, err error)
}

// WithAutoExplain enables auto_explain as a plan source.
func WithAutoExplain() func(*Optimizer) {
	return func(o *Optimizer) {
		o.planner.autoExplainAvailable = true
	}
}

// Analyze runs one optimizer cycle on the latest snapshot.
func (o *Optimizer) Analyze(
	ctx context.Context,
	snap *collector.Snapshot,
) (*Result, error) {
	if snap == nil {
		return nil, fmt.Errorf("nil snapshot")
	}

	cold, err := CheckColdStart(ctx, o.pool, o.cfg.MinSnapshots)
	if err != nil {
		o.logFn("optimizer", "cold start check failed: %v", err)
	}
	if cold {
		o.logFn("optimizer",
			"cold start: waiting for %d snapshots", o.cfg.MinSnapshots,
		)
		return &Result{PlanSource: "none"}, nil
	}

	contexts, planSource, err := BuildTableContexts(
		ctx, o.pool, snap, o.planner, int64(o.cfg.MinQueryCalls),
	)
	if err != nil {
		return nil, fmt.Errorf("build contexts: %w", err)
	}

	// Sort by total query time descending and cap to the most
	// impactful tables to bound LLM calls per cycle.
	sort.Slice(contexts, func(i, j int) bool {
		return totalQueryTime(contexts[i].Queries) >
			totalQueryTime(contexts[j].Queries)
	})
	if len(contexts) > maxTablesPerCycle {
		o.logFn("optimizer",
			"capping tables from %d to %d (by total query time)",
			len(contexts), maxTablesPerCycle,
		)
		contexts = contexts[:maxTablesPerCycle]
	}

	o.logFn("optimizer",
		"analyze: %d tables, %d queries in snapshot, plan_source=%s",
		len(contexts), len(snap.Queries), planSource)

	result := &Result{
		TablesAnalyzed: len(contexts),
		PlanSource:     planSource,
	}

	for i := range contexts {
		contexts[i].Queries = GroupByFingerprint(contexts[i].Queries)
		contexts[i].JoinPairs = DetectJoinPairs(contexts[i].Queries)
	}

	for _, tc := range contexts {
		if o.breaker.ShouldSkip(tc.Schema, tc.Table) {
			o.logFn("optimizer",
				"circuit open for %s.%s, skipping", tc.Schema, tc.Table,
			)
			continue
		}
		if open, hasOpen := o.openRecommendations(ctx, tc); hasOpen {
			// Re-emit the pending candidates so the analyzer keeps them
			// open instead of resolving them for not reappearing (C06).
			o.logFn("optimizer",
				"skipping %s.%s: %d open index recommendation(s) re-emitted",
				tc.Schema, tc.Table, len(open),
			)
			result.Recommendations = append(result.Recommendations, open...)
			continue
		}
		recs, tokens, rejections, err := o.analyzeTable(ctx, tc)
		if err != nil {
			if isBudgetExhausted(err) {
				o.logFn("WARN",
					"optimizer: daily token budget exhausted, "+
						"skipping remaining tables (%s.%s and after)",
					tc.Schema, tc.Table,
				)
				result.BudgetExhausted = true
				break
			}
			o.logFn("optimizer",
				"table %s.%s: %v", tc.Schema, tc.Table, err,
			)
			if shouldTripTableCircuit(err) {
				o.breaker.RecordFailure(tc.Schema, tc.Table)
			}
			continue
		}
		if len(recs) > 0 {
			o.breaker.RecordSuccess(tc.Schema, tc.Table)
		}
		result.TokensUsed += tokens
		result.Rejections += rejections
		result.Recommendations = append(result.Recommendations, recs...)
	}
	return result, nil
}

func shouldTripTableCircuit(err error) bool {
	return err != nil && !errors.Is(err, llm.ErrRequestCooldown)
}

func (o *Optimizer) analyzeTable(
	ctx context.Context,
	tc TableContext,
) ([]Recommendation, int, int, error) {
	response, tokens, err := o.chat(ctx, SystemPrompt(), FormatPrompt(tc))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("llm chat: %w", err)
	}

	recs, err := parseRecommendations(response)
	if err != nil {
		return nil, tokens, 0, fmt.Errorf("parse: %w", err)
	}

	var accepted []Recommendation
	rejections := 0
	for _, rec := range recs {
		rec, ok := o.admit(ctx, rec, tc)
		if !ok {
			rejections++
			continue
		}
		accepted = append(accepted, rec)
	}

	cap := o.maxNewPerTable()
	if len(accepted) > cap {
		accepted = accepted[:cap]
	}

	return accepted, tokens, rejections, nil
}

// chat calls the primary client and, on failure, a distinct fallback
// client. A fallback that is the primary is not retried (G3-B15).
func (o *Optimizer) chat(ctx context.Context, system, prompt string) (string, int, error) {
	response, tokens, err := o.client.Chat(ctx, system, prompt, o.maxOutput)
	if err != nil && o.fallbackClient != nil && o.fallbackClient != o.client {
		o.logFn("optimizer",
			"primary LLM failed, trying fallback: %v", err,
		)
		response, tokens, err = o.fallbackClient.Chat(
			ctx, system, prompt, o.maxOutput,
		)
	}
	return response, tokens, err
}

// admit canonicalizes, validates, HypoPG-checks and scores one LLM
// recommendation. It returns false when the recommendation is rejected.
func (o *Optimizer) admit(
	ctx context.Context, rec Recommendation, tc TableContext,
) (Recommendation, bool) {
	rec, err := canonicalizeRecommendation(rec, tc)
	if err != nil {
		o.logFn("optimizer", "rejected %s on %s.%s: %v",
			rec.DDL, tc.Schema, tc.Table, err)
		return rec, false
	}
	if ok, reason := o.validator.Validate(ctx, rec, tc); !ok {
		o.logFn("optimizer",
			"rejected %s on %s: %s", rec.DDL, rec.Table, reason,
		)
		return rec, false
	}
	rec, rejected := o.enrichWithHypoPG(ctx, rec, tc)
	if rejected {
		o.logFn("optimizer",
			"rejected %s on %s: HypoPG shows %.1f%% improvement (min %.1f%%)",
			rec.DDL, rec.Table, rec.EstimatedImprovementPct,
			o.cfg.HypoPGMinImprovePct)
		return rec, false
	}
	rec = o.scoreConfidence(rec, tc)
	// Record the queryids this index is expected to help so F1
	// verify-and-revert can drop it if those queries regress (A2).
	rec.AffectedQueryIDs = contextQueryIDs(tc)
	// Enforce the configured confidence threshold (default 0.5). A zero
	// threshold (unset) disables the gate.
	if o.cfg.ConfidenceThreshold > 0 &&
		rec.Confidence < o.cfg.ConfidenceThreshold {
		o.logFn("optimizer",
			"below confidence threshold (%.2f < %.2f): %s on %s",
			rec.Confidence, o.cfg.ConfidenceThreshold,
			rec.DDL, rec.Table)
		return rec, false
	}
	return rec, true
}

// enrichWithHypoPG measures the recommendation with hypothetical
// indexes. The verdict is tri-state (G3-B06): unavailable or
// inconclusive (no measurable query, error) is neutral; a measured
// improvement below HypoPGMinImprovePct — including zero or negative —
// rejects the recommendation instead of scoring like "no HypoPG".
func (o *Optimizer) enrichWithHypoPG(
	ctx context.Context,
	rec Recommendation,
	tc TableContext,
) (Recommendation, bool) {
	if o.whatIf == nil || !o.whatIf.IsAvailable(ctx) {
		return rec, false
	}
	accepted, improvement, estSize, err := o.whatIf.Validate(ctx, rec, tc.Queries)
	if err != nil {
		o.logFn("optimizer",
			"hypopg validation failed for %s: %v", rec.Table, err,
		)
		return rec, false
	}
	if estSize <= 0 {
		return rec, false // nothing measurable: inconclusive
	}
	rec.EstimatedImprovementPct = improvement
	rec.CostEstimate = &CostEstimate{EstimatedSizeBytes: estSize}
	rec.Validated = accepted
	return rec, !accepted
}

func (o *Optimizer) scoreConfidence(
	rec Recommendation,
	tc TableContext,
) Recommendation {
	totalCalls := totalQueryCalls(tc.Queries)

	// QueryVolume: based on max calls for any query hitting this table.
	var maxCalls int64
	for _, q := range tc.Queries {
		if q.Calls > maxCalls {
			maxCalls = q.Calls
		}
	}
	var qv float64
	switch {
	case maxCalls >= 500:
		qv = 1.0
	case maxCalls >= 100:
		qv = 0.7
	case maxCalls >= 10:
		qv = 0.4
	default:
		qv = 0.1
	}

	// PlanClarity: 1.0 if EXPLAIN plans available, 0.5 if query text only.
	var pc float64
	if len(tc.Plans) > 0 {
		pc = 1.0
	} else if len(tc.Queries) > 0 {
		pc = 0.5
	}

	// WriteRateKnown: 1.0 only when the table recorded activity
	// (G3-B23: WriteRate >= 0 was always true).
	var wr float64
	if tc.WriteRateKnown {
		wr = 1.0
	}

	// HypoPGValidated: rejected verdicts never reach scoring (G3-B06),
	// so this is 1.0 for a measured accept and 0 when unavailable.
	var hv float64
	if rec.Validated {
		hv = 1.0
	}

	// SelectivityKnown: based on pg_stats data availability.
	var sk float64
	if len(tc.ColStats) > 0 {
		hasDistinct := false
		hasMCV := false
		for _, s := range tc.ColStats {
			if s.NDistinct != 0 {
				hasDistinct = true
			}
			if len(s.MostCommonVals) > 0 {
				hasMCV = true
			}
		}
		if hasDistinct && hasMCV {
			sk = 1.0
		} else if hasDistinct {
			sk = 0.5
		}
	}

	// TableCallVolume: total queries/day hitting this table.
	var tv float64
	switch {
	case totalCalls >= 1000:
		tv = 1.0
	case totalCalls >= 100:
		tv = 0.6
	case totalCalls >= 10:
		tv = 0.3
	default:
		tv = 0.1
	}

	input := ConfidenceInput{
		QueryVolume:      qv,
		PlanClarity:      pc,
		WriteRateKnown:   wr,
		HypoPGValidated:  hv,
		SelectivityKnown: sk,
		TableCallVolume:  tv,
	}
	rec.Confidence = ComputeConfidence(input)
	rec.ActionLevel = ActionLevel(rec.Confidence)
	return rec
}

// contextQueryIDs returns the queryids the optimizer analyzed for a
// table — the queries a new index is expected to help. Used by F1
// verify-and-revert (A2) to drop an index that regresses them.
func contextQueryIDs(tc TableContext) []int64 {
	ids := make([]int64, 0, len(tc.Queries))
	for _, q := range tc.Queries {
		if q.QueryID != 0 {
			ids = append(ids, q.QueryID)
		}
	}
	return ids
}

func (o *Optimizer) maxNewPerTable() int {
	if o.cfg.MaxNewPerTable > 0 {
		return o.cfg.MaxNewPerTable
	}
	return defaultMaxNewPerTable
}

func totalQueryCalls(queries []QueryInfo) int64 {
	var total int64
	for _, q := range queries {
		total += q.Calls
	}
	return total
}

func totalQueryTime(queries []QueryInfo) float64 {
	var total float64
	for _, q := range queries {
		total += q.TotalTimeMs
	}
	return total
}

// isBudgetExhausted returns true if the error indicates
// the daily token budget has been exhausted.
func isBudgetExhausted(err error) bool {
	return err != nil &&
		strings.Contains(err.Error(), "budget exhausted")
}
