package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// EventDispatcher sends notification events. Nil means no
// notifications are sent.
type EventDispatcher interface {
	Dispatch(ctx context.Context, event notify.Event) error
}

// ConfigAdvisor is satisfied by *advisor.Advisor without importing it.
type ConfigAdvisor interface {
	Analyze(ctx context.Context) ([]Finding, error)
}

// WorkloadForecaster produces capacity forecast findings.
type WorkloadForecaster interface {
	Forecast(ctx context.Context) ([]Finding, error)
}

// QueryTuner produces per-query tuning findings.
//
// deferredTables contains canonical "schema.table" entries that
// have pending index optimizer recommendations from the current
// or prior cycles. The tuner must skip candidates whose plans
// reference any of these tables, so a hint plan isn't installed
// just before a covering index would render it obsolete.
type QueryTuner interface {
	Tune(
		ctx context.Context,
		deferredTables map[string]bool,
	) ([]Finding, error)
}

// RCAEngine is satisfied by *rca.Engine without importing it.
type RCAEngine interface {
	Analyze(
		current *collector.Snapshot,
		previous *collector.Snapshot,
		cfg *config.Config,
		lockChainFindings []Finding,
	)
	PersistIncidents(ctx context.Context, pool *pgxpool.Pool) error
}

// Analyzer runs the rules engine on a recurring interval, producing
// findings and persisting them to the sage.findings table.
type Analyzer struct {
	pool      *pgxpool.Pool
	cfg       *config.Config
	collector *collector.Collector
	extras    *RuleExtras
	optimizer  *optimizer.Optimizer
	advisor    ConfigAdvisor
	forecaster WorkloadForecaster
	tuner      QueryTuner
	rcaEngine    RCAEngine
	logFn        func(string, string, ...any)
	dispatcher   EventDispatcher
	databaseName string
	mu           sync.RWMutex
	findings     []Finding
}

// New creates a new Analyzer.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	coll *collector.Collector,
	opt *optimizer.Optimizer,
	adv ConfigAdvisor,
	fc WorkloadForecaster,
	qt QueryTuner,
	logFn func(string, string, ...any),
) *Analyzer {
	return &Analyzer{
		pool:       pool,
		cfg:        cfg,
		collector:  coll,
		optimizer:  opt,
		advisor:    adv,
		forecaster: fc,
		tuner:      qt,
		extras: &RuleExtras{
			FirstSeen:       make(map[string]time.Time),
			RecentlyCreated: make(map[string]time.Time),
		},
		logFn: logFn,
	}
}

// WithDispatcher sets the notification dispatcher for critical
// finding alerts. Nil is safe (default).
func (a *Analyzer) WithDispatcher(d EventDispatcher) {
	a.dispatcher = d
}

// WithRCAEngine sets the root cause analysis engine for the analyzer.
func (a *Analyzer) WithRCAEngine(e RCAEngine) {
	a.rcaEngine = e
}

// WithDatabaseName sets the database name included in events.
func (a *Analyzer) WithDatabaseName(name string) {
	a.databaseName = name
}

// Run starts the analyzer loop and blocks until ctx is cancelled.
func (a *Analyzer) Run(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.Analyzer.Interval())
	defer ticker.Stop()

	a.logFn("INFO", "analyzer started, interval=%s", a.cfg.Analyzer.Interval())

	// Run once immediately.
	a.cycle(ctx)

	for {
		select {
		case <-ctx.Done():
			a.logFn("INFO", "analyzer stopped")
			return
		case <-ticker.C:
			a.cycle(ctx)
		}
	}
}

// SetFindings replaces the current findings (called by rule evaluation).
func (a *Analyzer) SetFindings(ff []Finding) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.findings = make([]Finding, len(ff))
	copy(a.findings, ff)
}

// LatestFindings returns a copy of the most recent findings under a
// read lock.
func (a *Analyzer) LatestFindings() []Finding {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Finding, len(a.findings))
	copy(out, a.findings)
	return out
}

// Findings returns a snapshot of the current findings (alias).
func (a *Analyzer) Findings() []Finding {
	return a.LatestFindings()
}

// OpenFindingsCount returns a count of current findings by severity.
func (a *Analyzer) OpenFindingsCount() map[string]int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	counts := make(map[string]int)
	for _, f := range a.findings {
		counts[f.Severity]++
	}
	return counts
}

// filterSchemaExclusions removes sage/pg_catalog/information_schema
// objects from snapshot data before rules run.
func filterSchemaExclusions(snap *collector.Snapshot) {
	excluded := map[string]bool{
		"sage": true, "pgsnap": true,
		"pg_catalog": true, "information_schema": true,
		"google_ml": true,
	}

	filtered := snap.Tables[:0]
	for _, t := range snap.Tables {
		if !excluded[t.SchemaName] {
			filtered = append(filtered, t)
		}
	}
	snap.Tables = filtered

	idxFiltered := snap.Indexes[:0]
	for _, idx := range snap.Indexes {
		if !excluded[idx.SchemaName] {
			idxFiltered = append(idxFiltered, idx)
		}
	}
	snap.Indexes = idxFiltered
}

func (a *Analyzer) cycle(ctx context.Context) {
	current := a.collector.LatestSnapshot()
	previous := a.collector.PreviousSnapshot()
	if current == nil {
		a.logFn("DEBUG", "analyzer: no snapshot yet, skipping")
		return
	}

	// Filter out sage schema objects before running rules.
	filterSchemaExclusions(current)
	if previous != nil {
		filterSchemaExclusions(previous)
	}

	// Load recently created indexes to prevent cooldown violations.
	a.loadRecentlyCreatedIndexes(ctx)

	// Skip query-based rules when pg_stat_statements was reset.
	skipQueryRules := current.StatsReset
	if skipQueryRules {
		a.logFn(
			"WARN",
			"stats reset detected, skipping query rules",
		)
	}

	var allFindings []Finding

	// Run all registered snapshot-based rules.
	queryRules := map[string]bool{
		"slow_queries":    true,
		"high_plan_time":  true,
		"high_total_time": true,
	}
	for _, rule := range AllRules {
		if skipQueryRules && queryRules[rule.Name] {
			continue
		}
		results := rule.Fn(current, previous, a.cfg, a.extras)
		allFindings = append(allFindings, results...)
	}

	// XID wraparound (requires separate query).
	xidFindings := a.checkXIDWraparound(ctx)
	allFindings = append(allFindings, xidFindings...)

	// Connection leaks (requires separate query).
	leakFindings := a.checkConnectionLeaks(ctx)
	allFindings = append(allFindings, leakFindings...)

	// Query regression (requires historical averages).
	if !skipQueryRules {
		historicalAvg := a.buildHistoricalAverages(ctx)
		regressionFindings := ruleQueryRegression(
			current, previous, historicalAvg, a.cfg,
		)
		allFindings = append(allFindings, regressionFindings...)
	}

	// Sort without index (requires explain_cache query).
	if !skipQueryRules {
		sortFindings := a.checkSortWithoutIndex(ctx)
		allFindings = append(allFindings, sortFindings...)
	}

	// Plan regression (compares recent explain plans per query).
	if !skipQueryRules {
		planDiffFindings := a.checkPlanRegression(ctx)
		allFindings = append(allFindings, planDiffFindings...)
	}

	// Seq scan watchdog — skip tables already flagged by missing FK.
	fkSkipTables := make(map[string]bool)
	for _, f := range allFindings {
		if f.Category == "missing_fk_index" {
			fkSkipTables[f.ObjectIdentifier] = true
		}
	}
	seqFindings := ruleSeqScanWatchdog(
		current, previous, a.cfg, fkSkipTables,
	)
	allFindings = append(allFindings, seqFindings...)

	// LLM index optimization (after all rule-based findings).
	// Tables with fresh recommendations are tracked so the tuner
	// (which runs later this cycle) defers any query reading from
	// them — applying a hint plan immediately before a covering
	// index lands risks installing a directive that becomes
	// obsolete or counterproductive once the index is created.
	deferredTables := make(map[string]bool)
	if a.optimizer != nil {
		optResult, err := a.optimizer.Analyze(ctx, current)
		if err != nil {
			a.logFn("WARN", "analyzer: index optimizer: %v", err)
		} else if optResult != nil {
			for _, rec := range optResult.Recommendations {
				if t := canonicalTable(rec.Table); t != "" {
					deferredTables[t] = true
				}
				allFindings = append(allFindings, Finding{
					Category:         rec.Category,
					Severity:         rec.Severity,
					ObjectType:       "index",
					ObjectIdentifier: rec.Table,
					Title: fmt.Sprintf(
						"Index recommendation for %s", rec.Table,
					),
					Detail: map[string]any{
						"ddl":                      rec.DDL,
						"drop_ddl":                 rec.DropDDL,
						"llm_rationale":            rec.Rationale,
						"confidence_score":         rec.Confidence,
						"action_level":             rec.ActionLevel,
						"index_type":               rec.IndexType,
						"category":                 rec.Category,
						"estimated_improvement_pct": rec.EstimatedImprovementPct,
						"hypopg_validated":         rec.Validated,
						"plan_source":              optResult.PlanSource,
						"affected_queries":         rec.AffectedQueries,
					},
					Recommendation: rec.Rationale,
					RecommendedSQL: rec.DDL,
					RollbackSQL:    rec.DropDDL,
					ActionRisk:     rec.ActionLevel,
				})
			}
		}
	}

	// Pull tables with open index recommendations from prior cycles
	// so the tuner also defers those (the trust-ramped executor may
	// not have applied them yet). The current-cycle entries above
	// already cover any rec the optimizer just produced.
	for _, t := range a.openIndexRecommendationTables(ctx) {
		deferredTables[t] = true
	}

	// LLM configuration advisor.
	if a.advisor != nil {
		advFindings, err := a.advisor.Analyze(ctx)
		if err != nil {
			a.logFn("WARN", "analyzer: advisor: %v", err)
		} else {
			allFindings = append(allFindings, advFindings...)
		}
	}

	// Workload forecasting.
	if a.forecaster != nil {
		fcFindings, err := a.forecaster.Forecast(ctx)
		if err != nil {
			a.logFn("WARN", "analyzer: forecaster: %v", err)
		} else {
			allFindings = append(allFindings, fcFindings...)
		}
	}

	// Per-query tuning. The tuner skips queries whose plans read
	// from tables in deferredTables (built from current + prior
	// optimizer recommendations).
	if a.tuner != nil {
		tunerFindings, err := a.tuner.Tune(ctx, deferredTables)
		if err != nil {
			a.logFn("WARN", "analyzer", "tuner: %v", err)
		} else {
			allFindings = append(allFindings, tunerFindings...)
		}
	}

	// v0.8.5 Feature 3: work_mem role-promotion advisor.
	// Runs after the tuner so it sees hints installed this cycle.
	allFindings = append(allFindings, a.checkWorkMemPromotion(ctx)...)

	// v0.8.5 Feature 4: extension drift detector.
	allFindings = append(allFindings, a.checkExtensionDrift(ctx)...)

	// v0.9 — Lock chain detection (requires DB query, not a RuleFunc).
	if a.cfg.Analyzer.LockChain.Enabled {
		chains, err := DetectLockChains(ctx, a.pool, a.cfg)
		if err != nil {
			a.logFn("WARN", "analyzer: lock chains: %v", err)
		} else if len(chains) > 0 {
			ownPID := a.getOwnPID(ctx)
			lcFindings := lockChainFindings(
				chains, a.cfg.Analyzer.LockChain, ownPID)
			allFindings = append(allFindings, lcFindings...)
		}
	}

	// v0.9 — Root Cause Analysis engine.
	if a.rcaEngine != nil {
		// Collect lock chain findings for RCA input.
		var lcfForRCA []Finding
		for _, f := range allFindings {
			if f.Category == "lock_chain" {
				lcfForRCA = append(lcfForRCA, f)
			}
		}
		a.rcaEngine.Analyze(current, previous, a.cfg, lcfForRCA)
		if err := a.rcaEngine.PersistIncidents(ctx, a.pool); err != nil {
			a.logFn("WARN", "analyzer: rca persist: %v", err)
		}
	}

	// Deduplicate conflicting findings across advisors.
	ioUtil := computeIOUtilPct(current)
	allFindings = DeduplicateFindings(allFindings, ioUtil, a.logFn)

	// Store findings in memory.
	a.mu.Lock()
	a.findings = allFindings
	a.mu.Unlock()

	// Persist to database.
	if err := UpsertFindings(ctx, a.pool, allFindings); err != nil {
		a.logFn("ERROR", "analyzer: upsert findings: %v", err)
	}

	// Notify on new critical findings.
	a.dispatchCriticalFindings(ctx, allFindings)

	// Notify on query rewrite suggestions.
	a.dispatchRewriteFindings(ctx, allFindings)

	// Resolve cleared findings by category.
	activeByCategory := make(map[string]map[string]bool)
	for _, f := range allFindings {
		if activeByCategory[f.Category] == nil {
			activeByCategory[f.Category] = make(map[string]bool)
		}
		activeByCategory[f.Category][f.ObjectIdentifier] = true
	}
	for cat, idents := range activeByCategory {
		if err := ResolveCleared(ctx, a.pool, idents, cat); err != nil {
			a.logFn(
				"ERROR", "analyzer: resolve cleared %s: %v", cat, err,
			)
		}
	}

	a.logFn("INFO", "analyzer cycle: %d findings", len(allFindings))
}

func (a *Analyzer) getOwnPID(ctx context.Context) int {
	var pid int
	_ = a.pool.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
	return pid
}

// dispatchCriticalFindings sends notifications for critical-severity
// findings. Only fires when a dispatcher is configured.
func (a *Analyzer) dispatchCriticalFindings(
	ctx context.Context, findings []Finding,
) {
	if a.dispatcher == nil {
		return
	}
	for _, f := range findings {
		if f.Severity != "critical" {
			continue
		}
		detail, _ := json.Marshal(f.Detail)
		event := notify.FindingCriticalEvent(
			f.Title, string(detail), a.databaseName)
		if err := a.dispatcher.Dispatch(ctx, event); err != nil {
			a.logFn("ERROR",
				"critical finding dispatch: %v", err)
		}
	}
}

// dispatchRewriteFindings sends notifications for query_tuning
// findings that include a suggested rewrite.
func (a *Analyzer) dispatchRewriteFindings(
	ctx context.Context, findings []Finding,
) {
	if a.dispatcher == nil {
		return
	}
	for _, f := range findings {
		if f.Category != "query_tuning" {
			continue
		}
		rewrite, _ := f.Detail["suggested_rewrite"].(string)
		if rewrite == "" {
			continue
		}
		query, _ := f.Detail["query"].(string)
		rationale, _ := f.Detail["rewrite_rationale"].(string)
		event := notify.QueryRewriteEvent(
			f.Title, query, rewrite, rationale,
			a.databaseName,
		)
		if err := a.dispatcher.Dispatch(ctx, event); err != nil {
			a.logFn("ERROR",
				"rewrite finding dispatch: %v", err)
		}
	}
}

func (a *Analyzer) loadRecentlyCreatedIndexes(ctx context.Context) {
	windowDays := a.cfg.Analyzer.UnusedIndexWindowDays
	if windowDays <= 0 {
		windowDays = 7
	}
	rows, err := a.pool.Query(ctx,
		`SELECT sql_executed, executed_at FROM sage.action_log
		 WHERE sql_executed ILIKE 'CREATE INDEX%'
		   AND outcome = 'success'
		   AND executed_at > now() - make_interval(days => $1)`,
		windowDays,
	)
	if err != nil {
		a.logFn("WARN", "analyzer: load recently created indexes: %v", err)
		return
	}
	defer rows.Close()

	created := make(map[string]time.Time)
	for rows.Next() {
		var sql string
		var executedAt time.Time
		if err := rows.Scan(&sql, &executedAt); err != nil {
			continue
		}
		name := extractIndexNameFromSQL(sql)
		if name != "" {
			created[name] = executedAt
		}
	}
	a.extras.RecentlyCreated = created
}

func (a *Analyzer) checkXIDWraparound(ctx context.Context) []Finding {
	var xidAge int64
	err := a.pool.QueryRow(ctx,
		`SELECT age(datfrozenxid) FROM pg_database
		 WHERE datname = current_database()`,
	).Scan(&xidAge)
	if err != nil {
		a.logFn("ERROR", "analyzer: xid query: %v", err)
		return nil
	}
	return ruleXIDWraparound(xidAge, a.cfg)
}

func (a *Analyzer) checkConnectionLeaks(ctx context.Context) []Finding {
	rows, err := a.pool.Query(ctx,
		`SELECT pid, usename, application_name, state,
		        now() - state_change AS idle_duration
		 FROM pg_stat_activity
		 WHERE state = 'idle in transaction'
		   AND now() - state_change > make_interval(mins => $1)
		   AND pid != pg_backend_pid()`,
		a.cfg.Analyzer.IdleInTxTimeoutMinutes,
	)
	if err != nil {
		a.logFn("ERROR", "analyzer: leak query: %v", err)
		return nil
	}
	defer rows.Close()

	var leaked []LeakedConn
	for rows.Next() {
		var c LeakedConn
		var state string
		if err := rows.Scan(
			&c.PID, &c.UserName, &c.AppName,
			&state, &c.IdleDuration,
		); err != nil {
			a.logFn("ERROR", "analyzer: scan leak: %v", err)
			continue
		}
		leaked = append(leaked, c)
	}
	if err := rows.Err(); err != nil {
		a.logFn("ERROR", "analyzer: iterate leaks: %v", err)
	}
	return ruleConnectionLeaks(leaked)
}

// buildHistoricalAverages loads recent query snapshots and computes
// per-queryid average mean_exec_time for regression detection.
func (a *Analyzer) buildHistoricalAverages(
	ctx context.Context,
) map[int64]float64 {
	rows, err := a.pool.Query(ctx,
		`SELECT data FROM sage.snapshots
		 WHERE category = 'queries'
		   AND collected_at > now() - make_interval(days => $1)
		 ORDER BY collected_at DESC`,
		a.cfg.Analyzer.RegressionLookbackDays,
	)
	if err != nil {
		a.logFn("ERROR", "analyzer: history query: %v", err)
		return nil
	}
	defer rows.Close()

	type queryEntry struct {
		QueryID        int64   `json:"queryid"`
		MeanExecTimeMs float64 `json:"mean_exec_time_ms"`
	}

	var allSnapshots [][]byte
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			continue
		}
		allSnapshots = append(allSnapshots, data)
	}
	if err := rows.Err(); err != nil {
		a.logFn("ERROR", "analyzer: iterate history: %v", err)
	}

	// Downsample to ~100 snapshots.
	sampled := downsample(allSnapshots, 100)

	sums := make(map[int64]float64)
	counts := make(map[int64]int)

	for _, data := range sampled {
		var entries []queryEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			continue
		}
		for _, e := range entries {
			sums[e.QueryID] += e.MeanExecTimeMs
			counts[e.QueryID]++
		}
	}

	avgs := make(map[int64]float64, len(sums))
	for qid, sum := range sums {
		if c := counts[qid]; c > 0 {
			avgs[qid] = sum / float64(c)
		}
	}
	return avgs
}

// downsample returns up to maxN evenly-spaced items from the input.
func downsample[T any](items []T, maxN int) []T {
	n := len(items)
	if n <= maxN {
		return items
	}
	step := float64(n) / float64(maxN)
	out := make([]T, 0, maxN)
	for i := 0; i < maxN; i++ {
		idx := int(float64(i) * step)
		if idx >= n {
			idx = n - 1
		}
		out = append(out, items[idx])
	}
	return out
}

// computeIOUtilPct estimates I/O utilization as the ratio of
// combined I/O wait time (blk_read_time + blk_write_time from
// pg_stat_database) to total query execution time. Returns 0-100.
//
// When I/O wait dominates execution time, aggressive vacuum
// recommendations would make things worse on an already I/O-bound
// system.
func computeIOUtilPct(snap *collector.Snapshot) float64 {
	if snap == nil {
		return 0
	}
	ioWait := snap.System.BlkReadTime + snap.System.BlkWriteTime
	if ioWait <= 0 {
		return 0
	}
	var totalExecTime float64
	for _, q := range snap.Queries {
		totalExecTime += q.TotalExecTime
	}
	if totalExecTime <= 0 {
		return 0
	}
	pct := ioWait / totalExecTime * 100
	if pct > 100 {
		pct = 100
	}
	return pct
}

// canonicalTable normalizes a table reference (e.g. "orders" or
// "public.orders") to canonical lowercase "schema.table" form,
// defaulting to schema "public". Returns "" for empty input.
//
// Used to build the deferredTables set so the tuner can reliably
// match plan-extracted relations against optimizer recommendations
// even when the LLM returns unqualified names.
func canonicalTable(ref string) string {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		return ""
	}
	if strings.Contains(ref, ".") {
		return ref
	}
	return "public." + ref
}

// openIndexRecommendationTables returns the canonical names of
// tables that have open (unresolved, unsuppressed) index-related
// findings. The tuner uses these to defer queries on tables where
// an index recommendation is still pending — applying a hint plan
// before the index lands risks installing a directive that becomes
// stale once the executor creates the index.
func (a *Analyzer) openIndexRecommendationTables(
	ctx context.Context,
) []string {
	if a.pool == nil {
		return nil
	}
	rows, err := a.pool.Query(ctx,
		`SELECT DISTINCT object_identifier
		 FROM sage.findings
		 WHERE category ILIKE '%index%'
		   AND status NOT IN ('resolved','suppressed')`,
	)
	if err != nil {
		a.logFn("WARN",
			"analyzer: load open index findings: %v", err)
		return nil
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var ident string
		if err := rows.Scan(&ident); err != nil {
			continue
		}
		if t := canonicalTable(ident); t != "" {
			out = append(out, t)
		}
	}
	return out
}
