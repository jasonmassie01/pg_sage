package collector

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 1. Query Stats SQL variant selection
// ---------------------------------------------------------------------------

func TestQueryStatsSQL_SelectsCorrectVariant(t *testing.T) {
	t.Run("base has no WAL or plan time columns", func(t *testing.T) {
		if strings.Contains(queryStatsSQL, "wal_records") {
			t.Error("queryStatsSQL should NOT contain wal_records")
		}
		if strings.Contains(queryStatsSQL, "total_plan_time") {
			t.Error("queryStatsSQL should NOT contain total_plan_time")
		}
	})

	t.Run("WAL variant has wal_records but not total_plan_time", func(t *testing.T) {
		if !strings.Contains(queryStatsWithWALSQL, "wal_records") {
			t.Error("queryStatsWithWALSQL must contain wal_records")
		}
		if strings.Contains(queryStatsWithWALSQL, "total_plan_time") {
			t.Error("queryStatsWithWALSQL should NOT contain total_plan_time")
		}
	})

	t.Run("plan time variant has total_plan_time but not wal_records", func(t *testing.T) {
		if !strings.Contains(queryStatsWithPlanTimeSQL, "total_plan_time") {
			t.Error("queryStatsWithPlanTimeSQL must contain total_plan_time")
		}
		if strings.Contains(queryStatsWithPlanTimeSQL, "wal_records") {
			t.Error("queryStatsWithPlanTimeSQL should NOT contain wal_records")
		}
	})

	t.Run("WAL+plan variant has both", func(t *testing.T) {
		if !strings.Contains(queryStatsWithWALAndPlanTimeSQL, "wal_records") {
			t.Error("queryStatsWithWALAndPlanTimeSQL must contain wal_records")
		}
		if !strings.Contains(queryStatsWithWALAndPlanTimeSQL, "total_plan_time") {
			t.Error("queryStatsWithWALAndPlanTimeSQL must contain total_plan_time")
		}
	})

	t.Run("all variants filter NULL queryid (v1 Bug #1 fix)", func(t *testing.T) {
		variants := map[string]string{
			"queryStatsSQL":                   queryStatsSQL,
			"queryStatsWithWALSQL":            queryStatsWithWALSQL,
			"queryStatsWithPlanTimeSQL":        queryStatsWithPlanTimeSQL,
			"queryStatsWithWALAndPlanTimeSQL": queryStatsWithWALAndPlanTimeSQL,
		}
		for name, sql := range variants {
			if !strings.Contains(sql, "AND queryid IS NOT NULL") {
				t.Errorf("%s must contain 'AND queryid IS NOT NULL'", name)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// 2 & 3. System Stats SQL — PG17 vs PG14
// ---------------------------------------------------------------------------

func TestSystemStatsSQL_PG17Checkpointer(t *testing.T) {
	if !strings.Contains(systemStatsSQL17, "pg_stat_checkpointer") {
		t.Error("systemStatsSQL17 must reference pg_stat_checkpointer")
	}
	if strings.Contains(systemStatsSQL17, "pg_stat_bgwriter") {
		t.Error("systemStatsSQL17 should NOT reference pg_stat_bgwriter")
	}
}

func TestSystemStatsSQL_PG14Baseline(t *testing.T) {
	if !strings.Contains(systemStatsSQL14, "pg_stat_bgwriter") {
		t.Error("systemStatsSQL14 must reference pg_stat_bgwriter")
	}
	if strings.Contains(systemStatsSQL14, "pg_stat_checkpointer") {
		t.Error("systemStatsSQL14 should NOT reference pg_stat_checkpointer")
	}
}

// ---------------------------------------------------------------------------
// 4-7. Schema exclusion filters
// ---------------------------------------------------------------------------

func TestTableStatsSQL_SchemaExclusion(t *testing.T) {
	expected := "NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')"
	if !strings.Contains(tableStatsSQL, expected) {
		t.Errorf("tableStatsSQL must contain schema exclusion: %s", expected)
	}
}

func TestIndexStatsSQL_SchemaExclusion(t *testing.T) {
	expected := "NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')"
	if !strings.Contains(indexStatsSQL, expected) {
		t.Errorf("indexStatsSQL must contain schema exclusion: %s", expected)
	}
}

func TestForeignKeysSQL_SchemaExclusion(t *testing.T) {
	expected := "NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')"
	if !strings.Contains(foreignKeysSQL, expected) {
		t.Errorf("foreignKeysSQL must contain schema exclusion: %s", expected)
	}
}

func TestPartitionSQL_SchemaExclusion(t *testing.T) {
	expected := "NOT IN ('sage', 'pg_catalog', 'information_schema', 'google_ml')"
	if !strings.Contains(partitionInheritanceSQL, expected) {
		t.Errorf("partitionInheritanceSQL must contain schema exclusion: %s", expected)
	}
}

// ---------------------------------------------------------------------------
// 8. Circuit breaker — basic state
// ---------------------------------------------------------------------------

func TestCircuitBreaker_SkipOnHighLoad(t *testing.T) {
	cb := NewCircuitBreaker(80, 5)

	t.Run("new breaker is not dormant", func(t *testing.T) {
		if cb.IsDormant() {
			t.Error("fresh circuit breaker should not be dormant")
		}
	})

	t.Run("after RecordSuccess, still not dormant", func(t *testing.T) {
		cb.RecordSuccess()
		if cb.IsDormant() {
			t.Error("breaker should not be dormant after a success")
		}
	})
}

// ---------------------------------------------------------------------------
// 9. Circuit breaker — dormant recovery
// ---------------------------------------------------------------------------

func TestCircuitBreaker_DormantRecovery(t *testing.T) {
	cb := NewCircuitBreaker(80, 1)

	// Force dormant state by directly setting internal fields.
	cb.mu.Lock()
	cb.isDormant = true
	cb.consecutiveSkips = 5
	cb.successCount = 0
	cb.mu.Unlock()

	if !cb.IsDormant() {
		t.Fatal("breaker should be dormant after manual set")
	}

	// First two successes should NOT exit dormant (need 3).
	cb.RecordSuccess()
	if !cb.IsDormant() {
		t.Error("breaker should still be dormant after 1 success")
	}

	cb.RecordSuccess()
	if !cb.IsDormant() {
		t.Error("breaker should still be dormant after 2 successes")
	}

	// Third success exits dormant.
	cb.RecordSuccess()
	if cb.IsDormant() {
		t.Error("breaker should exit dormant after 3 consecutive successes")
	}
}

// ---------------------------------------------------------------------------
// 10. Snapshot categories via JSON marshaling
// ---------------------------------------------------------------------------

func TestSnapshotCategories_PersistMap(t *testing.T) {
	snap := &Snapshot{
		CollectedAt: time.Now().UTC(),
		Queries:     []QueryStats{{QueryID: 1}},
		Tables:      []TableStats{{RelName: "t"}},
		Indexes:     []IndexStats{{IndexRelName: "i"}},
		ForeignKeys: []ForeignKey{{TableName: "fk"}},
		System:      SystemStats{ActiveBackends: 1},
		Locks:       []LockInfo{{LockType: "relation"}},
		Sequences:   []SequenceStats{{SequenceName: "s"}},
		Replication: &ReplicationStats{},
		IO:          []IOStats{{BackendType: "client backend"}},
		Partitions:  []PartitionInfo{{ChildTable: "p"}},
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("failed to marshal snapshot: %v", err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal to map: %v", err)
	}

	expectedKeys := []string{
		"CollectedAt", "Queries", "Tables", "Indexes",
		"ForeignKeys", "System", "Locks", "Sequences",
		"Replication", "io", "partitions",
	}
	for _, key := range expectedKeys {
		if _, ok := m[key]; !ok {
			t.Errorf("snapshot JSON missing expected key %q", key)
		}
	}
}

// ---------------------------------------------------------------------------
// 11. COALESCE usage in stats SQL
// ---------------------------------------------------------------------------

func TestCoalesceInSQL(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"tableStatsSQL", tableStatsSQL},
		{"indexStatsSQL", indexStatsSQL},
		{"sequencesSQL", sequencesSQL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.sql, "COALESCE") {
				t.Errorf("%s must use COALESCE on nullable numeric columns", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 12. detectStatsReset
// ---------------------------------------------------------------------------

func TestDetectStatsReset(t *testing.T) {
	t.Run("no previous returns false", func(t *testing.T) {
		current := []QueryStats{{QueryID: 1, Calls: 5}}
		if detectStatsReset(current, nil) {
			t.Error("expected false when previous is empty")
		}
	})

	t.Run("calls increased returns false", func(t *testing.T) {
		prev := []QueryStats{
			{QueryID: 1, Calls: 100},
			{QueryID: 2, Calls: 200},
		}
		cur := []QueryStats{
			{QueryID: 1, Calls: 150},
			{QueryID: 2, Calls: 250},
		}
		if detectStatsReset(cur, prev) {
			t.Error("expected false when all calls increased")
		}
	})

	t.Run("majority decreased returns true", func(t *testing.T) {
		prev := []QueryStats{
			{QueryID: 1, Calls: 1000},
			{QueryID: 2, Calls: 500},
		}
		cur := []QueryStats{
			{QueryID: 1, Calls: 5},
			{QueryID: 2, Calls: 3},
		}
		if !detectStatsReset(cur, prev) {
			t.Error("expected true when >50% calls decreased")
		}
	})

	t.Run("below threshold returns false", func(t *testing.T) {
		prev := []QueryStats{
			{QueryID: 1, Calls: 100},
			{QueryID: 2, Calls: 200},
			{QueryID: 3, Calls: 300},
			{QueryID: 4, Calls: 400},
		}
		// Only 1 of 4 decreased (25% < 50%).
		cur := []QueryStats{
			{QueryID: 1, Calls: 50},
			{QueryID: 2, Calls: 250},
			{QueryID: 3, Calls: 350},
			{QueryID: 4, Calls: 450},
		}
		if detectStatsReset(cur, prev) {
			t.Error("expected false when <50% decreased")
		}
	})

	t.Run("no overlap returns false", func(t *testing.T) {
		prev := []QueryStats{{QueryID: 1, Calls: 100}}
		cur := []QueryStats{{QueryID: 99, Calls: 5}}
		if detectStatsReset(cur, prev) {
			t.Error("expected false when no overlapping queries")
		}
	})
}

// ---------------------------------------------------------------------------
// 13. TableStats.IsUnlogged
// ---------------------------------------------------------------------------

func TestTableStats_IsUnlogged(t *testing.T) {
	tests := []struct {
		name           string
		relpersistence string
		want           bool
	}{
		{"permanent", "p", false},
		{"unlogged", "u", true},
		{"temp", "t", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := TableStats{Relpersistence: tt.relpersistence}
			if got := ts.IsUnlogged(); got != tt.want {
				t.Errorf("IsUnlogged() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 14. tableStatsSQL includes relpersistence
// ---------------------------------------------------------------------------

func TestTableStatsSQL_HasRelpersistence(t *testing.T) {
	if !strings.Contains(tableStatsSQL, "relpersistence") {
		t.Error("tableStatsSQL must select relpersistence from pg_class")
	}
}

// ---------------------------------------------------------------------------
// 15. Query SQL uses format placeholder for LIMIT
// ---------------------------------------------------------------------------

func TestQueryStatsSQL_HasLimitPlaceholder(t *testing.T) {
	variants := map[string]string{
		"queryStatsSQL":                   queryStatsSQL,
		"queryStatsWithWALSQL":            queryStatsWithWALSQL,
		"queryStatsWithPlanTimeSQL":        queryStatsWithPlanTimeSQL,
		"queryStatsWithWALAndPlanTimeSQL": queryStatsWithWALAndPlanTimeSQL,
	}
	for name, sql := range variants {
		if !strings.Contains(sql, "LIMIT %d") {
			t.Errorf("%s must contain 'LIMIT %%d' placeholder", name)
		}
	}
}
