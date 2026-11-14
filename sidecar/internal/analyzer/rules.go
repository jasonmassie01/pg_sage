package analyzer

import (
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre/probes"
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
	// IndexOID records the object (oid) each FirstSeen clock belongs to: a
	// dropped and recreated index under the same name is a new object.
	IndexOID map[string]uint32
	// InactiveSlotSince records when each replication slot was first seen
	// inactive, for servers without pg_replication_slots.inactive_since
	// (before PG17). It lives in memory: a restart only delays a finding.
	InactiveSlotSince map[string]time.Time
	// InactiveSlotPending is set by the slot rule when an inactive slot is
	// below the threshold on the in-memory clock (no inactive_since): it
	// may carry an open finding from before a restart, so its absence is
	// not proof of clearance and inactive_slot must not resolve.
	InactiveSlotPending bool
	// FreezeHorizons is this cycle's reading of the database's freeze
	// sampler (the custodian's horizon reader); nil when it could not run.
	FreezeHorizons []probes.FreezeHorizon
	// ArchiverOwner is the fleet database the cluster-wide archiver
	// findings are left to this cycle; "" when this database reports them.
	ArchiverOwner string
	// Now is the rule clock; nil means time.Now (tests set it).
	Now func() time.Time
	// zeroScanSeen records the identities whose zero-scan clock the
	// unused_index rule read this cycle (rules-04); nil outside a cycle.
	zeroScanSeen map[string]bool
	// subErrorsSeen is each subscription's error count when the analyzer
	// last read it, and cdcStall each active logical slot's stall clock
	// (market-13). Both live in memory: a restart only delays a finding.
	subErrorsSeen map[string]int64
	cdcStall      map[string]cdcStallClock
	// undecided names, per category, identities a rule could not decide
	// this cycle: their open findings stay open (runSnapshotRules).
	undecided map[string][]string
	// ext is this cycle's pg_cron, pg_partman and TimescaleDB reading
	// (market-16, market-16b);
	// extWarned names the reads in a streak of failures already logged.
	ext       *extReading
	extWarned map[string]bool
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
// Rules that need extra parameters (XID, leaked connections, the
// regression baseline) are called separately in the analyzer loop.
var AllRules = []RuleSpec{
	// Index rules
	{"unused_indexes", ruleUnusedIndexes, cats("unused_index"), hasIndexes},
	{"invalid_indexes", ruleInvalidIndexes, cats("invalid_index"), hasIndexList},
	{"duplicate_indexes", ruleDuplicateIndexes, cats("duplicate_index"), hasIndexList},
	{"missing_fk_indexes", ruleMissingFKIndexes, cats("missing_fk_index"), hasForeignKeys},
	{"index_bloat", ruleIndexBloat, cats("index_bloat"), hasIndexBloat},

	// Vacuum / bloat rules
	{"table_bloat", ruleTableBloat, cats("table_bloat"), hasTables},
	{"autovacuum_tuning", ruleAutovacuumTuning, cats("autovacuum_tuning"), hasTables},
	{"stale_statistics", ruleStaleStatistics, cats("stale_statistics"), hasTables},
	{"wraparound_freeze", ruleWraparoundFreeze, cats("wraparound_freeze"), hasTables},

	// Query rules
	{"slow_queries", ruleSlowQueries, cats("slow_query"), hasQueryInterval},
	{"high_plan_time", ruleHighPlanTime, cats("high_plan_time"), hasQueries},
	{"temp_spills", ruleTempSpills, cats("temp_spill"), hasTempSpillWindow},

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

	// Subscriber and CDC consumer health (market-13)
	{"subscription_errors", ruleSubscriptionErrors, cats("subscription_errors"),
		hasSubscriptions},
	{"subscription_lag", ruleSubscriptionLag, cats("subscription_lag"), hasSubscriptions},
	{"cdc_consumer_stalled", ruleCDCConsumerStalled, cats("cdc_consumer_stalled"),
		hasSlotList},
	{"cdc_heartbeat_stale", ruleCDCHeartbeatStale, cats("cdc_heartbeat_stale"), nil},

	// WAL archiver rules (market-07, rules_backup.go)
	{"wal_archive_failing", ruleWALArchiveFailing, cats(categoryWALArchiveFailing),
		hasArchiverInterval},
	{"wal_archive_stale", ruleWALArchiveStale, cats(categoryWALArchiveStale), hasArchiver},

	// Extension health (market-16, rules_ext_*.go): the cycle's extension
	// reading marks a category not evaluated when it could not run.
	{categoryCronJobFailed, rulePgCronJobFailed, cats(categoryCronJobFailed), nil},
	{categoryCronHistory, rulePgCronHistoryUnbounded, cats(categoryCronHistory), nil},
	{categoryPartmanPremake, rulePartmanPremakeGap, cats(categoryPartmanPremake), nil},
	{categoryPartmanDefault, rulePartmanDefaultRows, cats(categoryPartmanDefault), nil},
	// TimescaleDB jobs and continuous aggregates (market-16b), behind the
	// same gate.
	{categoryTSJobFailed, ruleTimescaleJobFailed, cats(categoryTSJobFailed), nil},
	{categoryTSCaggStale, ruleTimescaleCaggStale, cats(categoryTSCaggStale), nil},
}

func cats(c ...string) []string { return c }

// hasIndexes needs the index list and the foreign keys: an index that
// supports a key must not look unused when the keys could not be read, or
// when the partitions a key is enforced on are unknown (fkKeysPlaced).
// The partitioned indexes (rules-03) say which indexes are attached: without
// them an unused partitioned index's roll-up would resolve.
func hasIndexes(cur, _ *collector.Snapshot) bool {
	return len(cur.Indexes) > 0 && cur.Available("indexes") &&
		cur.Available("foreign_keys") && cur.Available("partitioned_indexes") &&
		fkKeysPlaced(cur)
}

// partitionsKnown reports whether the partition tree and the partitioned
// indexes were read this cycle.
func partitionsKnown(cur *collector.Snapshot) bool {
	return cur.Available("partitions") && cur.Available("partitioned_indexes")
}

// hasIndexList needs only the index list.
func hasIndexList(cur, _ *collector.Snapshot) bool {
	return len(cur.Indexes) > 0 && cur.Available("indexes")
}

// hasForeignKeys needs the keys and the index list: a missing index list
// would make every key look unindexed. It needs the tables (relkind) and
// the partition categories too: a partitioned table read as a plain one
// would get CREATE INDEX CONCURRENTLY, which PostgreSQL rejects (rules-03).
func hasForeignKeys(cur, _ *collector.Snapshot) bool {
	return len(cur.ForeignKeys) > 0 && cur.Available("foreign_keys") &&
		cur.Available("indexes") && cur.Available("tables") && partitionsKnown(cur)
}

func hasTables(cur, _ *collector.Snapshot) bool {
	return len(cur.Tables) > 0 && cur.Available("tables")
}
func hasQueries(cur, _ *collector.Snapshot) bool     { return len(cur.Queries) > 0 }
func hasSequences(cur, _ *collector.Snapshot) bool   { return len(cur.Sequences) > 0 }
func hasReplication(cur, _ *collector.Snapshot) bool { return cur.Replication != nil }

// hasCacheRatio needs a measured interval with enough block accesses: the
// first cycle after a start, a reset or a quiet interval proves nothing
// either way, so an open cache finding is left as it is.
func hasCacheRatio(cur, _ *collector.Snapshot) bool {
	_, _, ok := IntervalCacheRatio(cur)
	return ok
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
