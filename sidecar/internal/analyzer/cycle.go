package analyzer

import (
	"context"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// queryRuleNames are the snapshot rules that read pg_stat_statements and
// are skipped for a cycle in which the statistics were reset.
var queryRuleNames = map[string]bool{
	"slow_queries":    true,
	"high_plan_time":  true,
	"high_total_time": true,
}

func (a *Analyzer) cycle(ctx context.Context) error {
	// An out-of-cycle run (RunOutOfCycle) never overlaps a cycle.
	a.cycleMu.Lock()
	defer a.cycleMu.Unlock()
	latest := a.collector.LatestSnapshot()
	if latest == nil {
		a.logFn("DEBUG", "analyzer: no snapshot yet, skipping")
		return errNoSnapshot
	}
	if !a.claimFreshSnapshot(latest) {
		return errStaleSnapshot
	}
	current := snapshotForAnalysis(latest)
	previous := snapshotForAnalysis(a.collector.PreviousSnapshot())
	filterSchemaExclusions(current)
	if previous != nil {
		filterSchemaExclusions(previous)
	}
	excluded := a.excludeFactSchemas(ctx, current, previous)
	a.eval = newCycleEval()
	a.loadCycleInputs(ctx, current)
	// Skip query-based rules when pg_stat_statements was reset.
	skipQueryRules := current.StatsReset
	if skipQueryRules {
		a.logFn("WARN", "stats reset detected, skipping query rules")
	}

	all := a.runSnapshotRules(current, previous, skipQueryRules)
	all = append(all, a.runDatabaseRules(ctx, current, previous, skipQueryRules)...)
	all = append(all, a.runSeqScanWatchdog(current, previous, all)...)
	all = append(all, a.runProducers(ctx, current, previous)...)
	all = append(all, a.runLateChecks(ctx)...)
	all = append(append(all, a.checkSageFootprint(ctx, current)...), a.checkSelfCost(ctx)...)
	all = a.applyDisabledRules(append(all, a.runCustomChecks(ctx)...))
	all = a.deriveFindings(ctx, current, all, excluded)
	a.runRCA(ctx, current, previous, all)

	// Deduplicate conflicting findings across advisors.
	all = DeduplicateFindings(all, computeIOUtilPct(current, previous), a.logFn)
	all = annotateReloptionOverrides(all, current)
	// A bounded query list cannot prove absent queries stopped spilling.
	// Temp spills clear only on a new, observed, zero-spill execution.
	a.finishTempSpillCycle(ctx, current, previous, all)
	err := a.finalizeCycle(ctx, all, a.eval.resolvable(all))
	a.saveUnusedClock(ctx)
	return err
}

// deriveFindings applies the steps that turn findings into derived ones
// (app_managed_index, clone_schemas), then the fact filter. The rule pack
// runs again last: a derived category made after the first pass may itself
// be disabled (market-11), and must be dropped like any other, or it would
// reopen every cycle only to be resolved as rule_disabled.
func (a *Analyzer) deriveFindings(ctx context.Context, current *collector.Snapshot,
	all []Finding, excluded []string) []Finding {
	all = a.applyAppManaged(ctx, all)
	all = a.applyFactFilter(ctx, all, excluded)
	if current.Available("tables") && len(current.Tables) > 0 {
		all = collapseCloneSchemas(current, all, a.cloneSignals(ctx))
		a.eval.evaluated(CategoryCloneSchemas)
	}
	return a.applyDisabledRules(dropNonWorkloadFindings(all))
}

// loadCycleInputs reads the state the rules need besides the snapshot:
// recently created indexes (cooldown), the stats epoch, index builds and
// the persisted unused_index clock (after the epoch, which it needs). The
// snapshot's collection time is the cycle's evidence time (rules-06).
func (a *Analyzer) loadCycleInputs(ctx context.Context, current *collector.Snapshot) {
	a.eval.collectedAt = current.CollectedAt
	a.loadRecentlyCreatedIndexes(ctx)
	a.loadCycleCatalogState(ctx)
	a.loadExtensionState(ctx)
	a.prepareUnusedClock(ctx, current)
}

// claimFreshSnapshot reports whether latest is newer than the snapshot the
// previous cycle analyzed, and records it as analyzed. When collection has
// failed since then, the collector still returns the old snapshot; running
// the cycle on it again would refresh finding occurrence counts and
// last_seen, count RCA cycles and feed LLM producers from stale evidence,
// so the whole cycle is skipped until fresh evidence arrives.
func (a *Analyzer) claimFreshSnapshot(latest *collector.Snapshot) bool {
	stale := latest == a.lastAnalyzed ||
		(!latest.CollectedAt.IsZero() && !a.lastAnalyzedAt.IsZero() &&
			!latest.CollectedAt.After(a.lastAnalyzedAt))
	if stale {
		a.logFn("WARN", "analyzer: snapshot collected at %s was already "+
			"analyzed; no fresh evidence from the collector, skipping cycle",
			latest.CollectedAt.Format(time.RFC3339Nano))
		return false
	}
	a.lastAnalyzed = latest
	a.lastAnalyzedAt = latest.CollectedAt
	return true
}

// runSnapshotRules runs every registered snapshot-based rule.
func (a *Analyzer) runSnapshotRules(
	current, previous *collector.Snapshot, skipQueryRules bool,
) []Finding {
	var out []Finding
	for _, rule := range AllRules {
		if a.ruleDisabled(rule) {
			continue // resolved as rule_disabled, not failed (market-11)
		}
		skip := skipQueryRules && queryRuleNames[rule.Name]
		if skip || (rule.Needs != nil && !rule.Needs(current, previous)) {
			a.eval.fail(rule.Categories...)
		}
		if skip {
			continue
		}
		// The two-cycle invalid-index rule can withhold a finding during
		// warmup/build uncertainty; that absence is not proof of clearance.
		if rule.Name == "invalid_indexes" && !invalidIndexResolutionReady(current, a.extras) {
			a.eval.fail("invalid_index")
		}
		a.eval.evaluated(rule.Categories...)
		got := rule.Fn(current, previous, a.cfg, a.extras)
		out = append(out, got...)
		a.holdUndecided(rule, current, previous, got)
	}
	return out
}

// holdUndecided keeps or fails the categories a rule ran on but could not
// decide this cycle, so their open findings are not resolved on no verdict.
func (a *Analyzer) holdUndecided(
	rule RuleSpec, current, previous *collector.Snapshot, got []Finding,
) {
	if rule.Name == "slow_queries" && hasQueryInterval(current, previous) {
		// An idle or thin interval is no verdict (rules-05).
		a.eval.keep("slow_query",
			undecidedQueries(current, previous, slowQueryMinCalls(a.cfg))...)
	}
	if rule.Name == "unused_indexes" {
		// The zero-scan clock's restart warmup and holds (rules-04).
		a.holdUnusedFindings(current, previous, got)
	}
	if rule.Name == "index_bloat" {
		// An index read without an estimate is no verdict (rules-13).
		a.eval.keep("index_bloat", current.IndexBloatUnknown...)
	}
	if rule.Name == "inactive_slots" && a.extras != nil && a.extras.InactiveSlotPending {
		a.eval.fail("inactive_slot")
	}
	if rule.Name == "wal_archive_failing" && !archiverDeferred(a.extras) &&
		archiverStillFailing(current) {
		// No success since the last failure: no verdict of recovery.
		a.eval.keep(categoryWALArchiveFailing, archiverIdentity)
	}
	for category, idents := range a.extras.takeUndecided() {
		a.eval.keep(category, idents...)
	}
}

// runDatabaseRules runs rules that need their own catalog/sage queries.
// Each check marks its category failed on a query error (evalFail).
func (a *Analyzer) runDatabaseRules(
	ctx context.Context,
	current, previous *collector.Snapshot,
	skipQueryRules bool,
) []Finding {
	// Before query_regression, which reads the active statement buckets.
	out := a.runWorkloadAnomaly(ctx, current, previous)
	out = append(out, a.checkXIDWraparound(ctx)...)
	out = append(out, a.checkConnectionLeaks(ctx)...)
	a.eval.evaluated("xid_wraparound", "connection_leak")
	if skipQueryRules {
		a.eval.fail("query_regression", "sort_without_index", "plan_regression")
		return out
	}
	out = append(out, a.runQueryRegression(ctx, current, previous)...)
	out = append(out, a.checkSortWithoutIndex(ctx)...)
	planDiff := a.checkPlanRegression(ctx)
	// Enrich plan regressions with a plain-English "why did the plan
	// change" narrative when the LLM narrator is attached (C6).
	if a.planNarrator != nil && len(planDiff) > 0 {
		planDiff = a.planNarrator.Narrate(ctx, planDiff)
	}
	a.eval.evaluated("query_regression", "sort_without_index", "plan_regression")
	return append(out, planDiff...)
}

// runSeqScanWatchdog skips tables already flagged by the missing-FK rule
// and records the flagged tables for the index floor. Without a
// table-counter interval (no previous snapshot, a moved stats epoch)
// seq_scan_heavy is not evaluated, so its open findings stay open; tables
// with no interval of their own are kept open (rules-10).
func (a *Analyzer) runSeqScanWatchdog(
	current, previous *collector.Snapshot, prior []Finding,
) []Finding {
	if len(current.Tables) == 0 || !hasRelationInterval(current, previous) {
		a.eval.fail("seq_scan_heavy")
	}
	a.eval.evaluated("seq_scan_heavy")
	a.eval.keep("seq_scan_heavy", seqScanUndecided(current, previous)...)
	out := ruleSeqScanWatchdog(current, previous, a.cfg, missingFKTables(prior))
	a.eval.seqHeavy = seqHeavyTables(current, out) // the index floor's tables
	return out
}

// missingFKTables returns the identifiers already flagged by the missing
// FK index rule so the seq-scan watchdog does not double-flag them.
func missingFKTables(findings []Finding) map[string]bool {
	out := make(map[string]bool)
	for _, f := range findings {
		if f.Category == "missing_fk_index" {
			// Identifier is "schema.table(cols)"; the watchdog keys on
			// "schema.table".
			table, _, _ := strings.Cut(f.ObjectIdentifier, "(")
			out[table] = true
		}
	}
	return out
}

// runProducers runs the tuning agent (or, when it is inactive, the index
// floor), advisor, forecaster and tuner.
// Tables with fresh or still-open index recommendations are deferred so
// the tuner does not install a hint that a pending index would obsolete.
func (a *Analyzer) runProducers(
	ctx context.Context, current, previous *collector.Snapshot,
) []Finding {
	deferredTables := make(map[string]bool)
	tuned := a.runTuning(ctx, current, previous)
	floor := a.runIndexFloor(ctx, current, tuned)
	out := append(tuned.Findings, floor...)
	for _, t := range append(tuned.IndexTables, findingTables(floor)...) {
		if c := canonicalTable(t); c != "" {
			deferredTables[c] = true
		}
	}
	for _, t := range a.openIndexRecommendationTables(ctx) {
		deferredTables[t] = true
	}
	out = append(out, a.runAdvisorAndForecaster(ctx)...)
	if a.tuner != nil {
		tunerFindings, err := a.tuner.Tune(ctx, deferredTables)
		if err != nil {
			a.logFn("WARN", "analyzer: tuner: %v", err)
		} else {
			out = append(out, tunerFindings...)
		}
	}
	return out
}

func (a *Analyzer) runAdvisorAndForecaster(ctx context.Context) []Finding {
	var out []Finding
	if a.advisor != nil {
		advFindings, err := a.advisor.Analyze(ctx)
		if err != nil {
			a.logFn("WARN", "analyzer: advisor: %v", err)
		} else {
			out = append(out, advFindings...)
			// The advisor re-checks its open findings deterministically
			// (advisors-02): the categories it fully read may resolve.
			if r, ok := a.advisor.(EvaluatedCategoryReporter); ok {
				a.eval.evaluated(r.LastEvaluatedCategories()...)
			}
		}
	}
	if a.forecaster != nil {
		fcFindings, err := a.forecaster.Forecast(ctx)
		if err != nil {
			a.logFn("WARN", "analyzer: forecaster: %v", err)
		} else {
			out = append(out, fcFindings...)
			if r, ok := a.forecaster.(EvaluatedCategoryReporter); ok {
				a.eval.evaluated(r.LastEvaluatedCategories()...)
			}
		}
	}
	return out
}

// runLateChecks runs checks that must observe this cycle's tuner output
// (work_mem promotion) plus extension drift, lock chains and detectors.
func (a *Analyzer) runLateChecks(ctx context.Context) []Finding {
	out := a.checkWorkMemPromotion(ctx)
	if a.pool != nil && a.cfg.Analyzer.WorkMemPromotionThreshold > 0 {
		a.eval.evaluated("work_mem_promotion")
	}
	out = append(out, a.checkExtensionDrift(ctx)...)
	a.eval.evaluated("extension_drift")
	if a.cfg.Analyzer.LockChain.Enabled {
		chains, err := ProbeLockChains(ctx, a.pool, a.cfg, a.lockObs)
		a.eval.evaluated("lock_chain")
		if err != nil {
			a.eval.fail("lock_chain")
			a.logFn("WARN", "analyzer: lock chains: %v", err)
		} else {
			out = append(out, chains...)
		}
	}
	return append(out, a.runSupplementalDetectors(ctx)...)
}

// runSupplementalDetectors runs the attached detectors. A detector that
// reports the categories its run evaluated (EvaluatedCategoryReporter, as
// the agent posture monitor does) lets their cleared findings resolve; a
// detector that failed evaluated nothing, and one that reports incomplete
// categories (IncompleteCategoryReporter) keeps their open findings.
func (a *Analyzer) runSupplementalDetectors(ctx context.Context) []Finding {
	var out []Finding
	for _, detector := range a.detectors {
		findings, err := detector.Detect(ctx)
		if err != nil {
			a.logFn("WARN", "analyzer: supplemental detector: %v", err)
			continue
		}
		if r, ok := detector.(EvaluatedCategoryReporter); ok {
			a.eval.evaluated(r.LastEvaluatedCategories()...)
		}
		if r, ok := detector.(IncompleteCategoryReporter); ok {
			a.eval.fail(r.LastIncompleteCategories()...)
		}
		out = append(out, findings...)
	}
	return out
}

// runRCA feeds this cycle's lock-chain findings into the RCA engine.
func (a *Analyzer) runRCA(
	ctx context.Context,
	current, previous *collector.Snapshot,
	findings []Finding,
) {
	if a.rcaEngine == nil {
		return
	}
	var lockChains []Finding
	for _, f := range findings {
		if f.Category == "lock_chain" {
			lockChains = append(lockChains, f)
		}
	}
	a.rcaEngine.Analyze(current, previous, a.cfg, lockChains)
	if err := a.rcaEngine.PersistIncidents(ctx, a.pool); err != nil {
		a.logFn("WARN", "analyzer: rca persist: %v", err)
	}
}
