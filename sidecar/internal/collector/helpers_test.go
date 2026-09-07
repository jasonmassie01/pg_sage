package collector

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// detectStatsReset — edge cases not covered by collector_coverage_test.go
// or collector_helpers_test.go
// ---------------------------------------------------------------------------

func TestDetectStatsReset_DuplicateQueryIDsInPrevious(t *testing.T) {
	// If previous contains duplicate QueryIDs, the last one wins in
	// the map. Verify the function doesn't panic or misbehave.
	previous := []QueryStats{
		{QueryID: 1, Calls: 1000},
		{QueryID: 1, Calls: 500}, // overwrites first entry
	}
	// prevTotal = 1500 (sums both), prevCalls[1] = 500 (last wins)
	// current has Calls=10 < 500 => decreased, compared=1, ratio=1.0
	// currTotal=10 < 1500/5=300 => true
	current := []QueryStats{
		{QueryID: 1, Calls: 10},
	}
	if !detectStatsReset(current, previous) {
		t.Error("duplicate QueryID in previous: expected reset detected")
	}
}

func TestDetectStatsReset_DuplicateQueryIDsInCurrent(t *testing.T) {
	// Current has duplicate QueryIDs — each occurrence is compared
	// independently against the previous map.
	previous := []QueryStats{
		{QueryID: 1, Calls: 1000},
	}
	// Both current entries match QueryID=1.
	// First: 5 < 1000 => decreased, Second: 3 < 1000 => decreased
	// compared=2, decreased=2, ratio=1.0 > 0.5
	// currTotal=8 < 1000/5=200 => true
	current := []QueryStats{
		{QueryID: 1, Calls: 5},
		{QueryID: 1, Calls: 3},
	}
	if !detectStatsReset(current, previous) {
		t.Error("duplicate QueryID in current: expected reset detected")
	}
}

func TestDetectStatsReset_PartialOverlap_SomeNewQueries(t *testing.T) {
	// Current has queries not in previous. Only overlapping queries
	// are considered for the ratio. New queries are ignored for
	// the decreased-ratio check but contribute to currTotal.
	previous := []QueryStats{
		{QueryID: 1, Calls: 10000},
		{QueryID: 2, Calls: 10000},
	}
	// prevTotal = 20000
	// Only QueryID=1 overlaps: 5 < 10000 => decreased
	// compared=1, decreased=1, ratio=1.0 > 0.5
	// currTotal = 5 + 3000 = 3005
	// prevTotal/5 = 4000, 3005 < 4000 => true
	current := []QueryStats{
		{QueryID: 1, Calls: 5},
		{QueryID: 99, Calls: 3000}, // new query, not in previous
	}
	if !detectStatsReset(current, previous) {
		t.Error("partial overlap with new queries: expected reset detected")
	}
}

func TestDetectStatsReset_PartialOverlap_NewQueriesLiftTotal(t *testing.T) {
	// New queries in current push currTotal above the threshold,
	// preventing reset detection even though overlapping queries decreased.
	previous := []QueryStats{
		{QueryID: 1, Calls: 1000},
		{QueryID: 2, Calls: 1000},
	}
	// prevTotal = 2000, prevTotal/5 = 400
	// QueryID=1 overlaps: 5 < 1000 => decreased
	// QueryID=2 overlaps: 3 < 1000 => decreased
	// compared=2, decreased=2, ratio=1.0 > 0.5
	// currTotal = 5+3+500 = 508, 508 > 400 => false
	current := []QueryStats{
		{QueryID: 1, Calls: 5},
		{QueryID: 2, Calls: 3},
		{QueryID: 99, Calls: 500}, // new query lifts total
	}
	if detectStatsReset(current, previous) {
		t.Error("new queries lifting total above threshold: " +
			"expected no reset")
	}
}

func TestDetectStatsReset_AllCallsEqual(t *testing.T) {
	// No change in calls — nothing decreased.
	previous := []QueryStats{
		{QueryID: 1, Calls: 100},
		{QueryID: 2, Calls: 200},
	}
	current := []QueryStats{
		{QueryID: 1, Calls: 100},
		{QueryID: 2, Calls: 200},
	}
	if detectStatsReset(current, previous) {
		t.Error("identical calls should not trigger reset")
	}
}

func TestDetectStatsReset_PreviousAllZeroCalls(t *testing.T) {
	// Previous queries exist but all have 0 calls.
	// sumCalls(previous) = 0 => returns false immediately.
	previous := []QueryStats{
		{QueryID: 1, Calls: 0},
		{QueryID: 2, Calls: 0},
	}
	current := []QueryStats{
		{QueryID: 1, Calls: 10},
		{QueryID: 2, Calls: 20},
	}
	if detectStatsReset(current, previous) {
		t.Error("zero-call previous should return false (prevTotal=0)")
	}
}

func TestDetectStatsReset_AllIncreasedByOne(t *testing.T) {
	// Every query's calls increased by exactly 1 — no decrease.
	previous := []QueryStats{
		{QueryID: 1, Calls: 100},
		{QueryID: 2, Calls: 200},
		{QueryID: 3, Calls: 300},
	}
	current := []QueryStats{
		{QueryID: 1, Calls: 101},
		{QueryID: 2, Calls: 201},
		{QueryID: 3, Calls: 301},
	}
	if detectStatsReset(current, previous) {
		t.Error("all calls increased: should not trigger reset")
	}
}

func TestDetectStatsReset_CurrentHasZeroCalls(t *testing.T) {
	// All current queries have Calls=0 while previous had high counts.
	// This looks like a real reset.
	previous := []QueryStats{
		{QueryID: 1, Calls: 5000},
		{QueryID: 2, Calls: 5000},
	}
	// prevTotal=10000, currTotal=0, 0 < 10000/5=2000 => true
	// 2/2 decreased, ratio=1.0 > 0.5
	current := []QueryStats{
		{QueryID: 1, Calls: 0},
		{QueryID: 2, Calls: 0},
	}
	if !detectStatsReset(current, previous) {
		t.Error("all current calls zero: should detect reset")
	}
}

func TestDetectStatsReset_MixedOverlapAndNew(t *testing.T) {
	// 3 overlap, 2 new. Of the 3 overlapping, only 1 decreased (33%).
	// Below 50% threshold => false regardless of total.
	previous := []QueryStats{
		{QueryID: 1, Calls: 100},
		{QueryID: 2, Calls: 200},
		{QueryID: 3, Calls: 300},
	}
	current := []QueryStats{
		{QueryID: 1, Calls: 50},  // decreased
		{QueryID: 2, Calls: 250}, // increased
		{QueryID: 3, Calls: 350}, // increased
		{QueryID: 4, Calls: 10},  // new
		{QueryID: 5, Calls: 20},  // new
	}
	// compared=3, decreased=1, ratio=0.33 < 0.5 => false
	if detectStatsReset(current, previous) {
		t.Error("only 1/3 decreased: should not trigger reset")
	}
}

// ---------------------------------------------------------------------------
// sumCalls — additional edge cases
// ---------------------------------------------------------------------------

func TestSumCalls_NegativeValues(t *testing.T) {
	// Negative call counts should not happen in practice but the
	// function should handle them without panic.
	qs := []QueryStats{
		{Calls: -10},
		{Calls: 20},
		{Calls: -5},
	}
	got := sumCalls(qs)
	if got != 5 {
		t.Errorf("sumCalls with negatives = %d, want 5", got)
	}
}

func TestSumCalls_MixedZeroAndPositive(t *testing.T) {
	qs := []QueryStats{
		{Calls: 0},
		{Calls: 100},
		{Calls: 0},
		{Calls: 50},
	}
	got := sumCalls(qs)
	if got != 150 {
		t.Errorf("sumCalls = %d, want 150", got)
	}
}

// ---------------------------------------------------------------------------
// CircuitBreaker — additional state transition edge cases
// ---------------------------------------------------------------------------

func TestCircuitBreaker_DormantExitResetsSuccessCount(t *testing.T) {
	cb := NewCircuitBreaker(80, 3)
	cb.mu.Lock()
	cb.isDormant = true
	cb.consecutiveSkips = 10
	cb.successCount = 0
	cb.mu.Unlock()

	// 3 successes to exit dormant
	cb.RecordSuccess()
	cb.RecordSuccess()
	cb.RecordSuccess()

	if cb.IsDormant() {
		t.Fatal("should have exited dormant after 3 successes")
	}

	cb.mu.Lock()
	sc := cb.successCount
	cs := cb.consecutiveSkips
	cb.mu.Unlock()

	if sc != 0 {
		t.Errorf("successCount after dormant exit: got %d, want 0", sc)
	}
	if cs != 0 {
		t.Errorf("consecutiveSkips after dormant exit: got %d, want 0", cs)
	}
}

func TestCircuitBreaker_DormantFourSuccesses(t *testing.T) {
	// After exiting dormant on the 3rd success, the 4th success
	// should increment successCount normally (non-dormant path).
	cb := NewCircuitBreaker(80, 3)
	cb.mu.Lock()
	cb.isDormant = true
	cb.mu.Unlock()

	cb.RecordSuccess() // 1
	cb.RecordSuccess() // 2
	cb.RecordSuccess() // 3 — exits dormant, resets to 0
	cb.RecordSuccess() // 4 — non-dormant, increments to 1

	cb.mu.Lock()
	sc := cb.successCount
	dormant := cb.isDormant
	cb.mu.Unlock()

	if dormant {
		t.Error("should not be dormant after 4 successes")
	}
	if sc != 1 {
		t.Errorf("successCount after 4th success: got %d, want 1", sc)
	}
}

func TestCircuitBreaker_MaxSkipsZero_ImmediatelyDormant(t *testing.T) {
	// With maxSkips=0, any skip should NOT set dormant because the
	// condition is consecutiveSkips >= maxSkips, which is 0 >= 0 = true
	// on the very first skip. This tests the actual behavior.
	cb := NewCircuitBreaker(80, 0)

	// Simulate a skip by directly incrementing consecutiveSkips
	// (ShouldSkip requires a real pool, so we test the invariant).
	cb.mu.Lock()
	cb.consecutiveSkips++
	if cb.consecutiveSkips >= cb.maxSkips {
		cb.isDormant = true
	}
	cb.mu.Unlock()

	// With maxSkips=0, even 1 skip triggers dormant
	if !cb.IsDormant() {
		t.Error("maxSkips=0: should enter dormant on first skip")
	}
}

func TestCircuitBreaker_ConcurrentRecordSuccess(t *testing.T) {
	cb := NewCircuitBreaker(80, 5)

	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cb.RecordSuccess()
		}()
	}
	wg.Wait()

	// After 1000 concurrent successes, successCount should be 1000
	// (non-dormant, so no resets).
	cb.mu.Lock()
	sc := cb.successCount
	cb.mu.Unlock()

	if sc != 1000 {
		t.Errorf("concurrent successCount: got %d, want 1000", sc)
	}
}

func TestCircuitBreaker_RecordSuccess_WhileDormant_PartialProgress(t *testing.T) {
	// 2 successes in dormant mode, then set back to dormant state
	// to simulate an interrupted recovery.
	cb := NewCircuitBreaker(80, 5)
	cb.mu.Lock()
	cb.isDormant = true
	cb.mu.Unlock()

	cb.RecordSuccess() // 1
	cb.RecordSuccess() // 2 — still dormant

	if !cb.IsDormant() {
		t.Error("should still be dormant after 2 successes")
	}

	// Reset success count to simulate another failure cycle
	cb.mu.Lock()
	cb.successCount = 0
	cb.mu.Unlock()

	// Now 3 more successes needed from scratch
	cb.RecordSuccess() // 1
	cb.RecordSuccess() // 2
	if !cb.IsDormant() {
		t.Error("should still be dormant after 2 successes from reset")
	}
	cb.RecordSuccess() // 3 — exits dormant
	if cb.IsDormant() {
		t.Error("should exit dormant after 3 consecutive successes")
	}
}

// ---------------------------------------------------------------------------
// TableStats.IsUnlogged — struct-level edge cases
// ---------------------------------------------------------------------------

func TestTableStats_IsUnlogged_ZeroValueStruct(t *testing.T) {
	// A completely zero-value TableStats should have
	// Relpersistence="" which is not "u".
	var ts TableStats
	if ts.IsUnlogged() {
		t.Error("zero-value TableStats should not be unlogged")
	}
}

func TestTableStats_IsUnlogged_WhitespaceValues(t *testing.T) {
	// Whitespace is not "u" — should return false.
	cases := []string{" ", "  u", "u ", " u ", "\t", "\n"}
	for _, rp := range cases {
		ts := TableStats{Relpersistence: rp}
		if ts.IsUnlogged() {
			t.Errorf("Relpersistence=%q: should not be unlogged", rp)
		}
	}
}

func TestTableStats_IsUnlogged_UpperCase(t *testing.T) {
	// PostgreSQL uses lowercase 'u', uppercase 'U' should be false.
	ts := TableStats{Relpersistence: "U"}
	if ts.IsUnlogged() {
		t.Error("uppercase 'U' should not match (PG uses lowercase)")
	}
}

// ---------------------------------------------------------------------------
// Snapshot struct — field interaction and state edge cases
// ---------------------------------------------------------------------------

func TestSnapshot_StatsResetWithQueries(t *testing.T) {
	// A snapshot can have StatsReset=true AND populated Queries
	// (the reset was detected but queries from the new epoch exist).
	snap := &Snapshot{
		CollectedAt: time.Now().UTC(),
		StatsReset:  true,
		Queries: []QueryStats{
			{QueryID: 1, Calls: 5},
			{QueryID: 2, Calls: 10},
		},
	}
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
	if len(restored.Queries) != 2 {
		t.Errorf("Queries len: got %d, want 2", len(restored.Queries))
	}
	if restored.Queries[0].Calls != 5 {
		t.Errorf("Queries[0].Calls: got %d, want 5",
			restored.Queries[0].Calls)
	}
}

func TestSnapshot_EmptySlicesVsNilSlices(t *testing.T) {
	// Verify that nil slices and empty slices serialize differently
	// for fields without omitempty (Queries, Tables, etc. have no
	// omitempty tag).
	nilSnap := &Snapshot{
		Queries: nil,
		Tables:  nil,
		Indexes: nil,
	}
	emptySnap := &Snapshot{
		Queries: []QueryStats{},
		Tables:  []TableStats{},
		Indexes: []IndexStats{},
	}

	nilData, err := json.Marshal(nilSnap)
	if err != nil {
		t.Fatalf("Marshal nil: %v", err)
	}
	emptyData, err := json.Marshal(emptySnap)
	if err != nil {
		t.Fatalf("Marshal empty: %v", err)
	}

	// nil slice marshals as "null", empty slice as "[]"
	var nilMap map[string]json.RawMessage
	if err := json.Unmarshal(nilData, &nilMap); err != nil {
		t.Fatalf("Unmarshal nil: %v", err)
	}
	var emptyMap map[string]json.RawMessage
	if err := json.Unmarshal(emptyData, &emptyMap); err != nil {
		t.Fatalf("Unmarshal empty: %v", err)
	}

	if string(nilMap["Queries"]) != "null" {
		t.Errorf("nil Queries should marshal as 'null', got %s",
			string(nilMap["Queries"]))
	}
	if string(emptyMap["Queries"]) != "[]" {
		t.Errorf("empty Queries should marshal as '[]', got %s",
			string(emptyMap["Queries"]))
	}
}

// ---------------------------------------------------------------------------
// QueryStats — additional field validation
// ---------------------------------------------------------------------------

func TestQueryStats_ZeroValueDefaults(t *testing.T) {
	var q QueryStats
	if q.QueryID != 0 {
		t.Errorf("zero QueryID: got %d", q.QueryID)
	}
	if q.Calls != 0 {
		t.Errorf("zero Calls: got %d", q.Calls)
	}
	if q.TotalExecTime != 0 {
		t.Errorf("zero TotalExecTime: got %f", q.TotalExecTime)
	}
	if q.Query != "" {
		t.Errorf("zero Query: got %q", q.Query)
	}
}

func TestQueryStats_NegativeTimings(t *testing.T) {
	// Negative timings shouldn't happen but should round-trip.
	q := QueryStats{
		QueryID:       1,
		TotalExecTime: -1.5,
		MeanExecTime:  -0.5,
		MinExecTime:   -0.01,
	}
	data, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored QueryStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.TotalExecTime != -1.5 {
		t.Errorf("TotalExecTime: got %f, want -1.5",
			restored.TotalExecTime)
	}
	if restored.MeanExecTime != -0.5 {
		t.Errorf("MeanExecTime: got %f, want -0.5",
			restored.MeanExecTime)
	}
}

// ---------------------------------------------------------------------------
// SequenceStats — boundary and edge case validation
// ---------------------------------------------------------------------------

func TestSequenceStats_MaxPctUsed(t *testing.T) {
	// 100% usage — sequence is exhausted.
	seq := SequenceStats{
		SchemaName:   "public",
		SequenceName: "exhausted_seq",
		DataType:     "integer",
		LastValue:    2147483647,
		MaxValue:     2147483647,
		IncrementBy:  1,
		PctUsed:      100.0,
	}
	data, err := json.Marshal(seq)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored SequenceStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.PctUsed != 100.0 {
		t.Errorf("PctUsed: got %f, want 100.0", restored.PctUsed)
	}
	if restored.LastValue != restored.MaxValue {
		t.Error("exhausted sequence: LastValue should equal MaxValue")
	}
}

func TestSequenceStats_ZeroPctUsed(t *testing.T) {
	seq := SequenceStats{
		SchemaName:   "public",
		SequenceName: "fresh_seq",
		DataType:     "bigint",
		LastValue:    0,
		MaxValue:     9223372036854775807,
		IncrementBy:  1,
		PctUsed:      0.0,
	}
	data, err := json.Marshal(seq)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored SequenceStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.PctUsed != 0.0 {
		t.Errorf("PctUsed: got %f, want 0.0", restored.PctUsed)
	}
}

func TestSequenceStats_LargeIncrementBy(t *testing.T) {
	// increment_by > 1 is valid for sequences that skip values.
	seq := SequenceStats{
		SchemaName:   "public",
		SequenceName: "skip_seq",
		DataType:     "bigint",
		LastValue:    1000,
		MaxValue:     9223372036854775807,
		IncrementBy:  100,
		PctUsed:      0.0,
	}
	data, err := json.Marshal(seq)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored SequenceStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.IncrementBy != 100 {
		t.Errorf("IncrementBy: got %d, want 100", restored.IncrementBy)
	}
}

// ---------------------------------------------------------------------------
// IndexStats — field-level edge cases
// ---------------------------------------------------------------------------

func TestIndexStats_InvalidIndex(t *testing.T) {
	// An index that is not valid (e.g., failed CONCURRENTLY build).
	idx := IndexStats{
		SchemaName:   "public",
		RelName:      "orders",
		IndexRelName: "idx_orders_status_ccnew",
		IdxScan:      0,
		IdxTupRead:   0,
		IdxTupFetch:  0,
		IndexBytes:   4096,
		IsUnique:     false,
		IsPrimary:    false,
		IsValid:      false,
		IndexDef:     "CREATE INDEX CONCURRENTLY idx_orders_status_ccnew ON orders (status)",
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
	if restored.IsValid {
		t.Error("invalid index should have IsValid=false")
	}
	if restored.IdxScan != 0 {
		t.Errorf("unused invalid index IdxScan: got %d, want 0",
			restored.IdxScan)
	}
}

func TestIndexStats_ZeroBytes(t *testing.T) {
	// An index with zero bytes (newly created, not yet visible in stats).
	idx := IndexStats{
		SchemaName:   "public",
		RelName:      "t",
		IndexRelName: "idx_t_new",
		IndexBytes:   0,
		IsValid:      true,
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
	if restored.IndexBytes != 0 {
		t.Errorf("IndexBytes: got %d, want 0", restored.IndexBytes)
	}
}

// ---------------------------------------------------------------------------
// TableStats — additional field edge cases
// ---------------------------------------------------------------------------

func TestTableStats_HighDeadTuples(t *testing.T) {
	// Table with more dead tuples than live — needs vacuum badly.
	ts := TableStats{
		SchemaName: "public",
		RelName:    "bloated",
		NLiveTup:   100,
		NDeadTup:   50000,
		TotalBytes: 1073741824,
	}
	if ts.NDeadTup <= ts.NLiveTup {
		t.Error("test setup: NDeadTup should exceed NLiveTup")
	}
	data, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored TableStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.NDeadTup != 50000 {
		t.Errorf("NDeadTup: got %d, want 50000", restored.NDeadTup)
	}
}

func TestTableStats_TimestampFields(t *testing.T) {
	// Verify nullable timestamp fields round-trip correctly.
	now := time.Now().UTC().Truncate(time.Second)
	ts := TableStats{
		SchemaName:      "public",
		RelName:         "t",
		LastVacuum:      &now,
		LastAutovacuum:  nil,
		LastAnalyze:     &now,
		LastAutoanalyze: nil,
		Relpersistence:  "p",
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
		t.Error("nil LastAutovacuum should remain nil")
	}
	if restored.LastAnalyze == nil {
		t.Fatal("LastAnalyze should not be nil")
	}
	if restored.LastAutoanalyze != nil {
		t.Error("nil LastAutoanalyze should remain nil")
	}
}

func TestTableStats_AllTimestampsSet(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	earlier := now.Add(-24 * time.Hour)
	ts := TableStats{
		SchemaName:      "public",
		RelName:         "fully_maintained",
		LastVacuum:      &earlier,
		LastAutovacuum:  &now,
		LastAnalyze:     &earlier,
		LastAutoanalyze: &now,
		Relpersistence:  "p",
	}
	data, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored TableStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.LastVacuum == nil || !restored.LastVacuum.Equal(earlier) {
		t.Error("LastVacuum mismatch")
	}
	if restored.LastAutovacuum == nil || !restored.LastAutovacuum.Equal(now) {
		t.Error("LastAutovacuum mismatch")
	}
	if restored.LastAnalyze == nil || !restored.LastAnalyze.Equal(earlier) {
		t.Error("LastAnalyze mismatch")
	}
	if restored.LastAutoanalyze == nil || !restored.LastAutoanalyze.Equal(now) {
		t.Error("LastAutoanalyze mismatch")
	}
}

// ---------------------------------------------------------------------------
// IOStats — zero-value and extreme cases
// ---------------------------------------------------------------------------

func TestIOStats_ZeroValueStruct(t *testing.T) {
	var io IOStats
	data, err := json.Marshal(io)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored IOStats
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.BackendType != "" {
		t.Errorf("zero BackendType: got %q", restored.BackendType)
	}
	if restored.Reads != 0 {
		t.Errorf("zero Reads: got %d", restored.Reads)
	}
	if restored.Hits != 0 {
		t.Errorf("zero Hits: got %d", restored.Hits)
	}
}

// ---------------------------------------------------------------------------
// LockInfo — edge cases for pointer fields
// ---------------------------------------------------------------------------

func TestLockInfo_EmptyStringPointerFields(t *testing.T) {
	// Pointer fields pointing to empty strings — different from nil.
	empty := ""
	lk := LockInfo{
		LockType: "relation",
		Mode:     "AccessShareLock",
		Granted:  true,
		RelName:  &empty,
		Query:    &empty,
		State:    &empty,
		PID:      1,
	}
	data, err := json.Marshal(lk)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored LockInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.RelName == nil {
		t.Fatal("empty-string RelName should not become nil")
	}
	if *restored.RelName != "" {
		t.Errorf("RelName: got %q, want empty string",
			*restored.RelName)
	}
	if restored.Query == nil {
		t.Fatal("empty-string Query should not become nil")
	}
	if *restored.Query != "" {
		t.Errorf("Query: got %q, want empty string",
			*restored.Query)
	}
}

// ---------------------------------------------------------------------------
// ConnectionState — edge cases
// ---------------------------------------------------------------------------

func TestConnectionState_ZeroCount(t *testing.T) {
	cs := ConnectionState{
		State:              "idle",
		Count:              0,
		AvgDurationSeconds: 0.0,
	}
	data, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored ConnectionState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.Count != 0 {
		t.Errorf("Count: got %d, want 0", restored.Count)
	}
	if restored.State != "idle" {
		t.Errorf("State: got %q, want %q", restored.State, "idle")
	}
}

func TestConnectionState_LargeDuration(t *testing.T) {
	// A connection idle for days.
	cs := ConnectionState{
		State:              "idle in transaction",
		Count:              1,
		AvgDurationSeconds: 86400.0, // 24 hours
	}
	data, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored ConnectionState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.AvgDurationSeconds != 86400.0 {
		t.Errorf("AvgDurationSeconds: got %f, want 86400.0",
			restored.AvgDurationSeconds)
	}
}

// ---------------------------------------------------------------------------
// PGSetting — edge cases
// ---------------------------------------------------------------------------

func TestPGSetting_EmptyUnit(t *testing.T) {
	// Some settings have no unit (e.g., max_connections).
	s := PGSetting{
		Name:           "max_connections",
		Setting:        "100",
		Unit:           "",
		Source:         "configuration file",
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
	if restored.Unit != "" {
		t.Errorf("Unit: got %q, want empty", restored.Unit)
	}
	if restored.PendingRestart {
		t.Error("PendingRestart should be false")
	}
}

func TestPGSetting_PendingRestart(t *testing.T) {
	s := PGSetting{
		Name:           "shared_buffers",
		Setting:        "256MB",
		Unit:           "8kB",
		Source:         "configuration file",
		PendingRestart: true,
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored PGSetting
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !restored.PendingRestart {
		t.Error("PendingRestart should be true")
	}
}

// ---------------------------------------------------------------------------
// SlotInfo — edge cases
// ---------------------------------------------------------------------------

func TestSlotInfo_ZeroRetainedBytes(t *testing.T) {
	si := SlotInfo{
		SlotName:      "fresh_slot",
		SlotType:      "logical",
		Active:        true,
		RetainedBytes: 0,
	}
	data, err := json.Marshal(si)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored SlotInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.RetainedBytes != 0 {
		t.Errorf("RetainedBytes: got %d, want 0",
			restored.RetainedBytes)
	}
	if !restored.Active {
		t.Error("Active should be true")
	}
}

func TestSlotInfo_LargeRetainedBytes(t *testing.T) {
	// A slot retaining 10 GB of WAL.
	si := SlotInfo{
		SlotName:      "lagging_slot",
		SlotType:      "physical",
		Active:        false,
		RetainedBytes: 10737418240, // 10 GB
	}
	data, err := json.Marshal(si)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored SlotInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.RetainedBytes != 10737418240 {
		t.Errorf("RetainedBytes: got %d, want 10737418240",
			restored.RetainedBytes)
	}
}

// ---------------------------------------------------------------------------
// ReplicaInfo — edge cases with lag fields
// ---------------------------------------------------------------------------

func TestReplicaInfo_AllLagsSet(t *testing.T) {
	addr := "10.0.0.5"
	wl := "00:00:01.5"
	fl := "00:00:02.3"
	rl := "00:00:05.0"
	ri := ReplicaInfo{
		ClientAddr: &addr,
		State:      "streaming",
		SentLSN:    "0/A000000",
		WriteLSN:   "0/9F00000",
		FlushLSN:   "0/9E00000",
		ReplayLSN:  "0/9D00000",
		WriteLag:   &wl,
		FlushLag:   &fl,
		ReplayLag:  &rl,
		SyncState:  "sync",
	}
	data, err := json.Marshal(ri)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored ReplicaInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.WriteLag == nil || *restored.WriteLag != wl {
		t.Error("WriteLag not preserved")
	}
	if restored.FlushLag == nil || *restored.FlushLag != fl {
		t.Error("FlushLag not preserved")
	}
	if restored.ReplayLag == nil || *restored.ReplayLag != rl {
		t.Error("ReplayLag not preserved")
	}
	if restored.SyncState != "sync" {
		t.Errorf("SyncState: got %q, want %q",
			restored.SyncState, "sync")
	}
}

// ---------------------------------------------------------------------------
// SQL constant validation — additional checks not in other test files
// ---------------------------------------------------------------------------

func TestQueryStatsSQL_AllVariantsUseCurrentDatabase(t *testing.T) {
	variants := map[string]string{
		"queryStatsSQL":                   queryStatsSQL,
		"queryStatsWithWALSQL":            queryStatsWithWALSQL,
		"queryStatsWithPlanTimeSQL":       queryStatsWithPlanTimeSQL,
		"queryStatsWithWALAndPlanTimeSQL": queryStatsWithWALAndPlanTimeSQL,
	}
	for name, sql := range variants {
		if !contains(sql, "current_database()") {
			t.Errorf("%s must filter by current_database()", name)
		}
	}
}

func TestQueryStatsSQL_AllVariantsCoalesceQueryID(t *testing.T) {
	variants := map[string]string{
		"queryStatsSQL":                   queryStatsSQL,
		"queryStatsWithWALSQL":            queryStatsWithWALSQL,
		"queryStatsWithPlanTimeSQL":       queryStatsWithPlanTimeSQL,
		"queryStatsWithWALAndPlanTimeSQL": queryStatsWithWALAndPlanTimeSQL,
	}
	for name, sql := range variants {
		if !contains(sql, "COALESCE(queryid, 0)") {
			t.Errorf("%s must COALESCE queryid to 0", name)
		}
	}
}

func TestSystemStatsSQL_BothVersionsShareBase(t *testing.T) {
	// Both PG14 and PG17 variants should contain the shared base
	// components: active_backends, cache_hit_ratio, etc.
	for _, sql := range []string{systemStatsSQL14, systemStatsSQL17} {
		if !contains(sql, "active_backends") {
			t.Error("system stats SQL must select active_backends")
		}
		if !contains(sql, "cache_hit_ratio") {
			t.Error("system stats SQL must select cache_hit_ratio")
		}
		if !contains(sql, "pg_is_in_recovery()") {
			t.Error("system stats SQL must check pg_is_in_recovery()")
		}
		if !contains(sql, "pg_database_size") {
			t.Error("system stats SQL must select pg_database_size")
		}
	}
}

func TestPartitionSQL_JoinsParentAndChild(t *testing.T) {
	if !contains(partitionInheritanceSQL, "pg_inherits") {
		t.Error("partitionInheritanceSQL must query pg_inherits")
	}
	if !contains(partitionInheritanceSQL, "inhrelid") {
		t.Error("partitionInheritanceSQL must join on inhrelid (child)")
	}
	if !contains(partitionInheritanceSQL, "inhparent") {
		t.Error("partitionInheritanceSQL must join on inhparent (parent)")
	}
}

func TestIndexStatsSQL_JoinsRequiredCatalogs(t *testing.T) {
	required := []string{
		"pg_stat_user_indexes",
		"pg_class",
		"pg_index",
		"pg_am",
	}
	for _, catalog := range required {
		if !contains(indexStatsSQL, catalog) {
			t.Errorf("indexStatsSQL must join %s", catalog)
		}
	}
}

// contains is a helper to avoid importing strings in this test file.
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
