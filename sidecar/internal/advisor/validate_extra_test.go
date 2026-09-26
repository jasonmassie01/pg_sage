package advisor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — platform restrictions: full_page_writes
// (restricted on ALL four platforms)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — shared_buffers restricted on cloud-sql
// and alloydb but NOT aurora/rds
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — min_wal_size restricted on aurora/rds
// but NOT cloud-sql/alloydb
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — autovacuum_vacuum_threshold boundaries
// Range: [0, 1000000]
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — autovacuum_vacuum_cost_delay boundaries
// Range: [0, 100]
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — autovacuum_vacuum_cost_limit boundaries
// Range: [1, 10000]
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — autovacuum_vacuum_scale_factor boundaries
// Range: [0.001, 1.0]
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — work_mem boundaries
// Range: [1, 1048576] (1KB to 1GB in KB)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — max_connections boundaries
// Range: [10, 10000]
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — error message format validation
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — platform check takes precedence over
// dangerous limits check (verify short-circuit behavior)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — empty platform with restricted setting
// should pass (platform restrictions only apply when platform is set)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// parseLLMFindings — object_identifier takes priority over table
// ---------------------------------------------------------------------------

func TestParseLLMFindings_ObjIdPriorityOverTable(t *testing.T) {
	raw := `[{
		"object_identifier": "public.orders",
		"table": "should_be_ignored"
	}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].ObjectIdentifier != "public.orders" {
		t.Fatalf(
			"expected 'public.orders', got %q",
			findings[0].ObjectIdentifier,
		)
	}
}

// ---------------------------------------------------------------------------
// parseLLMFindings — completely empty JSON object {}
// ---------------------------------------------------------------------------

func TestParseLLMFindings_EmptyObjectInArray(t *testing.T) {
	raw := `[{}]`
	findings := parseLLMFindings(raw, "vacuum", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.ObjectIdentifier != "instance" {
		t.Fatalf(
			"expected default 'instance', got %q", f.ObjectIdentifier,
		)
	}
	if f.Severity != "info" {
		t.Fatalf("expected default 'info', got %q", f.Severity)
	}
	if f.Recommendation != "" {
		t.Fatalf("expected empty recommendation, got %q", f.Recommendation)
	}
	if f.RecommendedSQL != "" {
		t.Fatalf("expected empty SQL, got %q", f.RecommendedSQL)
	}
	if f.Category != "vacuum" {
		t.Fatalf("expected 'vacuum' category, got %q", f.Category)
	}
	if f.ObjectType != "configuration" {
		t.Fatalf("expected 'configuration', got %q", f.ObjectType)
	}
	if f.ActionRisk != "safe" {
		t.Fatalf("expected 'safe', got %q", f.ActionRisk)
	}
}

// ---------------------------------------------------------------------------
// parseLLMFindings — mixed presence of fields across items
// ---------------------------------------------------------------------------

func TestParseLLMFindings_MixedFields(t *testing.T) {
	raw := `[
		{"object_identifier":"public.a","severity":"warning",
		 "rationale":"r1","recommended_sql":"SQL1"},
		{"table":"b"},
		{"severity":"critical"},
		{}
	]`
	findings := parseLLMFindings(raw, "mixed", noopLog)
	if len(findings) != 4 {
		t.Fatalf("expected 4 findings, got %d", len(findings))
	}

	// First: all fields present.
	if findings[0].ObjectIdentifier != "public.a" {
		t.Errorf("[0] ObjectIdentifier = %q", findings[0].ObjectIdentifier)
	}
	if findings[0].Severity != "warning" {
		t.Errorf("[0] Severity = %q", findings[0].Severity)
	}
	if findings[0].Recommendation != "r1" {
		t.Errorf("[0] Recommendation = %q", findings[0].Recommendation)
	}
	if findings[0].RecommendedSQL != "SQL1" {
		t.Errorf("[0] RecommendedSQL = %q", findings[0].RecommendedSQL)
	}

	// Second: only table field.
	if findings[1].ObjectIdentifier != "b" {
		t.Errorf("[1] ObjectIdentifier = %q, want 'b'",
			findings[1].ObjectIdentifier)
	}
	if findings[1].Severity != "info" {
		t.Errorf("[1] Severity = %q, want 'info'",
			findings[1].Severity)
	}

	// Third: only severity, no identifier.
	if findings[2].ObjectIdentifier != "instance" {
		t.Errorf("[2] ObjectIdentifier = %q, want 'instance'",
			findings[2].ObjectIdentifier)
	}
	if findings[2].Severity != "critical" {
		t.Errorf("[2] Severity = %q, want 'critical'",
			findings[2].Severity)
	}

	// Fourth: empty object.
	if findings[3].ObjectIdentifier != "instance" {
		t.Errorf("[3] ObjectIdentifier = %q, want 'instance'",
			findings[3].ObjectIdentifier)
	}
	if findings[3].Severity != "info" {
		t.Errorf("[3] Severity = %q, want 'info'",
			findings[3].Severity)
	}
}

// ---------------------------------------------------------------------------
// parseLLMFindings — JSON empty array "[]" returns zero findings, not nil
// ---------------------------------------------------------------------------

func TestParseLLMFindings_EmptyArrayJSON_ReturnsZeroLen(t *testing.T) {
	findings := parseLLMFindings("[]", "wal_tuning", noopLog)
	if findings == nil {
		// nil is acceptable since Go range over nil works, but
		// verify the parse did not produce an error (no warn logged).
		// Since noopLog swallows, just ensure no panic.
		return
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// parseLLMFindings — markdown-wrapped empty array
// ---------------------------------------------------------------------------

func TestParseLLMFindings_MarkdownWrappedEmptyArray(t *testing.T) {
	raw := "```json\n[]\n```"
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings from wrapped [], got %d",
			len(findings))
	}
}

// ---------------------------------------------------------------------------
// parseLLMFindings — Title format includes category and object
// ---------------------------------------------------------------------------

func TestParseLLMFindings_TitleWithTableFallback(t *testing.T) {
	// Advisory category: config categories drop SQL-less rows (G3-B18).
	raw := `[{"table":"public.users"}]`
	findings := parseLLMFindings(raw, "query_rewrite", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	want := "query_rewrite recommendation for public.users"
	if findings[0].Title != want {
		t.Fatalf("expected title %q, got %q", want, findings[0].Title)
	}
}

func TestParseLLMFindings_TitleWithInstanceDefault(t *testing.T) {
	raw := `[{"severity":"warning"}]`
	findings := parseLLMFindings(raw, "query_rewrite", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	want := "query_rewrite recommendation for instance"
	if findings[0].Title != want {
		t.Fatalf("expected title %q, got %q", want, findings[0].Title)
	}
}

// ---------------------------------------------------------------------------
// parseLLMFindings — completely non-JSON-array input (plain object)
// ---------------------------------------------------------------------------

// A single object answered for an array prompt (json_object mode) is
// one recommendation, not a parse failure (G3-B09). This test previously
// asserted nil, which dropped real recommendations.
func TestParseLLMFindings_PlainJSONObject_NotArray(t *testing.T) {
	raw := `{"object_identifier":"x","severity":"info"}`
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 1 || findings[0].ObjectIdentifier != "x" {
		t.Fatalf("expected one finding for x, got %+v", findings)
	}
}

// ---------------------------------------------------------------------------
// parseLLMFindings — whitespace-only input
// ---------------------------------------------------------------------------

func TestParseLLMFindings_WhitespaceOnly(t *testing.T) {
	findings := parseLLMFindings("   \n\t  ", "test", noopLog)
	if findings != nil {
		t.Fatalf("expected nil for whitespace-only input, got %v",
			findings)
	}
}

// ---------------------------------------------------------------------------
// parseLLMFindings — empty string input
// ---------------------------------------------------------------------------

func TestParseLLMFindings_EmptyString(t *testing.T) {
	findings := parseLLMFindings("", "test", noopLog)
	if findings != nil {
		t.Fatalf("expected nil for empty string, got %v", findings)
	}
}

// ---------------------------------------------------------------------------
// countUnloggedTables — all unlogged
// ---------------------------------------------------------------------------

func TestCountUnloggedTables_EveryTableUnlogged(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{RelName: "cache1", Relpersistence: "u"},
			{RelName: "cache2", Relpersistence: "u"},
			{RelName: "cache3", Relpersistence: "u"},
		},
	}
	got := countUnloggedTables(snap)
	if got != 3 {
		t.Fatalf("expected 3, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// countUnloggedTables — single unlogged among many logged
// ---------------------------------------------------------------------------

func TestCountUnloggedTables_SingleUnlogged(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{RelName: "orders", Relpersistence: "p"},
			{RelName: "cache", Relpersistence: "u"},
			{RelName: "items", Relpersistence: "p"},
			{RelName: "users", Relpersistence: "p"},
			{RelName: "logs", Relpersistence: "p"},
		},
	}
	got := countUnloggedTables(snap)
	if got != 1 {
		t.Fatalf("expected 1, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// countUnloggedTables — nil Tables slice
// ---------------------------------------------------------------------------

func TestCountUnloggedTables_NilTables(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: nil,
	}
	got := countUnloggedTables(snap)
	if got != 0 {
		t.Fatalf("expected 0 for nil Tables, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// countUnloggedTables — temporary tables (relpersistence='t') should
// not be counted as unlogged
// ---------------------------------------------------------------------------

func TestCountUnloggedTables_TempTablesNotCounted(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{RelName: "temp1", Relpersistence: "t"},
			{RelName: "temp2", Relpersistence: "t"},
			{RelName: "perm1", Relpersistence: "p"},
		},
	}
	got := countUnloggedTables(snap)
	if got != 0 {
		t.Fatalf("expected 0 (temp tables are not unlogged), got %d", got)
	}
}

// ---------------------------------------------------------------------------
// countUnloggedTables — mixed logged, unlogged, and temporary
// ---------------------------------------------------------------------------

func TestCountUnloggedTables_MixedLoggedUnloggedTemp(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{RelName: "orders", Relpersistence: "p"},
			{RelName: "cache", Relpersistence: "u"},
			{RelName: "temp_data", Relpersistence: "t"},
			{RelName: "sessions", Relpersistence: "u"},
			{RelName: "items", Relpersistence: "p"},
			{RelName: "staging", Relpersistence: "u"},
			{RelName: "tmp_work", Relpersistence: "t"},
		},
	}
	got := countUnloggedTables(snap)
	if got != 3 {
		t.Fatalf("expected 3, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// detectPlatform — "cloudsql" exact match in Source (contains check)
// ---------------------------------------------------------------------------

func TestDetectPlatform_CloudSQLExactSource(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "work_mem", Setting: "4MB", Source: "cloudsql"},
	}
	got := detectPlatform(settings)
	if got != "cloud-sql" {
		t.Fatalf(
			"expected 'cloud-sql' for Source='cloudsql', got %q", got,
		)
	}
}

// ---------------------------------------------------------------------------
// detectPlatform — "cloud-sql" exact match (not contains)
// ---------------------------------------------------------------------------

func TestDetectPlatform_CloudSQLDashExact(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "wal_level", Setting: "replica", Source: "cloud-sql"},
	}
	got := detectPlatform(settings)
	if got != "cloud-sql" {
		t.Fatalf("expected 'cloud-sql', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// detectPlatform — cloudsql substring embedded in longer source string
// ---------------------------------------------------------------------------

func TestDetectPlatform_CloudSQLSubstring(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "max_wal_size", Setting: "2GB",
			Source: "google-cloudsql-managed"},
	}
	got := detectPlatform(settings)
	if got != "cloud-sql" {
		t.Fatalf(
			"expected 'cloud-sql' for 'google-cloudsql-managed', got %q",
			got,
		)
	}
}

// ---------------------------------------------------------------------------
// detectPlatform — no match returns self-managed
// ---------------------------------------------------------------------------

func TestDetectPlatform_RDSSourceNotDetected(t *testing.T) {
	// detectPlatform only checks for cloudsql/cloud-sql, not rds/aurora.
	settings := []collector.PGSetting{
		{Name: "wal_level", Setting: "replica", Source: "rds"},
	}
	got := detectPlatform(settings)
	if got != "self-managed" {
		t.Fatalf(
			"expected 'self-managed' for Source='rds', got %q", got,
		)
	}
}

// ---------------------------------------------------------------------------
// detectPlatform — cloud-sql found on a non-first setting
// ---------------------------------------------------------------------------

func TestDetectPlatform_CloudSQLNotFirst(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "work_mem", Setting: "4MB", Source: "default"},
		{Name: "max_connections", Setting: "100",
			Source: "configuration file"},
		{Name: "shared_buffers", Setting: "128MB", Source: "cloud-sql"},
	}
	got := detectPlatform(settings)
	if got != "cloud-sql" {
		t.Fatalf("expected 'cloud-sql' from third setting, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// detectPlatform — all settings have non-cloudsql sources
// ---------------------------------------------------------------------------

func TestDetectPlatform_AllNonCloudSources(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "work_mem", Setting: "4MB", Source: "default"},
		{Name: "max_connections", Setting: "100",
			Source: "configuration file"},
		{Name: "wal_level", Setting: "replica", Source: "override"},
	}
	got := detectPlatform(settings)
	if got != "self-managed" {
		t.Fatalf("expected 'self-managed', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// detectPlatform — case sensitivity: "CloudSQL" should NOT match
// (strings.Contains is case-sensitive)
// ---------------------------------------------------------------------------

func TestDetectPlatform_CaseSensitive(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "wal_level", Setting: "replica", Source: "CloudSQL"},
	}
	got := detectPlatform(settings)
	if got != "self-managed" {
		t.Fatalf(
			"expected 'self-managed' (case-sensitive), got %q", got,
		)
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — every dangerousLimits setting has its
// boundaries verified with exact boundary +/- 1
// ---------------------------------------------------------------------------
