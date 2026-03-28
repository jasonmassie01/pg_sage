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
}

// RuleFunc is the standard signature for snapshot-based rules.
type RuleFunc func(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	extras *RuleExtras,
) []Finding

// AllRules is the registry of all snapshot-based rules.
// Rules that need extra parameters (XID, leaked connections, regression
// history) are called separately in the analyzer loop.
var AllRules = []struct {
	Name string
	Fn   RuleFunc
}{
	// Index rules
	{"unused_indexes", ruleUnusedIndexes},
	{"invalid_indexes", ruleInvalidIndexes},
	{"duplicate_indexes", ruleDuplicateIndexes},
	{"missing_fk_indexes", ruleMissingFKIndexes},

	// Vacuum / bloat rules
	{"table_bloat", ruleTableBloat},

	// Query rules
	{"slow_queries", ruleSlowQueries},
	{"high_plan_time", ruleHighPlanTime},

	// System rules
	{"cache_hit_ratio", ruleCacheHitRatio},
	{"checkpoint_pressure", ruleCheckpointPressure},
	{"stat_statements_capacity", ruleStatStatementsCapacity},

	// Total-time rules
	{"high_total_time", ruleTotalTimeHeavy},
	{"high_freq_first_cycle", ruleHighFreqFirstCycle},

	// Sequence rules
	{"sequence_exhaustion", ruleSequenceExhaustion},

	// Replication rules
	{"replication_lag", ruleReplicationLag},
	{"inactive_slots", ruleInactiveSlots},
}
