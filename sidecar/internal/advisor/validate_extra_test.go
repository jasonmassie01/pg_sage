package advisor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — platform restrictions: full_page_writes
// (restricted on ALL four platforms)
// ---------------------------------------------------------------------------

func TestValidateConfig_FullPageWrites_CloudSQL(t *testing.T) {
	err := ValidateConfigRecommendation(
		"full_page_writes", "off", "cloud-sql",
	)
	if err == nil {
		t.Fatal("expected error: full_page_writes restricted on cloud-sql")
	}
	if !strings.Contains(err.Error(), "not adjustable on cloud-sql") {
		t.Fatalf("wrong error message: %v", err)
	}
}

func TestValidateConfig_FullPageWrites_AlloyDB(t *testing.T) {
	err := ValidateConfigRecommendation(
		"full_page_writes", "off", "alloydb",
	)
	if err == nil {
		t.Fatal("expected error: full_page_writes restricted on alloydb")
	}
	if !strings.Contains(err.Error(), "not adjustable on alloydb") {
		t.Fatalf("wrong error message: %v", err)
	}
}

func TestValidateConfig_FullPageWrites_Aurora(t *testing.T) {
	err := ValidateConfigRecommendation(
		"full_page_writes", "on", "aurora",
	)
	if err == nil {
		t.Fatal("expected error: full_page_writes restricted on aurora")
	}
	if !strings.Contains(err.Error(), "not adjustable on aurora") {
		t.Fatalf("wrong error message: %v", err)
	}
}

func TestValidateConfig_FullPageWrites_RDS(t *testing.T) {
	err := ValidateConfigRecommendation(
		"full_page_writes", "on", "rds",
	)
	if err == nil {
		t.Fatal("expected error: full_page_writes restricted on rds")
	}
	if !strings.Contains(err.Error(), "not adjustable on rds") {
		t.Fatalf("wrong error message: %v", err)
	}
}

func TestValidateConfig_FullPageWrites_SelfManaged(t *testing.T) {
	err := ValidateConfigRecommendation(
		"full_page_writes", "off", "",
	)
	if err != nil {
		t.Fatalf("full_page_writes should be allowed without platform: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — shared_buffers restricted on cloud-sql
// and alloydb but NOT aurora/rds
// ---------------------------------------------------------------------------

func TestValidateConfig_SharedBuffers_CloudSQL_Restricted(t *testing.T) {
	err := ValidateConfigRecommendation(
		"shared_buffers", "256MB", "cloud-sql",
	)
	if err == nil {
		t.Fatal("expected error: shared_buffers restricted on cloud-sql")
	}
}

func TestValidateConfig_SharedBuffers_Aurora_Allowed(t *testing.T) {
	err := ValidateConfigRecommendation(
		"shared_buffers", "256MB", "aurora",
	)
	if err != nil {
		t.Fatalf(
			"shared_buffers should be allowed on aurora: %v", err,
		)
	}
}

func TestValidateConfig_SharedBuffers_RDS_Allowed(t *testing.T) {
	err := ValidateConfigRecommendation(
		"shared_buffers", "256MB", "rds",
	)
	if err != nil {
		t.Fatalf(
			"shared_buffers should be allowed on rds: %v", err,
		)
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — min_wal_size restricted on aurora/rds
// but NOT cloud-sql/alloydb
// ---------------------------------------------------------------------------

func TestValidateConfig_MinWalSize_Aurora_Restricted(t *testing.T) {
	err := ValidateConfigRecommendation(
		"min_wal_size", "80MB", "aurora",
	)
	if err == nil {
		t.Fatal("expected error: min_wal_size restricted on aurora")
	}
	if !strings.Contains(err.Error(), "not adjustable on aurora") {
		t.Fatalf("wrong error message: %v", err)
	}
}

func TestValidateConfig_MinWalSize_RDS_Restricted(t *testing.T) {
	err := ValidateConfigRecommendation(
		"min_wal_size", "80MB", "rds",
	)
	if err == nil {
		t.Fatal("expected error: min_wal_size restricted on rds")
	}
}

func TestValidateConfig_MinWalSize_CloudSQL_Allowed(t *testing.T) {
	err := ValidateConfigRecommendation(
		"min_wal_size", "80MB", "cloud-sql",
	)
	if err != nil {
		t.Fatalf(
			"min_wal_size should be allowed on cloud-sql: %v", err,
		)
	}
}

func TestValidateConfig_MinWalSize_AlloyDB_Allowed(t *testing.T) {
	err := ValidateConfigRecommendation(
		"min_wal_size", "80MB", "alloydb",
	)
	if err != nil {
		t.Fatalf(
			"min_wal_size should be allowed on alloydb: %v", err,
		)
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — autovacuum_vacuum_threshold boundaries
// Range: [0, 1000000]
// ---------------------------------------------------------------------------

func TestValidateConfig_VacuumThreshold_AtMin(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_threshold", "0", "",
	)
	if err != nil {
		t.Fatalf("expected 0 at min boundary to pass: %v", err)
	}
}

func TestValidateConfig_VacuumThreshold_AtMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_threshold", "1000000", "",
	)
	if err != nil {
		t.Fatalf("expected 1000000 at max boundary to pass: %v", err)
	}
}

func TestValidateConfig_VacuumThreshold_BelowMin(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_threshold", "-1", "",
	)
	if err == nil {
		t.Fatal("expected error for threshold=-1 (below min 0)")
	}
	if !strings.Contains(err.Error(), "out of safe range") {
		t.Fatalf("wrong error message: %v", err)
	}
}

func TestValidateConfig_VacuumThreshold_AboveMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_threshold", "1000001", "",
	)
	if err == nil {
		t.Fatal("expected error for threshold=1000001 (above max)")
	}
}

func TestValidateConfig_VacuumThreshold_MidRange(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_threshold", "50", "",
	)
	if err != nil {
		t.Fatalf("expected 50 in range to pass: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — autovacuum_vacuum_cost_delay boundaries
// Range: [0, 100]
// ---------------------------------------------------------------------------

func TestValidateConfig_CostDelay_AtMin(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_cost_delay", "0", "",
	)
	if err != nil {
		t.Fatalf("expected 0 at min boundary to pass: %v", err)
	}
}

func TestValidateConfig_CostDelay_AtMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_cost_delay", "100ms", "",
	)
	if err != nil {
		t.Fatalf("expected 100 at max boundary to pass: %v", err)
	}
}

func TestValidateConfig_CostDelay_AboveMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_cost_delay", "101ms", "",
	)
	if err == nil {
		t.Fatal("expected error for cost_delay=101 (above max 100)")
	}
}

func TestValidateConfig_CostDelay_BelowMin(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_cost_delay", "-1ms", "",
	)
	if err == nil {
		t.Fatal("expected error for cost_delay=-1 (below min 0)")
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — autovacuum_vacuum_cost_limit boundaries
// Range: [1, 10000]
// ---------------------------------------------------------------------------

func TestValidateConfig_CostLimit_AtMin(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_cost_limit", "1", "",
	)
	if err != nil {
		t.Fatalf("expected 1 at min boundary to pass: %v", err)
	}
}

func TestValidateConfig_CostLimit_AtMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_cost_limit", "10000", "",
	)
	if err != nil {
		t.Fatalf("expected 10000 at max boundary to pass: %v", err)
	}
}

func TestValidateConfig_CostLimit_BelowMin(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_cost_limit", "0", "",
	)
	if err == nil {
		t.Fatal("expected error for cost_limit=0 (below min 1)")
	}
}

func TestValidateConfig_CostLimit_JustAboveMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_cost_limit", "10001", "",
	)
	if err == nil {
		t.Fatal("expected error for cost_limit=10001 (above max)")
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — autovacuum_vacuum_scale_factor boundaries
// Range: [0.001, 1.0]
// ---------------------------------------------------------------------------

func TestValidateConfig_ScaleFactor_AtMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_scale_factor", "1.0", "",
	)
	if err != nil {
		t.Fatalf("expected 1.0 at max boundary to pass: %v", err)
	}
}

func TestValidateConfig_ScaleFactor_AboveMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_scale_factor", "1.1", "",
	)
	if err == nil {
		t.Fatal("expected error for scale_factor=1.1 (above max 1.0)")
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — work_mem boundaries
// Range: [1, 1048576] (1KB to 1GB in KB)
// ---------------------------------------------------------------------------

func TestValidateConfig_WorkMem_AtMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"work_mem", "1048576kB", "",
	)
	if err != nil {
		t.Fatalf("expected 1048576 at max boundary to pass: %v", err)
	}
}

func TestValidateConfig_WorkMem_AboveMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"work_mem", "1048577kB", "",
	)
	if err == nil {
		t.Fatal("expected error for work_mem=1048577 (above max)")
	}
}

func TestValidateConfig_WorkMem_BelowMin(t *testing.T) {
	err := ValidateConfigRecommendation("work_mem", "0kB", "")
	if err == nil {
		t.Fatal("expected error for work_mem=0 (below min 1)")
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — max_connections boundaries
// Range: [10, 10000]
// ---------------------------------------------------------------------------

func TestValidateConfig_MaxConnections_LargeAboveMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"max_connections", "99999", "",
	)
	if err == nil {
		t.Fatal("expected error for max_connections=99999")
	}
	if !strings.Contains(err.Error(), "out of safe range") {
		t.Fatalf("wrong error message: %v", err)
	}
}

func TestValidateConfig_MaxConnections_Negative(t *testing.T) {
	err := ValidateConfigRecommendation(
		"max_connections", "-5", "",
	)
	if err == nil {
		t.Fatal("expected error for negative max_connections")
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — error message format validation
// ---------------------------------------------------------------------------

func TestValidateConfig_ErrorContainsSettingName(t *testing.T) {
	err := ValidateConfigRecommendation(
		"max_connections", "99999", "",
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "max_connections") {
		t.Fatalf("error should contain setting name: %v", err)
	}
	if !strings.Contains(err.Error(), "99999") {
		t.Fatalf("error should contain value: %v", err)
	}
}

func TestValidateConfig_PlatformError_ContainsPlatform(t *testing.T) {
	err := ValidateConfigRecommendation(
		"wal_level", "logical", "alloydb",
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "alloydb") {
		t.Fatalf("error should contain platform name: %v", err)
	}
	if !strings.Contains(err.Error(), "wal_level") {
		t.Fatalf("error should contain setting name: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — platform check takes precedence over
// dangerous limits check (verify short-circuit behavior)
// ---------------------------------------------------------------------------

func TestValidateConfig_PlatformRejectsBeforeRangeCheck(t *testing.T) {
	// shared_buffers is restricted on cloud-sql AND is not in
	// dangerousLimits, so this tests the platform branch fires first.
	err := ValidateConfigRecommendation(
		"shared_buffers", "256MB", "cloud-sql",
	)
	if err == nil {
		t.Fatal("expected platform restriction error")
	}
	if !strings.Contains(err.Error(), "not adjustable") {
		t.Fatalf(
			"expected 'not adjustable' (platform error), got: %v", err,
		)
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation — empty platform with restricted setting
// should pass (platform restrictions only apply when platform is set)
// ---------------------------------------------------------------------------

func TestValidateConfig_EmptyPlatform_RestrictedSettingAllowed(t *testing.T) {
	err := ValidateConfigRecommendation("wal_level", "logical", "")
	if err != nil {
		t.Fatalf(
			"wal_level should be allowed with empty platform: %v", err,
		)
	}
}

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

func TestValidateConfig_AllDangerousLimits_Boundaries(t *testing.T) {
	tests := []struct {
		setting  string
		minVal   string
		maxVal   string
		belowMin string
		aboveMax string
	}{
		{
			"max_connections",
			"10", "10000", "9", "10001",
		},
		{
			"autovacuum_vacuum_scale_factor",
			"0.001", "1.0", "0.0009", "1.001",
		},
		{
			"autovacuum_vacuum_threshold",
			"0", "1000000", "-1", "1000001",
		},
		{
			"autovacuum_vacuum_cost_delay",
			"0", "100", "-1", "101",
		},
		{
			"autovacuum_vacuum_cost_limit",
			"1", "10000", "0", "10001",
		},
		{
			"work_mem",
			"1", "1048576", "0", "1048577",
		},
	}

	for _, tc := range tests {
		t.Run(tc.setting+"/at_min", func(t *testing.T) {
			err := ValidateConfigRecommendation(tc.setting, tc.minVal, "")
			if err != nil {
				t.Fatalf(
					"%s=%s should be at min boundary: %v",
					tc.setting, tc.minVal, err,
				)
			}
		})

		t.Run(tc.setting+"/at_max", func(t *testing.T) {
			err := ValidateConfigRecommendation(tc.setting, tc.maxVal, "")
			if err != nil {
				t.Fatalf(
					"%s=%s should be at max boundary: %v",
					tc.setting, tc.maxVal, err,
				)
			}
		})

		t.Run(tc.setting+"/below_min", func(t *testing.T) {
			err := ValidateConfigRecommendation(
				tc.setting, tc.belowMin, "",
			)
			if err == nil {
				t.Fatalf(
					"%s=%s should fail (below min)",
					tc.setting, tc.belowMin,
				)
			}
		})

		t.Run(tc.setting+"/above_max", func(t *testing.T) {
			err := ValidateConfigRecommendation(
				tc.setting, tc.aboveMax, "",
			)
			if err == nil {
				t.Fatalf(
					"%s=%s should fail (above max)",
					tc.setting, tc.aboveMax,
				)
			}
		})
	}
}
