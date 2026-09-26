package analyzer

import (
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// RuleExtras carries cross-cycle state for rules that need historical context.
type RuleExtras struct {
	FirstSeen       map[string]time.Time
	RecentlyCreated map[string]time.Time
	// StatsEpoch is the latest of pg_stat_database.stats_reset and
	// pg_postmaster_start_time(): cumulative index counters only cover
	// the time since this instant.
	StatsEpoch time.Time
	// InvalidFirstSeen records when each invalid index was first observed.
	InvalidFirstSeen map[string]time.Time
	// IndexBuildTables holds "schema.table" keys with an index build in
	// progress (pg_stat_progress_create_index).
	IndexBuildTables map[string]bool
	// IndexBuildProbeFailed is true when the in-progress build probe
	// could not run this cycle.
	IndexBuildProbeFailed bool
}

// RuleFunc is the standard signature for snapshot-based rules.
type RuleFunc func(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	extras *RuleExtras,
) []Finding

// RuleSpec registers a snapshot rule together with the finding
// categories it owns and the snapshot data it needs. A rule that ran on
// complete input and emitted nothing proves its categories are clear, so
// their open findings can be resolved (G2-B02). When Needs reports the
// input is missing, the categories are left untouched this cycle.
type RuleSpec struct {
	Name       string
	Fn         RuleFunc
	Categories []string
	Needs      func(current, previous *collector.Snapshot) bool
}

// AllRules is the registry of all snapshot-based rules.
// Rules that need extra parameters (XID, leaked connections, regression
// history) are called separately in the analyzer loop.
var AllRules = []RuleSpec{
	// Index rules
	{"unused_indexes", ruleUnusedIndexes, cats("unused_index"), hasIndexes},
	{"invalid_indexes", ruleInvalidIndexes, cats("invalid_index"), hasIndexes},
	{"duplicate_indexes", ruleDuplicateIndexes, cats("duplicate_index"), hasIndexes},
	{"missing_fk_indexes", ruleMissingFKIndexes, cats("missing_fk_index"), hasForeignKeys},

	// Vacuum / bloat rules
	{"table_bloat", ruleTableBloat, cats("table_bloat"), hasTables},
	{"autovacuum_tuning", ruleAutovacuumTuning, cats("autovacuum_tuning"), hasTables},
	{"stale_statistics", ruleStaleStatistics, cats("stale_statistics"), hasTables},
	{"wraparound_freeze", ruleWraparoundFreeze, cats("wraparound_freeze"), hasTables},

	// Query rules
	{"slow_queries", ruleSlowQueries, cats("slow_query"), hasQueries},
	{"high_plan_time", ruleHighPlanTime, cats("high_plan_time"), hasQueries},

	// System rules
	{"cache_hit_ratio", ruleCacheHitRatio, cats("cache_hit_ratio"), hasCacheRatio},
	{"checkpoint_pressure", ruleCheckpointPressure,
		cats("checkpoint_pressure"), hasCheckpointWindow},
	{"stat_statements_capacity", ruleStatStatementsCapacity,
		cats("stat_statements_pressure"), hasStatStatementsMax},

	// Total-time rules
	{"high_total_time", ruleTotalTimeHeavy, cats("high_total_time"), hasQueryDelta},
	// First-cycle supplement: emits high_total_time but does not own it.
	{"high_freq_first_cycle", ruleHighFreqFirstCycle, nil, nil},

	// Sequence rules
	{"sequence_exhaustion", ruleSequenceExhaustion,
		cats("sequence_exhaustion"), hasSequences},

	// Replication rules
	{"replication_lag", ruleReplicationLag, cats("replication_lag"), hasReplication},
	{"inactive_slots", ruleInactiveSlots,
		cats("inactive_slot", "slow_replication_slot"), hasReplication},
}

func cats(c ...string) []string { return c }

func hasIndexes(cur, _ *collector.Snapshot) bool     { return len(cur.Indexes) > 0 }
func hasForeignKeys(cur, _ *collector.Snapshot) bool { return len(cur.ForeignKeys) > 0 }
func hasTables(cur, _ *collector.Snapshot) bool      { return len(cur.Tables) > 0 }
func hasQueries(cur, _ *collector.Snapshot) bool     { return len(cur.Queries) > 0 }
func hasSequences(cur, _ *collector.Snapshot) bool   { return len(cur.Sequences) > 0 }
func hasReplication(cur, _ *collector.Snapshot) bool { return cur.Replication != nil }

func hasCacheRatio(cur, _ *collector.Snapshot) bool {
	return NormalizeCacheHitRatio(cur.System.CacheHitRatio) > 0
}

func hasStatStatementsMax(cur, _ *collector.Snapshot) bool {
	return cur.System.StatStatementsMax > 0
}

func hasQueryDelta(cur, prev *collector.Snapshot) bool {
	return prev != nil && len(cur.Queries) > 0 && len(prev.Queries) > 0 &&
		cur.CollectedAt.After(prev.CollectedAt)
}

func hasCheckpointWindow(cur, prev *collector.Snapshot) bool {
	if prev == nil || cur.CollectedAt.IsZero() || prev.CollectedAt.IsZero() {
		return false
	}
	return cur.CollectedAt.Sub(prev.CollectedAt) >= time.Minute
}
