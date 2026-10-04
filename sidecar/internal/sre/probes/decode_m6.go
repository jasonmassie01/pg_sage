package probes

import (
	"fmt"
	"math"
	"time"
)

// M6 decoders. Unknown numbers are NaN, never zero; an unavailable
// result is an *UnavailableError.

// CheckpointStat is one checkpoint_activity sample: cumulative counters
// since their statistics resets, plus the settings that bound them.
type CheckpointStat struct {
	Timed, Requested     float64
	WriteMS, SyncMS      float64
	BuffersWritten       float64
	BackendWrites        float64
	BackendFsyncs        float64
	StatsReset           time.Time
	WALBytes, WALRecords float64
	WALFPI               float64
	WALStatsReset        time.Time
	MaxWALSize           float64 // bytes
	TimeoutS             float64
	CompletionTarget     float64
	ServerStartedAt      time.Time
}

// TempStat is one temp_file_activity sample of this database.
type TempStat struct {
	Files, Bytes       float64
	StatsReset         time.Time
	WorkMemBytes       float64
	HashMemMultiplier  float64
	TempFileLimitKB    float64
	TempTablespacesSet bool
	ServerStartedAt    time.Time
}

// TempHolder is one backend holding live temp files.
type TempHolder struct {
	PID               int64
	BackendStart      time.Time
	InCurrentDatabase bool
	State             string
	QueryID           int64
	QueryIDKnown      bool
	QueryAgeS         float64
	Files             int64
	Bytes             float64
	TotalBytes        float64
}

// SpillStatement is one statement's cumulative temp block writes.
type SpillStatement struct {
	QueryID         int64
	Calls           int64
	TempBlksWritten float64
	TempBlksRead    float64
	TotalExecMS     float64
	BlockSize       float64
	StatsReset      time.Time
	// OwnRole marks a statement run by the probing (pg_sage's) role.
	OwnRole bool
}

// TempBytesWritten is the temp blocks written in bytes (NaN if unknown).
func (s SpillStatement) TempBytesWritten() float64 {
	return s.TempBlksWritten * s.BlockSize
}

// ReplicationStage is one replica's lag split by where it sits.
type ReplicationStage struct {
	PID                              int64
	Application, ClientAddr, State   string
	SyncState, Kind                  string
	WriteLagS, FlushLagS, ReplayLagS float64
	SendBacklog, FlushBacklog        float64
	ReplayBacklog, ReplayLagBytes    float64
}

// StandbyState is the monitored server's own replay state.
type StandbyState struct {
	InRecovery         bool
	ReplayPaused       bool
	ReceiveReplayBytes float64
	LastReplayAgeS     float64
	Conflicts          float64
	ReceiverStatus     string
	MaxStandbyDelayMS  float64
	LongestQueryS      float64
	ServerStartedAt    time.Time
}

// WaitGroup is the active backends of one sample waiting on one event
// (Type "CPU" when they wait on nothing) for one query.
type WaitGroup struct {
	Type, Event       string
	QueryID           int64
	QueryIDKnown      bool
	InCurrentDatabase bool
	Backends          int64
	ActiveBackends    int64
}

// CheckpointStats decodes a checkpoint_activity result.
func CheckpointStats(res Result) (CheckpointStat, error) {
	r, err := singleRow(res, CheckpointActivity)
	if err != nil {
		return CheckpointStat{}, err
	}
	return CheckpointStat{Timed: floatField(r, "timed_checkpoints"),
		Requested:      floatField(r, "requested_checkpoints"),
		WriteMS:        floatField(r, "checkpoint_write_ms"),
		SyncMS:         floatField(r, "checkpoint_sync_ms"),
		BuffersWritten: floatField(r, "buffers_written"),
		BackendWrites:  floatField(r, "backend_writes"),
		BackendFsyncs:  floatField(r, "backend_fsyncs"),
		StatsReset:     timeField(r, "checkpointer_stats_reset"),
		WALBytes:       floatField(r, "wal_bytes"), WALRecords: floatField(r, "wal_records"),
		WALFPI: floatField(r, "wal_fpi"), WALStatsReset: timeField(r, "wal_stats_reset"),
		MaxWALSize:       floatField(r, "max_wal_size_bytes"),
		TimeoutS:         floatField(r, "checkpoint_timeout_s"),
		CompletionTarget: floatField(r, "checkpoint_completion_target"),
		ServerStartedAt:  timeField(r, "server_started_at")}, nil
}

// TempStats decodes a temp_file_activity result.
func TempStats(res Result) (TempStat, error) {
	r, err := singleRow(res, TempFileActivity)
	if err != nil {
		return TempStat{}, err
	}
	return TempStat{Files: floatField(r, "temp_files"), Bytes: floatField(r, "temp_bytes"),
		StatsReset: timeField(r, "stats_reset"), WorkMemBytes: floatField(r, "work_mem_bytes"),
		HashMemMultiplier:  floatField(r, "hash_mem_multiplier"),
		TempFileLimitKB:    floatField(r, "temp_file_limit_kb"),
		TempTablespacesSet: boolField(r, "temp_tablespaces_set"),
		ServerStartedAt:    timeField(r, "server_started_at")}, nil
}

// TempHolders decodes a temp_file_holders result; an empty result is an
// observed absence of live temp files (nil, no error).
func TempHolders(res Result) ([]TempHolder, error) {
	rows, err := rowsFor(res, TempFileHolders)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]TempHolder, 0, len(rows))
	for i, r := range rows {
		pid, err := intField(r, "pid")
		if err != nil {
			return nil, fmt.Errorf("temp_file_holders row %d: %w", i+1, err)
		}
		qid, qerr := intField(r, "query_id")
		files, _ := intField(r, "files")
		out = append(out, TempHolder{PID: pid, BackendStart: timeField(r, "backend_start"),
			InCurrentDatabase: boolField(r, "in_current_database"),
			State:             strField(r, "state"), QueryID: qid, QueryIDKnown: qerr == nil,
			QueryAgeS: floatField(r, "query_age_s"), Files: files,
			Bytes: floatField(r, "bytes"), TotalBytes: floatField(r, "total_bytes")})
	}
	return out, nil
}

// SpillStatements decodes a temp_spill_statements result.
func SpillStatements(res Result) ([]SpillStatement, error) {
	rows, err := rowsFor(res, TempSpillStatements)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]SpillStatement, 0, len(rows))
	for i, r := range rows {
		qid, err := intField(r, "queryid")
		if err != nil {
			return nil, fmt.Errorf("temp_spill_statements row %d: %w", i+1, err)
		}
		calls, _ := intField(r, "calls")
		out = append(out, SpillStatement{QueryID: qid, Calls: calls,
			TempBlksWritten: floatField(r, "temp_blks_written"),
			TempBlksRead:    floatField(r, "temp_blks_read"),
			TotalExecMS:     floatField(r, "total_exec_ms"),
			BlockSize:       floatField(r, "block_size"),
			StatsReset:      timeField(r, "stats_reset"),
			OwnRole:         r["own_role"] == true})
	}
	return out, nil
}

// ReplicationStages decodes a replication_lag (v2) result; no rows is an
// observed absence of replicas.
func ReplicationStages(res Result) ([]ReplicationStage, error) {
	rows, err := rowsFor(res, ReplicationLag)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]ReplicationStage, 0, len(rows))
	for _, r := range rows {
		pid, _ := intField(r, "pid")
		out = append(out, ReplicationStage{PID: pid,
			Application: strField(r, "application_name"),
			ClientAddr:  strField(r, "client_addr"), State: strField(r, "state"),
			SyncState: strField(r, "sync_state"), Kind: strField(r, "kind"),
			WriteLagS: floatField(r, "write_lag_s"), FlushLagS: floatField(r, "flush_lag_s"),
			ReplayLagS:     floatField(r, "replay_lag_s"),
			SendBacklog:    floatField(r, "send_backlog_bytes"),
			FlushBacklog:   floatField(r, "flush_backlog_bytes"),
			ReplayBacklog:  floatField(r, "replay_backlog_bytes"),
			ReplayLagBytes: floatField(r, "replay_lag_bytes")})
	}
	return out, nil
}

// StandbyStates decodes a standby_replay_state result.
func StandbyStates(res Result) (StandbyState, error) {
	r, err := singleRow(res, StandbyReplayState)
	if err != nil {
		return StandbyState{}, err
	}
	return StandbyState{InRecovery: boolField(r, "in_recovery"),
		ReplayPaused:       boolField(r, "replay_paused"),
		ReceiveReplayBytes: floatField(r, "receive_replay_bytes"),
		LastReplayAgeS:     floatField(r, "last_replay_age_s"),
		Conflicts:          floatField(r, "conflicts"),
		ReceiverStatus:     strField(r, "receiver_status"),
		MaxStandbyDelayMS:  floatField(r, "max_standby_streaming_delay_ms"),
		LongestQueryS:      floatField(r, "longest_query_s"),
		ServerStartedAt:    timeField(r, "server_started_at")}, nil
}

// WaitGroups decodes an lwlock_waits sample; no rows means no backend
// was active.
func WaitGroups(res Result) ([]WaitGroup, error) {
	rows, err := rowsFor(res, LWLockWaits)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]WaitGroup, 0, len(rows))
	for i, r := range rows {
		n, err := intField(r, "backends")
		if err != nil {
			return nil, fmt.Errorf("lwlock_waits row %d: %w", i+1, err)
		}
		qid, qerr := intField(r, "query_id")
		active, _ := intField(r, "active_backends")
		out = append(out, WaitGroup{Type: strField(r, "wait_event_type"),
			Event: strField(r, "wait_event"), QueryID: qid, QueryIDKnown: qerr == nil,
			InCurrentDatabase: boolField(r, "in_current_database"), Backends: n,
			ActiveBackends: active})
	}
	return out, nil
}

// Delta is b - a when both are known and b did not fall; ok is false for
// an unknown or reset counter.
func Delta(a, b float64) (float64, bool) {
	if math.IsNaN(a) || math.IsNaN(b) || b < a {
		return 0, false
	}
	return b - a, true
}
