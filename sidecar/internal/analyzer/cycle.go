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

func (a *Analyzer) cycle(ctx context.Context) {
	latest := a.collector.LatestSnapshot()
	if latest == nil {
		a.logFn("DEBUG", "analyzer: no snapshot yet, skipping")
		return
	}
	if !a.claimFreshSnapshot(latest) {
		return
	}
	current := snapshotForAnalysis(latest)
	previous := snapshotForAnalysis(a.collector.PreviousSnapshot())
	filterSchemaExclusions(current)
	if previous != nil {
		filterSchemaExclusions(previous)
	}
	excluded := a.excludeFactSchemas(ctx, current, previous)

	a.eval = newCycleEval()
	// Load recently created indexes to prevent cooldown violations.
	a.loadRecentlyCreatedIndexes(ctx)
	a.loadStatsEpoch(ctx)
	a.loadIndexBuilds(ctx)

	// Skip query-based rules when pg_stat_statements was reset.
	skipQueryRules := current.StatsReset
	if skipQueryRules {
		a.logFn("WARN", "stats reset detected, skipping query rules")
	}

	all := a.runSnapshotRules(current, previous, skipQueryRules)
	all = append(all, a.runDatabaseRules(ctx, current, previous, skipQueryRules)...)
	all = append(all, a.runSeqScanWatchdog(current, previous, all)...)
	all = append(all, a.runProducers(ctx, current)...)
	all = append(all, a.runLateChecks(ctx)...)
	all = append(all, a.checkSageFootprint(ctx, current)...)
	all = append(all, a.checkSelfCost(ctx)...)
	all = a.applyAppManaged(ctx, all)
	all = a.applyFactFilter(ctx, all, excluded)
	if current.Available("tables") && len(current.Tables) > 0 {
		all = collapseCloneSchemas(current, all, a.cloneSignals(ctx))
		a.eval.evaluated(CategoryCloneSchemas)
	}
	all = dropNonWorkloadFindings(all)
	a.runRCA(ctx, current, previous, all)

	// Deduplicate conflicting findings across advisors.
	all = DeduplicateFindings(all, computeIOUtilPct(current), a.logFn)
	a.finalizeCycle(ctx, all, a.eval.resolvable(all))
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
		skip := skipQueryRules && queryRuleNames[rule.Name]
		if skip || (rule.Needs != nil && !rule.Needs(current, previous)) {
			a.eval.fail(rule.Categories...)
		}
		if skip {
			continue
		}
		a.eval.evaluated(rule.Categories...)
		out = append(out, rule.Fn(current, previous, a.cfg, a.extras)...)
	}
	return out
}

// runDatabaseRules runs rules that need their own catalog/sage queries.
// Each check marks its category failed on a query error (evalFail).
func (a *Analyzer) runDatabaseRules(
	ctx context.Context,
	current, previous *collector.Snapshot,
	skipQueryRules bool,
) []Finding {
	out := a.checkXIDWraparound(ctx)
	out = append(out, a.checkConnectionLeaks(ctx)...)
	a.eval.evaluated("xid_wraparound", "connection_leak")
	if skipQueryRules {
		a.eval.fail("query_regression", "sort_without_index", "plan_regression")
		return out
	}
	historicalAvg := a.buildHistoricalAverages(ctx)
	out = append(out, ruleQueryRegression(
		current, previous, historicalAvg, a.cfg)...)
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

// runSeqScanWatchdog skips tables already flagged by the missing-FK rule.
func (a *Analyzer) runSeqScanWatchdog(
	current, previous *collector.Snapshot, prior []Finding,
) []Finding {
	if len(current.Tables) == 0 {
		a.eval.fail("seq_scan_heavy")
	}
	a.eval.evaluated("seq_scan_heavy")
	return ruleSeqScanWatchdog(current, previous, a.cfg, missingFKTables(prior))
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

// runProducers runs the LLM optimizer, advisor, forecaster and tuner.
// Tables with fresh or still-open index recommendations are deferred so
// the tuner does not install a hint that a pending index would obsolete.
func (a *Analyzer) runProducers(
	ctx context.Context, current *collector.Snapshot,
) []Finding {
	deferredTables := make(map[string]bool)
	var out []Finding
	// Without the index list the optimizer would see tables as unindexed
	// and propose indexes that exist (dogfood lifeos-1).
	if a.optimizer != nil && !current.Available("indexes") {
		a.logFn("WARN", "analyzer: index optimizer skipped: indexes unavailable "+
			"this cycle")
	} else if a.optimizer != nil {
		optResult, err := a.optimizer.Analyze(ctx, current)
		if err != nil {
			a.logFn("WARN", "analyzer: index optimizer: %v", err)
		} else if optResult != nil {
			for _, rec := range optResult.Recommendations {
				if t := canonicalTable(rec.Table); t != "" {
					deferredTables[t] = true
				}
				out = append(out,
					optimizerRecommendationToFinding(rec, optResult))
			}
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
		chains, err := ProbeLockChains(ctx, a.pool, a.cfg)
		a.eval.evaluated("lock_chain")
		if err != nil {
			a.eval.fail("lock_chain")
			a.logFn("WARN", "analyzer: lock chains: %v", err)
		} else {
			out = append(out, chains...)
		}
	}
	for _, detector := range a.detectors {
		findings, err := detector.Detect(ctx)
		if err != nil {
			a.logFn("WARN", "analyzer: supplemental detector: %v", err)
			continue
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
