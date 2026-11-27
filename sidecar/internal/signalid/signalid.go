// Package signalid names every signal id written to
// sage.incidents.signal_ids: the RCA engine's snapshot signals, the log
// signals logwatch classifies and RCA turns into incidents, and the SRE
// reactive detector's episode signals. It is a leaf package so the
// writers (rca, logwatch, sre) and the readers (cases, sre triggers and
// probes) share one vocabulary without an import cycle: rca imports cases
// through sre, so cases cannot import rca.
package signalid

// Family is where a signal is produced.
type Family string

const (
	// FamilySnapshot signals come from RCA's per-cycle snapshot detectors.
	FamilySnapshot Family = "snapshot"
	// FamilyLog signals come from logwatch's PostgreSQL log classifier.
	FamilyLog Family = "log"
	// FamilyDetector signals come from the SRE reactive detector.
	FamilyDetector Family = "detector"
)

// Kind is the operational area a signal is about.
type Kind string

const (
	KindConnections  Kind = "connections"
	KindLocks        Kind = "locks"
	KindMemory       Kind = "memory"
	KindReplication  Kind = "replication"
	KindWAL          Kind = "wal"
	KindVacuum       Kind = "vacuum"
	KindQuery        Kind = "query"
	KindStorage      Kind = "storage"
	KindAvailability Kind = "availability"
	KindIntegrity    Kind = "integrity"
	KindCheckpoint   Kind = "checkpoint"
	KindTempFiles    Kind = "temp_files"
	KindSecurity     Kind = "security"
)

// Snapshot signals (rca/signals.go).
const (
	ConnectionsHigh          = "connections_high"
	IdleInTxElevated         = "idle_in_tx_elevated"
	CacheHitRatioDrop        = "cache_hit_ratio_drop"
	ReplicationLagIncreasing = "replication_lag_increasing"
	VacuumBlocked            = "vacuum_blocked"
	LockContention           = "lock_contention"
	WALGrowthSpike           = "wal_growth_spike"
	OrphanedPreparedTx       = "orphaned_prepared_tx"
)

// Log signals (logwatch/classifier.go, rca/log_trees.go).
const (
	LogDeadlockDetected        = "log_deadlock_detected"
	LogConnectionRefused       = "log_connection_refused"
	LogOutOfMemory             = "log_out_of_memory"
	LogDiskFull                = "log_disk_full"
	LogPanicServerCrash        = "log_panic_server_crash"
	LogDataCorruption          = "log_data_corruption"
	LogTxidWraparoundWarning   = "log_txid_wraparound_warning"
	LogArchiveFailed           = "log_archive_failed"
	LogTempFileCreated         = "log_temp_file_created"
	LogCheckpointTooFrequent   = "log_checkpoint_too_frequent"
	LogLockTimeout             = "log_lock_timeout"
	LogStatementTimeout        = "log_statement_timeout"
	LogReplicationConflict     = "log_replication_conflict"
	LogWALSegmentRemoved       = "log_wal_segment_removed"
	LogAutovacuumCancel        = "log_autovacuum_cancel"
	LogReplicationSlotInactive = "log_replication_slot_inactive"
	LogAuthenticationFailure   = "log_authentication_failure"
	LogSlowQuery               = "log_slow_query"
	// rca-02: internal errors apart from corruption, and the wraparound
	// outages and the MultiXact warning.
	LogInternalError              = "log_internal_error"
	LogWraparoundOutage           = "log_wraparound_outage"
	LogMultiXactWraparoundWarning = "log_multixact_wraparound_warning"
	LogMultiXactWraparoundOutage  = "log_multixact_wraparound_outage"
	// rca-07: a statement cancelled by pg_cancel_backend or a client
	// cancel request. Info only: it never becomes an incident.
	LogStatementCancelled = "log_statement_cancelled"
)

// Detector signals (sre/detect_incident.go).
const (
	SRECheckpointStorm   = "sre_checkpoint_storm"
	SRETempFileExplosion = "sre_temp_file_explosion"
	SRELWLockContention  = "sre_lwlock_contention"
)

// Info describes one signal id.
type Info struct {
	ID     string
	Family Family
	Kind   Kind
}

var all = []Info{
	{ConnectionsHigh, FamilySnapshot, KindConnections},
	{IdleInTxElevated, FamilySnapshot, KindLocks},
	{CacheHitRatioDrop, FamilySnapshot, KindMemory},
	{ReplicationLagIncreasing, FamilySnapshot, KindReplication},
	{VacuumBlocked, FamilySnapshot, KindVacuum},
	{LockContention, FamilySnapshot, KindLocks},
	{WALGrowthSpike, FamilySnapshot, KindWAL},
	{OrphanedPreparedTx, FamilySnapshot, KindVacuum},

	{LogDeadlockDetected, FamilyLog, KindLocks},
	{LogConnectionRefused, FamilyLog, KindConnections},
	{LogOutOfMemory, FamilyLog, KindMemory},
	{LogDiskFull, FamilyLog, KindStorage},
	{LogPanicServerCrash, FamilyLog, KindAvailability},
	{LogDataCorruption, FamilyLog, KindIntegrity},
	{LogTxidWraparoundWarning, FamilyLog, KindVacuum},
	{LogArchiveFailed, FamilyLog, KindWAL},
	{LogTempFileCreated, FamilyLog, KindTempFiles},
	{LogCheckpointTooFrequent, FamilyLog, KindCheckpoint},
	{LogLockTimeout, FamilyLog, KindLocks},
	{LogStatementTimeout, FamilyLog, KindQuery},
	{LogReplicationConflict, FamilyLog, KindReplication},
	{LogWALSegmentRemoved, FamilyLog, KindWAL},
	{LogAutovacuumCancel, FamilyLog, KindVacuum},
	{LogReplicationSlotInactive, FamilyLog, KindWAL},
	{LogAuthenticationFailure, FamilyLog, KindSecurity},
	{LogSlowQuery, FamilyLog, KindQuery},
	{LogInternalError, FamilyLog, KindIntegrity},
	{LogWraparoundOutage, FamilyLog, KindVacuum},
	{LogMultiXactWraparoundWarning, FamilyLog, KindVacuum},
	{LogMultiXactWraparoundOutage, FamilyLog, KindVacuum},

	{SRECheckpointStorm, FamilyDetector, KindCheckpoint},
	{SRETempFileExplosion, FamilyDetector, KindTempFiles},
	{SRELWLockContention, FamilyDetector, KindLocks},
}

// classifierOnly are log ids the classifier emits that no incident carries.
var classifierOnly = []string{LogStatementCancelled}

// ClassifierOnly returns the log signal ids logwatch classifies that never
// become an incident (so they are not in All). The slice is a copy.
func ClassifierOnly() []string {
	return append([]string(nil), classifierOnly...)
}

// All returns every signal id an incident can carry, in a fixed order
// (snapshot, then log, then detector). The slice is a copy.
func All() []Info {
	return append([]Info(nil), all...)
}
