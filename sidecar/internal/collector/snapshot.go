package collector

import "time"

// Snapshot holds all stats collected in a single cycle.
type Snapshot struct {
	CollectedAt time.Time
	Queries     []QueryStats
	Tables      []TableStats
	Indexes     []IndexStats
	ForeignKeys []ForeignKey
	System      SystemStats
	Locks       []LockInfo
	Sequences   []SequenceStats
	Replication *ReplicationStats
	IO          []IOStats        `json:"io,omitempty"`
	Partitions  []PartitionInfo  `json:"partitions,omitempty"`
	ConfigData  *ConfigSnapshot  `json:"config_data,omitempty"`
	StatsReset  bool             `json:"stats_reset,omitempty"`
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
}

// ForeignKey describes a foreign key constraint.
type ForeignKey struct {
	TableName       string `json:"table_name"`
	ReferencedTable string `json:"referenced_table"`
	FKColumn        string `json:"fk_column"`
	ConstraintName  string `json:"constraint_name"`
}

// SystemStats holds database-wide health metrics.
type SystemStats struct {
	ActiveBackends    int     `json:"active_backends"`
	IdleInTransaction int     `json:"idle_in_transaction"`
	TotalBackends     int     `json:"total_backends"`
	MaxConnections    int     `json:"max_connections"`
	CacheHitRatio     float64 `json:"cache_hit_ratio"`
	Deadlocks         int64   `json:"deadlocks"`
	BlkReadTime       float64 `json:"blk_read_time"`
	BlkWriteTime      float64 `json:"blk_write_time"`
	TotalCheckpoints  int64   `json:"total_checkpoints"`
	IsReplica         bool    `json:"is_replica"`
	DBSizeBytes       int64   `json:"db_size_bytes"`
	StatStatementsMax int     `json:"stat_statements_max"`
}

// LockInfo describes a single lock from pg_locks + pg_stat_activity.
type LockInfo struct {
	LockType      string     `json:"locktype"`
	Mode          string     `json:"mode"`
	Granted       bool       `json:"granted"`
	RelName       *string    `json:"relname"`
	Query         *string    `json:"query"`
	State         *string    `json:"state"`
	WaitEventType *string    `json:"wait_event_type"`
	WaitEvent     *string    `json:"wait_event"`
	PID           int        `json:"pid"`
	BackendStart  *time.Time `json:"backend_start"`
	QueryStart    *time.Time `json:"query_start"`
}

// SequenceStats tracks sequence usage and exhaustion risk.
type SequenceStats struct {
	SchemaName   string  `json:"schemaname"`
	SequenceName string  `json:"sequencename"`
	DataType     string  `json:"data_type"`
	LastValue    int64   `json:"last_value"`
	MaxValue     int64   `json:"max_value"`
	IncrementBy  int64   `json:"increment_by"`
	PctUsed      float64 `json:"pct_used"`
}

// ReplicationStats aggregates replica and slot info.
type ReplicationStats struct {
	Replicas []ReplicaInfo `json:"replicas"`
	Slots    []SlotInfo    `json:"slots"`
}

// ReplicaInfo describes a single streaming replica.
type ReplicaInfo struct {
	ClientAddr *string `json:"client_addr"`
	State      string  `json:"state"`
	SentLSN    string  `json:"sent_lsn"`
	WriteLSN   string  `json:"write_lsn"`
	FlushLSN   string  `json:"flush_lsn"`
	ReplayLSN  string  `json:"replay_lsn"`
	WriteLag   *string `json:"write_lag"`
	FlushLag   *string `json:"flush_lag"`
	ReplayLag  *string `json:"replay_lag"`
	SyncState  string  `json:"sync_state"`
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
	ConnectionChurn     int               `json:"connection_churn"`
}

// PGSetting holds a single row from pg_settings.
type PGSetting struct {
	Name           string `json:"name"`
	Setting        string `json:"setting"`
	Unit           string `json:"unit"`
	Source         string `json:"source"`
	PendingRestart bool   `json:"pending_restart"`
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
}
