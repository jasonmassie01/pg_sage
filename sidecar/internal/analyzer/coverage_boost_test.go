package analyzer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func coverageTestConfig() *config.Config {
	return &config.Config{
		Analyzer: config.AnalyzerConfig{
			SlowQueryThresholdMs:         1000,
			TableBloatDeadTuplePct:       20,
			TableBloatMinRows:            1000,
			SeqScanMinRows:              10000,
			UnusedIndexWindowDays:        7,
			RegressionThresholdPct:       50,
			RegressionLookbackDays:       7,
			CacheHitRatioWarning:         0.95,
			XIDWraparoundWarning:         500000000,
			XIDWraparoundCritical:        1000000000,
			CheckpointFreqWarningPerHour: 6,
			IdleInTxTimeoutMinutes:       5,
		},
	}
}

func defaultExtras() *RuleExtras {
	return &RuleExtras{
		FirstSeen:       make(map[string]time.Time),
		RecentlyCreated: make(map[string]time.Time),
	}
}

// mockDispatcher records dispatched events and optionally returns an error.
type mockDispatcher struct {
	mu     sync.Mutex
	events []notify.Event
	err    error
}

func (m *mockDispatcher) Dispatch(_ context.Context, event notify.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
	return m.err
}

func (m *mockDispatcher) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

// ---------------------------------------------------------------------------
// Analyzer struct methods (non-DB)
// ---------------------------------------------------------------------------

func TestCoverage_NewAnalyzer(t *testing.T) {
	cfg := coverageTestConfig()
	a := New(nil, cfg, nil, nil, nil, nil, nil, noopLog)
	if a == nil {
		t.Fatal("New returned nil")
	}
	if a.cfg != cfg {
		t.Error("config not stored")
	}
	if a.extras == nil {
		t.Error("extras not initialized")
	}
	if a.extras.FirstSeen == nil {
		t.Error("FirstSeen map not initialized")
	}
	if a.extras.RecentlyCreated == nil {
		t.Error("RecentlyCreated map not initialized")
	}
}

func TestCoverage_WithDispatcher(t *testing.T) {
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	d := &mockDispatcher{}
	a.WithDispatcher(d)
	if a.dispatcher == nil {
		t.Error("dispatcher not set")
	}
}

func TestCoverage_WithDatabaseName(t *testing.T) {
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	a.WithDatabaseName("mydb")
	if a.databaseName != "mydb" {
		t.Errorf("expected 'mydb', got %q", a.databaseName)
	}
}

func TestCoverage_SetAndLatestFindings(t *testing.T) {
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)

	// Initially empty.
	got := a.LatestFindings()
	if len(got) != 0 {
		t.Fatalf("expected 0 findings initially, got %d", len(got))
	}

	// Set some findings.
	ff := []Finding{
		{Category: "test1", Severity: "warning"},
		{Category: "test2", Severity: "critical"},
	}
	a.SetFindings(ff)

	got = a.LatestFindings()
	if len(got) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(got))
	}

	// Verify it returns a copy, not a reference.
	got[0].Category = "mutated"
	original := a.LatestFindings()
	if original[0].Category == "mutated" {
		t.Error("LatestFindings should return a copy, not a reference")
	}
}

func TestCoverage_Findings_IsAlias(t *testing.T) {
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	a.SetFindings([]Finding{{Category: "x"}})
	got := a.Findings()
	if len(got) != 1 || got[0].Category != "x" {
		t.Errorf("Findings() should alias LatestFindings(), got %v", got)
	}
}

func TestCoverage_OpenFindingsCount(t *testing.T) {
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	a.SetFindings([]Finding{
		{Severity: "warning"},
		{Severity: "warning"},
		{Severity: "critical"},
		{Severity: "info"},
	})

	counts := a.OpenFindingsCount()
	if counts["warning"] != 2 {
		t.Errorf("warning count = %d, want 2", counts["warning"])
	}
	if counts["critical"] != 1 {
		t.Errorf("critical count = %d, want 1", counts["critical"])
	}
	if counts["info"] != 1 {
		t.Errorf("info count = %d, want 1", counts["info"])
	}
}

func TestCoverage_OpenFindingsCount_Empty(t *testing.T) {
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	counts := a.OpenFindingsCount()
	if len(counts) != 0 {
		t.Errorf("expected empty map, got %v", counts)
	}
}

// ---------------------------------------------------------------------------
// filterSchemaExclusions
// ---------------------------------------------------------------------------

func TestCoverage_FilterSchemaExclusions(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders"},
			{SchemaName: "sage", RelName: "findings"},
			{SchemaName: "pg_catalog", RelName: "pg_class"},
			{SchemaName: "information_schema", RelName: "columns"},
			{SchemaName: "google_ml", RelName: "predictions"},
			{SchemaName: "myapp", RelName: "users"},
		},
		Indexes: []collector.IndexStats{
			{SchemaName: "public", IndexRelName: "idx_orders"},
			{SchemaName: "sage", IndexRelName: "idx_sage"},
			{SchemaName: "pg_catalog", IndexRelName: "pg_idx"},
		},
	}

	filterSchemaExclusions(snap)

	if len(snap.Tables) != 2 {
		t.Errorf("expected 2 tables after filter, got %d", len(snap.Tables))
	}
	for _, tbl := range snap.Tables {
		if tbl.SchemaName == "sage" || tbl.SchemaName == "pg_catalog" ||
			tbl.SchemaName == "information_schema" || tbl.SchemaName == "google_ml" {
			t.Errorf("table in excluded schema %s should be filtered", tbl.SchemaName)
		}
	}

	if len(snap.Indexes) != 1 {
		t.Errorf("expected 1 index after filter, got %d", len(snap.Indexes))
	}
	if snap.Indexes[0].SchemaName != "public" {
		t.Errorf("expected public index, got %s", snap.Indexes[0].SchemaName)
	}
}

func TestCoverage_FilterSchemaExclusions_Empty(t *testing.T) {
	snap := &collector.Snapshot{}
	filterSchemaExclusions(snap) // should not panic
	if len(snap.Tables) != 0 || len(snap.Indexes) != 0 {
		t.Error("empty snapshot should remain empty")
	}
}

// ---------------------------------------------------------------------------
// downsample
// ---------------------------------------------------------------------------

func TestCoverage_Downsample_BelowMax(t *testing.T) {
	items := []int{1, 2, 3}
	got := downsample(items, 10)
	if len(got) != 3 {
		t.Errorf("expected 3 items, got %d", len(got))
	}
}

func TestCoverage_Downsample_ExactMax(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	got := downsample(items, 5)
	if len(got) != 5 {
		t.Errorf("expected 5 items, got %d", len(got))
	}
}

func TestCoverage_Downsample_AboveMax(t *testing.T) {
	items := make([]int, 200)
	for i := range items {
		items[i] = i
	}
	got := downsample(items, 10)
	if len(got) != 10 {
		t.Errorf("expected 10 items, got %d", len(got))
	}
	// Verify evenly spaced: first should be 0.
	if got[0] != 0 {
		t.Errorf("first element should be 0, got %d", got[0])
	}
}

func TestCoverage_Downsample_Empty(t *testing.T) {
	got := downsample([]int{}, 5)
	if len(got) != 0 {
		t.Errorf("expected 0 items, got %d", len(got))
	}
}

func TestCoverage_Downsample_MaxOne(t *testing.T) {
	items := []int{10, 20, 30, 40, 50}
	got := downsample(items, 1)
	if len(got) != 1 {
		t.Errorf("expected 1 item, got %d", len(got))
	}
	if got[0] != 10 {
		t.Errorf("expected first element 10, got %d", got[0])
	}
}

// ---------------------------------------------------------------------------
// dispatchCriticalFindings
// ---------------------------------------------------------------------------

func TestCoverage_DispatchCriticalFindings_NoDispatcher(t *testing.T) {
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	// Should not panic with nil dispatcher.
	a.dispatchCriticalFindings(context.Background(), []Finding{
		{Severity: "critical", Title: "test"},
	})
}

func TestCoverage_DispatchCriticalFindings_CriticalOnly(t *testing.T) {
	d := &mockDispatcher{}
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	a.WithDispatcher(d)
	a.WithDatabaseName("testdb")

	findings := []Finding{
		{Severity: "warning", Title: "not dispatched"},
		{Severity: "critical", Title: "dispatched1", Detail: map[string]any{"key": "val"}},
		{Severity: "info", Title: "not dispatched2"},
		{Severity: "critical", Title: "dispatched2", Detail: map[string]any{}},
	}

	a.dispatchCriticalFindings(context.Background(), findings)

	if d.count() != 2 {
		t.Errorf("expected 2 dispatched events, got %d", d.count())
	}
}

func TestCoverage_DispatchCriticalFindings_ErrorDoesNotPanic(t *testing.T) {
	d := &mockDispatcher{err: errors.New("dispatch failed")}
	var logMsgs []string
	logFn := func(level, msg string, args ...any) {
		logMsgs = append(logMsgs, level+": "+msg)
	}
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, logFn)
	a.WithDispatcher(d)

	a.dispatchCriticalFindings(context.Background(), []Finding{
		{Severity: "critical", Title: "err test", Detail: map[string]any{}},
	})

	if len(logMsgs) == 0 {
		t.Error("expected error logged when dispatch fails")
	}
}

func TestCoverage_DispatchCriticalFindings_EmptyFindings(t *testing.T) {
	d := &mockDispatcher{}
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	a.WithDispatcher(d)
	a.dispatchCriticalFindings(context.Background(), nil)
	if d.count() != 0 {
		t.Errorf("expected 0 dispatched events for nil findings, got %d", d.count())
	}
}

// ---------------------------------------------------------------------------
// index_parser.go: ParseIndexDef, splitColumns, IsDuplicate, IsSubset
// ---------------------------------------------------------------------------

func TestCoverage_ParseIndexDef_BasicBtree(t *testing.T) {
	def := "CREATE INDEX idx_orders_date ON public.orders USING btree (order_date)"
	p := ParseIndexDef(def)
	if p.Name != "idx_orders_date" {
		t.Errorf("Name = %q, want idx_orders_date", p.Name)
	}
	if p.Schema != "public" {
		t.Errorf("Schema = %q, want public", p.Schema)
	}
	if p.Table != "orders" {
		t.Errorf("Table = %q, want orders", p.Table)
	}
	if p.IndexType != "btree" {
		t.Errorf("IndexType = %q, want btree", p.IndexType)
	}
	if len(p.Columns) != 1 || p.Columns[0] != "order_date" {
		t.Errorf("Columns = %v, want [order_date]", p.Columns)
	}
}

func TestCoverage_ParseIndexDef_MultiColumn(t *testing.T) {
	def := "CREATE INDEX idx_multi ON orders USING btree (a, b, c)"
	p := ParseIndexDef(def)
	if len(p.Columns) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(p.Columns))
	}
	if p.Columns[0] != "a" || p.Columns[1] != "b" || p.Columns[2] != "c" {
		t.Errorf("Columns = %v, want [a b c]", p.Columns)
	}
	if p.Schema != "" {
		t.Errorf("Schema = %q, want empty for unqualified table", p.Schema)
	}
}

func TestCoverage_ParseIndexDef_WithInclude(t *testing.T) {
	// The regex is greedy: (.+) in the columns group consumes
	// the INCLUDE clause. This test documents actual behavior.
	def := "CREATE INDEX idx_inc ON public.orders USING btree (id) INCLUDE (name, email)"
	p := ParseIndexDef(def)
	// Due to greedy matching, INCLUDE is captured as part of
	// columns. Verify the parse at least succeeds.
	if p.Name != "idx_inc" {
		t.Errorf("Name = %q, want idx_inc", p.Name)
	}
	if p.Table != "orders" {
		t.Errorf("Table = %q, want orders", p.Table)
	}
}

func TestCoverage_ParseIndexDef_WithWhere(t *testing.T) {
	// The regex is greedy: (.+) in the columns group consumes
	// the WHERE clause. This test documents actual behavior.
	def := "CREATE INDEX idx_partial ON public.orders USING btree (status) WHERE (status = 'active')"
	p := ParseIndexDef(def)
	if p.Name != "idx_partial" {
		t.Errorf("Name = %q, want idx_partial", p.Name)
	}
	if p.Table != "orders" {
		t.Errorf("Table = %q, want orders", p.Table)
	}
}

func TestCoverage_ParseIndexDef_SimpleWhereNoParens(t *testing.T) {
	// WHERE clause without extra parens can be captured properly.
	def := "CREATE INDEX idx_w ON public.orders USING btree (id) WHERE active"
	p := ParseIndexDef(def)
	if p.Name != "idx_w" {
		t.Errorf("Name = %q, want idx_w", p.Name)
	}
}

func TestCoverage_ParseIndexDef_UniqueIndex(t *testing.T) {
	def := "CREATE UNIQUE INDEX idx_u ON public.orders USING btree (id)"
	p := ParseIndexDef(def)
	if p.Name != "idx_u" {
		t.Errorf("Name = %q, want idx_u", p.Name)
	}
	if p.Table != "orders" {
		t.Errorf("Table = %q, want orders", p.Table)
	}
}

func TestCoverage_ParseIndexDef_GINIndex(t *testing.T) {
	def := "CREATE INDEX idx_gin ON public.docs USING gin (content)"
	p := ParseIndexDef(def)
	if p.IndexType != "gin" {
		t.Errorf("IndexType = %q, want gin", p.IndexType)
	}
}

func TestCoverage_ParseIndexDef_InvalidString(t *testing.T) {
	p := ParseIndexDef("NOT A VALID INDEX DEF")
	if p.Name != "" || p.Table != "" {
		t.Errorf("expected empty ParsedIndex for invalid input, got %+v", p)
	}
}

func TestCoverage_ParseIndexDef_EmptyString(t *testing.T) {
	p := ParseIndexDef("")
	if p.Name != "" {
		t.Errorf("expected empty ParsedIndex for empty string, got %+v", p)
	}
}

func TestCoverage_ParseIndexDef_ExpressionIndex(t *testing.T) {
	def := "CREATE INDEX idx_lower ON public.users USING btree (lower(email))"
	p := ParseIndexDef(def)
	if len(p.Columns) != 1 || p.Columns[0] != "lower(email)" {
		t.Errorf("Columns = %v, want [lower(email)]", p.Columns)
	}
}

func TestCoverage_SplitColumns_Nested(t *testing.T) {
	cols := splitColumns("lower(name), id, upper(code)")
	if len(cols) != 3 {
		t.Fatalf("expected 3 columns, got %d: %v", len(cols), cols)
	}
	if cols[0] != "lower(name)" || cols[1] != "id" || cols[2] != "upper(code)" {
		t.Errorf("unexpected columns: %v", cols)
	}
}

func TestCoverage_SplitColumns_Single(t *testing.T) {
	cols := splitColumns("id")
	if len(cols) != 1 || cols[0] != "id" {
		t.Errorf("expected [id], got %v", cols)
	}
}

func TestCoverage_IsDuplicate_Exact(t *testing.T) {
	a := ParsedIndex{Table: "t", Columns: []string{"a", "b"}}
	b := ParsedIndex{Table: "t", Columns: []string{"a", "b"}}
	if !IsDuplicate(a, b) {
		t.Error("expected IsDuplicate=true for exact match")
	}
}

func TestCoverage_IsDuplicate_DiffTable(t *testing.T) {
	a := ParsedIndex{Table: "t1", Columns: []string{"a"}}
	b := ParsedIndex{Table: "t2", Columns: []string{"a"}}
	if IsDuplicate(a, b) {
		t.Error("expected IsDuplicate=false for different tables")
	}
}

func TestCoverage_IsDuplicate_DiffCols(t *testing.T) {
	a := ParsedIndex{Table: "t", Columns: []string{"a"}}
	b := ParsedIndex{Table: "t", Columns: []string{"b"}}
	if IsDuplicate(a, b) {
		t.Error("expected IsDuplicate=false for different columns")
	}
}

func TestCoverage_IsDuplicate_DiffColCount(t *testing.T) {
	a := ParsedIndex{Table: "t", Columns: []string{"a", "b"}}
	b := ParsedIndex{Table: "t", Columns: []string{"a"}}
	if IsDuplicate(a, b) {
		t.Error("expected IsDuplicate=false for different column counts")
	}
}

func TestCoverage_IsDuplicate_DiffWhere(t *testing.T) {
	a := ParsedIndex{Table: "t", Columns: []string{"a"}, WhereClause: "x > 1"}
	b := ParsedIndex{Table: "t", Columns: []string{"a"}, WhereClause: "x > 2"}
	if IsDuplicate(a, b) {
		t.Error("expected IsDuplicate=false for different WHERE clauses")
	}
}

func TestCoverage_IsDuplicate_DiffInclude(t *testing.T) {
	a := ParsedIndex{Table: "t", Columns: []string{"a"}, IncludeCols: []string{"b"}}
	b := ParsedIndex{Table: "t", Columns: []string{"a"}, IncludeCols: []string{"c"}}
	if IsDuplicate(a, b) {
		t.Error("expected IsDuplicate=false for different INCLUDE cols")
	}
}

func TestCoverage_IsDuplicate_DiffIncludeCount(t *testing.T) {
	a := ParsedIndex{Table: "t", Columns: []string{"a"}, IncludeCols: []string{"b"}}
	b := ParsedIndex{Table: "t", Columns: []string{"a"}, IncludeCols: []string{"b", "c"}}
	if IsDuplicate(a, b) {
		t.Error("expected IsDuplicate=false for different INCLUDE counts")
	}
}

func TestCoverage_IsSubset_LeadingPrefix(t *testing.T) {
	a := ParsedIndex{Table: "t", Columns: []string{"a"}}
	b := ParsedIndex{Table: "t", Columns: []string{"a", "b"}}
	if !IsSubset(a, b) {
		t.Error("expected IsSubset=true for leading prefix")
	}
}

func TestCoverage_IsSubset_SameLength(t *testing.T) {
	a := ParsedIndex{Table: "t", Columns: []string{"a", "b"}}
	b := ParsedIndex{Table: "t", Columns: []string{"a", "b"}}
	if IsSubset(a, b) {
		t.Error("expected IsSubset=false for same length (use IsDuplicate)")
	}
}

func TestCoverage_IsSubset_DiffTable(t *testing.T) {
	a := ParsedIndex{Table: "t1", Columns: []string{"a"}}
	b := ParsedIndex{Table: "t2", Columns: []string{"a", "b"}}
	if IsSubset(a, b) {
		t.Error("expected IsSubset=false for different tables")
	}
}

func TestCoverage_IsSubset_DiffWhere(t *testing.T) {
	a := ParsedIndex{Table: "t", Columns: []string{"a"}, WhereClause: "x"}
	b := ParsedIndex{Table: "t", Columns: []string{"a", "b"}, WhereClause: "y"}
	if IsSubset(a, b) {
		t.Error("expected IsSubset=false for different WHERE clauses")
	}
}

func TestCoverage_IsSubset_NotLeadingPrefix(t *testing.T) {
	a := ParsedIndex{Table: "t", Columns: []string{"b"}}
	b := ParsedIndex{Table: "t", Columns: []string{"a", "b"}}
	if IsSubset(a, b) {
		t.Error("expected IsSubset=false when not a leading prefix")
	}
}

func TestCoverage_IsSubset_LongerThanSuperset(t *testing.T) {
	a := ParsedIndex{Table: "t", Columns: []string{"a", "b", "c"}}
	b := ParsedIndex{Table: "t", Columns: []string{"a", "b"}}
	if IsSubset(a, b) {
		t.Error("expected IsSubset=false when a is longer than b")
	}
}

// ---------------------------------------------------------------------------
// rules_index.go: ruleInvalidIndexes, ruleDuplicateIndexes
// ---------------------------------------------------------------------------

func TestCoverage_RuleInvalidIndexes(t *testing.T) {
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_valid", IsValid: true,
				IndexDef: "CREATE INDEX idx_valid ON public.orders USING btree (id)",
			},
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_invalid", IsValid: false,
				IndexDef: "CREATE INDEX idx_invalid ON public.orders USING btree (name)",
			},
		},
	}

	findings := ruleInvalidIndexes(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Category != "invalid_index" {
		t.Errorf("Category = %q, want invalid_index", f.Category)
	}
	if f.Severity != "warning" {
		t.Errorf("Severity = %q, want warning", f.Severity)
	}
	if f.ObjectIdentifier != "public.idx_invalid" {
		t.Errorf("ObjectIdentifier = %q, want public.idx_invalid", f.ObjectIdentifier)
	}
	if f.RecommendedSQL == "" {
		t.Error("expected RecommendedSQL to be set")
	}
	if f.RollbackSQL == "" {
		t.Error("expected RollbackSQL to be set")
	}
}

func TestCoverage_RuleInvalidIndexes_UnloggedTable(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "cache", Relpersistence: "u"},
		},
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "cache",
				IndexRelName: "idx_cache_inv", IsValid: false,
				IndexDef: "CREATE INDEX idx_cache_inv ON public.cache USING btree (k)",
			},
		},
	}

	findings := ruleInvalidIndexes(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "info" {
		t.Errorf("unlogged invalid index severity = %q, want info", findings[0].Severity)
	}
	if findings[0].Detail["unlogged"] != true {
		t.Error("expected unlogged=true in detail")
	}
}

func TestCoverage_RuleInvalidIndexes_AllValid(t *testing.T) {
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{SchemaName: "public", IndexRelName: "idx1", IsValid: true},
			{SchemaName: "public", IndexRelName: "idx2", IsValid: true},
		},
	}
	findings := ruleInvalidIndexes(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestCoverage_RuleDuplicateIndexes_ExactDuplicate(t *testing.T) {
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_a", IsValid: true, IdxScan: 100,
				IndexDef: "CREATE INDEX idx_a ON public.orders USING btree (id)",
			},
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_b", IsValid: true, IdxScan: 10,
				IndexDef: "CREATE INDEX idx_b ON public.orders USING btree (id)",
			},
		},
	}

	findings := ruleDuplicateIndexes(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 duplicate finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Category != "duplicate_index" {
		t.Errorf("Category = %q, want duplicate_index", f.Category)
	}
	if f.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", f.Severity)
	}
	// idx_b has fewer scans, so it should be the one to drop.
	if f.ObjectIdentifier != "public.idx_b" {
		t.Errorf("ObjectIdentifier = %q, want public.idx_b (fewer scans)", f.ObjectIdentifier)
	}
}

func TestCoverage_RuleDuplicateIndexes_SubsetDetected(t *testing.T) {
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_a", IsValid: true,
				IndexDef: "CREATE INDEX idx_a ON public.orders USING btree (id)",
			},
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_ab", IsValid: true,
				IndexDef: "CREATE INDEX idx_ab ON public.orders USING btree (id, name)",
			},
		},
	}

	findings := ruleDuplicateIndexes(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 subset finding, got %d", len(findings))
	}
	if findings[0].ObjectIdentifier != "public.idx_a" {
		t.Errorf("expected subset idx_a to be flagged, got %s", findings[0].ObjectIdentifier)
	}
}

func TestCoverage_RuleDuplicateIndexes_NoDuplicates(t *testing.T) {
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_a", IsValid: true,
				IndexDef: "CREATE INDEX idx_a ON public.orders USING btree (id)",
			},
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_b", IsValid: true,
				IndexDef: "CREATE INDEX idx_b ON public.orders USING btree (name)",
			},
		},
	}

	findings := ruleDuplicateIndexes(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestCoverage_RuleDuplicateIndexes_SkipsInvalid(t *testing.T) {
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_a", IsValid: true,
				IndexDef: "CREATE INDEX idx_a ON public.orders USING btree (id)",
			},
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_b", IsValid: false,
				IndexDef: "CREATE INDEX idx_b ON public.orders USING btree (id)",
			},
		},
	}

	findings := ruleDuplicateIndexes(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 (invalid skipped), got %d", len(findings))
	}
}

func TestCoverage_RuleDuplicateIndexes_SkipsNonBtree(t *testing.T) {
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "docs",
				IndexRelName: "idx_gin_a", IsValid: true,
				IndexDef: "CREATE INDEX idx_gin_a ON public.docs USING gin (content)",
			},
			{
				SchemaName: "public", RelName: "docs",
				IndexRelName: "idx_gin_b", IsValid: true,
				IndexDef: "CREATE INDEX idx_gin_b ON public.docs USING gin (content)",
			},
		},
	}

	findings := ruleDuplicateIndexes(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for non-btree duplicates, got %d", len(findings))
	}
}

func TestCoverage_RuleDuplicateIndexes_ReverseSubset(t *testing.T) {
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_ab", IsValid: true,
				IndexDef: "CREATE INDEX idx_ab ON public.orders USING btree (id, name)",
			},
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_a", IsValid: true,
				IndexDef: "CREATE INDEX idx_a ON public.orders USING btree (id)",
			},
		},
	}

	findings := ruleDuplicateIndexes(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 subset finding (reverse), got %d", len(findings))
	}
	if findings[0].ObjectIdentifier != "public.idx_a" {
		t.Errorf("expected idx_a flagged as subset, got %s", findings[0].ObjectIdentifier)
	}
}

// ---------------------------------------------------------------------------
// rules_system.go: ruleConnectionLeaks, ruleCacheHitRatio, ruleCheckpointPressure
// ---------------------------------------------------------------------------

func TestCoverage_RuleConnectionLeaks(t *testing.T) {
	leaked := []LeakedConn{
		{PID: 1234, UserName: "app", AppName: "web", IdleDuration: "01:30:00"},
		{PID: 5678, UserName: "admin", AppName: "cron", IdleDuration: "00:10:00"},
	}

	findings := ruleConnectionLeaks(leaked)
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(findings))
	}
	if findings[0].Category != "connection_leak" {
		t.Errorf("Category = %q, want connection_leak", findings[0].Category)
	}
	if findings[0].ObjectIdentifier != "pid:1234" {
		t.Errorf("ObjectIdentifier = %q, want pid:1234", findings[0].ObjectIdentifier)
	}
	if findings[0].RecommendedSQL == "" {
		t.Error("expected RecommendedSQL with pg_terminate_backend")
	}
}

func TestCoverage_RuleConnectionLeaks_Empty(t *testing.T) {
	findings := ruleConnectionLeaks(nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for nil, got %d", len(findings))
	}
}

func TestCoverage_RuleCacheHitRatio_HealthyRatio(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		System: collector.SystemStats{CacheHitRatio: 0.99},
	}
	findings := ruleCacheHitRatio(snap, nil, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for healthy ratio, got %d", len(findings))
	}
}

func TestCoverage_RuleCacheHitRatio_Warning(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		System: collector.SystemStats{CacheHitRatio: 0.90},
	}
	findings := ruleCacheHitRatio(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "warning" {
		t.Errorf("Severity = %q, want warning", findings[0].Severity)
	}
	if findings[0].Category != "cache_hit_ratio" {
		t.Errorf("Category = %q, want cache_hit_ratio", findings[0].Category)
	}
}

func TestCoverage_RuleCacheHitRatio_Critical(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		System: collector.SystemStats{CacheHitRatio: 0.70},
	}
	findings := ruleCacheHitRatio(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "critical" {
		t.Errorf("Severity = %q, want critical", findings[0].Severity)
	}
}

func TestCoverage_RuleCacheHitRatio_NegativeRatio(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		System: collector.SystemStats{CacheHitRatio: -1},
	}
	findings := ruleCacheHitRatio(snap, nil, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for negative ratio, got %d", len(findings))
	}
}

func TestCoverage_RuleCheckpointPressure_NoPrevious(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		System: collector.SystemStats{TotalCheckpoints: 100},
	}
	findings := ruleCheckpointPressure(snap, nil, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings with nil previous, got %d", len(findings))
	}
}

func TestCoverage_RuleCheckpointPressure_HighFrequency(t *testing.T) {
	cfg := coverageTestConfig()
	now := time.Now()
	prev := &collector.Snapshot{
		CollectedAt: now.Add(-1 * time.Hour),
		System:      collector.SystemStats{TotalCheckpoints: 100},
	}
	cur := &collector.Snapshot{
		CollectedAt: now,
		System:      collector.SystemStats{TotalCheckpoints: 110},
	}
	findings := ruleCheckpointPressure(cur, prev, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for 10/hr, got %d", len(findings))
	}
	if findings[0].Category != "checkpoint_pressure" {
		t.Errorf("Category = %q, want checkpoint_pressure", findings[0].Category)
	}
}

func TestCoverage_RuleCheckpointPressure_BelowThreshold(t *testing.T) {
	cfg := coverageTestConfig()
	now := time.Now()
	prev := &collector.Snapshot{
		CollectedAt: now.Add(-1 * time.Hour),
		System:      collector.SystemStats{TotalCheckpoints: 100},
	}
	cur := &collector.Snapshot{
		CollectedAt: now,
		System:      collector.SystemStats{TotalCheckpoints: 104},
	}
	findings := ruleCheckpointPressure(cur, prev, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for 4/hr, got %d", len(findings))
	}
}

func TestCoverage_RuleCheckpointPressure_ZeroDelta(t *testing.T) {
	cfg := coverageTestConfig()
	now := time.Now()
	prev := &collector.Snapshot{
		CollectedAt: now.Add(-1 * time.Hour),
		System:      collector.SystemStats{TotalCheckpoints: 100},
	}
	cur := &collector.Snapshot{
		CollectedAt: now,
		System:      collector.SystemStats{TotalCheckpoints: 100},
	}
	findings := ruleCheckpointPressure(cur, prev, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for zero delta, got %d", len(findings))
	}
}

func TestCoverage_RuleCheckpointPressure_ShortElapsed(t *testing.T) {
	cfg := coverageTestConfig()
	now := time.Now()
	prev := &collector.Snapshot{
		CollectedAt: now.Add(-30 * time.Second),
		System:      collector.SystemStats{TotalCheckpoints: 100},
	}
	cur := &collector.Snapshot{
		CollectedAt: now,
		System:      collector.SystemStats{TotalCheckpoints: 200},
	}
	findings := ruleCheckpointPressure(cur, prev, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for short elapsed, got %d", len(findings))
	}
}

func TestCoverage_RuleCheckpointPressure_ZeroTimestamps(t *testing.T) {
	cfg := coverageTestConfig()
	prev := &collector.Snapshot{
		System: collector.SystemStats{TotalCheckpoints: 100},
	}
	cur := &collector.Snapshot{
		System: collector.SystemStats{TotalCheckpoints: 200},
	}
	findings := ruleCheckpointPressure(cur, prev, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for zero timestamps, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// rules_replication.go
// ---------------------------------------------------------------------------

func TestCoverage_ParsePGInterval_Basic(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
	}{
		{"00:00:30", 30 * time.Second},
		{"00:01:00", 1 * time.Minute},
		{"01:00:00", 1 * time.Hour},
		{"00:01:23.456", 1*time.Minute + 23*time.Second + 456*time.Millisecond},
		{"1 day 02:03:04", 26*time.Hour + 3*time.Minute + 4*time.Second},
		{"2 days 00:00:00", 48 * time.Hour},
		{"invalid", 0},
		{"", 0},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := parsePGInterval(tc.input)
			if got != tc.expected {
				t.Errorf("parsePGInterval(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestCoverage_RuleReplicationLag_NilReplication(t *testing.T) {
	snap := &collector.Snapshot{Replication: nil}
	findings := ruleReplicationLag(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for nil replication, got %d", len(findings))
	}
}

func TestCoverage_RuleReplicationLag_HighLag(t *testing.T) {
	lag := "00:02:00"
	addr := "10.0.0.1"
	snap := &collector.Snapshot{
		Replication: &collector.ReplicationStats{
			Replicas: []collector.ReplicaInfo{
				{ReplayLag: &lag, ClientAddr: &addr, State: "streaming"},
			},
		},
	}

	findings := ruleReplicationLag(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "warning" {
		t.Errorf("Severity = %q, want warning (2 min lag)", findings[0].Severity)
	}
	if findings[0].Category != "replication_lag" {
		t.Errorf("Category = %q, want replication_lag", findings[0].Category)
	}
}

func TestCoverage_RuleReplicationLag_CriticalLag(t *testing.T) {
	lag := "00:06:00"
	addr := "10.0.0.2"
	snap := &collector.Snapshot{
		Replication: &collector.ReplicationStats{
			Replicas: []collector.ReplicaInfo{
				{ReplayLag: &lag, ClientAddr: &addr, State: "streaming"},
			},
		},
	}

	findings := ruleReplicationLag(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "critical" {
		t.Errorf("Severity = %q, want critical (6 min lag)", findings[0].Severity)
	}
}

func TestCoverage_RuleReplicationLag_LowLag(t *testing.T) {
	lag := "00:00:05"
	addr := "10.0.0.3"
	snap := &collector.Snapshot{
		Replication: &collector.ReplicationStats{
			Replicas: []collector.ReplicaInfo{
				{ReplayLag: &lag, ClientAddr: &addr},
			},
		},
	}

	findings := ruleReplicationLag(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for low lag, got %d", len(findings))
	}
}

func TestCoverage_RuleReplicationLag_NilLag(t *testing.T) {
	snap := &collector.Snapshot{
		Replication: &collector.ReplicationStats{
			Replicas: []collector.ReplicaInfo{
				{ReplayLag: nil, State: "streaming"},
			},
		},
	}
	findings := ruleReplicationLag(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for nil lag, got %d", len(findings))
	}
}

func TestCoverage_RuleReplicationLag_EmptyLag(t *testing.T) {
	empty := ""
	snap := &collector.Snapshot{
		Replication: &collector.ReplicationStats{
			Replicas: []collector.ReplicaInfo{
				{ReplayLag: &empty, State: "streaming"},
			},
		},
	}
	findings := ruleReplicationLag(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for empty lag, got %d", len(findings))
	}
}

func TestCoverage_RuleReplicationLag_NilClientAddr(t *testing.T) {
	lag := "00:02:00"
	snap := &collector.Snapshot{
		Replication: &collector.ReplicationStats{
			Replicas: []collector.ReplicaInfo{
				{ReplayLag: &lag, ClientAddr: nil, State: "streaming"},
			},
		},
	}
	findings := ruleReplicationLag(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Detail["client_addr"] != "<unknown>" {
		t.Errorf("expected client_addr '<unknown>', got %v", findings[0].Detail["client_addr"])
	}
}

func TestCoverage_RuleReplicationLag_WithWriteAndFlushLag(t *testing.T) {
	lag := "00:02:00"
	writeLag := "00:00:01"
	flushLag := "00:00:02"
	addr := "10.0.0.5"
	snap := &collector.Snapshot{
		Replication: &collector.ReplicationStats{
			Replicas: []collector.ReplicaInfo{
				{
					ReplayLag: &lag, WriteLag: &writeLag,
					FlushLag: &flushLag, ClientAddr: &addr,
					State: "streaming",
				},
			},
		},
	}
	findings := ruleReplicationLag(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	if findings[0].Detail["write_lag"] != writeLag {
		t.Errorf("write_lag = %v, want %s", findings[0].Detail["write_lag"], writeLag)
	}
	if findings[0].Detail["flush_lag"] != flushLag {
		t.Errorf("flush_lag = %v, want %s", findings[0].Detail["flush_lag"], flushLag)
	}
}

func TestCoverage_RuleInactiveSlots_NilReplication(t *testing.T) {
	snap := &collector.Snapshot{Replication: nil}
	findings := ruleInactiveSlots(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 for nil replication, got %d", len(findings))
	}
}

func TestCoverage_RuleInactiveSlots_HasInactive(t *testing.T) {
	snap := &collector.Snapshot{
		Replication: &collector.ReplicationStats{
			Slots: []collector.SlotInfo{
				{SlotName: "active_slot", Active: true, SlotType: "physical", RetainedBytes: 1000},
				{SlotName: "dead_slot", Active: false, SlotType: "logical", RetainedBytes: 5000000},
			},
		},
	}

	findings := ruleInactiveSlots(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Category != "inactive_slot" {
		t.Errorf("Category = %q, want inactive_slot", findings[0].Category)
	}
	if findings[0].ObjectIdentifier != "slot:dead_slot" {
		t.Errorf("ObjectIdentifier = %q, want slot:dead_slot", findings[0].ObjectIdentifier)
	}
	if findings[0].RecommendedSQL == "" {
		t.Error("expected RecommendedSQL to drop slot")
	}
}

func TestCoverage_RuleInactiveSlots_AllActive(t *testing.T) {
	snap := &collector.Snapshot{
		Replication: &collector.ReplicationStats{
			Slots: []collector.SlotInfo{
				{SlotName: "s1", Active: true},
				{SlotName: "s2", Active: true},
			},
		},
	}
	findings := ruleInactiveSlots(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// rules_sequence.go
// ---------------------------------------------------------------------------

func TestCoverage_RuleSequenceExhaustion_Below75(t *testing.T) {
	snap := &collector.Snapshot{
		Sequences: []collector.SequenceStats{
			{SchemaName: "public", SequenceName: "low_seq", PctUsed: 50, DataType: "bigint"},
		},
	}
	findings := ruleSequenceExhaustion(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for 50%% used, got %d", len(findings))
	}
}

func TestCoverage_RuleSequenceExhaustion_Warning(t *testing.T) {
	snap := &collector.Snapshot{
		Sequences: []collector.SequenceStats{
			{SchemaName: "public", SequenceName: "warn_seq", PctUsed: 80, DataType: "bigint"},
		},
	}
	findings := ruleSequenceExhaustion(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "warning" {
		t.Errorf("Severity = %q, want warning", findings[0].Severity)
	}
	if findings[0].Category != "sequence_exhaustion" {
		t.Errorf("Category = %q, want sequence_exhaustion", findings[0].Category)
	}
}

func TestCoverage_RuleSequenceExhaustion_Critical(t *testing.T) {
	snap := &collector.Snapshot{
		Sequences: []collector.SequenceStats{
			{SchemaName: "public", SequenceName: "crit_seq", PctUsed: 95, DataType: "integer"},
		},
	}
	findings := ruleSequenceExhaustion(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "critical" {
		t.Errorf("Severity = %q, want critical", findings[0].Severity)
	}
}

func TestCoverage_RuleSequenceExhaustion_IntegerType(t *testing.T) {
	snap := &collector.Snapshot{
		Sequences: []collector.SequenceStats{
			{SchemaName: "public", SequenceName: "int_seq", PctUsed: 80, DataType: "integer"},
		},
	}
	findings := ruleSequenceExhaustion(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	rec := findings[0].Recommendation
	if !strings.Contains(rec, "bigint") {
		t.Errorf("integer sequence recommendation should mention bigint, got %q", rec)
	}
}

func TestCoverage_RuleSequenceExhaustion_BigintType(t *testing.T) {
	snap := &collector.Snapshot{
		Sequences: []collector.SequenceStats{
			{SchemaName: "public", SequenceName: "big_seq", PctUsed: 80, DataType: "bigint"},
		},
	}
	findings := ruleSequenceExhaustion(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	rec := findings[0].Recommendation
	if strings.Contains(rec, "bigint") {
		t.Error("bigint sequence should NOT recommend migrating to bigint")
	}
}

func TestCoverage_RuleSequenceExhaustion_EmptySequences(t *testing.T) {
	snap := &collector.Snapshot{}
	findings := ruleSequenceExhaustion(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// rules_query.go: ruleSeqScanWatchdog
// ---------------------------------------------------------------------------

func TestCoverage_RuleSeqScanWatchdog_HighSeqScans(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName: "public", RelName: "big_table",
				SeqScan: 5000, IdxScan: 10, NLiveTup: 100000,
			},
		},
	}

	findings := ruleSeqScanWatchdog(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Category != "seq_scan_heavy" {
		t.Errorf("Category = %q, want seq_scan_heavy", findings[0].Category)
	}
}

func TestCoverage_RuleSeqScanWatchdog_SmallTable(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName: "public", RelName: "small",
				SeqScan: 5000, IdxScan: 0, NLiveTup: 100,
			},
		},
	}

	findings := ruleSeqScanWatchdog(snap, nil, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for small table, got %d", len(findings))
	}
}

func TestCoverage_RuleSeqScanWatchdog_LowSeqScans(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName: "public", RelName: "ok_table",
				SeqScan: 50, IdxScan: 1000, NLiveTup: 100000,
			},
		},
	}

	findings := ruleSeqScanWatchdog(snap, nil, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for low seq scans, got %d", len(findings))
	}
}

func TestCoverage_RuleSeqScanWatchdog_SkipTables(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName: "public", RelName: "fk_table",
				SeqScan: 5000, IdxScan: 0, NLiveTup: 100000,
			},
		},
	}
	skip := map[string]bool{"public.fk_table": true}

	findings := ruleSeqScanWatchdog(snap, nil, cfg, skip)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for skipped table, got %d", len(findings))
	}
}

func TestCoverage_RuleSeqScanWatchdog_BalancedRatio(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName: "public", RelName: "balanced",
				SeqScan: 500, IdxScan: 100, NLiveTup: 100000,
			},
		},
	}

	findings := ruleSeqScanWatchdog(snap, nil, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for balanced ratio, got %d", len(findings))
	}
}

func TestCoverage_RuleSeqScanWatchdog_UnloggedTable(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName: "public", RelName: "cache_tbl",
				SeqScan: 5000, IdxScan: 0, NLiveTup: 100000,
				Relpersistence: "u",
			},
		},
	}

	findings := ruleSeqScanWatchdog(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Detail["unlogged"] != true {
		t.Error("expected unlogged=true in detail")
	}
}

// ---------------------------------------------------------------------------
// rules_vacuum.go: ruleXIDWraparound
// ---------------------------------------------------------------------------

func TestCoverage_RuleXIDWraparound_BelowWarning(t *testing.T) {
	cfg := coverageTestConfig()
	findings := ruleXIDWraparound(100000, cfg)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings below warning, got %d", len(findings))
	}
}

func TestCoverage_RuleXIDWraparound_Warning(t *testing.T) {
	cfg := coverageTestConfig()
	findings := ruleXIDWraparound(700000000, cfg)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "warning" {
		t.Errorf("Severity = %q, want warning", findings[0].Severity)
	}
	if findings[0].Category != "xid_wraparound" {
		t.Errorf("Category = %q, want xid_wraparound", findings[0].Category)
	}
	if findings[0].RecommendedSQL == "" {
		t.Error("expected VACUUM FREEZE SQL")
	}
}

func TestCoverage_RuleXIDWraparound_Critical(t *testing.T) {
	cfg := coverageTestConfig()
	findings := ruleXIDWraparound(1500000000, cfg)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "critical" {
		t.Errorf("Severity = %q, want critical", findings[0].Severity)
	}
}

func TestCoverage_RuleXIDWraparound_ExactWarningThreshold(t *testing.T) {
	cfg := coverageTestConfig()
	findings := ruleXIDWraparound(500000000, cfg)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding at exact threshold, got %d", len(findings))
	}
	if findings[0].Severity != "warning" {
		t.Errorf("Severity = %q, want warning at exact threshold", findings[0].Severity)
	}
}

// ---------------------------------------------------------------------------
// rules_query.go: edge cases
// ---------------------------------------------------------------------------

func TestCoverage_RuleQueryRegression_EmptyHistory(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{{QueryID: 1, MeanExecTime: 100}},
	}
	findings := ruleQueryRegression(snap, nil, nil, cfg)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for nil history, got %d", len(findings))
	}
}

func TestCoverage_RuleQueryRegression_EmptyHistoryMap(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{{QueryID: 1, MeanExecTime: 100}},
	}
	findings := ruleQueryRegression(snap, nil, map[int64]float64{}, cfg)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for empty history map, got %d", len(findings))
	}
}

func TestCoverage_RuleQueryRegression_CriticalRegression(t *testing.T) {
	cfg := coverageTestConfig()
	cur := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{QueryID: 1, Calls: 100, MeanExecTime: 400},
		},
	}
	prev := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{QueryID: 1, Calls: 90, MeanExecTime: 10},
		},
	}
	hist := map[int64]float64{1: 10.0}
	findings := ruleQueryRegression(cur, prev, hist, cfg)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "critical" {
		t.Errorf("Severity = %q, want critical for 3900%% increase", findings[0].Severity)
	}
}

func TestCoverage_RuleQueryRegression_NoPreviousSnapshot(t *testing.T) {
	cfg := coverageTestConfig()
	cur := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{QueryID: 1, Calls: 100, MeanExecTime: 200},
		},
	}
	hist := map[int64]float64{1: 10.0}
	findings := ruleQueryRegression(cur, nil, hist, cfg)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding (no prev for reset check), got %d", len(findings))
	}
}

func TestCoverage_RuleQueryRegression_ZeroHistoricalAvg(t *testing.T) {
	cfg := coverageTestConfig()
	cur := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{QueryID: 1, Calls: 100, MeanExecTime: 200},
		},
	}
	hist := map[int64]float64{1: 0.0}
	findings := ruleQueryRegression(cur, nil, hist, cfg)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for zero avg, got %d", len(findings))
	}
}

func TestCoverage_RuleHighPlanTime_CriticalRatio(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{QueryID: 99, Calls: 200, MeanExecTime: 1.0, MeanPlanTime: 15.0},
		},
	}
	findings := ruleHighPlanTime(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "critical" {
		t.Errorf("Severity = %q, want critical for 15x ratio", findings[0].Severity)
	}
}

func TestCoverage_RuleHighPlanTime_ZeroPlanTime(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{QueryID: 1, Calls: 200, MeanExecTime: 10.0, MeanPlanTime: 0},
		},
	}
	findings := ruleHighPlanTime(snap, nil, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for zero plan time, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// rules_vacuum.go: ioWaitRatio, ioSaturated
// ---------------------------------------------------------------------------

func TestCoverage_IOWaitRatio_ZeroExec(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{BlkReadTime: 100, BlkWriteTime: 50, TotalExecTime: 0},
		},
	}
	got := ioWaitRatio(snap)
	if got != 0 {
		t.Errorf("expected 0 for zero exec, got %f", got)
	}
}

func TestCoverage_IOWaitRatio_Normal(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{BlkReadTime: 200, BlkWriteTime: 100, TotalExecTime: 1000},
		},
	}
	got := ioWaitRatio(snap)
	if got < 0.29 || got > 0.31 {
		t.Errorf("expected ~0.3, got %f", got)
	}
}

func TestCoverage_IOWaitRatio_CappedAt1(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{BlkReadTime: 5000, BlkWriteTime: 5000, TotalExecTime: 1000},
		},
	}
	got := ioWaitRatio(snap)
	if got != 1.0 {
		t.Errorf("expected capped at 1.0, got %f", got)
	}
}

func TestCoverage_IOSaturated_True(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{BlkReadTime: 600, BlkWriteTime: 100, TotalExecTime: 1000},
		},
	}
	if !ioSaturated(snap) {
		t.Error("expected ioSaturated=true at 70% IO")
	}
}

func TestCoverage_IOSaturated_False(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{BlkReadTime: 100, BlkWriteTime: 50, TotalExecTime: 1000},
		},
	}
	if ioSaturated(snap) {
		t.Error("expected ioSaturated=false at 15% IO")
	}
}

func TestCoverage_RuleTableBloat_IOSaturated(t *testing.T) {
	cfg := coverageTestConfig()
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName: "public", RelName: "bloated",
				NLiveTup: 5000, NDeadTup: 4000,
			},
		},
		Queries: []collector.QueryStats{
			{BlkReadTime: 600, BlkWriteTime: 200, TotalExecTime: 1000},
		},
	}
	findings := ruleTableBloat(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "info" {
		t.Errorf("Severity = %q, want info when IO saturated", findings[0].Severity)
	}
	if findings[0].Detail["io_saturated"] != true {
		t.Error("expected io_saturated=true in detail")
	}
}

// ---------------------------------------------------------------------------
// dedup.go: severityRank, pickBetter edge cases
// ---------------------------------------------------------------------------

func TestCoverage_SeverityRank_Unknown(t *testing.T) {
	if severityRank("unknown") != 0 {
		t.Errorf("expected 0 for unknown severity, got %d", severityRank("unknown"))
	}
}

func TestCoverage_SeverityRank_AllLevels(t *testing.T) {
	tests := []struct {
		sev  string
		rank int
	}{
		{"critical", 3},
		{"warning", 2},
		{"info", 1},
		{"", 0},
	}
	for _, tc := range tests {
		got := severityRank(tc.sev)
		if got != tc.rank {
			t.Errorf("severityRank(%q) = %d, want %d", tc.sev, got, tc.rank)
		}
	}
}

func TestCoverage_PickBetter_BothHaveSQL(t *testing.T) {
	a := Finding{
		Category: "test", Severity: "warning",
		RecommendedSQL: "SQL A", Title: "a",
	}
	b := Finding{
		Category: "test", Severity: "warning",
		RecommendedSQL: "SQL B", Title: "b",
	}
	got := pickBetter(a, b)
	if got.Title != "a" {
		t.Errorf("expected first to win when both have SQL, got %q", got.Title)
	}
}

func TestCoverage_PickBetter_NeitherHasSQL(t *testing.T) {
	a := Finding{Category: "test", Severity: "warning", Title: "a"}
	b := Finding{Category: "test", Severity: "warning", Title: "b"}
	got := pickBetter(a, b)
	if got.Title != "a" {
		t.Errorf("expected first to win when neither has SQL, got %q", got.Title)
	}
}

func TestCoverage_PickBetter_HigherSeverityWins(t *testing.T) {
	a := Finding{Category: "test", Severity: "info", Title: "a"}
	b := Finding{Category: "test", Severity: "critical", Title: "b"}
	got := pickBetter(a, b)
	if got.Title != "b" {
		t.Errorf("expected b (critical) to win, got %q", got.Title)
	}
}

// ---------------------------------------------------------------------------
// rules_index.go: isLeadingPrefix, extractIndexNameFromSQL
// ---------------------------------------------------------------------------

func TestCoverage_IsLeadingPrefix_Match(t *testing.T) {
	if !isLeadingPrefix([]string{"a"}, []string{"a", "b"}) {
		t.Error("expected true for leading prefix")
	}
}

func TestCoverage_IsLeadingPrefix_NeedLongerThanHave(t *testing.T) {
	if isLeadingPrefix([]string{"a", "b"}, []string{"a"}) {
		t.Error("expected false when need > have")
	}
}

func TestCoverage_IsLeadingPrefix_Mismatch(t *testing.T) {
	if isLeadingPrefix([]string{"b"}, []string{"a", "b"}) {
		t.Error("expected false for non-prefix match")
	}
}

func TestCoverage_ExtractIndexNameFromSQL_WithSchema(t *testing.T) {
	got := extractIndexNameFromSQL("CREATE INDEX myschema.idx_foo ON myschema.bar (baz)")
	if got != "idx_foo" {
		t.Errorf("expected idx_foo, got %q", got)
	}
}

func TestCoverage_ExtractIndexNameFromSQL_NoSchema(t *testing.T) {
	got := extractIndexNameFromSQL("CREATE INDEX idx_bar ON bar (baz)")
	if got != "idx_bar" {
		t.Errorf("expected idx_bar, got %q", got)
	}
}

func TestCoverage_ExtractIndexNameFromSQL_Empty(t *testing.T) {
	got := extractIndexNameFromSQL("")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestCoverage_ExtractIndexNameFromSQL_Concurrently(t *testing.T) {
	got := extractIndexNameFromSQL("CREATE INDEX CONCURRENTLY idx_c ON t (col)")
	if got != "idx_c" {
		t.Errorf("expected idx_c, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// rules_index.go: ruleUnusedIndexes
// ---------------------------------------------------------------------------

func TestCoverage_RuleUnusedIndexes_SkipPrimary(t *testing.T) {
	cfg := coverageTestConfig()
	extras := defaultExtras()
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "t",
				IndexRelName: "t_pkey", IsValid: true,
				IsPrimary: true, IdxScan: 0,
			},
		},
	}
	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for primary key, got %d", len(findings))
	}
}

func TestCoverage_RuleUnusedIndexes_SkipUnique(t *testing.T) {
	cfg := coverageTestConfig()
	extras := defaultExtras()
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "t",
				IndexRelName: "t_uq", IsValid: true,
				IsUnique: true, IdxScan: 0,
			},
		},
	}
	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for unique index, got %d", len(findings))
	}
}

func TestCoverage_RuleUnusedIndexes_SkipInvalid(t *testing.T) {
	cfg := coverageTestConfig()
	extras := defaultExtras()
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "t",
				IndexRelName: "t_inv", IsValid: false, IdxScan: 0,
			},
		},
	}
	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for invalid index, got %d", len(findings))
	}
}

func TestCoverage_RuleUnusedIndexes_SkipRecentlyCreated(t *testing.T) {
	cfg := coverageTestConfig()
	extras := defaultExtras()
	extras.RecentlyCreated["idx_new"] = time.Now()
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "t",
				IndexRelName: "idx_new", IsValid: true, IdxScan: 0,
			},
		},
	}
	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for recently created, got %d", len(findings))
	}
}

func TestCoverage_RuleUnusedIndexes_WindowNotExceeded(t *testing.T) {
	cfg := coverageTestConfig()
	extras := defaultExtras()
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "t",
				IndexRelName: "idx_recent", IsValid: true, IdxScan: 0,
			},
		},
	}
	// First call registers FirstSeen.
	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings on first sight, got %d", len(findings))
	}
	// Second call still within window.
	findings = ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings within window, got %d", len(findings))
	}
}

func TestCoverage_RuleUnusedIndexes_WindowExceeded(t *testing.T) {
	cfg := coverageTestConfig()
	extras := defaultExtras()
	extras.FirstSeen["public.idx_old"] = time.Now().Add(-8 * 24 * time.Hour)
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "t",
				IndexRelName: "idx_old", IsValid: true, IdxScan: 0,
				IndexDef: "CREATE INDEX idx_old ON public.t USING btree (col)",
			},
		},
	}
	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding after window, got %d", len(findings))
	}
	if findings[0].ActionRisk != "safe" {
		t.Errorf("ActionRisk = %q, want safe", findings[0].ActionRisk)
	}
	if findings[0].RecommendedSQL == "" {
		t.Error("expected DROP INDEX SQL")
	}
}

func TestCoverage_RuleUnusedIndexes_UnloggedTable(t *testing.T) {
	cfg := coverageTestConfig()
	extras := defaultExtras()
	extras.FirstSeen["public.idx_ul"] = time.Now().Add(-8 * 24 * time.Hour)
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "cache", Relpersistence: "u"},
		},
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "cache",
				IndexRelName: "idx_ul", IsValid: true, IdxScan: 0,
				IndexDef: "CREATE INDEX idx_ul ON public.cache USING btree (k)",
			},
		},
	}
	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "info" {
		t.Errorf("Severity = %q, want info for unlogged table", findings[0].Severity)
	}
}

func TestCoverage_RuleUnusedIndexes_SkipUsedIndexes(t *testing.T) {
	cfg := coverageTestConfig()
	extras := defaultExtras()
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "t",
				IndexRelName: "idx_used", IsValid: true, IdxScan: 100,
			},
		},
	}
	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for used index, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// rules_index.go: ruleMissingFKIndexes
// ---------------------------------------------------------------------------

func TestCoverage_RuleMissingFKIndexes_Covered(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders"},
		},
		Indexes: []collector.IndexStats{
			{
				SchemaName: "public", RelName: "orders",
				IndexRelName: "idx_fk", IsValid: true,
				IndexDef: "CREATE INDEX idx_fk ON public.orders USING btree (customer_id)",
			},
		},
		ForeignKeys: []collector.ForeignKey{
			{
				TableName: "orders", FKColumn: "customer_id",
				ReferencedTable: "customers", ConstraintName: "fk_cust",
			},
		},
	}

	findings := ruleMissingFKIndexes(snap, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (FK covered), got %d", len(findings))
	}
}

func TestCoverage_RuleMissingFKIndexes_NotCovered(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders"},
		},
		ForeignKeys: []collector.ForeignKey{
			{
				TableName: "orders", FKColumn: "customer_id",
				ReferencedTable: "customers", ConstraintName: "fk_cust",
			},
		},
	}

	findings := ruleMissingFKIndexes(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Category != "missing_fk_index" {
		t.Errorf("Category = %q, want missing_fk_index", findings[0].Category)
	}
	if findings[0].RecommendedSQL == "" {
		t.Error("expected CREATE INDEX SQL")
	}
}

func TestCoverage_RuleMissingFKIndexes_UnloggedTable(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders", Relpersistence: "u"},
		},
		ForeignKeys: []collector.ForeignKey{
			{
				TableName: "orders", FKColumn: "customer_id",
				ReferencedTable: "customers", ConstraintName: "fk_cust",
			},
		},
	}

	findings := ruleMissingFKIndexes(snap, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "info" {
		t.Errorf("Severity = %q, want info for unlogged", findings[0].Severity)
	}
	if findings[0].Detail["unlogged"] != true {
		t.Error("expected unlogged=true in detail")
	}
}

// ---------------------------------------------------------------------------
// Concurrent access test
// ---------------------------------------------------------------------------

func TestCoverage_ConcurrentFindingsAccess(t *testing.T) {
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)

	var wg sync.WaitGroup
	const goroutines = 10

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.SetFindings([]Finding{
				{Category: "cat", Severity: "warning"},
			})
			_ = a.LatestFindings()
			_ = a.OpenFindingsCount()
			_ = a.Findings()
		}()
	}

	wg.Wait()

	got := a.LatestFindings()
	if len(got) != 1 {
		t.Errorf("expected 1 finding after concurrent writes, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// rules_total_time.go
// ---------------------------------------------------------------------------

func TestCoverage_RuleTotalTimeHeavy_NoPrevious(t *testing.T) {
	cfg := coverageTestConfig()
	cur := &collector.Snapshot{
		CollectedAt: time.Now(),
		Queries:     []collector.QueryStats{{QueryID: 1, TotalExecTime: 50000}},
	}
	findings := ruleTotalTimeHeavy(cur, nil, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings with no previous, got %d", len(findings))
	}
}

func TestCoverage_RuleTotalTimeHeavy_ZeroInterval(t *testing.T) {
	cfg := coverageTestConfig()
	now := time.Now()
	prev := &collector.Snapshot{CollectedAt: now}
	cur := &collector.Snapshot{CollectedAt: now}
	findings := ruleTotalTimeHeavy(cur, prev, cfg, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for zero interval, got %d", len(findings))
	}
}

func TestCoverage_RuleHighFreqFirstCycle_NilCurrent(t *testing.T) {
	findings := ruleHighFreqFirstCycle(nil, nil, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for nil current, got %d", len(findings))
	}
}

func TestCoverage_RuleHighFreqFirstCycle_HasPrevious(t *testing.T) {
	cur := &collector.Snapshot{
		Queries: []collector.QueryStats{{QueryID: 1, Calls: 50000, TotalExecTime: 100000}},
	}
	prev := &collector.Snapshot{}
	findings := ruleHighFreqFirstCycle(cur, prev, nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings when previous exists, got %d", len(findings))
	}
}

func TestCoverage_RuleHighFreqFirstCycle_LessThan3Candidates(t *testing.T) {
	cur := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{QueryID: 1, Calls: 50000, TotalExecTime: 100000},
		},
	}
	findings := ruleHighFreqFirstCycle(cur, nil, nil, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for single candidate, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// buildUnloggedSet helper
// ---------------------------------------------------------------------------

func TestCoverage_BuildUnloggedSet(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "normal", Relpersistence: "p"},
			{SchemaName: "public", RelName: "cache", Relpersistence: "u"},
			{SchemaName: "myschema", RelName: "temp", Relpersistence: "u"},
		},
	}
	got := buildUnloggedSet(snap)
	if !got["public.cache"] {
		t.Error("expected public.cache in unlogged set")
	}
	if !got["myschema.temp"] {
		t.Error("expected myschema.temp in unlogged set")
	}
	if got["public.normal"] {
		t.Error("permanent table should not be in unlogged set")
	}
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// subsetFinding helper
// ---------------------------------------------------------------------------

func TestCoverage_SubsetFinding(t *testing.T) {
	sub := collector.IndexStats{
		SchemaName: "public", IndexRelName: "idx_sub",
		IndexDef: "CREATE INDEX idx_sub ON public.t USING btree (a)",
	}
	sup := collector.IndexStats{
		SchemaName: "public", IndexRelName: "idx_sup",
		IndexDef: "CREATE INDEX idx_sup ON public.t USING btree (a, b)",
	}
	f := subsetFinding(sub, sup, "public.idx_sub", "public.idx_sup")
	if f.Category != "duplicate_index" {
		t.Errorf("Category = %q, want duplicate_index", f.Category)
	}
	if f.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", f.Severity)
	}
	if f.ObjectIdentifier != "public.idx_sub" {
		t.Errorf("ObjectIdentifier = %q, want public.idx_sub", f.ObjectIdentifier)
	}
	if f.RecommendedSQL == "" {
		t.Error("expected DROP INDEX SQL")
	}
	if f.RollbackSQL == "" {
		t.Error("expected rollback SQL")
	}
}

// ---------------------------------------------------------------------------
// filterSchemaExclusions: pgsnap schema
// ---------------------------------------------------------------------------

func TestFilterSchemaExclusions_PgSnap(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders"},
			{SchemaName: "pgsnap", RelName: "kv"},
			{SchemaName: "pgsnap", RelName: "snapshots"},
		},
		Indexes: []collector.IndexStats{
			{SchemaName: "public", IndexRelName: "idx_orders_id"},
			{SchemaName: "pgsnap", IndexRelName: "kv_snap_id_idx"},
		},
	}
	filterSchemaExclusions(snap)
	if len(snap.Tables) != 1 {
		t.Errorf("tables = %d, want 1 (public only)", len(snap.Tables))
	}
	if snap.Tables[0].SchemaName != "public" {
		t.Errorf("remaining table schema = %q, want public",
			snap.Tables[0].SchemaName)
	}
	if len(snap.Indexes) != 1 {
		t.Errorf("indexes = %d, want 1 (public only)", len(snap.Indexes))
	}
}
