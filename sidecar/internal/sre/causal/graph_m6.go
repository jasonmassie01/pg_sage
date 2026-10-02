package causal

import "github.com/pg-sage/sidecar/internal/sre/probes"

// Graph v3 (Sage SRE M6) families: checkpoint storms, temp-file
// explosions, replication lag and LWLock contention (AI-SRE-SPEC §4 R2;
// prior-art taxonomy §4 rows 6, 8, 12 and 17).
const (
	FamilyCheckpoint     Family = "checkpoint_storm"
	FamilyTempFiles      Family = "temp_file_explosion"
	FamilyReplicationLag Family = "replication_lag"
	FamilyLWLock         Family = "lwlock_contention"
)

// Checkpoint storm and temp-file explosion nodes.
const (
	MaxWALSizeUndersized   NodeID = "max_wal_size_undersized"
	ForcedCheckpoints      NodeID = "forced_checkpoints"
	ShortCheckpointTimeout NodeID = "short_checkpoint_timeout"
	CheckpointWriteBurst   NodeID = "checkpoint_write_burst"
	RunawaySpillQuery      NodeID = "runaway_spill_query"
	RepeatedSpillStatement NodeID = "repeated_spill_statement"
	WorkMemUndersized      NodeID = "work_mem_undersized"
)

var m6Nodes = append(append(checkpointNodes, tempNodes...),
	append(replicationNodes, lwlockNodes...)...)

var checkpointNodes = []Node{
	{ID: MaxWALSizeUndersized, Family: FamilyCheckpoint, Label: "max_wal_size too small",
		Mechanism: "WAL volume reaches the checkpoint distance (max_wal_size) before " +
			"checkpoint_timeout, so checkpoints are requested back to back.",
		Predicted: "requested checkpoints between samples with at least a quarter of " +
			"max_wal_size of WAL each; the WAL rate fills max_wal_size sooner than " +
			"checkpoint_timeout",
		Refutation:  string(probes.CheckpointActivity),
		Confounders: "a one-off bulk load that max_wal_size absorbs once",
		OperatorStep: "Raise max_wal_size (a reload, reversible) so checkpoints are spaced " +
			"by checkpoint_timeout, and keep checkpoint_completion_target near 0.9. " +
			"Check disk headroom for the extra WAL first."},
	{ID: ForcedCheckpoints, Family: FamilyCheckpoint, Label: "forced checkpoints",
		Mechanism: "Something issues CHECKPOINT (a backup tool with a fast checkpoint, " +
			"a script or an application) far more often than WAL volume would.",
		Predicted: "requested checkpoints with little WAL between them, under a quarter " +
			"of max_wal_size each",
		Refutation:  string(probes.CheckpointActivity),
		Confounders: "a small max_wal_size with light writes",
		OperatorStep: "Find the client issuing CHECKPOINT (the server log with " +
			"log_checkpoints shows each request) and stop or space it out."},
	{ID: ShortCheckpointTimeout, Family: FamilyCheckpoint,
		Label:      "checkpoint_timeout too short",
		Mechanism:  "checkpoint_timeout is set below the default, so timed checkpoints run often.",
		Predicted:  "timed checkpoints between samples with checkpoint_timeout under 300 s",
		Refutation: string(probes.CheckpointActivity), Confounders: "a deliberate RPO setting",
		OperatorStep: "Return checkpoint_timeout to 5 minutes or more (a reload) unless " +
			"recovery time objectives require it."},
	{ID: CheckpointWriteBurst, Family: FamilyCheckpoint, Label: "write burst",
		Mechanism: "A burst of writes produces WAL far faster than usual and drives the " +
			"requested checkpoints.",
		Predicted:   "WAL rate a step above its average while checkpoints are requested",
		Refutation:  string(probes.CheckpointActivity),
		Confounders: "an expected bulk load",
		Amplifies:   []NodeID{MaxWALSizeUndersized, ForcedCheckpoints},
		OperatorStep: "Identify the bulk write (load, migration or batch job); spreading " +
			"it out or batching it lowers the checkpoint rate."},
}

var tempNodes = []Node{
	{ID: RunawaySpillQuery, Family: FamilyTempFiles, Label: "runaway spilling query",
		Mechanism: "One running query spills a large sort or hash to temp files.",
		Predicted: "one backend of this database holds at least 64 MiB of live temp " +
			"files and most of the live total",
		Refutation:  string(probes.TempFileHolders),
		Confounders: "a legitimate large report or index build",
		OperatorStep: "Review the query of that backend (pid and backend_start in the " +
			"evidence) and cancel it through your normal process if it is runaway; set " +
			"temp_file_limit so one query cannot fill the disk."},
	{ID: RepeatedSpillStatement, Family: FamilyTempFiles,
		Label:     "statement spilling on every call",
		Mechanism: "One frequently run statement spills to temp files each time it runs.",
		Predicted: "one queryid writes at least 32 MiB of temp blocks over several calls " +
			"between samples, most of the total",
		Refutation:  string(probes.TempSpillStatements),
		Confounders: "a batch job that runs the statement once per item by design",
		OperatorStep: "Tune that statement (an index that avoids the sort, or a smaller " +
			"result); raise work_mem only for its role or session, never globally."},
	{ID: WorkMemUndersized, Family: FamilyTempFiles,
		Label: "work_mem small for the workload",
		Mechanism: "Many statements each spill a little: their sorts and hashes " +
			"slightly exceed work_mem.",
		Predicted: "three or more statements spill between samples, none dominates, and " +
			"temp files average within 16 times work_mem",
		Refutation:  string(probes.TempSpillStatements),
		Confounders: "one dominant statement, or spills far larger than work_mem",
		OperatorStep: "Raise work_mem for the roles or sessions that run these statements " +
			"(ALTER ROLE ... SET), sized against connections times work_mem; a global " +
			"increase multiplies memory per connection."},
}
