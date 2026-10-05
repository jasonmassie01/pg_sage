// Package optimizer is the index toolbox of the tuning agent (roadmap
// 2.2; internal/tuning) and the single source of truth for index
// recommendations (category = "missing_index"). The model no longer runs
// here: the agent hands over a candidate, and the optimizer keeps every
// deterministic gate — the canonical form, the validator, rejection memory
// and the HypoPG what-if — plus the table context the agent's tools read.
// Schema lint intentionally does NOT register rules that propose new
// indexes — those live here, where plan capture + HypoPG validation decide
// admission instead of raw heuristics.
package optimizer

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
)

// Optimizer admits index candidates with plan-aware, HypoPG-validated
// evidence.
type Optimizer struct {
	pool      *pgxpool.Pool
	cfg       *config.OptimizerConfig
	validator *Validator
	planner   *PlanCapture
	hypopg    *HypoPG
	whatIf    whatIfValidator  // defaults to hypopg; tests inject fakes
	memory    *rejectionMemory // nil: rejection memory off or no database
	logFn     func(string, string, ...any)
	// catalogTimeouts bound the context builder's catalog reads.
	catalogTimeouts catalogread.Timeouts

	// whatIfSkips counts candidates rejection memory skipped (MemoryStats).
	whatIfSkips atomic.Int64
}

// WithCatalogReadTimeouts bounds the context builder's catalog reads
// (columns, pg_stats, collation) with the safety configuration's
// statement and lock timeouts; the default is the shipped safety values.
func WithCatalogReadTimeouts(t catalogread.Timeouts) func(*Optimizer) {
	return func(o *Optimizer) { o.catalogTimeouts = t }
}

// New creates an Optimizer with all sub-components.
func New(
	pool *pgxpool.Pool,
	cfg *config.OptimizerConfig,
	pgVersionNum int,
	logFn func(string, string, ...any),
	options ...func(*Optimizer),
) *Optimizer {
	o := &Optimizer{
		pool:      pool,
		cfg:       cfg,
		validator: NewValidator(pool, cfg, logFn),
		planner: NewPlanCapture(
			pool, pgVersionNum, false,
			cfg.PlanSource, logFn,
		),
		hypopg: NewHypoPG(pool, logFn),
		logFn:  logFn,

		catalogTimeouts: catalogread.Default(),
	}
	o.whatIf = o.hypopg
	if pool != nil && cfg.RejectionMemory.Enabled {
		o.memory = newRejectionMemory(newPGRejectionStore(pool),
			memorySettingsFrom(cfg.RejectionMemory), logFn)
	}
	for _, opt := range options {
		opt(o)
	}
	return o
}

// whatIfValidator is the hypothetical-index check (HypoPG).
type whatIfValidator interface {
	IsAvailable(ctx context.Context) bool
	Validate(ctx context.Context, rec Recommendation, queries []QueryInfo,
	) (WhatIfResult, error)
}

// WithAutoExplain enables auto_explain as a plan source.
func WithAutoExplain() func(*Optimizer) {
	return func(o *Optimizer) {
		o.planner.autoExplainAvailable = true
	}
}

// ColdStart reports whether there are fewer than min_snapshots snapshots,
// too little history to judge a workload. A failed check counts as cold
// (fail closed) and is logged.
func (o *Optimizer) ColdStart(ctx context.Context) bool {
	if o.cfg.MinSnapshots <= 0 {
		return false
	}
	if o.pool == nil {
		return true
	}
	cold, err := CheckColdStart(ctx, o.pool, o.cfg.MinSnapshots)
	if err != nil {
		o.logFn("WARN", "optimizer: cold start check failed: %v", err)
	}
	return cold
}

// AdmissionOutcome is what admission decided about a candidate.
type AdmissionOutcome string

// Admission outcomes.
const (
	AdmitAccepted AdmissionOutcome = "admitted"
	AdmitInvalid  AdmissionOutcome = "invalid"
	AdmitMeasured AdmissionOutcome = "already_measured"
	AdmitRejected AdmissionOutcome = "what_if_rejected"
)

// Admission is the decision on one candidate, with why when it is not
// admitted. An admitted candidate is verified by HypoPG, or unverified
// (no complete measurement), which the executor's gate sends to an
// operator.
type Admission struct {
	Rec     Recommendation
	Outcome AdmissionOutcome
	Reason  string
}

// Admit canonicalizes, validates, checks rejection memory and HypoPG-
// measures one candidate for tc. An idea rejection memory already measured
// on this workload skips the what-if (unless an operator asked); a new
// what-if rejection is remembered.
func (o *Optimizer) Admit(ctx context.Context, rec Recommendation, tc TableContext,
) Admission {
	rec, err := canonicalizeRecommendation(rec, tc)
	if err != nil {
		return Admission{Rec: rec, Outcome: AdmitInvalid, Reason: err.Error()}
	}
	if ok, reason := o.validator.Validate(ctx, rec, tc); !ok {
		return Admission{Rec: rec, Outcome: AdmitInvalid, Reason: reason}
	}
	if !operatorRequested(ctx) {
		if r, seen := o.memory.view(ctx, tc).suppress(rec); seen {
			o.whatIfSkips.Add(1)
			return Admission{Rec: rec, Outcome: AdmitMeasured, Reason: fmt.Sprintf(
				"already measured %dx, last %s: %s", max(r.MeasureCount, 1),
				r.MeasuredAt.UTC().Format("2006-01-02 15:04 UTC"), r.Reason)}
		}
	}
	rec, rejected := o.enrichWithHypoPG(ctx, rec, tc)
	if rejected {
		o.memory.remember(ctx, tc, rec, o.cfg.HypoPGMinImprovePct)
		return Admission{Rec: rec, Outcome: AdmitRejected, Reason: rec.WhatIfReason}
	}
	// The queryids the index is expected to help, for verify-and-revert.
	rec.AffectedQueryIDs = contextQueryIDs(tc)
	return Admission{Rec: rec, Outcome: AdmitAccepted}
}

// MeasuredRejections lists the newest shapes rejection memory already
// measured and rejected on tc's workload, for the agent's case packet.
func (o *Optimizer) MeasuredRejections(ctx context.Context, tc TableContext) []string {
	return o.memory.view(ctx, tc).promptLines()
}

// enrichWithHypoPG measures the recommendation with hypothetical indexes
// and records the verdict (Phase 0 item 7). A complete measurement below
// HypoPGMinImprovePct rejects it. Anything less than a complete
// measurement — HypoPG unavailable, an error, no measurable query, a
// query that could not be planned — is "unverified": the recommendation
// is kept, but only an operator can approve it (the executor gates on
// the verdict).
func (o *Optimizer) enrichWithHypoPG(
	ctx context.Context,
	rec Recommendation,
	tc TableContext,
) (Recommendation, bool) {
	rec.Validated = false
	if o.whatIf == nil || !o.whatIf.IsAvailable(ctx) {
		rec.WhatIf, rec.WhatIfReason = WhatIfUnverified, "HypoPG unavailable"
		return rec, false
	}
	res, err := o.whatIf.Validate(ctx, rec, tc.Queries)
	if err != nil {
		o.logFn("optimizer", "hypopg validation failed for %s: %v", rec.Table, err)
	}
	verdict, reason := whatIfVerdict(res, err, o.cfg.HypoPGMinImprovePct)
	rec.WhatIf, rec.WhatIfReason = verdict, reason
	if res.Measured > 0 {
		rec.EstimatedImprovementPct = res.Improvement
	}
	if res.SizeBytes > 0 {
		rec.CostEstimate = &CostEstimate{EstimatedSizeBytes: res.SizeBytes}
	}
	rec.Validated = verdict == WhatIfVerified
	return rec, verdict == WhatIfRejected
}

// contextQueryIDs returns the queryids of a table's context — the queries
// a new index is expected to help. Used by F1 verify-and-revert (A2) to
// drop an index that regresses them.
func contextQueryIDs(tc TableContext) []int64 {
	ids := make([]int64, 0, len(tc.Queries))
	for _, q := range tc.Queries {
		if q.QueryID != 0 {
			ids = append(ids, q.QueryID)
		}
	}
	return ids
}

// MemoryStats are the rejection-memory counters since start.
type MemoryStats struct {
	WhatIfSkipped int64 // what-if evaluations skipped (already measured)
}

// MemoryStats returns the rejection-memory counters (zero for nil).
func (o *Optimizer) MemoryStats() MemoryStats {
	if o == nil {
		return MemoryStats{}
	}
	return MemoryStats{WhatIfSkipped: o.whatIfSkips.Load()}
}

type operatorRequestKey struct{}

// WithOperatorRequest marks an admission an operator asked for: every
// candidate is measured, whatever rejection memory says (memory still
// records what it measures).
func WithOperatorRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, operatorRequestKey{}, true)
}

func operatorRequested(ctx context.Context) bool {
	requested, _ := ctx.Value(operatorRequestKey{}).(bool)
	return requested
}
