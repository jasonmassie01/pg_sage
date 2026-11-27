package tuner

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/planfresh"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Tuner produces per-query tuning findings from plan analysis.
type Tuner struct {
	pool *pgxpool.Pool
	cfg  TunerConfig
	// hintPlan holds the pg_hint_plan facts; read it through hints(). It is
	// replaced, never changed in place, under hintMu: the tuning agent reads
	// it from its own goroutine while a cycle re-reads it (tuning-04).
	hintPlan *HintPlanAvailability
	hintMu   sync.RWMutex
	// hintSource re-reads the facts each cycle (the pool; nil without one).
	hintSource HintQuerier

	logFn         func(string, string, ...any)
	recentlyTuned map[int64]int

	// mu serializes Tune() and Revalidate() so the two loops
	// never interleave writes to sage.query_hints.
	mu sync.Mutex

	// staleStats is the per-cycle cache of pg_stat_user_tables
	// information; reloaded at the start of every Tune() call.
	staleStats *StaleStatsCache

	// staleStatsEmitted tracks tables already emitted as
	// stale_statistics findings during the current cycle so
	// multiple queries touching the same stale table produce
	// exactly one ANALYZE finding.
	staleStatsEmitted map[string]bool

	// facts are the catalog facts of the current cycle (table rows,
	// usable indexes); nil when they could not be loaded.
	facts *CatalogFacts

	// spill holds the stat-spill baselines across cycles (tuning-03);
	// guarded by mu.
	spill statSpill

	// hostMemory reads live host memory (nil: unknown); set before use.
	hostMemory advisor.HostMemorySource

	// planExplain replaces a stale cached plan (nil: no fallback) and
	// planTimeouts bound the plan reads (tuning-08); see plan_fresh.go.
	planExplain  planfresh.ExplainFunc
	planTimeouts catalogread.Timeouts
	// plans memoizes each candidate's plan for one tuneCandidates pass;
	// nil outside it. Guarded by mu.
	plans map[int64]planfresh.Plan

	// querySource is the collector's snapshot the parameterization detector
	// reads, and pgMajor the server's major version (tuning-11); paramEmitted
	// holds the shapes reported this cycle, guarded by mu.
	querySource  QuerySource
	pgMajor      int
	paramEmitted map[string]bool

	// compare configures the hinted EXPLAIN gate (tuning-05); nil: off.
	compare *hintCompare
}

// New creates a Tuner with the given dependencies.
func New(
	pool *pgxpool.Pool,
	cfg TunerConfig,
	hintPlan *HintPlanAvailability,
	logFn func(string, string, ...any),
) *Tuner {
	t := &Tuner{
		pool:          pool,
		cfg:           cfg,
		hintPlan:      hintPlan,
		logFn:         logFn,
		recentlyTuned: make(map[int64]int),
		spill:         statSpill{baselines: map[spillKey]spillBaseline{}},
		planTimeouts:  catalogread.Default(),
	}
	if pool != nil {
		t.hintSource = pool
	}
	// recentlyTuned is only the post-proposal cooldown: one hint per
	// statement is enforced from the durable state on every check
	// (hintStates), so nothing is bootstrapped here.
	return t
}

type candidate struct {
	QueryID         int64
	Query           string
	Calls           int64
	MeanExecTime    float64
	MeanPlanTime    float64
	TempBlksRead    int64
	TempBlksWritten int64
	UserID          int64
	// foreignHint: hint_plan.hints holds a hint pg_sage did not install.
	foreignHint bool
}

// Tune queries pg_stat_statements for slow queries, scans
// their plans, and returns actionable findings.
//
// deferredTables names "schema.table" entries that have pending
// index optimizer recommendations. The tuner skips any candidate
// whose plan reads from one of these tables — applying a hint
// (e.g. HashJoin) just before a covering index lands risks
// installing a directive that becomes obsolete or actively wrong
// once the index is created. The deferred set must be canonical
// "schema.table" form (use CanonicalizeTableRef on inputs).
// nil/empty disables gating, preserving prior behaviour.
func (t *Tuner) Tune(
	ctx context.Context,
	deferredTables map[string]bool,
) ([]analyzer.Finding, error) {
	ctx, done := selfbudget.Process().TrackFor(ctx, "tuner")
	defer done()
	t.mu.Lock()
	defer t.mu.Unlock()

	// Reload stale-stats cache and dedup set for this cycle.
	cache, staleErr := LoadStaleStatsCache(ctx, t.pool, t.cfg)
	if staleErr != nil {
		t.logFn("WARN",
			"tuner: load stale stats cache: %v", staleErr)
	}
	t.staleStats = cache
	t.staleStatsEmitted = map[string]bool{}
	t.refreshHintPlan(ctx)
	t.loadFacts(ctx)
	t.reconcileHintStatuses(ctx)

	t.tickCooldowns()
	paramFindings := t.parameterizationFindings()
	candidates, err := t.fetchCandidates(ctx)
	if err != nil {
		return nil, fmt.Errorf("tuner: fetch candidates: %w", err)
	}
	candidates = filterSelfMonitoringCandidates(candidates)
	t.observeSpills(ctx, candidates)
	findings, err := t.tuneCandidates(ctx, candidates, deferredTables)
	if err != nil {
		return nil, err
	}
	findings = append(paramFindings, findings...)
	// C11: remove installed hints whose metadata was retired/broken.
	findings = append(findings, t.hintRemovalFindings(ctx)...)
	return findings, nil
}

// tuneCandidates runs the deterministic rules over the candidates. A
// statement in its cooldown or with an installed pg_sage hint is skipped;
// one with a hint pg_sage did not install gets an informational finding
// without SQL (tuning-02). A failed read of the durable hint state fails
// the pass, so no hint is proposed blind. The caller holds t.mu.
func (t *Tuner) tuneCandidates(
	ctx context.Context, candidates []candidate, deferredTables map[string]bool,
) ([]analyzer.Finding, error) {
	states, err := t.hintStates(ctx, candidateIDs(candidates))
	if err != nil {
		return nil, fmt.Errorf("tuner: %w", err)
	}
	t.plans = map[int64]planfresh.Plan{}
	defer func() { t.plans = nil }()
	var findings []analyzer.Finding
	for _, c := range candidates {
		if _, ok := t.recentlyTuned[c.QueryID]; ok {
			continue
		}
		switch states[c.QueryID] {
		case hintInstalled:
			continue
		case hintForeign:
			c.foreignHint = true
		}
		t.plans[c.QueryID] = t.readPlan(ctx, c.QueryID, c.Query)
		if reason := t.deferralReason(ctx, c, deferredTables); reason != "" {
			t.logFn("INFO",
				"tuner: deferring queryid=%d, %s "+
					"(optimizer recommendation pending)",
				c.QueryID, reason)
			continue
		}
		f := t.processCandidate(ctx, c)
		if len(f) > 0 {
			t.recordTuned(c.QueryID)
		}
		findings = append(findings, f...)
	}
	return findings, nil
}

// maxRecentlyTuned caps the cooldown map so that a pathological
// workload (very high distinct-queryid cardinality with a long
// CascadeCooldownCycles) cannot grow it without bound between
// cycles. When the cap is exceeded, the entry with the smallest
// remaining cooldown is evicted — it is closest to expiry anyway.
const maxRecentlyTuned = 10_000

// recordTuned inserts a queryID into the cooldown map, evicting the
// entry nearest to expiry if the cap would be exceeded.
func (t *Tuner) recordTuned(queryID int64) {
	if len(t.recentlyTuned) >= maxRecentlyTuned {
		var evictID int64
		minRemaining := -1
		for qid, remaining := range t.recentlyTuned {
			if minRemaining == -1 || remaining < minRemaining {
				minRemaining = remaining
				evictID = qid
			}
		}
		delete(t.recentlyTuned, evictID)
	}
	t.recentlyTuned[queryID] = t.cooldownCycles()
}

// tickCooldowns decrements all cooldown counters and removes
// entries that have reached zero.
func (t *Tuner) tickCooldowns() {
	for qid, remaining := range t.recentlyTuned {
		remaining--
		if remaining <= 0 {
			delete(t.recentlyTuned, qid)
		} else {
			t.recentlyTuned[qid] = remaining
		}
	}
}

// cooldownCycles returns the configured cascade cooldown,
// defaulting to 3 if unset.
func (t *Tuner) cooldownCycles() int {
	if t.cfg.CascadeCooldownCycles > 0 {
		return t.cfg.CascadeCooldownCycles
	}
	return 3
}

// filterSelfMonitoringCandidates keeps workload candidates
// (internal/workload): never pg_sage's own statements or diagnostic
// tooling.
func filterSelfMonitoringCandidates(candidates []candidate) []candidate {
	out := candidates[:0]
	for _, c := range candidates {
		if workload.Excluded(c.Query) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (t *Tuner) fetchCandidates(
	ctx context.Context,
) ([]candidate, error) {
	rel, err := extschema.StatementsRel(ctx, t.pool, "pg_stat_statements")
	if err != nil {
		return nil, fmt.Errorf("query pg_stat_statements: %w", err)
	}
	rows, err := t.pool.Query(
		ctx, candidateSQL(rel),
		t.cfg.MinQueryCalls, t.cfg.PlanTimeRatio,
	)
	if err != nil {
		return nil, fmt.Errorf("query pg_stat_statements: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		err := rows.Scan(
			&c.QueryID, &c.Query, &c.Calls,
			&c.MeanExecTime, &c.MeanPlanTime,
			&c.TempBlksRead, &c.TempBlksWritten, &c.UserID,
		)
		if err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (t *Tuner) processCandidate(
	ctx context.Context, c candidate,
) []analyzer.Finding {
	symptoms := t.gatherSymptoms(ctx, c)
	if len(symptoms) == 0 {
		return nil
	}

	// Split stale_stats symptoms into separate table-level
	// ANALYZE findings so they bypass the query-hint flow.
	var staleFindings []analyzer.Finding
	symptoms, staleFindings = t.extractStaleStats(symptoms)
	// High planning time is parameterization advice, never a hint (tuning-11).
	symptoms, paramFindings := t.extractPlanTime(c, symptoms)
	staleFindings = append(staleFindings, paramFindings...)

	if len(symptoms) == 0 {
		return staleFindings
	}

	// The tuning agent proposes model-reasoned hints through CheckHint
	// (roadmap 2.2); this pass is the deterministic rules only.
	prescriptions := t.prescribeAll(symptoms)
	if len(prescriptions) == 0 {
		// The advice and ANALYZE findings split off above still stand.
		return staleFindings
	}

	combined := t.combinedHint(prescriptions)
	rationale := buildRationale(prescriptions)
	if c.foreignHint && combined != "" {
		f := operatorHintFinding(c, symptoms, combined, rationale)
		return append(staleFindings, withJoinHintNote(withAntipatterns(f, symptoms), symptoms))
	}
	// Every work_mem hint stays within min(work_mem_max_mb, 5% of host
	// memory); with host memory unknown it needs approval (tuning-03).
	budget := t.workMemBudget()
	combined, budgetDetail := budget.capHint(combined)
	cmp, refused := t.gateCandidate(ctx, c, combined)
	if refused {
		return staleFindings
	}
	finding := t.buildGatedFinding(ctx, c, symptoms, combined, buildTitle(symptoms),
		rationale, cmp)
	finding = withSpillSizing(budget.annotate(finding, combined, budgetDetail), symptoms)
	finding = withJoinHintNote(withAntipatterns(finding, symptoms), symptoms)
	return append(staleFindings, finding)
}

// combinedHint combines the prescriptions' directives. Deterministic
// prescriptions interpolate plan-derived identifiers raw (e.g.
// HashJoin(alias)), so the combined directive is re-validated the same way
// agent hints are (W4); a malformed hint is dropped ("") rather than
// emitted as an unparseable directive.
func (t *Tuner) combinedHint(prescriptions []Prescription) string {
	combined := CombineHints(prescriptions)
	if combined != "" && !validateHintSyntax(combined) {
		t.logFn("tuner", "dropping unparseable combined hint: %s", combined)
		return ""
	}
	return combined
}

func (t *Tuner) gatherSymptoms(
	ctx context.Context, c candidate,
) []PlanSymptom {
	var symptoms []PlanSymptom
	planSymptoms := t.scanPlanForQuery(ctx, c.QueryID)
	symptoms = append(symptoms, planSymptoms...)

	if t.isHighPlanTime(c) {
		symptoms = append(symptoms, PlanSymptom{
			Kind: SymptomHighPlanTime,
		})
	}

	// Stat-based temp spill (tuning-03): when no plan-level spill
	// symptom was found, the spill this cycle sized from the interval's
	// per-call temp blocks against the role's work_mem.
	if sz, ok := t.spill.needs[keyOf(c)]; ok && !hasSpillSymptom(symptoms) {
		symptoms = append(symptoms, PlanSymptom{
			Kind: SymptomStatTempSpill, Detail: sz.detail(),
		})
	}
	return symptoms
}

// scanPlan scans one cached plan with this tuner's thresholds, then routes
// bad_nested_loop to stale_stats symptoms when any relation in the plan has
// stale statistics.
func (t *Tuner) scanPlan(ctx context.Context, planJSON []byte, extra ...ScanOption) (
	[]PlanSymptom, error) {
	opts := append([]ScanOption{
		WithCatalogFacts(t.cycleFacts(ctx, planJSON), t.cfg.ParallelMinTableRows),
		WithNestedLoopMinRows(t.cfg.NestedLoopRowThreshold)}, extra...)
	symptoms, err := ScanPlan(planJSON, opts...)
	if err != nil {
		return nil, err
	}
	return annotateForStaleStats(planJSON, symptoms, t.staleStats), nil
}

func (t *Tuner) isHighPlanTime(c candidate) bool {
	return c.MeanPlanTime > 0 &&
		c.MeanPlanTime > c.MeanExecTime*t.cfg.PlanTimeRatio &&
		c.Calls >= int64(t.cfg.MinQueryCalls)
}

func (t *Tuner) prescribeAll(
	symptoms []PlanSymptom,
) []Prescription {
	var out []Prescription
	for _, s := range symptoms {
		p := Prescribe(s, t.cfg)
		if p != nil {
			out = append(out, *p)
		}
	}
	return out
}

func (t *Tuner) buildFinding(
	ctx context.Context,
	c candidate,
	symptoms []PlanSymptom,
	combinedHint, title, rationale string,
) analyzer.Finding {
	return t.buildGatedFinding(ctx, c, symptoms, combinedHint, title, rationale, nil)
}

// buildGatedFinding builds the query_tuning finding; cmp is the hinted
// EXPLAIN comparison (nil: the gate did not run). An unverifiable hint
// stays advisory and is not recorded as proposed (tuning-05).
func (t *Tuner) buildGatedFinding(
	ctx context.Context,
	c candidate,
	symptoms []PlanSymptom,
	combinedHint, title, rationale string,
	cmp *HintComparison,
) analyzer.Finding {
	names := symptomNames(symptoms)
	detail := map[string]any{
		"queryid":        c.QueryID,
		"query":          c.Query,
		"symptoms":       names,
		"hint_directive": combinedHint,
		"plan":           t.planProvenance(ctx, c.QueryID),
	}
	f := analyzer.Finding{
		Category:         "query_tuning",
		Severity:         "warning",
		ObjectType:       "query",
		ObjectIdentifier: fmt.Sprintf("queryid:%d", c.QueryID),
		Title:            title,
		Detail:           detail,
		Recommendation:   rationale,
		ActionRisk:       "safe",
	}
	// An empty hint is never installed or recorded (Phase 0 item 11): the
	// finding stays informational (e.g. sort-limit advice).
	if strings.TrimSpace(combinedHint) == "" {
		return f
	}
	// Executable SQL only when the hint would change the application's plan
	// (tuning-04); otherwise the finding stays advisory and names why.
	// A hint that cannot be installed is not recorded as proposed either
	// (tuning-06): it could only wait there until it expired.
	if !t.HintsAvailable() {
		detail["hint_readiness"] = t.hints().Readiness()
		return f
	}
	if cmp != nil {
		detail["hint_evidence"] = cmp.Evidence()
		if cmp.Verdict == HintUnverifiable {
			return f
		}
	}
	f.RecommendedSQL = BuildInsertSQL(c.QueryID, combinedHint)
	f.RollbackSQL = BuildDeleteSQL(c.QueryID, combinedHint)
	// Persist to sage.query_hints for the dashboard query-hints page.
	t.upsertQueryHint(ctx, c.QueryID, combinedHint, strings.Join(names, ", "))
	return f
}
