package probes

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

// M6 reactive-family decoders: checkpoint activity, temp files (database
// counters, live holders, spilling statements), replication stages, the
// standby's replay state and wait-event samples. Unknown numbers decode
// as NaN, never zero; an unavailable result is an *UnavailableError.

var m6At = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func m6Result(id ID, rows ...Row) Result {
	st := StatusOK
	if len(rows) == 0 {
		st = StatusEmpty
	}
	return Result{ProbeID: id, Version: "v1", Status: st, ObservedAt: m6At, Rows: rows}
}

func wantUnavailable(t *testing.T, err error, st Status) {
	t.Helper()
	var u *UnavailableError
	if !errors.As(err, &u) || u.Status != st {
		t.Fatalf("err = %v, want *UnavailableError with status %s", err, st)
	}
}

func TestCheckpointStats_DecodesCountersAndSettings(t *testing.T) {
	reset := m6At.Add(-time.Hour)
	res := m6Result(CheckpointActivity, Row{"timed_checkpoints": int64(4),
		"requested_checkpoints": json.Number("19"), "checkpoint_write_ms": 120.5,
		"checkpoint_sync_ms": int64(7), "buffers_written": int64(900),
		"backend_writes": nil, "backend_fsyncs": int64(3), "checkpointer_stats_reset": reset,
		"wal_bytes": int64(1 << 30), "wal_records": int64(5000), "wal_fpi": int64(800),
		"wal_stats_reset":    reset.Format(time.RFC3339Nano),
		"max_wal_size_bytes": int64(32 << 20), "checkpoint_timeout_s": int64(300),
		"checkpoint_completion_target": 0.9, "server_started_at": reset})
	c, err := CheckpointStats(res)
	if err != nil {
		t.Fatalf("CheckpointStats: %v", err)
	}
	if c.Timed != 4 || c.Requested != 19 || c.WriteMS != 120.5 || c.SyncMS != 7 ||
		c.BuffersWritten != 900 || c.BackendFsyncs != 3 || c.WALBytes != 1<<30 ||
		c.WALRecords != 5000 || c.WALFPI != 800 || c.MaxWALSize != 32<<20 ||
		c.TimeoutS != 300 || c.CompletionTarget != 0.9 {
		t.Fatalf("decoded = %+v", c)
	}
	if !math.IsNaN(c.BackendWrites) {
		t.Fatalf("NULL backend_writes decoded as %v, want NaN (unknown)", c.BackendWrites)
	}
	if !c.StatsReset.Equal(reset) || !c.WALStatsReset.Equal(reset) ||
		!c.ServerStartedAt.Equal(reset) {
		t.Fatalf("timestamps = %v %v %v", c.StatsReset, c.WALStatsReset, c.ServerStartedAt)
	}
}

func TestCheckpointStats_UnavailableAndMalformed(t *testing.T) {
	_, err := CheckpointStats(Result{ProbeID: CheckpointActivity,
		Status: StatusNoPrivilege, Reason: "insufficient_privilege"})
	wantUnavailable(t, err, StatusNoPrivilege)
	_, err = CheckpointStats(m6Result(CheckpointActivity))
	wantUnavailable(t, err, StatusEmpty)
	if _, err := CheckpointStats(m6Result(WALCheckpoint, Row{})); err == nil {
		t.Fatal("a wal_checkpoint result decoded as checkpoint_activity")
	}
	two := m6Result(CheckpointActivity, Row{}, Row{})
	if _, err := CheckpointStats(two); err == nil {
		t.Fatal("two rows decoded as one checkpoint sample")
	}
}

func TestTempStats_DecodesDatabaseCounters(t *testing.T) {
	res := m6Result(TempFileActivity, Row{"temp_files": int64(12),
		"temp_bytes": int64(5 << 30), "stats_reset": nil, "work_mem_bytes": int64(4 << 20),
		"hash_mem_multiplier": 2.0, "temp_file_limit_kb": int64(-1),
		"temp_tablespaces_set": true, "server_started_at": m6At})
	s, err := TempStats(res)
	if err != nil {
		t.Fatalf("TempStats: %v", err)
	}
	if s.Files != 12 || s.Bytes != 5<<30 || s.WorkMemBytes != 4<<20 ||
		s.HashMemMultiplier != 2 || s.TempFileLimitKB != -1 || !s.TempTablespacesSet ||
		!s.StatsReset.IsZero() || !s.ServerStartedAt.Equal(m6At) {
		t.Fatalf("decoded = %+v", s)
	}
	_, err = TempStats(Result{ProbeID: TempFileActivity, Status: StatusError,
		Reason: "statement_timeout"})
	wantUnavailable(t, err, StatusError)
}

func TestTempHolders_DecodesLiveHoldersAndEmpty(t *testing.T) {
	start := m6At.Add(-time.Minute)
	res := m6Result(TempFileHolders, Row{"pid": int64(77), "backend_start": start,
		"in_current_database": true, "state": "active", "query_id": int64(-42),
		"query_age_s": 31.5, "files": int64(3), "bytes": int64(200 << 20),
		"total_bytes": int64(210 << 20)}, Row{"pid": int64(78), "backend_start": nil,
		"in_current_database": false, "state": "gone", "query_id": nil,
		"query_age_s": nil, "files": int64(1), "bytes": int64(10 << 20),
		"total_bytes": int64(210 << 20)})
	hs, err := TempHolders(res)
	if err != nil || len(hs) != 2 {
		t.Fatalf("holders = %+v (%v)", hs, err)
	}
	a, b := hs[0], hs[1]
	if a.PID != 77 || !a.BackendStart.Equal(start) || !a.InCurrentDatabase ||
		a.State != "active" || a.QueryID != -42 || !a.QueryIDKnown || a.QueryAgeS != 31.5 ||
		a.Files != 3 || a.Bytes != 200<<20 || a.TotalBytes != 210<<20 {
		t.Fatalf("first holder = %+v", a)
	}
	if b.QueryIDKnown || b.InCurrentDatabase || !math.IsNaN(b.QueryAgeS) {
		t.Fatalf("second holder = %+v, want unknown query id and age", b)
	}
	empty, err := TempHolders(m6Result(TempFileHolders))
	if err != nil || empty != nil {
		t.Fatalf("empty = %+v (%v), want an observed absence", empty, err)
	}
	if _, err := TempHolders(m6Result(TempFileHolders, Row{"pid": "x"})); err == nil {
		t.Fatal("a non-integer pid decoded")
	}
}

func TestSpillStatements_DecodesAndComputesBytes(t *testing.T) {
	res := m6Result(TempSpillStatements, Row{"queryid": json.Number("-9007199254740993"),
		"calls": int64(20), "temp_blks_written": int64(1000), "temp_blks_read": int64(900),
		"total_exec_ms": 15.5, "block_size": int64(8192), "stats_reset": m6At})
	ss, err := SpillStatements(res)
	if err != nil || len(ss) != 1 {
		t.Fatalf("statements = %+v (%v)", ss, err)
	}
	s := ss[0]
	if s.QueryID != -9007199254740993 || s.Calls != 20 || s.TempBlksWritten != 1000 ||
		s.TempBlksRead != 900 || s.TotalExecMS != 15.5 || s.BlockSize != 8192 ||
		!s.StatsReset.Equal(m6At) {
		t.Fatalf("decoded = %+v", s)
	}
	if got := s.TempBytesWritten(); got != 1000*8192 {
		t.Fatalf("TempBytesWritten = %v", got)
	}
	unknown := SpillStatement{TempBlksWritten: 5, BlockSize: math.NaN()}
	if !math.IsNaN(unknown.TempBytesWritten()) {
		t.Fatal("bytes with an unknown block size must be unknown")
	}
	_, err = SpillStatements(Result{ProbeID: TempSpillStatements, Status: StatusUnsupported,
		Reason: "extension_not_installed"})
	wantUnavailable(t, err, StatusUnsupported)
}

func TestReplicationStages_DecodesBacklogs(t *testing.T) {
	res := m6Result(ReplicationLag, Row{"pid": int64(500), "application_name": "replica1",
		"client_addr": "10.0.0.9", "state": "streaming", "sync_state": "async",
		"kind": "logical", "write_lag_s": 0.5, "flush_lag_s": nil, "replay_lag_s": 12.0,
		"send_backlog_bytes": int64(1 << 20), "flush_backlog_bytes": int64(2 << 20),
		"replay_backlog_bytes": int64(40 << 20), "replay_lag_bytes": int64(43 << 20)})
	rs, err := ReplicationStages(res)
	if err != nil || len(rs) != 1 {
		t.Fatalf("stages = %+v (%v)", rs, err)
	}
	r := rs[0]
	if r.PID != 500 || r.Application != "replica1" || r.ClientAddr != "10.0.0.9" ||
		r.State != "streaming" || r.SyncState != "async" || r.Kind != "logical" ||
		r.WriteLagS != 0.5 || r.ReplayLagS != 12 || r.SendBacklog != 1<<20 ||
		r.FlushBacklog != 2<<20 || r.ReplayBacklog != 40<<20 || r.ReplayLagBytes != 43<<20 {
		t.Fatalf("decoded = %+v", r)
	}
	if !math.IsNaN(r.FlushLagS) {
		t.Fatalf("NULL flush lag decoded as %v", r.FlushLagS)
	}
	none, err := ReplicationStages(m6Result(ReplicationLag))
	if err != nil || none != nil {
		t.Fatalf("no replicas = %+v (%v)", none, err)
	}
}

func TestStandbyStates_DecodesPrimaryAndStandby(t *testing.T) {
	primary, err := StandbyStates(m6Result(StandbyReplayState, Row{"in_recovery": false,
		"replay_paused": nil, "receive_replay_bytes": nil, "last_replay_age_s": nil,
		"conflicts": int64(0), "receiver_status": nil,
		"max_standby_streaming_delay_ms": int64(30000), "longest_query_s": nil,
		"server_started_at": m6At}))
	if err != nil || primary.InRecovery || primary.ReplayPaused ||
		!math.IsNaN(primary.ReceiveReplayBytes) || primary.MaxStandbyDelayMS != 30000 {
		t.Fatalf("primary = %+v (%v)", primary, err)
	}
	standby, err := StandbyStates(m6Result(StandbyReplayState, Row{"in_recovery": true,
		"replay_paused": true, "receive_replay_bytes": int64(64 << 20),
		"last_replay_age_s": 95.0, "conflicts": int64(7), "receiver_status": "streaming",
		"max_standby_streaming_delay_ms": int64(-1), "longest_query_s": 300.0,
		"server_started_at": m6At}))
	if err != nil || !standby.InRecovery || !standby.ReplayPaused ||
		standby.ReceiveReplayBytes != 64<<20 || standby.LastReplayAgeS != 95 ||
		standby.Conflicts != 7 || standby.ReceiverStatus != "streaming" ||
		standby.MaxStandbyDelayMS != -1 || standby.LongestQueryS != 300 {
		t.Fatalf("standby = %+v (%v)", standby, err)
	}
	_, err = StandbyStates(m6Result(StandbyReplayState))
	wantUnavailable(t, err, StatusEmpty)
}

func TestWaitGroups_DecodesSamples(t *testing.T) {
	res := m6Result(LWLockWaits, Row{"wait_event_type": "LWLock", "wait_event": "WALWrite",
		"query_id": int64(9), "in_current_database": true, "backends": int64(20),
		"active_backends": int64(30)}, Row{"wait_event_type": "CPU", "wait_event": "",
		"query_id": nil, "in_current_database": false, "backends": int64(10),
		"active_backends": int64(30)})
	gs, err := WaitGroups(res)
	if err != nil || len(gs) != 2 {
		t.Fatalf("groups = %+v (%v)", gs, err)
	}
	if g := gs[0]; g.Type != "LWLock" || g.Event != "WALWrite" || g.QueryID != 9 ||
		!g.QueryIDKnown || !g.InCurrentDatabase || g.Backends != 20 || g.ActiveBackends != 30 {
		t.Fatalf("first group = %+v", g)
	}
	if g := gs[1]; g.Type != "CPU" || g.QueryIDKnown || g.InCurrentDatabase {
		t.Fatalf("second group = %+v", g)
	}
	if _, err := WaitGroups(m6Result(LWLockWaits, Row{"backends": "many"})); err == nil {
		t.Fatal("a non-integer backends count decoded")
	}
	idle, err := WaitGroups(m6Result(LWLockWaits))
	if err != nil || idle != nil {
		t.Fatalf("idle = %+v (%v)", idle, err)
	}
}
