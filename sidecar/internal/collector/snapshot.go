package collector

import (
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Snapshot holds all stats collected in a single cycle.
type Snapshot struct {
	CollectedAt   time.Time
	Queries       []QueryStats
	Tables        []TableStats
	Indexes       []IndexStats
	ForeignKeys   []ForeignKey
	System        SystemStats
	Locks         []LockInfo
	Sequences     []SequenceStats
	Replication   *ReplicationStats
	IO            []IOStats             `json:"io,omitempty"`
	Partitions    []PartitionInfo       `json:"partitions,omitempty"`
	PreparedXacts []PreparedTransaction `json:"prepared_xacts,omitempty"`
	// XminHolders are the holders of this database's xmin horizon
	// (probes.XminHorizonSQL: sessions, standbys' feedback, prepared
	// transactions, slots), oldest first. Not persisted.
	XminHolders []probes.XminHolder `json:"-"`
	ConfigData  *ConfigSnapshot     `json:"config_data,omitempty"`
	StatsReset  bool                `json:"stats_reset,omitempty"`
	// StatsEpoch is the pg_stat_statements statistics epoch the query
	// counters belong to; zero when it could not be read.
	StatsEpoch time.Time `json:"stats_epoch,omitzero"`
	// Archiver is the server's WAL archiver row (market-07); nil when the
	// read failed.
	Archiver *ArchiverStats `json:"archiver,omitempty"`
	// Unavailable names the catalog categories that could not be read this
	// cycle, with why (dogfood lifeos-1). Such a category is unknown, not
	// empty: rules that would read its absence as a fact do not run.
	Unavailable map[string]string `json:"unavailable,omitempty"`
	// LocksDropped is how many wait-graph rows the read saw past the
	// lockRowCap it keeps: 0, or 1 when the graph was truncated (the read
	// stops at the first row past the cap). Not persisted.
	LocksDropped int `json:"-"`
	// SequenceCoverage is how much of the sequence catalog this cycle
	// read (not persisted).
	SequenceCoverage *SequenceCoverage `json:"sequence_coverage,omitempty"`

	// PartitionedIndexes are the partitioned (relkind I) indexes, which
	// pg_stat_user_indexes leaves out.
	PartitionedIndexes []PartitionedIndex `json:"partitioned_indexes,omitempty"`

	// IndexBloat is the catalog-statistics bloat estimate of the largest
	// btree indexes (rules-13); nil when it was not read. Truncated is set
	// when more indexes were above the size floor than the row cap read;
	// Unknown names (schema.index) the indexes read without an estimate (a
	// key column without statistics). Neither proves that an index shrank.
	IndexBloat          []IndexBloat `json:"index_bloat,omitempty"`
	IndexBloatTruncated bool         `json:"-"`
	IndexBloatUnknown   []string     `json:"-"`

	// Subscriptions are this database's logical replication subscriptions
	// (market-13): empty when there are none, nil when not read.
	Subscriptions []SubscriptionInfo `json:"subscriptions,omitempty"`
	// CDCHeartbeats are this cycle's replication.cdc_heartbeats readings
	// for the logical slots of this database. Not persisted.
	CDCHeartbeats []CDCHeartbeatReading `json:"-"`
	// WorkloadSessions is who is using this database now, for workload
	// budgets (market-08); nil when it was not read.
	WorkloadSessions *WorkloadSessions `json:"workload_sessions,omitempty"`
	// WaitEvents are the active client sessions by wait class this cycle
	// (market-01); nil when they were not read.
	WaitEvents []WaitEventLoad `json:"wait_events,omitempty"`

	// stmtTotals are the statement totals the queries step read, copied
	// into System after the steps (applyStatementTotals).
	stmtTotals statementTotals
}

// Available reports whether category was read this cycle.
func (s *Snapshot) Available(category string) bool {
	_, missing := s.Unavailable[category]
	return !missing
}

// QueryStats mirrors pg_stat_statements columns.
type QueryStats struct {
	QueryID           int64   `json:"queryid"`
	Query             string  `json:"query"`
	Calls             int64   `json:"calls"`
	TotalExecTime     float64 `json:"total_exec_time"`
	MeanExecTime      float64 `json:"mean_exec_time"`
	MinExecTime       float64 `json:"min_exec_time"`
	MaxExecTime       float64 `json:"max_exec_time"`
	StddevExecTime    float64 `json:"stddev_exec_time"`
	Rows              int64   `json:"rows"`
	SharedBlksHit     int64   `json:"shared_blks_hit"`
	SharedBlksRead    int64   `json:"shared_blks_read"`
	SharedBlksDirtied int64   `json:"shared_blks_dirtied"`
	SharedBlksWritten int64   `json:"shared_blks_written"`
	TempBlksRead      int64   `json:"temp_blks_read"`
	TempBlksWritten   int64   `json:"temp_blks_written"`
	BlkReadTime       float64 `json:"blk_read_time"`
	BlkWriteTime      float64 `json:"blk_write_time"`
	WALRecords        int64   `json:"wal_records,omitempty"`
	WALFpi            int64   `json:"wal_fpi,omitempty"`
	WALBytes          int64   `json:"wal_bytes,omitempty"`
	TotalPlanTime     float64 `json:"total_plan_time,omitempty"`
	MeanPlanTime      float64 `json:"mean_plan_time,omitempty"`
	// AdmittedBy names the arms of the ranked read that admitted the query
	// (collection-07): pinned, mean, io, total.
	AdmittedBy []string `json:"admitted_by,omitempty"`
}

// TableStats mirrors pg_stat_user_tables + size info.
type TableStats struct {
	SchemaName       string     `json:"schemaname"`
	RelName          string     `json:"relname"`
	SeqScan          int64      `json:"seq_scan"`
	SeqTupRead       int64      `json:"seq_tup_read"`
	IdxScan          int64      `json:"idx_scan"`
	IdxTupFetch      int64      `json:"idx_tup_fetch"`
	NTupIns          int64      `json:"n_tup_ins"`
	NTupUpd          int64      `json:"n_tup_upd"`
	NTupDel          int64      `json:"n_tup_del"`
	NTupHotUpd       int64      `json:"n_tup_hot_upd"`
	NLiveTup         int64      `json:"n_live_tup"`
	NDeadTup         int64      `json:"n_dead_tup"`
	LastVacuum       *time.Time `json:"last_vacuum"`
	LastAutovacuum   *time.Time `json:"last_autovacuum"`
	LastAnalyze      *time.Time `json:"last_analyze"`
	LastAutoanalyze  *time.Time `json:"last_autoanalyze"`
	VacuumCount      int64      `json:"vacuum_count"`
	AutovacuumCount  int64      `json:"autovacuum_count"`
	AnalyzeCount     int64      `json:"analyze_count"`
	AutoanalyzeCount int64      `json:"autoanalyze_count"`
	TotalBytes       int64      `json:"total_bytes"`
	TableBytes       int64      `json:"table_bytes"`
	IndexBytes       int64      `json:"index_bytes"`
	Relpersistence   string     `json:"relpersistence"`
	// Relkind is pg_class.relkind: r (table), m (materialized view) or p
	// (partitioned parent, which has no storage of its own).
	Relkind string `json:"relkind,omitempty"`
	// XIDAge is the older of age(relfrozenxid) of the relation and of its
	// TOAST table. MXIDAge is mxid_age(relminmxid). Both are -1 when
	// unknown: a partitioned parent's relfrozenxid and relminmxid are 0,
	// whose age PostgreSQL reports as 2147483647.
	XIDAge  int64 `json:"xid_age"`
	MXIDAge int64 `json:"mxid_age"`
	// HeapBlksRead is the table's heap blocks read from outside
	// shared_buffers since its statistics epoch (pg_statio_user_tables.
	// heap_blks_read). The cache forecast ranks tables by its growth over
	// a day (forecast-04); rows written before it lack the key.
	HeapBlksRead int64 `json:"heap_blks_read"`
	// ModSinceAnalyze is pg_stat_get_mod_since_analyze: the rows inserted,
	// updated or deleted since the table was last analyzed, the count
	// autoanalyze compares with AnalyzeThreshold (rules-10). A partitioned
	// parent's own count stays 0: autoanalyze never processes it.
	ModSinceAnalyze int64 `json:"mod_since_analyze"`
	// AnalyzeThreshold is the autoanalyze trigger point,
	// autovacuum_analyze_threshold + autovacuum_analyze_scale_factor x
	// reltuples (0 when never counted), each from the table's reloptions
	// when set there, else from the server settings.
	AnalyzeThreshold float64 `json:"analyze_threshold"`
	// Reloptions is pg_class.reloptions ("name=value" storage parameters,
	// nil when none are set), read with the stats on every cycle.
	Reloptions []string `json:"reloptions"`
}

// IsUnlogged returns true if the table has relpersistence = 'u'.
// Unlogged tables do not generate WAL and are not crash-safe.
func (ts TableStats) IsUnlogged() bool {
	return ts.Relpersistence == "u"
}

// IndexStats mirrors pg_stat_user_indexes + pg_indexes metadata.
type IndexStats struct {
	SchemaName   string `json:"schemaname"`
	RelName      string `json:"relname"`
	IndexRelName string `json:"indexrelname"`
	IdxScan      int64  `json:"idx_scan"`
	IdxTupRead   int64  `json:"idx_tup_read"`
	IdxTupFetch  int64  `json:"idx_tup_fetch"`
	IndexBytes   int64  `json:"index_bytes"`
	IsUnique     bool   `json:"indisunique"`
	IsPrimary    bool   `json:"indisprimary"`
	IsValid      bool   `json:"indisvalid"`
	IndexDef     string `json:"indexdef"`
	IndexType    string `json:"index_type"`
	// IndexRelID is the index's oid: a dropped and recreated index under
	// the same name is a new object. Zero when unknown (legacy rows).
	IndexRelID uint32 `json:"indexrelid"`
	// LastIdxScan is pg_stat_user_indexes.last_idx_scan (PG16+): when the
	// index was last scanned. Nil before PG16 or when never scanned.
	LastIdxScan *time.Time `json:"last_idx_scan,omitempty"`
	// ParentIndexOID is the partitioned index this index is attached to
	// (pg_inherits); 0 for a standalone index. PostgreSQL refuses to drop
	// an attached index on its own.
	ParentIndexOID uint32 `json:"parent_index_oid,omitempty"`
}

// PartitionedIndex is a partitioned (relkind I) index: an index on a
// partitioned table, with no storage of its own and no statistics row.
type PartitionedIndex struct {
	SchemaName   string `json:"schemaname"`
	RelName      string `json:"relname"`
	IndexRelName string `json:"indexrelname"`
	IndexRelID   uint32 `json:"indexrelid"`
	// ParentIndexOID is the partitioned index this one is attached to
	// (a sub-partitioned partition's index); 0 at the top.
	ParentIndexOID uint32 `json:"parent_index_oid,omitempty"`
	// KeyColumns are the key columns in order (INCLUDE columns left out);
	// an expression key is "".
	KeyColumns []string `json:"key_columns"`
	IsValid    bool     `json:"indisvalid"`
	IsUnique   bool     `json:"indisunique"`
	IsPrimary  bool     `json:"indisprimary"`
	IsPartial  bool     `json:"partial,omitempty"`
}

// ForeignKey is one foreign key constraint (pg_constraint contype 'f'),
// partition clones left out. Columns are the referencing columns in
// declared (conkey) order. Schema is empty only for a row stored before
// collection-04 (DecodeForeignKeys): the owning schema is then unknown.
type ForeignKey struct {
	Schema          string   `json:"schemaname"`
	TableName       string   `json:"table_name"`
	RefSchema       string   `json:"ref_schemaname"`
	ReferencedTable string   `json:"referenced_table"`
	ConstraintName  string   `json:"constraint_name"`
	Columns         []string `json:"columns"`
	// OID is the constraint's pg_constraint oid; zero for legacy rows.
	OID uint32 `json:"oid"`
}

// SystemStats holds database-wide health metrics.
type SystemStats struct {
	ActiveBackends    int `json:"active_backends"`
	IdleInTransaction int `json:"idle_in_transaction"`
	// IdleInTxMaxSeconds is how long this database's longest
	// idle-in-transaction client session has been idle (rca-03); 0 with
	// none.
	IdleInTxMaxSeconds float64 `json:"idle_in_tx_max_seconds,omitempty"`
	TotalBackends      int     `json:"total_backends"`
	// ClientBackends counts the cluster's client sessions, the backends
	// that take max_connections slots (rca-08).
	ClientBackends int `json:"client_backends"`
	// ReservedConnections is the slots clients cannot take:
	// superuser_reserved_connections plus reserved_connections (PG 16+).
	// The connection forecast projects against max_connections less it
	// (forecast-08); legacy rows lack it.
	ReservedConnections int     `json:"reserved_connections"`
	MaxConnections      int     `json:"max_connections"`
	CacheHitRatio       float64 `json:"cache_hit_ratio"` // fraction 0..1, or CacheHitRatioUnknown
	Deadlocks           int64   `json:"deadlocks"`
	BlkReadTime         float64 `json:"blk_read_time"`
	BlkWriteTime        float64 `json:"blk_write_time"`
	TotalCheckpoints    int64   `json:"total_checkpoints"`
	// The checkpoint counters split (forecast-03): timed ones count every
	// checkpoint_timeout, skipped or not; requested ones are WAL-triggered
	// (max_wal_size) or explicit. CheckpointsDone is the performed count,
	// PG 18+ (nil before). Legacy rows lack all three.
	CheckpointsTimed     int64  `json:"checkpoints_timed"`
	CheckpointsRequested int64  `json:"checkpoints_requested"`
	CheckpointsDone      *int64 `json:"checkpoints_done,omitempty"`
	IsReplica            bool   `json:"is_replica"`
	DBSizeBytes          int64  `json:"db_size_bytes"`
	StatStatementsMax    int    `json:"stat_statements_max"`
	// StatStatements is how full pg_stat_statements is and with what;
	// nil when it was not read.
	StatStatements *StatStatementsUsage `json:"stat_statements_usage,omitempty"`
	// RelationStatsEpoch is the instant since which this database's table
	// and index counters accumulate: the later of pg_stat_database.
	// stats_reset (moved by pg_stat_reset() and by every single-relation
	// reset) and the postmaster start. Zero when unknown (legacy rows).
	RelationStatsEpoch time.Time `json:"relation_stats_epoch,omitzero"`
	// Lifetime pg_stat_database counters of this database (collection-06),
	// accumulated since RelationStatsEpoch; zero on legacy rows.
	BlksHit      int64 `json:"blks_hit"`
	BlksRead     int64 `json:"blks_read"`
	XactCommit   int64 `json:"xact_commit"`
	XactRollback int64 `json:"xact_rollback"`
	TempBytes    int64 `json:"temp_bytes"`
	// Sessions is pg_stat_database.sessions (PG 14+): the sessions this
	// database has had since RelationStatsEpoch, closed ones included.
	Sessions int64 `json:"sessions,omitempty"`
	// WALLSN is the cluster's WAL insert position in bytes, read at
	// WALReadAt (rca-03). Both are zero on a standby and when the position
	// could not be read this cycle.
	WALLSN    int64     `json:"wal_lsn,omitempty"`
	WALReadAt time.Time `json:"wal_read_at,omitzero"`
	// CheckpointStatsEpoch is the instant since which TotalCheckpoints
	// accumulates: the later of the checkpoint view's stats_reset
	// (pg_stat_bgwriter before 17, pg_stat_checkpointer from 17) and the
	// postmaster start. Zero when unknown (legacy rows).
	CheckpointStatsEpoch time.Time `json:"checkpoint_stats_epoch,omitzero"`
	// WALBytes is pg_stat_wal.wal_bytes, the WAL the cluster generated
	// since WALStatsEpoch, the later of pg_stat_wal.stats_reset and the
	// postmaster start (advisors-05). Both are zero when unread (legacy
	// rows, or pg_stat_wal unreadable this cycle).
	WALBytes      int64     `json:"cluster_wal_bytes,omitempty"`
	WALStatsEpoch time.Time `json:"wal_stats_epoch,omitzero"`
	// Interval is what the counters did since the previous snapshot; nil
	// on the first snapshot after a start and on legacy rows.
	Interval *SystemInterval `json:"interval,omitempty"`
	// StatementsEpoch is Snapshot.StatsEpoch, persisted here because the
	// system category is stored in full every cycle (evid-01a): the hourly
	// rollup breaks a pair of queries snapshots on its change. Zero (absent)
	// when unread and on legacy rows.
	StatementsEpoch time.Time `json:"statements_epoch,omitzero"`
	// The database's pg_stat_statements totals over all its entries but
	// pg_sage's known own, read with the queries step whatever the 500
	// cap admits (evid-01a: the rollup's coverage). All zero (absent) when
	// the queries step failed: unknown, not an idle database.
	StatementsTotalExecMs float64 `json:"statements_total_exec_ms,omitempty"`
	StatementsCalls       int64   `json:"statements_calls,omitempty"`
	StatementsEntries     int     `json:"statements_entries,omitempty"`
}

// LockInfo is one row of the lock wait graph (locksSQL): a waiting lock,
// or a session row (the session's own virtualxid lock; locktype session
// for a blocker holding a session-level lock outside a transaction) for a
// blocker, an idle-in-transaction session or an oldest-xmin holder.
type LockInfo struct {
	LockType string  `json:"locktype"`
	Mode     string  `json:"mode"`
	Granted  bool    `json:"granted"`
	RelName  *string `json:"relname"`
	// Query is the session's statement with its literals redacted, cut to
	// lockQueryMaxBytes.
	Query         *string    `json:"query"`
	State         *string    `json:"state"`
	WaitEventType *string    `json:"wait_event_type"`
	WaitEvent     *string    `json:"wait_event"`
	PID           int        `json:"pid"`
	BackendStart  *time.Time `json:"backend_start"`
	QueryStart    *time.Time `json:"query_start"`
	XactStart     *time.Time `json:"xact_start"`
	// StateChange is when the session entered its state: how long an
	// idle-in-transaction session has been idle.
	StateChange *time.Time `json:"state_change"`
	// BackendXminAge is the age of the older of the session's backend_xmin
	// and backend_xid: how far it holds back this database's xmin horizon.
	// Nil when it holds neither.
	BackendXminAge *int64 `json:"backend_xmin_age"`
	// BlockedBy is pg_blocking_pids of a waiting lock (0 is a prepared
	// transaction); nil on a session row.
	BlockedBy []int `json:"blocked_by"`
	// QueryID is pg_stat_activity.query_id; nil with compute_query_id off.
	QueryID *int64 `json:"query_id"`
}

// SequenceStats tracks sequence usage and exhaustion risk.
type SequenceStats struct {
	SchemaName   string  `json:"schemaname"`
	SequenceName string  `json:"sequencename"`
	DataType     string  `json:"data_type"`
	LastValue    int64   `json:"last_value"`
	MinValue     int64   `json:"min_value"`
	MaxValue     int64   `json:"max_value"`
	IncrementBy  int64   `json:"increment_by"`
	Cycle        bool    `json:"cycle"`
	PctUsed      float64 `json:"pct_used"` // consumed share of [min,max] in travel direction
}

// ReplicationStats aggregates replica and slot info.
type ReplicationStats struct {
	Replicas []ReplicaInfo `json:"replicas"`
	Slots    []SlotInfo    `json:"slots"`
}

// ReplicaInfo describes a single streaming replica. LSNs are NULL while a
// walsender is starting up or catching up (G1-B20).
type ReplicaInfo struct {
	// ApplicationName is the walsender's application_name: with
	// ClientAddr it names a replica stably across cycles (row order in
	// pg_stat_replication is not stable).
	ApplicationName string  `json:"application_name"`
	ClientAddr      *string `json:"client_addr"`
	State           string  `json:"state"`
	SentLSN         *string `json:"sent_lsn"`
	WriteLSN        *string `json:"write_lsn"`
	FlushLSN        *string `json:"flush_lsn"`
	ReplayLSN       *string `json:"replay_lsn"`
	WriteLag        *string `json:"write_lag"`
	FlushLag        *string `json:"flush_lag"`
	ReplayLag       *string `json:"replay_lag"`
	SyncState       string  `json:"sync_state"`
}

// IOStats holds pg_stat_io data (PG16+).
type IOStats struct {
	BackendType   string  `json:"backend_type"`
	Object        string  `json:"object"`
	Context       string  `json:"context"`
	Reads         int64   `json:"reads"`
	ReadTime      float64 `json:"read_time"`
	Writes        int64   `json:"writes"`
	WriteTime     float64 `json:"write_time"`
	Writebacks    int64   `json:"writebacks"`
	WritebackTime float64 `json:"writeback_time"`
	Extends       int64   `json:"extends"`
	ExtendTime    float64 `json:"extend_time"`
	Hits          int64   `json:"hits"`
	Evictions     int64   `json:"evictions"`
	Reuses        int64   `json:"reuses"`
	Fsyncs        int64   `json:"fsyncs"`
	FsyncTime     float64 `json:"fsync_time"`
}

// PartitionInfo maps a child partition to its parent table.
type PartitionInfo struct {
	ChildTable   string `json:"child_table"`
	ChildSchema  string `json:"child_schema"`
	ParentTable  string `json:"parent_table"`
	ParentSchema string `json:"parent_schema"`
}

// ConfigSnapshot holds PostgreSQL configuration and runtime state
// collected for the advisor features.
type ConfigSnapshot struct {
	PGSettings          []PGSetting       `json:"pg_settings"`
	TableReloptions     []TableReloption  `json:"table_reloptions"`
	ConnectionStates    []ConnectionState `json:"connection_states"`
	WALPosition         string            `json:"wal_position"`
	ExtensionsAvailable []string          `json:"extensions_available"`
	// OwnsDatabase: pg_sage's role has the database owner's privileges, so
	// ALTER DATABASE ... SET can run (advisors-07). Unread (legacy rows),
	// it is false: database-scope changes stay advisory.
	OwnsDatabase bool `json:"owns_database,omitempty"`
}

// PGSetting holds a single row from pg_settings.
type PGSetting struct {
	Name           string `json:"name"`
	Context        string `json:"context"`
	Setting        string `json:"setting"`
	Unit           string `json:"unit"`
	Source         string `json:"source"`
	PendingRestart bool   `json:"pending_restart"`
	SetPrivilege   bool   `json:"set_privilege,omitempty"`
}

// TableReloption holds per-table reloptions (autovacuum overrides).
type TableReloption struct {
	SchemaName string `json:"schemaname"`
	RelName    string `json:"relname"`
	Reloptions string `json:"reloptions"`
}

// ConnectionState holds connection count and duration by state.
type ConnectionState struct {
	State              string  `json:"state"`
	Count              int     `json:"count"`
	AvgDurationSeconds float64 `json:"avg_duration_seconds"`
}

// SlotInfo describes a replication slot.
type SlotInfo struct {
	SlotName      string `json:"slot_name"`
	SlotType      string `json:"slot_type"`
	Active        bool   `json:"active"`
	RetainedBytes int64  `json:"retained_bytes"`
	// The fields below exist only on newer servers and are NULL (nil,
	// false) where the column does not exist: wal_status and
	// safe_wal_size PG13+, inactive_since, synced and invalidation_reason
	// PG17+.
	WALStatus          *string    `json:"wal_status,omitempty"`
	SafeWALSize        *int64     `json:"safe_wal_size,omitempty"`
	Temporary          bool       `json:"temporary"`
	InactiveSince      *time.Time `json:"inactive_since,omitempty"`
	Synced             bool       `json:"synced"`
	InvalidationReason *string    `json:"invalidation_reason,omitempty"`
	// ConfirmedFlushLSN is a logical slot's confirmed_flush_lsn and
	// ConfirmedFlushLagBytes how far the server's WAL is past it; nil for
	// a physical slot (market-13).
	ConfirmedFlushLSN      *string `json:"confirmed_flush_lsn,omitempty"`
	ConfirmedFlushLagBytes *int64  `json:"confirmed_flush_lag_bytes,omitempty"`
	// InCurrentDatabase: a logical slot of the database this collector
	// reads (pg_replication_slots lists the whole cluster's slots).
	InCurrentDatabase bool `json:"in_current_database,omitempty"`
}

// PreparedTransaction describes a two-phase commit transaction from
// pg_prepared_xacts. These survive connection drops and server
// restarts, hold xmin and locks indefinitely, and are invisible to
// pg_stat_activity — a critical blind spot for vacuum and lock
// detection.
type PreparedTransaction struct {
	GID      string    `json:"gid"`
	Prepared time.Time `json:"prepared"`
	Owner    string    `json:"owner"`
	Database string    `json:"database"`
	XIDAge   int64     `json:"xid_age"`
}
