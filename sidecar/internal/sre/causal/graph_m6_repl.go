package causal

import "github.com/pg-sage/sidecar/internal/sre/probes"

// Replication lag and LWLock contention nodes (graph v3).
const (
	WALSendBacklog        NodeID = "wal_send_backlog"
	StandbyFlushBacklog   NodeID = "standby_flush_backlog"
	StandbyReplayBacklog  NodeID = "standby_replay_backlog"
	ReplayPaused          NodeID = "replay_paused"
	StandbyQueryDelay     NodeID = "standby_query_delay"
	ReplicationWriteSurge NodeID = "replication_write_surge"

	LockManagerContention   NodeID = "lock_manager_contention"
	SubtransSLRUContention  NodeID = "subtrans_slru_contention"
	MultiXactSLRUContention NodeID = "multixact_slru_contention"
	WALWriteContention      NodeID = "wal_write_contention"
	BufferContention        NodeID = "buffer_contention"
)

var replicationNodes = []Node{
	{ID: WALSendBacklog, Family: FamilyReplicationLag, Label: "WAL not leaving the primary",
		Mechanism: "The walsender cannot send WAL as fast as it is written: network " +
			"bandwidth, or for a logical stream the decoding of large transactions.",
		Predicted:   "most of the replica's lag is WAL not yet sent",
		Refutation:  string(probes.ReplicationLag),
		Confounders: "a write surge the stream catches up with",
		OperatorStep: "Check the network path to the replica and, for a logical stream, " +
			"large transactions being decoded (logical_decoding_work_mem spills)."},
	{ID: StandbyFlushBacklog, Family: FamilyReplicationLag,
		Label: "standby slow to write received WAL",
		Mechanism: "WAL is sent but the standby has not written and flushed it: it is " +
			"in flight on the network or the standby's disk is slow.",
		Predicted:   "most of the replica's lag is between sent and flushed",
		Refutation:  string(probes.ReplicationLag),
		Confounders: "network latency on a busy link",
		OperatorStep: "Check the standby's disk write latency and the network between " +
			"the servers; the primary is sending."},
	{ID: StandbyReplayBacklog, Family: FamilyReplicationLag,
		Label: "standby slow to replay",
		Mechanism: "The standby has the WAL but replays (or a logical subscriber applies) " +
			"it slowly: recovery conflicts, long standby queries, replay I/O or CPU.",
		Predicted:   "most of the replica's lag is between flushed and replayed",
		Refutation:  string(probes.ReplicationLag),
		Confounders: "replay paused by an operator",
		Amplifies:   []NodeID{ReplayPaused, StandbyQueryDelay},
		OperatorStep: "On the standby, check pg_is_wal_replay_paused(), long queries and " +
			"recovery conflicts; for a subscriber, its apply worker's errors and load."},
	{ID: ReplayPaused, Family: FamilyReplicationLag, Label: "replay paused",
		Mechanism:   "WAL replay on the standby is paused (pg_wal_replay_pause).",
		Predicted:   "the standby reports replay paused while WAL arrives",
		Refutation:  string(probes.StandbyReplayState),
		Confounders: "recovery_target settings pausing at a target",
		OperatorStep: "Confirm who paused replay and why, then resume it with " +
			"pg_wal_replay_resume() through your normal process."},
	{ID: StandbyQueryDelay, Family: FamilyReplicationLag,
		Label: "standby queries hold back replay",
		Mechanism: "Queries on the standby conflict with replay, which waits up to " +
			"max_standby_streaming_delay (forever at -1) before cancelling them.",
		Predicted: "recovery conflicts between samples, or a long standby query with a " +
			"long or unlimited max_standby_streaming_delay",
		Refutation:  string(probes.StandbyReplayState),
		Confounders: "replay I/O saturation",
		OperatorStep: "Bound max_standby_streaming_delay, move long reports to a replica " +
			"that may lag, or consider hot_standby_feedback (it holds back vacuum on " +
			"the primary)."},
	{ID: ReplicationWriteSurge, Family: FamilyReplicationLag, Label: "primary write surge",
		Mechanism: "The primary writes WAL much faster than usual; a replica falls " +
			"behind proportionally.",
		Predicted:   "WAL rate a step above its average while lag grows",
		Refutation:  string(probes.WALCheckpoint),
		Confounders: "an expected bulk load",
		Amplifies:   []NodeID{WALSendBacklog, StandbyFlushBacklog, StandbyReplayBacklog},
		OperatorStep: "Identify the bulk write; the lag recovers once it ends if the " +
			"replica keeps up otherwise. Never promote a lagging replica."},
}

var lwlockNodes = []Node{
	{ID: LockManagerContention, Family: FamilyLWLock, Label: "lock manager contention",
		Mechanism: "Queries take more relation locks than the per-backend fast-path " +
			"slots hold (many partitions or indexes), so backends queue on the shared " +
			"lock manager.",
		Predicted:  "sustained LWLock LockManager waits",
		Refutation: string(probes.LWLockWaits), Confounders: "heavy DDL",
		OperatorStep: "Attribute the waits to the query (queryid in the evidence): " +
			"enable partition pruning, drop unused indexes on hot tables; on " +
			"PostgreSQL 18 a larger max_locks_per_transaction adds fast-path slots."},
	{ID: SubtransSLRUContention, Family: FamilyLWLock,
		Label: "subtransaction SLRU contention",
		Mechanism: "Transactions with more than 64 subtransactions (SAVEPOINT-heavy " +
			"code) overflow, and every snapshot must read pg_subtrans.",
		Predicted:  "sustained LWLock SubtransSLRU/SubtransBuffer waits",
		Refutation: string(probes.LWLockWaits), Confounders: "a long transaction alone",
		OperatorStep: "Find the code creating many savepoints (ORM nested transactions " +
			"or exception blocks in loops) and remove them; PostgreSQL 17 can enlarge " +
			"subtransaction_buffers."},
	{ID: MultiXactSLRUContention, Family: FamilyLWLock, Label: "multixact SLRU contention",
		Mechanism: "Many concurrent row share locks (foreign-key checks, SELECT FOR " +
			"SHARE) create multixacts that backends queue to read.",
		Predicted:  "sustained LWLock MultiXactOffset/MultiXactMember waits",
		Refutation: string(probes.LWLockWaits), Confounders: "foreign-key checks on a hot parent",
		OperatorStep: "Find the lock-sharing workload (FK checks on a hot parent row, " +
			"SELECT FOR SHARE) and reduce it; vacuum tables with a high mxid_age."},
	{ID: WALWriteContention, Family: FamilyLWLock, Label: "WAL write contention",
		Mechanism: "Many small transactions commit at once and queue to write and " +
			"flush WAL.",
		Predicted:  "sustained LWLock WALWrite/WALInsert waits",
		Refutation: string(probes.LWLockWaits), Confounders: "slow WAL storage",
		OperatorStep: "Batch commits in the application (fewer, larger transactions) and " +
			"check WAL fsync latency; never turn fsync off."},
	{ID: BufferContention, Family: FamilyLWLock, Label: "buffer contention",
		Mechanism: "Backends queue on the same hot buffer pages or on buffer mapping " +
			"when shared_buffers churns.",
		Predicted:  "sustained LWLock BufferContent/BufferMapping waits",
		Refutation: string(probes.LWLockWaits), Confounders: "a sequential scan storm",
		OperatorStep: "Attribute the waits to the query: spread hot inserts or updates " +
			"(right-most index pages), and check shared_buffers against the working set."},
}
