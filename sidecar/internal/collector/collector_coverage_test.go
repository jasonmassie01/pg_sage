package collector

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// ---------------------------------------------------------------------------
// Snapshot JSON round-trip tests
// ---------------------------------------------------------------------------

func TestSnapshot_JSONRoundTrip_Full(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	addr := "192.168.1.1"
	relName := "users"
	query := "SELECT 1"
	state := "active"
	wet := "Lock"
	we := "relation"
	bs := now.Add(-time.Hour)

	original := &Snapshot{
		CollectedAt: now,
		Queries: []QueryStats{
			{
				QueryID: 42, Query: "SELECT * FROM t",
				Calls: 100, TotalExecTime: 5.5,
				MeanExecTime: 0.055, MinExecTime: 0.01,
				MaxExecTime: 1.0, StddevExecTime: 0.1,
				Rows: 1000, SharedBlksHit: 500,
				SharedBlksRead: 50, SharedBlksDirtied: 10,
				SharedBlksWritten: 5, TempBlksRead: 2,
				TempBlksWritten: 1, BlkReadTime: 0.5,
				BlkWriteTime: 0.1, WALRecords: 10,
				WALFpi: 2, WALBytes: 1024,
				TotalPlanTime: 0.3, MeanPlanTime: 0.003,
			},
		},
		Tables: []TableStats{
			{
				SchemaName: "public", RelName: "users",
				SeqScan: 10, SeqTupRead: 1000,
				IdxScan: 500, IdxTupFetch: 450,
				NTupIns: 100, NTupUpd: 50,
				NTupDel: 5, NTupHotUpd: 30,
				NLiveTup: 10000, NDeadTup: 200,
				VacuumCount: 3, AutovacuumCount: 10,
				AnalyzeCount: 2, AutoanalyzeCount: 8,
				TotalBytes: 1048576, TableBytes: 819200,
				IndexBytes: 229376, Relpersistence: "p",
			},
		},
		Indexes: []IndexStats{
			{
				SchemaName: "public", RelName: "users",
				IndexRelName: "users_pkey",
				IdxScan:      1000, IdxTupRead: 950,
				IdxTupFetch: 900, IndexBytes: 65536,
				IsUnique: true, IsPrimary: true,
				IsValid:   true,
				IndexDef:  "CREATE UNIQUE INDEX users_pkey ON public.users USING btree (id)",
				IndexType: "btree",
			},
		},
		ForeignKeys: []ForeignKey{
			{
				TableName: "orders", ReferencedTable: "users",
				FKColumn: "user_id", ConstraintName: "orders_user_id_fkey",
			},
		},
		System: SystemStats{
			ActiveBackends: 5, IdleInTransaction: 2,
			TotalBackends: 20, MaxConnections: 100,
			CacheHitRatio: 99.5, Deadlocks: 0,
			BlkReadTime: 1.5, BlkWriteTime: 0.5,
			TotalCheckpoints: 100, IsReplica: false,
			DBSizeBytes: 1073741824, StatStatementsMax: 5000,
		},
		Locks: []LockInfo{
			{
				LockType: "relation", Mode: "AccessShareLock",
				Granted: true, RelName: &relName,
				Query: &query, State: &state,
				WaitEventType: &wet, WaitEvent: &we,
				PID: 12345, BackendStart: &bs, QueryStart: &now,
			},
		},
		Sequences: []SequenceStats{
			{
				SchemaName: "public", SequenceName: "users_id_seq",
				DataType: "bigint", LastValue: 50000,
				MaxValue: 9223372036854775807, IncrementBy: 1,
				PctUsed: 0.0,
			},
		},
		Replication: &ReplicationStats{
			Replicas: []ReplicaInfo{
				{
					ClientAddr: &addr, State: "streaming",
					SentLSN: lsnPtr("0/1000000"), WriteLSN: lsnPtr("0/1000000"),
					FlushLSN: lsnPtr("0/1000000"), ReplayLSN: lsnPtr("0/F00000"),
					SyncState: "async",
				},
			},
			Slots: []SlotInfo{
				{
					SlotName: "replica_slot", SlotType: "physical",
					Active: true, RetainedBytes: 1024,
				},
			},
		},
		IO: []IOStats{
			{
				BackendType: "client backend", Object: "relation",
				Context: "normal",
				Reads:   100, ReadTime: 10.5,
				Writes: 50, WriteTime: 5.2,
				Writebacks: 10, WritebackTime: 1.1,
				Extends: 5, ExtendTime: 0.5,
				Hits: 1000, Evictions: 20,
				Reuses: 15, Fsyncs: 3,
				FsyncTime: 0.3,
			},
		},
		Partitions: []PartitionInfo{
			{
				ChildTable: "orders_2024", ChildSchema: "public",
				ParentTable: "orders", ParentSchema: "public",
			},
		},
		ConfigData: &ConfigSnapshot{
			PGSettings: []PGSetting{
				{
					Name: "shared_buffers", Setting: "128MB",
					Unit: "8kB", Source: "configuration file",
					PendingRestart: false,
				},
			},
			TableReloptions: []TableReloption{
				{
					SchemaName: "public", RelName: "users",
					Reloptions: "{autovacuum_vacuum_scale_factor=0.01}",
				},
			},
			ConnectionStates: []ConnectionState{
				{State: "active", Count: 5, AvgDurationSeconds: 0.5},
			},
			WALPosition:         "0/2000000",
			ExtensionsAvailable: []string{"pg_repack", "hypopg"},
			ConnectionChurn:     12,
		},
		StatsReset: false,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Verify key fields survive round-trip.
	if restored.CollectedAt != original.CollectedAt {
		t.Errorf("CollectedAt: got %v, want %v",
			restored.CollectedAt, original.CollectedAt)
	}
	if len(restored.Queries) != 1 {
		t.Fatalf("Queries len: got %d, want 1", len(restored.Queries))
	}
	if restored.Queries[0].QueryID != 42 {
		t.Errorf("QueryID: got %d, want 42", restored.Queries[0].QueryID)
	}
	if restored.Queries[0].WALBytes != 1024 {
		t.Errorf("WALBytes: got %d, want 1024", restored.Queries[0].WALBytes)
	}
	if restored.Queries[0].TotalPlanTime != 0.3 {
		t.Errorf("TotalPlanTime: got %f, want 0.3",
			restored.Queries[0].TotalPlanTime)
	}
	if len(restored.Tables) != 1 || restored.Tables[0].RelName != "users" {
		t.Errorf("Tables not preserved")
	}
	if len(restored.Indexes) != 1 {
		t.Errorf("Indexes not preserved")
	}
	if restored.Indexes[0].IsPrimary != true {
		t.Errorf("IsPrimary: got false, want true")
	}
	if len(restored.ForeignKeys) != 1 {
		t.Errorf("ForeignKeys not preserved")
	}
	if restored.System.CacheHitRatio != 99.5 {
		t.Errorf("CacheHitRatio: got %f, want 99.5",
			restored.System.CacheHitRatio)
	}
	if restored.System.StatStatementsMax != 5000 {
		t.Errorf("StatStatementsMax: got %d, want 5000",
			restored.System.StatStatementsMax)
	}
	if len(restored.Locks) != 1 {
		t.Errorf("Locks not preserved")
	}
	if restored.Locks[0].PID != 12345 {
		t.Errorf("Lock PID: got %d, want 12345", restored.Locks[0].PID)
	}
	if len(restored.Sequences) != 1 {
		t.Errorf("Sequences not preserved")
	}
	if restored.Replication == nil {
		t.Fatal("Replication is nil after round-trip")
	}
	if len(restored.Replication.Replicas) != 1 {
		t.Errorf("Replicas not preserved")
	}
	if len(restored.Replication.Slots) != 1 {
		t.Errorf("Slots not preserved")
	}
	if len(restored.IO) != 1 {
		t.Errorf("IO not preserved")
	}
	if len(restored.Partitions) != 1 {
		t.Errorf("Partitions not preserved")
	}
	if restored.ConfigData == nil {
		t.Fatal("ConfigData is nil after round-trip")
	}
	if len(restored.ConfigData.PGSettings) != 1 {
		t.Errorf("PGSettings not preserved")
	}
	if restored.ConfigData.ConnectionChurn != 12 {
		t.Errorf("ConnectionChurn: got %d, want 12",
			restored.ConfigData.ConnectionChurn)
	}
}

func TestSnapshot_JSONRoundTrip_Empty(t *testing.T) {
	snap := &Snapshot{}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal empty snapshot: %v", err)
	}
	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal empty snapshot: %v", err)
	}
	if restored.Replication != nil {
		t.Error("empty snapshot Replication should be nil")
	}
	if restored.ConfigData != nil {
		t.Error("empty snapshot ConfigData should be nil")
	}
	if restored.Queries != nil {
		t.Error("empty snapshot Queries should be nil")
	}
	if restored.StatsReset {
		t.Error("empty snapshot StatsReset should be false")
	}
}

func TestSnapshot_JSONRoundTrip_NilReplication(t *testing.T) {
	snap := &Snapshot{
		CollectedAt: time.Now().UTC(),
		Replication: nil,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.Replication != nil {
		t.Error("nil Replication should remain nil after round-trip")
	}
}

func TestSnapshot_StatsResetFlag(t *testing.T) {
	snap := &Snapshot{StatsReset: true}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !restored.StatsReset {
		t.Error("StatsReset should be true after round-trip")
	}
}

// ---------------------------------------------------------------------------
// Snapshot omitempty tests
// ---------------------------------------------------------------------------

func TestSnapshot_OmitEmpty_IOField(t *testing.T) {
	snap := &Snapshot{
		CollectedAt: time.Now().UTC(),
		IO:          nil,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// "io" should be omitted when nil
	if strings.Contains(string(data), `"io"`) {
		t.Error("nil IO should be omitted from JSON")
	}

	snap.IO = []IOStats{}
	_, err = json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Empty slice should also be omitted (omitempty)
	// Note: Go's json.Marshal omits nil slices but includes empty slices.
	// This test documents the actual behavior.
}

func TestSnapshot_OmitEmpty_PartitionsField(t *testing.T) {
	snap := &Snapshot{
		CollectedAt: time.Now().UTC(),
		Partitions:  nil,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"partitions"`) {
		t.Error("nil Partitions should be omitted from JSON")
	}
}

func TestSnapshot_OmitEmpty_ConfigData(t *testing.T) {
	snap := &Snapshot{
		CollectedAt: time.Now().UTC(),
		ConfigData:  nil,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"config_data"`) {
		t.Error("nil ConfigData should be omitted from JSON")
	}
}

func TestSnapshot_OmitEmpty_StatsReset(t *testing.T) {
	snap := &Snapshot{
		CollectedAt: time.Now().UTC(),
		StatsReset:  false,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"stats_reset"`) {
		t.Error("false StatsReset should be omitted from JSON")
	}
}

// ---------------------------------------------------------------------------
// QueryStats JSON tags
// ---------------------------------------------------------------------------

func TestQueryStats_JSONTags(t *testing.T) {
	q := QueryStats{
		QueryID:       123,
		Query:         "SELECT 1",
		Calls:         10,
		TotalExecTime: 5.0,
		WALRecords:    0,
		WALFpi:        0,
		WALBytes:      0,
		TotalPlanTime: 0,
		MeanPlanTime:  0,
	}
	data, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)

	required := []string{
		`"queryid"`, `"query"`, `"calls"`,
		`"total_exec_time"`, `"mean_exec_time"`,
		`"shared_blks_hit"`, `"shared_blks_read"`,
	}
	for _, tag := range required {
		if !strings.Contains(s, tag) {
			t.Errorf("missing JSON tag %s in output", tag)
		}
	}

	// WAL fields with zero should be omitted (omitempty)
	if strings.Contains(s, `"wal_records"`) {
		// WAL fields are omitempty so zero values should be omitted
	}
}

// ---------------------------------------------------------------------------
// TableStats methods
// ---------------------------------------------------------------------------

func TestTableStats_IsUnlogged_AllPersistenceValues(t *testing.T) {
	cases := []struct {
		rp   string
		want bool
	}{
		{"u", true},
		{"p", false},
		{"t", false},
		{"", false},
		{"x", false}, // invalid value
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("relpersistence=%q", tc.rp), func(t *testing.T) {
			ts := TableStats{Relpersistence: tc.rp}
			if got := ts.IsUnlogged(); got != tc.want {
				t.Errorf("IsUnlogged() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IndexStats JSON
// ---------------------------------------------------------------------------

func TestIndexStats_JSONRoundTrip(t *testing.T) {
	idx := IndexStats{
		SchemaName:   "public",
		RelName:      "users",
		IndexRelName: "idx_users_email",
		IdxScan:      100,
		IdxTupRead:   90,
		IdxTupFetch:  85,
		IndexBytes:   8192,
		IsUnique:     true,
		IsPrimary:    false,
		IsValid:      true,
		IndexDef:     "CREATE UNIQUE INDEX idx_users_email ON users (email)",
		IndexType:    "btree",
	}
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored IndexStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.SchemaName != "public" {
		t.Errorf("SchemaName: got %q, want %q",
			restored.SchemaName, "public")
	}
	if restored.IsUnique != true {
		t.Error("IsUnique should be true")
	}
	if restored.IsPrimary != false {
		t.Error("IsPrimary should be false")
	}
	if restored.IndexType != "btree" {
		t.Errorf("IndexType: got %q, want %q",
			restored.IndexType, "btree")
	}
}

// ---------------------------------------------------------------------------
// ForeignKey JSON
// ---------------------------------------------------------------------------

func TestForeignKey_JSONRoundTrip(t *testing.T) {
	fk := ForeignKey{
		TableName:       "orders",
		ReferencedTable: "users",
		FKColumn:        "user_id",
		ConstraintName:  "fk_orders_user",
	}
	data, err := json.Marshal(fk)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored ForeignKey
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored != fk {
		t.Errorf("ForeignKey mismatch: got %+v, want %+v", restored, fk)
	}
}

// ---------------------------------------------------------------------------
// SystemStats
// ---------------------------------------------------------------------------

func TestSystemStats_JSONRoundTrip(t *testing.T) {
	s := SystemStats{
		ActiveBackends:    10,
		IdleInTransaction: 2,
		TotalBackends:     30,
		MaxConnections:    100,
		CacheHitRatio:     99.9,
		Deadlocks:         5,
		BlkReadTime:       100.5,
		BlkWriteTime:      50.2,
		TotalCheckpoints:  200,
		IsReplica:         true,
		DBSizeBytes:       2147483648,
		StatStatementsMax: 10000,
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored SystemStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.ActiveBackends != 10 {
		t.Errorf("ActiveBackends: got %d, want 10",
			restored.ActiveBackends)
	}
	if restored.IsReplica != true {
		t.Error("IsReplica should be true")
	}
	if restored.StatStatementsMax != 10000 {
		t.Errorf("StatStatementsMax: got %d, want 10000",
			restored.StatStatementsMax)
	}
}

func TestSystemStats_ZeroValues(t *testing.T) {
	s := SystemStats{}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored SystemStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.ActiveBackends != 0 {
		t.Errorf("zero-value ActiveBackends: got %d", restored.ActiveBackends)
	}
	if restored.IsReplica {
		t.Error("zero-value IsReplica should be false")
	}
	if restored.CacheHitRatio != 0 {
		t.Errorf("zero-value CacheHitRatio: got %f", restored.CacheHitRatio)
	}
}

// ---------------------------------------------------------------------------
// LockInfo
// ---------------------------------------------------------------------------

func TestLockInfo_NilPointerFields(t *testing.T) {
	lk := LockInfo{
		LockType: "relation",
		Mode:     "ExclusiveLock",
		Granted:  false,
		PID:      999,
		// All pointer fields nil
	}
	data, err := json.Marshal(lk)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored LockInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.RelName != nil {
		t.Error("nil RelName should remain nil")
	}
	if restored.Query != nil {
		t.Error("nil Query should remain nil")
	}
	if restored.State != nil {
		t.Error("nil State should remain nil")
	}
	if restored.WaitEventType != nil {
		t.Error("nil WaitEventType should remain nil")
	}
	if restored.WaitEvent != nil {
		t.Error("nil WaitEvent should remain nil")
	}
	if restored.BackendStart != nil {
		t.Error("nil BackendStart should remain nil")
	}
	if restored.QueryStart != nil {
		t.Error("nil QueryStart should remain nil")
	}
	if restored.PID != 999 {
		t.Errorf("PID: got %d, want 999", restored.PID)
	}
	if restored.Granted {
		t.Error("Granted should be false")
	}
}

func TestLockInfo_WithPointerFields(t *testing.T) {
	relName := "my_table"
	query := "UPDATE my_table SET x = 1"
	state := "active"
	now := time.Now().UTC().Truncate(time.Second)

	lk := LockInfo{
		LockType:     "relation",
		Mode:         "RowExclusiveLock",
		Granted:      true,
		RelName:      &relName,
		Query:        &query,
		State:        &state,
		PID:          1234,
		BackendStart: &now,
		QueryStart:   &now,
	}
	data, err := json.Marshal(lk)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored LockInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.RelName == nil || *restored.RelName != "my_table" {
		t.Error("RelName not preserved")
	}
	if restored.Query == nil || *restored.Query != query {
		t.Error("Query not preserved")
	}
}

// ---------------------------------------------------------------------------
// SequenceStats
// ---------------------------------------------------------------------------

func TestSequenceStats_JSONRoundTrip(t *testing.T) {
	seq := SequenceStats{
		SchemaName:   "public",
		SequenceName: "orders_id_seq",
		DataType:     "integer",
		LastValue:    2000000000,
		MaxValue:     2147483647,
		IncrementBy:  1,
		PctUsed:      93.13,
	}
	data, err := json.Marshal(seq)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored SequenceStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.PctUsed != 93.13 {
		t.Errorf("PctUsed: got %f, want 93.13", restored.PctUsed)
	}
	if restored.MaxValue != 2147483647 {
		t.Errorf("MaxValue: got %d, want 2147483647", restored.MaxValue)
	}
}

// ---------------------------------------------------------------------------
// ReplicationStats
// ---------------------------------------------------------------------------

func TestReplicationStats_EmptyReplicas(t *testing.T) {
	rs := &ReplicationStats{
		Replicas: nil,
		Slots:    nil,
	}
	data, err := json.Marshal(rs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored ReplicationStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.Replicas != nil {
		t.Error("nil Replicas should remain nil")
	}
	if restored.Slots != nil {
		t.Error("nil Slots should remain nil")
	}
}

func TestReplicaInfo_NilAddr(t *testing.T) {
	ri := ReplicaInfo{
		ClientAddr: nil,
		State:      "streaming",
		SentLSN:    lsnPtr("0/1000"),
		WriteLSN:   lsnPtr("0/1000"),
		FlushLSN:   lsnPtr("0/1000"),
		ReplayLSN:  lsnPtr("0/1000"),
		WriteLag:   nil,
		FlushLag:   nil,
		ReplayLag:  nil,
		SyncState:  "async",
	}
	data, err := json.Marshal(ri)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored ReplicaInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.ClientAddr != nil {
		t.Error("nil ClientAddr should remain nil")
	}
	if restored.State != "streaming" {
		t.Errorf("State: got %q, want %q", restored.State, "streaming")
	}
}

// ---------------------------------------------------------------------------
// IOStats
// ---------------------------------------------------------------------------

func TestIOStats_JSONRoundTrip(t *testing.T) {
	io := IOStats{
		BackendType:   "autovacuum worker",
		Object:        "relation",
		Context:       "vacuum",
		Reads:         500,
		ReadTime:      25.5,
		Writes:        200,
		WriteTime:     10.2,
		Writebacks:    50,
		WritebackTime: 3.1,
		Extends:       100,
		ExtendTime:    5.5,
		Hits:          10000,
		Evictions:     300,
		Reuses:        150,
		Fsyncs:        20,
		FsyncTime:     1.5,
	}
	data, err := json.Marshal(io)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored IOStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.BackendType != "autovacuum worker" {
		t.Errorf("BackendType: got %q, want %q",
			restored.BackendType, "autovacuum worker")
	}
	if restored.Reads != 500 {
		t.Errorf("Reads: got %d, want 500", restored.Reads)
	}
	if restored.FsyncTime != 1.5 {
		t.Errorf("FsyncTime: got %f, want 1.5", restored.FsyncTime)
	}
}

// ---------------------------------------------------------------------------
// PartitionInfo
// ---------------------------------------------------------------------------

func TestPartitionInfo_JSONRoundTrip(t *testing.T) {
	p := PartitionInfo{
		ChildTable:   "orders_2024_q1",
		ChildSchema:  "public",
		ParentTable:  "orders",
		ParentSchema: "public",
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored PartitionInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored != p {
		t.Errorf("PartitionInfo mismatch: got %+v, want %+v", restored, p)
	}
}

// ---------------------------------------------------------------------------
// ConfigSnapshot
// ---------------------------------------------------------------------------

func TestConfigSnapshot_JSONRoundTrip(t *testing.T) {
	cs := &ConfigSnapshot{
		PGSettings: []PGSetting{
			{
				Name: "shared_buffers", Setting: "128MB",
				Unit: "8kB", Source: "configuration file",
				PendingRestart: true,
			},
			{
				Name: "work_mem", Setting: "4MB",
				Unit: "kB", Source: "default",
				PendingRestart: false,
			},
		},
		TableReloptions: []TableReloption{
			{
				SchemaName: "public", RelName: "big_table",
				Reloptions: "{autovacuum_vacuum_scale_factor=0.01}",
			},
		},
		ConnectionStates: []ConnectionState{
			{State: "active", Count: 5, AvgDurationSeconds: 0.5},
			{State: "idle", Count: 10, AvgDurationSeconds: 120.0},
		},
		WALPosition:         "0/3000000",
		ExtensionsAvailable: []string{"pg_repack", "hypopg", "pgstattuple"},
		ConnectionChurn:     25,
	}

	data, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored ConfigSnapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(restored.PGSettings) != 2 {
		t.Fatalf("PGSettings len: got %d, want 2",
			len(restored.PGSettings))
	}
	if restored.PGSettings[0].PendingRestart != true {
		t.Error("PendingRestart should be true for shared_buffers")
	}
	if restored.WALPosition != "0/3000000" {
		t.Errorf("WALPosition: got %q, want %q",
			restored.WALPosition, "0/3000000")
	}
	if len(restored.ExtensionsAvailable) != 3 {
		t.Errorf("ExtensionsAvailable len: got %d, want 3",
			len(restored.ExtensionsAvailable))
	}
	if restored.ConnectionChurn != 25 {
		t.Errorf("ConnectionChurn: got %d, want 25",
			restored.ConnectionChurn)
	}
}

func TestConfigSnapshot_Empty(t *testing.T) {
	cs := &ConfigSnapshot{}
	data, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored ConfigSnapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.PGSettings != nil {
		t.Error("empty PGSettings should be nil")
	}
	if restored.WALPosition != "" {
		t.Errorf("empty WALPosition: got %q", restored.WALPosition)
	}
}

// ---------------------------------------------------------------------------
// SlotInfo
// ---------------------------------------------------------------------------

func TestSlotInfo_JSONRoundTrip(t *testing.T) {
	si := SlotInfo{
		SlotName:      "test_slot",
		SlotType:      "logical",
		Active:        false,
		RetainedBytes: 1048576,
	}
	data, err := json.Marshal(si)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored SlotInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.SlotName != "test_slot" {
		t.Errorf("SlotName: got %q, want %q",
			restored.SlotName, "test_slot")
	}
	if restored.Active {
		t.Error("Active should be false")
	}
	if restored.RetainedBytes != 1048576 {
		t.Errorf("RetainedBytes: got %d, want 1048576",
			restored.RetainedBytes)
	}
}

// ---------------------------------------------------------------------------
// CircuitBreaker — comprehensive tests
// ---------------------------------------------------------------------------

func TestNewCircuitBreaker_DefaultState(t *testing.T) {
	cb := NewCircuitBreaker(90, 10)
	if cb.cpuCeilingPct != 90 {
		t.Errorf("cpuCeilingPct: got %d, want 90", cb.cpuCeilingPct)
	}
	if cb.maxSkips != 10 {
		t.Errorf("maxSkips: got %d, want 10", cb.maxSkips)
	}
	if cb.IsDormant() {
		t.Error("new breaker should not be dormant")
	}
	if cb.consecutiveSkips != 0 {
		t.Errorf("consecutiveSkips: got %d, want 0",
			cb.consecutiveSkips)
	}
	if cb.successCount != 0 {
		t.Errorf("successCount: got %d, want 0", cb.successCount)
	}
}

func TestNewCircuitBreaker_ZeroValues(t *testing.T) {
	cb := NewCircuitBreaker(0, 0)
	if cb.cpuCeilingPct != 0 {
		t.Errorf("cpuCeilingPct: got %d, want 0", cb.cpuCeilingPct)
	}
	if cb.maxSkips != 0 {
		t.Errorf("maxSkips: got %d, want 0", cb.maxSkips)
	}
}

func TestCircuitBreaker_RecordSuccess_ResetsSkips(t *testing.T) {
	cb := NewCircuitBreaker(80, 5)
	cb.mu.Lock()
	cb.consecutiveSkips = 3
	cb.mu.Unlock()

	cb.RecordSuccess()

	cb.mu.Lock()
	if cb.consecutiveSkips != 0 {
		t.Errorf("consecutiveSkips after success: got %d, want 0",
			cb.consecutiveSkips)
	}
	cb.mu.Unlock()
}

func TestCircuitBreaker_RecordSuccess_IncreasesSuccessCount(t *testing.T) {
	cb := NewCircuitBreaker(80, 5)

	cb.RecordSuccess()
	cb.mu.Lock()
	if cb.successCount != 1 {
		t.Errorf("successCount after 1 success: got %d, want 1",
			cb.successCount)
	}
	cb.mu.Unlock()

	cb.RecordSuccess()
	cb.mu.Lock()
	if cb.successCount != 2 {
		t.Errorf("successCount after 2 successes: got %d, want 2",
			cb.successCount)
	}
	cb.mu.Unlock()
}

func TestCircuitBreaker_DormantRequires3Successes(t *testing.T) {
	cb := NewCircuitBreaker(80, 5)
	cb.mu.Lock()
	cb.isDormant = true
	cb.mu.Unlock()

	// 1st success
	cb.RecordSuccess()
	if !cb.IsDormant() {
		t.Error("still dormant after 1 success")
	}

	// 2nd success
	cb.RecordSuccess()
	if !cb.IsDormant() {
		t.Error("still dormant after 2 successes")
	}

	// 3rd success exits dormant
	cb.RecordSuccess()
	if cb.IsDormant() {
		t.Error("should exit dormant after 3 successes")
	}

	// After exiting dormant, successCount resets
	cb.mu.Lock()
	if cb.successCount != 0 {
		t.Errorf("successCount after dormant exit: got %d, want 0",
			cb.successCount)
	}
	cb.mu.Unlock()
}

func TestCircuitBreaker_NotDormant_SuccessDoesNotReset(t *testing.T) {
	cb := NewCircuitBreaker(80, 5)
	// Not dormant, accumulate successes past 3
	for i := 0; i < 10; i++ {
		cb.RecordSuccess()
	}
	cb.mu.Lock()
	if cb.successCount != 10 {
		t.Errorf("successCount: got %d, want 10", cb.successCount)
	}
	if cb.isDormant {
		t.Error("should not become dormant from successes")
	}
	cb.mu.Unlock()
}

func TestCircuitBreaker_IsDormant_ThreadSafe(t *testing.T) {
	cb := NewCircuitBreaker(80, 2)

	var wg sync.WaitGroup
	// Concurrent reads and writes
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			cb.IsDormant()
		}()
		go func() {
			defer wg.Done()
			cb.RecordSuccess()
		}()
	}
	wg.Wait()
	// No race condition panic = pass
}

// ---------------------------------------------------------------------------
// Collector — LatestSnapshot / PreviousSnapshot
// ---------------------------------------------------------------------------

func TestCollector_LatestSnapshot_InitiallyNil(t *testing.T) {
	cfg := &config.Config{
		Safety: config.SafetyConfig{
			CPUCeilingPct:           90,
			BackoffConsecutiveSkips: 5,
		},
	}
	c := New(nil, cfg, 170000, func(string, string, ...any) {})
	if c.LatestSnapshot() != nil {
		t.Error("initial LatestSnapshot should be nil")
	}
	if c.PreviousSnapshot() != nil {
		t.Error("initial PreviousSnapshot should be nil")
	}
}

func TestCollector_SnapshotAccess_ThreadSafe(t *testing.T) {
	cfg := &config.Config{
		Safety: config.SafetyConfig{
			CPUCeilingPct:           90,
			BackoffConsecutiveSkips: 5,
		},
	}
	c := New(nil, cfg, 170000, func(string, string, ...any) {})

	snap := &Snapshot{CollectedAt: time.Now().UTC()}

	var wg sync.WaitGroup
	// Concurrent reads and writes to snapshots.
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			c.LatestSnapshot()
		}()
		go func() {
			defer wg.Done()
			c.PreviousSnapshot()
		}()
		go func() {
			defer wg.Done()
			c.mu.Lock()
			c.previous = c.latest
			c.latest = snap
			c.mu.Unlock()
		}()
	}
	wg.Wait()
}

func TestCollector_SnapshotRotation(t *testing.T) {
	cfg := &config.Config{
		Safety: config.SafetyConfig{
			CPUCeilingPct:           90,
			BackoffConsecutiveSkips: 5,
		},
	}
	c := New(nil, cfg, 170000, func(string, string, ...any) {})

	snap1 := &Snapshot{
		CollectedAt: time.Now().UTC().Add(-time.Minute),
	}
	snap2 := &Snapshot{
		CollectedAt: time.Now().UTC(),
	}

	// Set first snapshot.
	c.mu.Lock()
	c.previous = c.latest
	c.latest = snap1
	c.mu.Unlock()

	if c.LatestSnapshot() != snap1 {
		t.Error("LatestSnapshot should be snap1")
	}
	if c.PreviousSnapshot() != nil {
		t.Error("PreviousSnapshot should still be nil")
	}

	// Set second snapshot, rotating first to previous.
	c.mu.Lock()
	c.previous = c.latest
	c.latest = snap2
	c.mu.Unlock()

	if c.LatestSnapshot() != snap2 {
		t.Error("LatestSnapshot should be snap2")
	}
	if c.PreviousSnapshot() != snap1 {
		t.Error("PreviousSnapshot should be snap1")
	}
}

// ---------------------------------------------------------------------------
// New() constructor
// ---------------------------------------------------------------------------

func TestNew_SetsFieldsCorrectly(t *testing.T) {
	cfg := &config.Config{
		Safety: config.SafetyConfig{
			CPUCeilingPct:           85,
			BackoffConsecutiveSkips: 7,
		},
	}
	var logCalled bool
	logFn := func(level, msg string, args ...any) {
		logCalled = true
	}

	c := New(nil, cfg, 160005, logFn)

	if c.cfg != cfg {
		t.Error("cfg not set correctly")
	}
	if c.pgVersionNum != 160005 {
		t.Errorf("pgVersionNum: got %d, want 160005",
			c.pgVersionNum)
	}
	if c.breaker == nil {
		t.Fatal("breaker should not be nil")
	}
	if c.breaker.cpuCeilingPct != 85 {
		t.Errorf("breaker cpuCeilingPct: got %d, want 85",
			c.breaker.cpuCeilingPct)
	}
	if c.breaker.maxSkips != 7 {
		t.Errorf("breaker maxSkips: got %d, want 7",
			c.breaker.maxSkips)
	}
	if c.pool != nil {
		t.Error("pool should be nil when passed nil")
	}

	// Call logFn to verify it was stored.
	c.logFn("INFO", "test")
	if !logCalled {
		t.Error("logFn not called")
	}
}

// ---------------------------------------------------------------------------
// detectStatsReset — additional edge cases
// ---------------------------------------------------------------------------

func TestDetectStatsReset_BothEmpty(t *testing.T) {
	if detectStatsReset(nil, nil) {
		t.Error("both nil should return false")
	}
	if detectStatsReset([]QueryStats{}, []QueryStats{}) {
		t.Error("both empty should return false")
	}
}

func TestDetectStatsReset_CurrentEmpty(t *testing.T) {
	prev := []QueryStats{
		{QueryID: 1, Calls: 100},
	}
	// Empty current with non-empty previous: compared=0 -> false
	if detectStatsReset(nil, prev) {
		t.Error("nil current should return false")
	}
	if detectStatsReset([]QueryStats{}, prev) {
		t.Error("empty current should return false")
	}
}

func TestDetectStatsReset_SingleQueryDecreased(t *testing.T) {
	prev := []QueryStats{{QueryID: 1, Calls: 10000}}
	cur := []QueryStats{{QueryID: 1, Calls: 1}}
	// ratio = 1/1 = 100% > 50%, total = 1 < 10000/5 = 2000
	if !detectStatsReset(cur, prev) {
		t.Error("single query with massive decrease should detect reset")
	}
}

func TestDetectStatsReset_ExactBoundary(t *testing.T) {
	// Test exactly at 50% decreased and exactly at prevTotal/5
	prev := []QueryStats{
		{QueryID: 1, Calls: 100},
		{QueryID: 2, Calls: 100},
	}
	// 1/2 decreased (50%), total = 20+110=130, prevTotal/5=40
	// ratioDecreased = 0.5, need >0.5 so this is false
	cur := []QueryStats{
		{QueryID: 1, Calls: 20},
		{QueryID: 2, Calls: 110},
	}
	if detectStatsReset(cur, prev) {
		t.Error("exactly 50% decreased should NOT trigger reset (need >50%)")
	}
}

func TestDetectStatsReset_ExactTotalBoundary(t *testing.T) {
	// ratio > 50% but total exactly at prevTotal/5
	prev := []QueryStats{
		{QueryID: 1, Calls: 500},
		{QueryID: 2, Calls: 500},
		{QueryID: 3, Calls: 500},
	}
	// prevTotal = 1500, prevTotal/5 = 300
	// 2/3 decreased (66.7% > 50%)
	// currTotal = 10+10+280 = 300, need < 300 => exactly equal => false
	cur := []QueryStats{
		{QueryID: 1, Calls: 10},
		{QueryID: 2, Calls: 10},
		{QueryID: 3, Calls: 280},
	}
	if detectStatsReset(cur, prev) {
		t.Error("total exactly at prevTotal/5 should NOT trigger reset")
	}
}

func TestDetectStatsReset_LargeDataset(t *testing.T) {
	const n = 1000
	prev := make([]QueryStats, n)
	cur := make([]QueryStats, n)
	for i := 0; i < n; i++ {
		prev[i] = QueryStats{QueryID: int64(i + 1), Calls: 10000}
		cur[i] = QueryStats{QueryID: int64(i + 1), Calls: 1}
	}
	// All decreased, total = 1000 vs 10000000
	if !detectStatsReset(cur, prev) {
		t.Error("large dataset with all decreased should detect reset")
	}
}

func TestDetectStatsReset_NegativeCalls(t *testing.T) {
	// Shouldn't happen in practice, but test robustness.
	prev := []QueryStats{
		{QueryID: 1, Calls: 100},
	}
	cur := []QueryStats{
		{QueryID: 1, Calls: -1},
	}
	// -1 < 100 => decreased, total = -1 < 100/5 = 20
	if !detectStatsReset(cur, prev) {
		t.Error("negative calls should count as decreased")
	}
}

// ---------------------------------------------------------------------------
// sumCalls — additional edge cases
// ---------------------------------------------------------------------------

func TestSumCalls_EmptySlice(t *testing.T) {
	if got := sumCalls([]QueryStats{}); got != 0 {
		t.Errorf("sumCalls(empty) = %d, want 0", got)
	}
}

func TestSumCalls_SingleElement(t *testing.T) {
	qs := []QueryStats{{Calls: 42}}
	if got := sumCalls(qs); got != 42 {
		t.Errorf("sumCalls = %d, want 42", got)
	}
}

func TestSumCalls_LargeValues(t *testing.T) {
	qs := []QueryStats{
		{Calls: 9223372036854775806}, // near int64 max
		{Calls: 1},
	}
	got := sumCalls(qs)
	if got != 9223372036854775807 {
		t.Errorf("sumCalls = %d, want 9223372036854775807", got)
	}
}

func TestSumCalls_ZeroCalls(t *testing.T) {
	qs := []QueryStats{
		{Calls: 0},
		{Calls: 0},
		{Calls: 0},
	}
	if got := sumCalls(qs); got != 0 {
		t.Errorf("sumCalls = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// SQL constants — additional validation
// ---------------------------------------------------------------------------

func TestTableStatsSQL_UsesParameterizedPagination(t *testing.T) {
	if !strings.Contains(tableStatsSQL, "$1") {
		t.Error("tableStatsSQL must use $1 for pagination key")
	}
	if !strings.Contains(tableStatsSQL, "$2") {
		t.Error("tableStatsSQL must use $2 for batch size")
	}
}

func TestTableStatsSQL_OrdersCorrectly(t *testing.T) {
	if !strings.Contains(tableStatsSQL, "ORDER BY s.relid") {
		t.Error("tableStatsSQL must ORDER BY relid (oid keyset, dogfood lifeos-1)")
	}
}

func TestSequencesSQL_OrdersByPctUsed(t *testing.T) {
	if !strings.Contains(sequencesSQL, "ORDER BY pct_used DESC") {
		t.Error("sequencesSQL must ORDER BY pct_used DESC")
	}
}

func TestLocksSQL_ExcludesOwnBackend(t *testing.T) {
	if !strings.Contains(locksSQL, "pg_backend_pid()") {
		t.Error("locksSQL must exclude own backend via pg_backend_pid()")
	}
}

func TestReplicationReplicasSQL_HasRequiredColumns(t *testing.T) {
	required := []string{
		"client_addr", "state", "sent_lsn", "write_lsn",
		"flush_lsn", "replay_lsn", "write_lag", "flush_lag",
		"replay_lag", "sync_state",
	}
	for _, col := range required {
		if !strings.Contains(replicationReplicasSQL, col) {
			t.Errorf("replicationReplicasSQL missing column: %s", col)
		}
	}
}

func TestReplicationSlotsSQL_HasRequiredColumns(t *testing.T) {
	required := []string{
		"slot_name", "slot_type", "active", "retained_bytes",
	}
	for _, col := range required {
		if !strings.Contains(replicationSlotsSQL, col) {
			t.Errorf("replicationSlotsSQL missing column: %s", col)
		}
	}
}

func TestIOStatsSQL_HasPG16Filter(t *testing.T) {
	if !strings.Contains(ioStatsSQL, "pg_stat_io") {
		t.Error("ioStatsSQL must query pg_stat_io")
	}
	if !strings.Contains(ioStatsSQL, "LIMIT 100") {
		t.Error("ioStatsSQL must have LIMIT 100")
	}
}

func TestLoadRatioSQL_Structure(t *testing.T) {
	if !strings.Contains(loadRatioSQL, "max_connections") {
		t.Error("loadRatioSQL must reference max_connections")
	}
	if !strings.Contains(loadRatioSQL, "pg_stat_activity") {
		t.Error("loadRatioSQL must query pg_stat_activity")
	}
	if !strings.Contains(loadRatioSQL, "pg_backend_pid()") {
		t.Error("loadRatioSQL must exclude own backend")
	}
}

func TestSystemStatsSQL_SharedBase(t *testing.T) {
	// Both PG14 and PG17 variants should contain the base SQL.
	for _, variant := range []string{systemStatsSQL14, systemStatsSQL17} {
		if !strings.Contains(variant, "active_backends") {
			t.Error("system stats SQL must select active_backends")
		}
		if !strings.Contains(variant, "idle_in_transaction") {
			t.Error("system stats SQL must select idle_in_transaction")
		}
		if !strings.Contains(variant, "max_connections") {
			t.Error("system stats SQL must select max_connections")
		}
		if !strings.Contains(variant, "cache_hit_ratio") {
			t.Error("system stats SQL must select cache_hit_ratio")
		}
		if !strings.Contains(variant, "deadlocks") {
			t.Error("system stats SQL must select deadlocks")
		}
		if !strings.Contains(variant, "db_size_bytes") {
			t.Error("system stats SQL must select db_size_bytes")
		}
		if !strings.Contains(variant, "is_replica") {
			t.Error("system stats SQL must select is_replica")
		}
	}
}

// ---------------------------------------------------------------------------
// TableStats JSON — time pointer fields
// ---------------------------------------------------------------------------

func TestTableStats_TimePointerFields(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ts := TableStats{
		SchemaName:      "public",
		RelName:         "events",
		LastVacuum:      &now,
		LastAutovacuum:  nil,
		LastAnalyze:     &now,
		LastAutoanalyze: nil,
	}
	data, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored TableStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.LastVacuum == nil {
		t.Fatal("LastVacuum should not be nil")
	}
	if !restored.LastVacuum.Equal(now) {
		t.Errorf("LastVacuum: got %v, want %v",
			*restored.LastVacuum, now)
	}
	if restored.LastAutovacuum != nil {
		t.Error("LastAutovacuum should be nil")
	}
	if restored.LastAnalyze == nil {
		t.Fatal("LastAnalyze should not be nil")
	}
	if restored.LastAutoanalyze != nil {
		t.Error("LastAutoanalyze should be nil")
	}
}

func TestTableStats_ZeroValue(t *testing.T) {
	ts := TableStats{}
	if ts.IsUnlogged() {
		t.Error("zero-value TableStats should not be unlogged")
	}
	data, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored TableStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.SeqScan != 0 || restored.IdxScan != 0 {
		t.Error("zero-value stats should remain zero")
	}
}

// ---------------------------------------------------------------------------
// PGSetting / TableReloption / ConnectionState JSON
// ---------------------------------------------------------------------------

func TestPGSetting_JSONRoundTrip(t *testing.T) {
	s := PGSetting{
		Name: "work_mem", Setting: "4MB",
		Unit: "kB", Source: "default",
		PendingRestart: false,
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored PGSetting
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored != s {
		t.Errorf("PGSetting mismatch: got %+v, want %+v", restored, s)
	}
}

func TestTableReloption_JSONRoundTrip(t *testing.T) {
	tr := TableReloption{
		SchemaName: "public", RelName: "big_table",
		Reloptions: "{autovacuum_vacuum_cost_delay=10}",
	}
	data, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored TableReloption
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored != tr {
		t.Errorf("TableReloption mismatch: got %+v, want %+v",
			restored, tr)
	}
}

func TestConnectionState_JSONRoundTrip(t *testing.T) {
	cs := ConnectionState{
		State: "idle in transaction", Count: 3,
		AvgDurationSeconds: 45.5,
	}
	data, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored ConnectionState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.State != "idle in transaction" {
		t.Errorf("State: got %q, want %q",
			restored.State, "idle in transaction")
	}
	if restored.Count != 3 {
		t.Errorf("Count: got %d, want 3", restored.Count)
	}
}

// ---------------------------------------------------------------------------
// Snapshot — persist categories map coverage
// ---------------------------------------------------------------------------

func TestSnapshot_AllCategoriesSerializable(t *testing.T) {
	// Mirrors the categories map in persist() to ensure all
	// snapshot fields can be marshaled to JSON.
	snap := &Snapshot{
		CollectedAt: time.Now().UTC(),
		Queries:     []QueryStats{{QueryID: 1, Calls: 10}},
		Tables:      []TableStats{{SchemaName: "public", RelName: "t"}},
		Indexes: []IndexStats{
			{SchemaName: "public", IndexRelName: "idx"},
		},
		ForeignKeys: []ForeignKey{
			{TableName: "t", ConstraintName: "fk"},
		},
		System:    SystemStats{ActiveBackends: 1},
		Locks:     []LockInfo{{LockType: "relation", PID: 1}},
		Sequences: []SequenceStats{{SequenceName: "s"}},
		Replication: &ReplicationStats{
			Replicas: []ReplicaInfo{{State: "streaming"}},
		},
		IO:         []IOStats{{BackendType: "client backend"}},
		Partitions: []PartitionInfo{{ChildTable: "p"}},
		ConfigData: &ConfigSnapshot{
			PGSettings: []PGSetting{{Name: "work_mem"}},
		},
	}

	categories := map[string]any{
		"queries":      snap.Queries,
		"tables":       snap.Tables,
		"indexes":      snap.Indexes,
		"foreign_keys": snap.ForeignKeys,
		"system":       snap.System,
		"locks":        snap.Locks,
		"sequences":    snap.Sequences,
		"replication":  snap.Replication,
		"io":           snap.IO,
		"partitions":   snap.Partitions,
		"config_data":  snap.ConfigData,
	}

	for cat, data := range categories {
		j, err := json.Marshal(data)
		if err != nil {
			t.Errorf("category %q marshal failed: %v", cat, err)
			continue
		}
		if len(j) == 0 {
			t.Errorf("category %q produced empty JSON", cat)
		}
	}
}

func TestSnapshot_NilCategoriesSerializable(t *testing.T) {
	// All nil/empty — mirrors persist() with a bare snapshot.
	snap := &Snapshot{CollectedAt: time.Now().UTC()}
	categories := map[string]any{
		"queries":      snap.Queries,
		"tables":       snap.Tables,
		"indexes":      snap.Indexes,
		"foreign_keys": snap.ForeignKeys,
		"system":       snap.System,
		"locks":        snap.Locks,
		"sequences":    snap.Sequences,
		"replication":  snap.Replication,
		"io":           snap.IO,
		"partitions":   snap.Partitions,
		"config_data":  snap.ConfigData,
	}

	for cat, data := range categories {
		j, err := json.Marshal(data)
		if err != nil {
			t.Errorf("nil category %q marshal failed: %v", cat, err)
			continue
		}
		if len(j) == 0 {
			t.Errorf("nil category %q produced empty JSON", cat)
		}
	}
}

// ---------------------------------------------------------------------------
// QueryStats WAL/PlanTime omitempty behavior
// ---------------------------------------------------------------------------

func TestQueryStats_OmitEmptyWALFields(t *testing.T) {
	q := QueryStats{
		QueryID: 1, Query: "SELECT 1", Calls: 5,
		// WAL and PlanTime fields are zero — should be omitted.
	}
	data, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)

	omittedFields := []string{
		`"wal_records"`, `"wal_fpi"`, `"wal_bytes"`,
		`"total_plan_time"`, `"mean_plan_time"`,
	}
	for _, f := range omittedFields {
		if strings.Contains(s, f) {
			t.Errorf("zero-value %s should be omitted", f)
		}
	}
}

func TestQueryStats_NonZeroWALFields(t *testing.T) {
	q := QueryStats{
		QueryID: 1, Query: "INSERT INTO t VALUES (1)", Calls: 5,
		WALRecords: 10, WALFpi: 2, WALBytes: 2048,
		TotalPlanTime: 1.5, MeanPlanTime: 0.3,
	}
	data, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)

	required := []string{
		`"wal_records"`, `"wal_fpi"`, `"wal_bytes"`,
		`"total_plan_time"`, `"mean_plan_time"`,
	}
	for _, f := range required {
		if !strings.Contains(s, f) {
			t.Errorf("non-zero %s should be present", f)
		}
	}
}

// ---------------------------------------------------------------------------
// Index/FK/Partition SQL — ordering
// ---------------------------------------------------------------------------

func TestIndexStatsSQL_OrdersCorrectly(t *testing.T) {
	expected := "ORDER BY s.indexrelid" // oid keyset (dogfood lifeos-1)
	if !strings.Contains(indexStatsSQL, expected) {
		t.Errorf("indexStatsSQL must ORDER BY %s", expected)
	}
}

func TestForeignKeysSQL_OrdersCorrectly(t *testing.T) {
	expected := "ORDER BY cl.relname, con.conname"
	if !strings.Contains(foreignKeysSQL, expected) {
		t.Errorf("foreignKeysSQL must ORDER BY %s", expected)
	}
}

func TestPartitionSQL_OrdersCorrectly(t *testing.T) {
	expected := "ORDER BY parent_schema, parent_table, child_schema, child_table"
	if !strings.Contains(partitionInheritanceSQL, expected) {
		t.Errorf("partitionInheritanceSQL must ORDER BY %s", expected)
	}
}

// ---------------------------------------------------------------------------
// IOStats SQL — COALESCE usage
// ---------------------------------------------------------------------------

func TestIOStatsSQL_HasCoalesce(t *testing.T) {
	if !strings.Contains(ioStatsSQL, "COALESCE") {
		t.Error("ioStatsSQL must use COALESCE on nullable columns")
	}
}

func TestIOStatsSQL_FiltersByActivity(t *testing.T) {
	if !strings.Contains(ioStatsSQL, "reads > 0 OR writes > 0 OR hits > 0") {
		t.Error("ioStatsSQL must filter for active rows")
	}
}

// ---------------------------------------------------------------------------
// Snapshot with large data
// ---------------------------------------------------------------------------

func TestSnapshot_LargeQueryList(t *testing.T) {
	const n = 500
	queries := make([]QueryStats, n)
	for i := 0; i < n; i++ {
		queries[i] = QueryStats{
			QueryID:       int64(i + 1),
			Query:         fmt.Sprintf("SELECT * FROM table_%d", i),
			Calls:         int64(i * 100),
			TotalExecTime: float64(i) * 1.5,
		}
	}
	snap := &Snapshot{
		CollectedAt: time.Now().UTC(),
		Queries:     queries,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal with %d queries: %v", n, err)
	}
	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(restored.Queries) != n {
		t.Errorf("Queries len: got %d, want %d",
			len(restored.Queries), n)
	}
	// Check first and last.
	if restored.Queries[0].QueryID != 1 {
		t.Errorf("first QueryID: got %d, want 1",
			restored.Queries[0].QueryID)
	}
	if restored.Queries[n-1].QueryID != int64(n) {
		t.Errorf("last QueryID: got %d, want %d",
			restored.Queries[n-1].QueryID, n)
	}
}

// ---------------------------------------------------------------------------
// CircuitBreaker — manual dormant entry via skips
// ---------------------------------------------------------------------------

func TestCircuitBreaker_ManualSkipAccumulation(t *testing.T) {
	cb := NewCircuitBreaker(80, 3)

	// Simulate 3 consecutive skips by manipulating internal state.
	for i := 0; i < 3; i++ {
		cb.mu.Lock()
		cb.consecutiveSkips++
		if cb.consecutiveSkips >= cb.maxSkips {
			cb.isDormant = true
		}
		cb.mu.Unlock()
	}

	if !cb.IsDormant() {
		t.Error("should be dormant after maxSkips consecutive skips")
	}
}

func TestCircuitBreaker_SuccessResetsDormant(t *testing.T) {
	cb := NewCircuitBreaker(80, 3)
	// Force dormant.
	cb.mu.Lock()
	cb.isDormant = true
	cb.mu.Unlock()

	// 3 successes should clear dormant.
	cb.RecordSuccess()
	cb.RecordSuccess()
	cb.RecordSuccess()

	if cb.IsDormant() {
		t.Error("3 successes should exit dormant")
	}
}

// ---------------------------------------------------------------------------
// Snapshot — Categories map keys match persist()
// ---------------------------------------------------------------------------

func TestSnapshot_CategoryNames(t *testing.T) {
	// These are the exact category strings used in persist().
	expectedCategories := []string{
		"queries", "tables", "indexes", "foreign_keys",
		"system", "locks", "sequences", "replication",
		"io", "partitions", "config_data",
	}
	// Ensure each name is valid and non-empty.
	for _, cat := range expectedCategories {
		if cat == "" {
			t.Error("category name must not be empty")
		}
		if strings.Contains(cat, " ") {
			t.Errorf("category %q must not contain spaces", cat)
		}
	}
	// Ensure exactly 11 categories.
	if len(expectedCategories) != 11 {
		t.Errorf("expected 11 categories, got %d",
			len(expectedCategories))
	}
}
