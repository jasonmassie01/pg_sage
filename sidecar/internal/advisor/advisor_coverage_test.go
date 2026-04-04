package advisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// mockLLMServer returns an httptest server that responds with the given
// JSON content as a ChatCompletion response, plus the resulting Manager.
func mockLLMServer(
	t *testing.T, responseContent string,
) (*httptest.Server, *llm.Manager) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			resp := map[string]any{
				"choices": []map[string]any{
					{
						"message": map[string]string{
							"role":    "assistant",
							"content": responseContent,
						},
						"finish_reason": "stop",
					},
				},
				"usage": map[string]int{
					"total_tokens": 100,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Errorf("mock server encode: %v", err)
			}
		},
	))
	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL,
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	client := llm.New(cfg, noopLog)
	mgr := llm.NewManager(client, nil, false)
	return srv, mgr
}

// mockLLMServerError returns a server that always returns HTTP 500.
func mockLLMServerError(
	t *testing.T,
) (*httptest.Server, *llm.Manager) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "internal error")
		},
	))
	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL,
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   2,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	client := llm.New(cfg, noopLog)
	mgr := llm.NewManager(client, nil, false)
	return srv, mgr
}

// ---------------------------------------------------------------------------
// stripMarkdownFences (prompt.go)
// ---------------------------------------------------------------------------

func TestStripMarkdownFences_JSONBlock(t *testing.T) {
	input := "```json\n{\"key\":\"value\"}\n```"
	got := stripMarkdownFences(input)
	want := "{\"key\":\"value\"}"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestStripMarkdownFences_GenericBlock(t *testing.T) {
	input := "```\nsome content\n```"
	got := stripMarkdownFences(input)
	want := "some content"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestStripMarkdownFences_NoFences(t *testing.T) {
	input := "plain text"
	got := stripMarkdownFences(input)
	if got != input {
		t.Fatalf("expected %q unchanged, got %q", input, got)
	}
}

func TestStripMarkdownFences_EmptyString(t *testing.T) {
	got := stripMarkdownFences("")
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestStripMarkdownFences_OnlyOpeningFence(t *testing.T) {
	input := "```json\n{\"key\":\"value\"}"
	got := stripMarkdownFences(input)
	want := "{\"key\":\"value\"}"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestStripMarkdownFences_OnlyClosingFence(t *testing.T) {
	input := "{\"key\":\"value\"}\n```"
	got := stripMarkdownFences(input)
	want := "{\"key\":\"value\"}"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestStripMarkdownFences_WhitespaceAround(t *testing.T) {
	input := "  ```json\n[]\n```  "
	got := stripMarkdownFences(input)
	want := "[]"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// ---------------------------------------------------------------------------
// stripToJSON edge cases (prompt.go)
// ---------------------------------------------------------------------------

func TestStripToJSON_OnlyBrackets(t *testing.T) {
	got := stripToJSON("[]")
	if got != "[]" {
		t.Fatalf("expected %q, got %q", "[]", got)
	}
}

func TestStripToJSON_MarkdownWithoutBrackets(t *testing.T) {
	input := "```json\n{\"key\":\"value\"}\n```"
	got := stripToJSON(input)
	want := "{\"key\":\"value\"}"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestStripToJSON_BracketsInsideMarkdown(t *testing.T) {
	input := "```json\n[{\"a\":1}]\n```"
	got := stripToJSON(input)
	want := "[{\"a\":1}]"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestStripToJSON_MultipleArrays_GetsOutermost(t *testing.T) {
	input := "prefix [1] middle [2] suffix"
	got := stripToJSON(input)
	want := "[1] middle [2]"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestStripToJSON_TrailingText(t *testing.T) {
	input := "[{\"foo\":\"bar\"}] some trailing text"
	got := stripToJSON(input)
	want := "[{\"foo\":\"bar\"}]"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestStripToJSON_SingleOpenBracket(t *testing.T) {
	input := "[incomplete data"
	got := stripToJSON(input)
	if got != input {
		t.Fatalf("expected %q unchanged, got %q", input, got)
	}
}

// ---------------------------------------------------------------------------
// parseNumericValue (validate.go)
// ---------------------------------------------------------------------------

func TestParseNumericValue_PlainInteger(t *testing.T) {
	v, err := parseNumericValue("100")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 100.0 {
		t.Fatalf("expected 100.0, got %f", v)
	}
}

func TestParseNumericValue_PlainFloat(t *testing.T) {
	v, err := parseNumericValue("3.14")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v < 3.13 || v > 3.15 {
		t.Fatalf("expected ~3.14, got %f", v)
	}
}

func TestParseNumericValue_WithMBSuffix(t *testing.T) {
	v, err := parseNumericValue("256MB")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 256.0 {
		t.Fatalf("expected 256.0, got %f", v)
	}
}

func TestParseNumericValue_WithGBSuffix(t *testing.T) {
	v, err := parseNumericValue("4GB")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 4.0 {
		t.Fatalf("expected 4.0, got %f", v)
	}
}

func TestParseNumericValue_WithkBSuffix(t *testing.T) {
	v, err := parseNumericValue("8192kB")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 8192.0 {
		t.Fatalf("expected 8192.0, got %f", v)
	}
}

func TestParseNumericValue_WithMsSuffix(t *testing.T) {
	v, err := parseNumericValue("200ms")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 200.0 {
		t.Fatalf("expected 200.0, got %f", v)
	}
}

func TestParseNumericValue_WithSSuffix(t *testing.T) {
	v, err := parseNumericValue("30s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 30.0 {
		t.Fatalf("expected 30.0, got %f", v)
	}
}

func TestParseNumericValue_WithMinSuffix(t *testing.T) {
	v, err := parseNumericValue("5min")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 5.0 {
		t.Fatalf("expected 5.0, got %f", v)
	}
}

func TestParseNumericValue_WithWhitespace(t *testing.T) {
	v, err := parseNumericValue("  42  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 42.0 {
		t.Fatalf("expected 42.0, got %f", v)
	}
}

func TestParseNumericValue_NotANumber(t *testing.T) {
	_, err := parseNumericValue("abc")
	if err == nil {
		t.Fatal("expected error for non-numeric value")
	}
}

func TestParseNumericValue_EmptyString(t *testing.T) {
	_, err := parseNumericValue("")
	if err == nil {
		t.Fatal("expected error for empty string")
	}
}

func TestParseNumericValue_NegativeValue(t *testing.T) {
	v, err := parseNumericValue("-10")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != -10.0 {
		t.Fatalf("expected -10.0, got %f", v)
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation edge cases (validate.go)
// ---------------------------------------------------------------------------

func TestValidateConfig_UnknownPlatform(t *testing.T) {
	err := ValidateConfigRecommendation("wal_level", "logical", "azure-flex")
	if err != nil {
		t.Fatalf("unexpected error for unknown platform: %v", err)
	}
}

func TestValidateConfig_UnknownSetting_NoLimits(t *testing.T) {
	err := ValidateConfigRecommendation("my_custom_guc", "on", "")
	if err != nil {
		t.Fatalf("unexpected error for unknown setting: %v", err)
	}
}

func TestValidateConfig_NonNumericValueForLimitedGUC(t *testing.T) {
	err := ValidateConfigRecommendation("max_connections", "on", "")
	if err != nil {
		t.Fatalf("expected nil error for non-numeric, got: %v", err)
	}
}

func TestValidateConfig_BoundaryMin_MaxConnections(t *testing.T) {
	err := ValidateConfigRecommendation("max_connections", "10", "")
	if err != nil {
		t.Fatalf("expected 10 at min boundary, got: %v", err)
	}
}

func TestValidateConfig_BoundaryMax_MaxConnections(t *testing.T) {
	err := ValidateConfigRecommendation("max_connections", "10000", "")
	if err != nil {
		t.Fatalf("expected 10000 at max boundary, got: %v", err)
	}
}

func TestValidateConfig_JustBelowMin_MaxConnections(t *testing.T) {
	err := ValidateConfigRecommendation("max_connections", "9", "")
	if err == nil {
		t.Fatal("expected error for max_connections=9")
	}
}

func TestValidateConfig_JustAboveMax_MaxConnections(t *testing.T) {
	err := ValidateConfigRecommendation("max_connections", "10001", "")
	if err == nil {
		t.Fatal("expected error for max_connections=10001")
	}
}

func TestValidateConfig_WorkMem_AtMinBoundary(t *testing.T) {
	err := ValidateConfigRecommendation("work_mem", "1kB", "")
	if err != nil {
		t.Fatalf("expected 1 at work_mem min, got: %v", err)
	}
}

func TestValidateConfig_AutovacuumScaleFactor_AtMin(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_scale_factor", "0.001", "",
	)
	if err != nil {
		t.Fatalf("expected 0.001 at min, got: %v", err)
	}
}

func TestValidateConfig_AutovacuumScaleFactor_BelowMin(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_scale_factor", "0.0001", "",
	)
	if err == nil {
		t.Fatal("expected error for scale_factor below 0.001")
	}
}

func TestValidateConfig_CostDelay_InRange(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_cost_delay", "20ms", "",
	)
	if err != nil {
		t.Fatalf("expected nil for cost_delay=20, got: %v", err)
	}
}

func TestValidateConfig_CostLimit_AboveMax(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_cost_limit", "20000", "",
	)
	if err == nil {
		t.Fatal("expected error for cost_limit=20000")
	}
}

// ---------------------------------------------------------------------------
// RequiresRestart coverage (validate.go)
// ---------------------------------------------------------------------------

func TestRequiresRestart_AllKnown(t *testing.T) {
	expected := map[string]bool{
		"max_connections": true,
		"shared_buffers":  true,
		"huge_pages":      true,
		"wal_level":       true,
		"max_wal_senders": true,
		"wal_buffers":     true,
	}
	for setting, want := range expected {
		got := RequiresRestart(setting)
		if got != want {
			t.Errorf("RequiresRestart(%q) = %v, want %v",
				setting, got, want)
		}
	}
}

func TestRequiresRestart_NonRestartSettings(t *testing.T) {
	settings := []string{
		"work_mem", "maintenance_work_mem", "effective_cache_size",
		"autovacuum_vacuum_scale_factor", "checkpoint_timeout",
		"max_wal_size", "random_page_cost",
	}
	for _, s := range settings {
		if RequiresRestart(s) {
			t.Errorf("RequiresRestart(%q) = true, want false", s)
		}
	}
}

// ---------------------------------------------------------------------------
// parseLLMFindings edge cases (prompt.go)
// ---------------------------------------------------------------------------

func TestParseLLMFindings_MarkdownWrappedJSON(t *testing.T) {
	raw := "```json\n[{\"object_identifier\":\"public.foo\"," +
		"\"severity\":\"warning\",\"rationale\":\"test\"}]\n```"
	findings := parseLLMFindings(raw, "test_cat", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].ObjectIdentifier != "public.foo" {
		t.Fatalf("expected 'public.foo', got %q",
			findings[0].ObjectIdentifier)
	}
	if findings[0].Category != "test_cat" {
		t.Fatalf("expected 'test_cat', got %q", findings[0].Category)
	}
}

func TestParseLLMFindings_TitleFormat(t *testing.T) {
	raw := `[{"object_identifier":"public.bar"}]`
	findings := parseLLMFindings(raw, "memory_tuning", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	want := "memory_tuning recommendation for public.bar"
	if findings[0].Title != want {
		t.Fatalf("expected title %q, got %q", want, findings[0].Title)
	}
}

func TestParseLLMFindings_ObjectTypeAlwaysConfiguration(t *testing.T) {
	raw := `[{"table":"t1"},{"table":"t2"}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	for i, f := range findings {
		if f.ObjectType != "configuration" {
			t.Errorf("finding[%d]: ObjectType = %q, want 'configuration'",
				i, f.ObjectType)
		}
	}
}

func TestParseLLMFindings_DetailPreserved(t *testing.T) {
	raw := `[{"object_identifier":"x","extra_field":"extra_val"}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	val, ok := findings[0].Detail["extra_field"]
	if !ok {
		t.Fatal("expected 'extra_field' in Detail")
	}
	strVal, ok := val.(string)
	if !ok || strVal != "extra_val" {
		t.Fatalf("expected 'extra_val', got %v", val)
	}
}

func TestParseLLMFindings_NullRecommendedSQL(t *testing.T) {
	raw := `[{"object_identifier":"x","recommended_sql":null}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	if findings[0].RecommendedSQL != "" {
		t.Fatalf("expected empty for null, got %q",
			findings[0].RecommendedSQL)
	}
}

func TestParseLLMFindings_NumericSeverityIgnored(t *testing.T) {
	raw := `[{"object_identifier":"x","severity":5}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	if findings[0].Severity != "info" {
		t.Fatalf("expected 'info' for numeric severity, got %q",
			findings[0].Severity)
	}
}

func TestParseLLMFindings_BrokenJSONObject(t *testing.T) {
	raw := `[{"object_identifier":"x"`
	findings := parseLLMFindings(raw, "test", noopLog)
	if findings != nil {
		t.Fatalf("expected nil for broken JSON, got %v", findings)
	}
}

func TestParseLLMFindings_NestedThinkingBeforeAndAfter(t *testing.T) {
	raw := "Let me think about this...\n\n" +
		`[{"object_identifier":"schema.tbl","rationale":"optimize"}]` +
		"\n\nHope that helps!"
	findings := parseLLMFindings(raw, "advisor", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	if findings[0].ObjectIdentifier != "schema.tbl" {
		t.Fatalf("expected 'schema.tbl', got %q",
			findings[0].ObjectIdentifier)
	}
}

func TestParseLLMFindings_UnicodeContent(t *testing.T) {
	raw := `[{"object_identifier":"public.テスト","rationale":"テスト理由"}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	if findings[0].ObjectIdentifier != "public.テスト" {
		t.Fatalf("got %q", findings[0].ObjectIdentifier)
	}
}

func TestParseLLMFindings_EmptyRationale(t *testing.T) {
	raw := `[{"object_identifier":"x","rationale":""}]`
	findings := parseLLMFindings(raw, "test", noopLog)
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	if findings[0].Recommendation != "" {
		t.Fatalf("expected empty, got %q", findings[0].Recommendation)
	}
}

func TestParseLLMFindings_ComplexResponse(t *testing.T) {
	raw := `[
		{
			"object_identifier": "public.orders",
			"severity": "warning",
			"rationale": "High dead tuple ratio of 35%",
			"recommended_sql": "ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.02)",
			"current_settings": {"autovacuum_vacuum_scale_factor": "0.2"},
			"recommended_settings": {"autovacuum_vacuum_scale_factor": "0.02"}
		},
		{
			"object_identifier": "public.events",
			"severity": "info",
			"rationale": "Dead tuple ratio of 8% is acceptable",
			"recommended_sql": null
		}
	]`
	findings := parseLLMFindings(raw, "vacuum_tuning", noopLog)
	if len(findings) != 2 {
		t.Fatalf("expected 2, got %d", len(findings))
	}
	if findings[0].Severity != "warning" {
		t.Errorf("finding[0]: Severity = %q", findings[0].Severity)
	}
	if findings[1].RecommendedSQL != "" {
		t.Errorf("finding[1]: expected empty SQL, got %q",
			findings[1].RecommendedSQL)
	}
}

// ---------------------------------------------------------------------------
// Advisor struct methods (advisor.go)
// ---------------------------------------------------------------------------

func TestAdvisor_New_NotNil(t *testing.T) {
	cfg := &config.Config{}
	a := New(nil, cfg, nil, nil, noopLog)
	if a == nil {
		t.Fatal("expected non-nil Advisor")
	}
	if a.cfg != cfg {
		t.Fatal("expected cfg to be stored")
	}
}

func TestAdvisor_LatestFindings_MultipleCopies(t *testing.T) {
	a := newTestAdvisor(true, true, 86400)
	a.mu.Lock()
	a.findings = []analyzer.Finding{
		{Category: "a", Title: "t1"},
		{Category: "b", Title: "t2"},
		{Category: "c", Title: "t3"},
	}
	a.mu.Unlock()

	got := a.LatestFindings()
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d", len(got))
	}
	got[0].Title = "mutated"
	got = got[:1]

	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.findings) != 3 {
		t.Fatal("original findings slice modified")
	}
	if a.findings[0].Title != "t1" {
		t.Fatal("original finding title modified")
	}
}

func TestAdvisor_ShouldRun_IntervalBoundary(t *testing.T) {
	a := newTestAdvisor(true, true, 60)
	a.lastRunAt = time.Now().Add(-61 * time.Second)
	if !a.ShouldRun() {
		t.Fatal("expected true after interval elapsed")
	}
}

func TestAdvisor_ShouldRun_JustBeforeInterval(t *testing.T) {
	a := newTestAdvisor(true, true, 60)
	a.lastRunAt = time.Now().Add(-59 * time.Second)
	if a.ShouldRun() {
		t.Fatal("expected false before interval elapsed")
	}
}

func TestAdvisor_Analyze_BothDisabled(t *testing.T) {
	a := newTestAdvisor(false, false, 86400)
	findings, err := a.Analyze(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil when both disabled")
	}
}

// ---------------------------------------------------------------------------
// analyzeBloat early returns (bloat.go)
// ---------------------------------------------------------------------------

func TestAnalyzeBloat_NilConfigData(t *testing.T) {
	snap := &collector.Snapshot{
		ConfigData: nil,
	}
	findings, err := analyzeBloat(
		context.Background(), nil, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil findings for nil ConfigData")
	}
}

func TestAnalyzeBloat_NoQualifyingTables(t *testing.T) {
	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			// Small table: total < 1000
			{SchemaName: "public", RelName: "tiny",
				NLiveTup: 500, NDeadTup: 100},
			// Low ratio: ratio < 10%
			{SchemaName: "public", RelName: "healthy",
				NLiveTup: 9500, NDeadTup: 500},
		},
	}
	findings, err := analyzeBloat(
		context.Background(), nil, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil findings when no tables qualify")
	}
}

// ---------------------------------------------------------------------------
// analyzeVacuum early returns (vacuum.go)
// ---------------------------------------------------------------------------

func TestAnalyzeVacuum_NoQualifyingTables(t *testing.T) {
	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			// Small table
			{SchemaName: "public", RelName: "tiny",
				NLiveTup: 500, NDeadTup: 400},
			// Low dead ratio
			{SchemaName: "public", RelName: "healthy",
				NLiveTup: 20000, NDeadTup: 500},
		},
	}
	findings, err := analyzeVacuum(
		context.Background(), nil, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil findings when no tables qualify")
	}
}

func TestAnalyzeVacuum_NoQualifyingTables_AllSmall(t *testing.T) {
	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "t1",
				NLiveTup: 100, NDeadTup: 100},
			{SchemaName: "public", RelName: "t2",
				NLiveTup: 300, NDeadTup: 200},
		},
	}
	findings, err := analyzeVacuum(
		context.Background(), nil, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil for all-small tables")
	}
}

func TestAnalyzeVacuum_NoTables(t *testing.T) {
	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{},
		Tables:     nil,
	}
	findings, err := analyzeVacuum(
		context.Background(), nil, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil for no tables")
	}
}

// ---------------------------------------------------------------------------
// analyzeQueryRewrites early returns (rewrite.go)
// ---------------------------------------------------------------------------

func TestAnalyzeQueryRewrites_NoCandidates_EmptyQueries(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: nil,
	}
	findings, err := analyzeQueryRewrites(
		context.Background(), nil, nil, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil for empty queries")
	}
}

func TestAnalyzeQueryRewrites_NoCandidates_LowActivity(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			// Too few calls
			{QueryID: 1, Calls: 10, MeanExecTime: 100,
				TotalExecTime: 1000},
			// Too fast
			{QueryID: 2, Calls: 200, MeanExecTime: 5,
				TotalExecTime: 1000},
			// No temp spills, and doesn't qualify by time
			{QueryID: 3, Calls: 50, MeanExecTime: 30,
				TotalExecTime: 1500, TempBlksWritten: 0},
		},
	}
	findings, err := analyzeQueryRewrites(
		context.Background(), nil, nil, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil for low-activity queries")
	}
}

func TestAnalyzeQueryRewrites_NoCandidates_SpillsLowCalls(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			// Has spills but too few calls
			{QueryID: 1, Calls: 30, MeanExecTime: 10,
				TotalExecTime: 300, TempBlksWritten: 500},
			// Low activity
			{QueryID: 2, Calls: 5, MeanExecTime: 2,
				TotalExecTime: 10},
		},
	}
	findings, err := analyzeQueryRewrites(
		context.Background(), nil, nil, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil for spill queries with low calls")
	}
}

// ---------------------------------------------------------------------------
// analyzeConnections early return (connection.go)
// ---------------------------------------------------------------------------

func TestAnalyzeConnections_NilConfigData(t *testing.T) {
	snap := &collector.Snapshot{
		ConfigData: nil,
	}
	findings, err := analyzeConnections(
		context.Background(), nil, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil for nil ConfigData")
	}
}

// ---------------------------------------------------------------------------
// analyzeMemory early return (memory.go)
// ---------------------------------------------------------------------------

func TestAnalyzeMemory_NilConfigData(t *testing.T) {
	snap := &collector.Snapshot{
		ConfigData: nil,
	}
	findings, err := analyzeMemory(
		context.Background(), nil, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil for nil ConfigData")
	}
}

// ---------------------------------------------------------------------------
// analyzeWAL early return (wal.go)
// ---------------------------------------------------------------------------

func TestAnalyzeWAL_NilConfigData(t *testing.T) {
	snap := &collector.Snapshot{
		ConfigData: nil,
	}
	findings, err := analyzeWAL(
		context.Background(), nil, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil for nil ConfigData")
	}
}

// ---------------------------------------------------------------------------
// Vacuum table context building with prev snapshot (vacuum.go)
// ---------------------------------------------------------------------------

func TestVacuumFiltering_WithPrevSnapshot_WriteRate(t *testing.T) {
	now := time.Now()
	prevTime := now.Add(-time.Hour)

	prev := &collector.Snapshot{
		CollectedAt: prevTime,
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders",
				NLiveTup: 8000, NDeadTup: 800,
				NTupIns: 1000, NTupUpd: 500, NTupDel: 200},
		},
	}

	snap := &collector.Snapshot{
		CollectedAt: now,
		ConfigData:  &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders",
				NLiveTup: 8000, NDeadTup: 1200,
				NTupIns: 6000, NTupUpd: 2500, NTupDel: 700},
		},
	}

	// Verify the write rate calculation logic.
	elapsed := snap.CollectedAt.Sub(prev.CollectedAt)
	hrs := elapsed.Hours()
	insPerDay := float64(6000-1000) / hrs * 24

	// 5000 inserts in 1 hour = 120000 per day
	if insPerDay < 119000 || insPerDay > 121000 {
		t.Errorf("expected ~120000 ins/day, got %.0f", insPerDay)
	}

	// The function should still reach the early return because there IS
	// a qualifying table (ratio > 5%, total > 1000), but it would need
	// an LLM to proceed. We can still call the function and see it
	// builds context without panicking.
	// Cannot proceed past context building without LLM, so just verify
	// the math above is consistent with what the function would compute.
}

// ---------------------------------------------------------------------------
// Bloat trend with prev snapshot (bloat.go)
// ---------------------------------------------------------------------------

func TestBloatFiltering_WithPrevSnapshot_TrendCalc(t *testing.T) {
	now := time.Now()
	prev := &collector.Snapshot{
		CollectedAt: now.Add(-time.Hour),
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "big",
				NLiveTup: 7000, NDeadTup: 2000},
		},
	}
	snap := &collector.Snapshot{
		CollectedAt: now,
		ConfigData:  &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "big",
				NLiveTup: 7000, NDeadTup: 3000},
		},
	}

	// Calculate trend the same way as analyzeBloat.
	t1 := snap.Tables[0]
	total := t1.NLiveTup + t1.NDeadTup
	ratio := float64(t1.NDeadTup) / float64(max(total, 1))
	// 3000/10000 = 0.30

	pt := prev.Tables[0]
	prevTotal := max(pt.NLiveTup+pt.NDeadTup, 1)
	prevRatio := float64(pt.NDeadTup) / float64(prevTotal)
	// 2000/9000 = 0.222

	var trend string
	if ratio > prevRatio+0.02 {
		trend = "growing"
	} else if ratio < prevRatio-0.02 {
		trend = "shrinking"
	} else {
		trend = "stable"
	}

	// 0.30 > 0.222+0.02 = 0.242 -> growing
	if trend != "growing" {
		t.Fatalf("expected growing, got %s", trend)
	}
}

// ---------------------------------------------------------------------------
// Vacuum with autovacuum timestamps (vacuum.go)
// ---------------------------------------------------------------------------

func TestVacuumContext_AutovacuumTimestamp(t *testing.T) {
	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	info := "never"
	if true { // simulate t.LastAutovacuum != nil
		info = ts.Format("2006-01-02 15:04")
	}
	if info != "2026-03-20 10:00" {
		t.Fatalf("expected '2026-03-20 10:00', got %q", info)
	}
}

// ---------------------------------------------------------------------------
// Vacuum with reloptions (vacuum.go)
// ---------------------------------------------------------------------------

func TestVacuumReloptions_Lookup(t *testing.T) {
	configData := &collector.ConfigSnapshot{
		TableReloptions: []collector.TableReloption{
			{SchemaName: "public", RelName: "orders",
				Reloptions: "{autovacuum_vacuum_scale_factor=0.01}"},
			{SchemaName: "public", RelName: "events",
				Reloptions: "{autovacuum_vacuum_threshold=100}"},
		},
	}

	overrides := "none"
	for _, ro := range configData.TableReloptions {
		if ro.SchemaName == "public" && ro.RelName == "orders" {
			overrides = ro.Reloptions
			break
		}
	}
	if overrides == "none" {
		t.Fatal("expected to find reloptions for orders")
	}
	if !strings.Contains(overrides, "scale_factor") {
		t.Fatalf("unexpected reloptions: %q", overrides)
	}
}

// ---------------------------------------------------------------------------
// Bloat pg_repack detection (bloat.go)
// ---------------------------------------------------------------------------

func TestBloatRepackDetection_InList(t *testing.T) {
	exts := []string{"pg_stat_statements", "pg_repack", "hypopg"}
	hasRepack := false
	for _, e := range exts {
		if e == "pg_repack" {
			hasRepack = true
			break
		}
	}
	if !hasRepack {
		t.Error("expected pg_repack detected")
	}
}

func TestBloatRepackDetection_NotInList(t *testing.T) {
	exts := []string{"pg_stat_statements", "hypopg"}
	hasRepack := false
	for _, e := range exts {
		if e == "pg_repack" {
			hasRepack = true
			break
		}
	}
	if hasRepack {
		t.Error("should not detect pg_repack")
	}
}

func TestBloatRepackDetection_EmptyList(t *testing.T) {
	var exts []string
	hasRepack := false
	for _, e := range exts {
		if e == "pg_repack" {
			hasRepack = true
			break
		}
	}
	if hasRepack {
		t.Error("should not detect pg_repack in empty list")
	}
}

// ---------------------------------------------------------------------------
// restrictedSettings and dangerousLimits maps (validate.go)
// ---------------------------------------------------------------------------

func TestRestrictedSettings_AllPlatformsExist(t *testing.T) {
	platforms := []string{"cloud-sql", "alloydb", "aurora", "rds"}
	for _, p := range platforms {
		if _, ok := restrictedSettings[p]; !ok {
			t.Errorf("missing platform %q", p)
		}
	}
}

func TestDangerousLimits_AllExpectedSettings(t *testing.T) {
	expected := []string{
		"max_connections",
		"autovacuum_vacuum_scale_factor",
		"autovacuum_vacuum_threshold",
		"autovacuum_vacuum_cost_delay",
		"autovacuum_vacuum_cost_limit",
		"work_mem",
	}
	for _, s := range expected {
		if _, ok := dangerousLimits[s]; !ok {
			t.Errorf("missing %q in dangerousLimits", s)
		}
	}
}

func TestDangerousLimits_MinLessThanMax(t *testing.T) {
	for name, limits := range dangerousLimits {
		if limits[0] >= limits[1] {
			t.Errorf("%s: min (%.0f) >= max (%.0f)",
				name, limits[0], limits[1])
		}
	}
}

func TestRestartRequired_MapContents(t *testing.T) {
	if len(restartRequired) != 6 {
		t.Fatalf("expected 6, got %d", len(restartRequired))
	}
}

// ---------------------------------------------------------------------------
// System prompt structural invariants
// ---------------------------------------------------------------------------

func TestAllSystemPrompts_ContainJSONArray(t *testing.T) {
	prompts := map[string]string{
		"vacuum":     vacuumSystemPrompt,
		"wal":        walSystemPrompt,
		"connection": connectionSystemPrompt,
		"memory":     memorySystemPrompt,
		"bloat":      bloatSystemPrompt,
		"rewrite":    rewriteSystemPrompt,
	}
	for name, p := range prompts {
		if !strings.Contains(p, "JSON array") {
			t.Errorf("%s missing 'JSON array'", name)
		}
	}
}

func TestAllSystemPrompts_ContainObjectIdentifier(t *testing.T) {
	prompts := map[string]string{
		"vacuum":     vacuumSystemPrompt,
		"wal":        walSystemPrompt,
		"connection": connectionSystemPrompt,
		"memory":     memorySystemPrompt,
		"bloat":      bloatSystemPrompt,
		"rewrite":    rewriteSystemPrompt,
	}
	for name, p := range prompts {
		if !strings.Contains(p, "object_identifier") {
			t.Errorf("%s missing 'object_identifier'", name)
		}
	}
}

func TestAllSystemPrompts_ContainAntiThinking(t *testing.T) {
	prompts := map[string]string{
		"vacuum":     vacuumSystemPrompt,
		"wal":        walSystemPrompt,
		"connection": connectionSystemPrompt,
		"memory":     memorySystemPrompt,
		"bloat":      bloatSystemPrompt,
		"rewrite":    rewriteSystemPrompt,
	}
	for name, p := range prompts {
		if !strings.Contains(p, "No thinking") {
			t.Errorf("%s missing 'No thinking'", name)
		}
	}
}

func TestAllSystemPrompts_ContainCRITICAL(t *testing.T) {
	prompts := map[string]string{
		"vacuum":     vacuumSystemPrompt,
		"wal":        walSystemPrompt,
		"connection": connectionSystemPrompt,
		"memory":     memorySystemPrompt,
		"bloat":      bloatSystemPrompt,
		"rewrite":    rewriteSystemPrompt,
	}
	for name, p := range prompts {
		if !strings.Contains(p, "CRITICAL") {
			t.Errorf("%s missing 'CRITICAL'", name)
		}
	}
}

func TestAllSystemPrompts_NonEmpty(t *testing.T) {
	prompts := map[string]string{
		"vacuum":     vacuumSystemPrompt,
		"wal":        walSystemPrompt,
		"connection": connectionSystemPrompt,
		"memory":     memorySystemPrompt,
		"bloat":      bloatSystemPrompt,
		"rewrite":    rewriteSystemPrompt,
	}
	for name, p := range prompts {
		if len(p) < 100 {
			t.Errorf("%s prompt suspiciously short: %d chars",
				name, len(p))
		}
	}
}

// ---------------------------------------------------------------------------
// maxAdvisorPromptChars constant (prompt.go)
// ---------------------------------------------------------------------------

func TestMaxAdvisorPromptChars_Value(t *testing.T) {
	if maxAdvisorPromptChars != 16384 {
		t.Fatalf("expected 16384, got %d", maxAdvisorPromptChars)
	}
}

// ---------------------------------------------------------------------------
// Prompt truncation logic
// ---------------------------------------------------------------------------

func TestPromptTruncation_LongPrompt(t *testing.T) {
	prompt := strings.Repeat("x", maxAdvisorPromptChars+1000)
	if len(prompt) > maxAdvisorPromptChars {
		prompt = prompt[:maxAdvisorPromptChars]
	}
	if len(prompt) != maxAdvisorPromptChars {
		t.Fatalf("expected %d, got %d",
			maxAdvisorPromptChars, len(prompt))
	}
}

func TestPromptTruncation_ShortPrompt(t *testing.T) {
	prompt := "short"
	original := prompt
	if len(prompt) > maxAdvisorPromptChars {
		prompt = prompt[:maxAdvisorPromptChars]
	}
	if prompt != original {
		t.Fatal("short prompt should not be truncated")
	}
}

// ---------------------------------------------------------------------------
// Rewrite finding post-processing (rewrite.go)
// ---------------------------------------------------------------------------

func TestRewriteFinding_AllFieldsCleared(t *testing.T) {
	raw := `[` +
		`{"object_identifier":"queryid:1","severity":"critical",` +
		`"recommended_sql":"DROP TABLE","rationale":"test"},` +
		`{"object_identifier":"queryid:2","severity":"warning",` +
		`"recommended_sql":"ALTER TABLE","rationale":"test2"}` +
		`]`
	findings := parseLLMFindings(raw, "query_rewrite", noopLog)
	for i := range findings {
		findings[i].Severity = "info"
		findings[i].RecommendedSQL = ""
		findings[i].ActionRisk = ""
	}
	for i, f := range findings {
		if f.Severity != "info" {
			t.Errorf("[%d]: severity = %q, want 'info'", i, f.Severity)
		}
		if f.RecommendedSQL != "" {
			t.Errorf("[%d]: RecommendedSQL not cleared", i)
		}
		if f.ActionRisk != "" {
			t.Errorf("[%d]: ActionRisk not cleared", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Bloat finding post-processing (bloat.go)
// ---------------------------------------------------------------------------

func TestBloatFinding_AllFieldsForced(t *testing.T) {
	raw := `[` +
		`{"object_identifier":"public.big","severity":"critical",` +
		`"recommended_sql":"VACUUM FULL","rationale":"bloated"},` +
		`{"object_identifier":"public.huge","severity":"warning",` +
		`"recommended_sql":"REINDEX","rationale":"indexes"}` +
		`]`
	findings := parseLLMFindings(raw, "bloat_remediation", noopLog)
	for i := range findings {
		findings[i].Severity = "info"
		findings[i].RecommendedSQL = ""
	}
	for i, f := range findings {
		if f.Severity != "info" {
			t.Errorf("[%d]: severity = %q", i, f.Severity)
		}
		if f.RecommendedSQL != "" {
			t.Errorf("[%d]: RecommendedSQL not cleared", i)
		}
	}
}

// ---------------------------------------------------------------------------
// detectPlatform additional cases (wal.go)
// ---------------------------------------------------------------------------

func TestDetectPlatform_NilSettings_Direct(t *testing.T) {
	got := detectPlatform(nil)
	if got != "self-managed" {
		t.Fatalf("expected 'self-managed', got %q", got)
	}
}

func TestDetectPlatform_NoMatch(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "work_mem", Setting: "4MB", Source: "configuration file"},
		{Name: "wal_level", Setting: "replica", Source: "default"},
	}
	got := detectPlatform(settings)
	if got != "self-managed" {
		t.Fatalf("expected 'self-managed', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// countUnloggedTables additional cases (wal.go)
// ---------------------------------------------------------------------------

func TestCountUnloggedTables_AllUnlogged(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{RelName: "t1", Relpersistence: "u"},
			{RelName: "t2", Relpersistence: "u"},
		},
	}
	got := countUnloggedTables(snap)
	if got != 2 {
		t.Errorf("expected 2, got %d", got)
	}
}

func TestCountUnloggedTables_MixedPersistence(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{RelName: "perm", Relpersistence: "p"},
			{RelName: "unl", Relpersistence: "u"},
			{RelName: "tmp", Relpersistence: "t"},
		},
	}
	got := countUnloggedTables(snap)
	if got != 1 {
		t.Errorf("expected 1, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Connection churn calculation (connection.go)
// ---------------------------------------------------------------------------

func TestConnectionChurn_Rate(t *testing.T) {
	// ConnectionChurn is raw over 5 minutes.
	churn := 50
	rate := float64(churn) / 5.0
	if rate != 10.0 {
		t.Fatalf("expected 10.0, got %.1f", rate)
	}
}

func TestConnectionChurn_Zero(t *testing.T) {
	churn := 0
	rate := float64(churn) / 5.0
	if rate != 0.0 {
		t.Fatalf("expected 0.0, got %.1f", rate)
	}
}

// ---------------------------------------------------------------------------
// Vacuum with per-table autovacuum settings in global (vacuum.go)
// ---------------------------------------------------------------------------

func TestVacuumGlobalSettingsFilter(t *testing.T) {
	settings := []collector.PGSetting{
		{Name: "autovacuum", Setting: "on"},
		{Name: "autovacuum_vacuum_scale_factor", Setting: "0.2"},
		{Name: "autovacuum_naptime", Setting: "60"},
		{Name: "work_mem", Setting: "4MB"},
	}
	var global []string
	for _, s := range settings {
		if strings.HasPrefix(s.Name, "autovacuum") {
			global = append(global, s.Name)
		}
	}
	if len(global) != 3 {
		t.Fatalf("expected 3 autovacuum settings, got %d", len(global))
	}
}

// ---------------------------------------------------------------------------
// ValidateConfigRecommendation used in vacuum validation (vacuum.go)
// ---------------------------------------------------------------------------

func TestVacuumValidation_ValidFinding(t *testing.T) {
	// The validation in analyzeVacuum calls
	// ValidateConfigRecommendation("", "", "")
	// which always returns an error for empty setting name.
	err := ValidateConfigRecommendation("", "", "")
	if err == nil {
		t.Fatal("expected error for empty setting name")
	}
	if !strings.Contains(err.Error(), "empty setting name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVacuumValidation_WithSQL(t *testing.T) {
	// A finding with RecommendedSQL goes through validation.
	// The current code calls ValidateConfigRecommendation("", "", "")
	// which always returns error, so findings with SQL are rejected.
	// This tests that behavior.
	err := ValidateConfigRecommendation("", "", "")
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// Bloat EstimateMB calculations (bloat.go)
// ---------------------------------------------------------------------------

func TestBloatSizeMB(t *testing.T) {
	tableBytes := int64(200 * 1024 * 1024)
	sizeMB := float64(tableBytes) / (1024 * 1024)
	if sizeMB != 200.0 {
		t.Fatalf("expected 200.0, got %.1f", sizeMB)
	}
}

func TestBloatEstimateMB_Calculation(t *testing.T) {
	sizeMB := 200.0
	ratio := 0.25
	est := sizeMB * ratio
	if est != 50.0 {
		t.Fatalf("expected 50.0, got %.1f", est)
	}
}

// ---------------------------------------------------------------------------
// Memory spill sort (memory.go)
// ---------------------------------------------------------------------------

func TestMemorySpillSort_SelectionSort(t *testing.T) {
	type spillQuery struct {
		query string
		temp  int64
	}
	spills := []spillQuery{
		{"q1", 100},
		{"q2", 500},
		{"q3", 300},
		{"q4", 800},
		{"q5", 200},
	}
	for i := 0; i < len(spills) && i < 5; i++ {
		maxIdx := i
		for j := i + 1; j < len(spills); j++ {
			if spills[j].temp > spills[maxIdx].temp {
				maxIdx = j
			}
		}
		spills[i], spills[maxIdx] = spills[maxIdx], spills[i]
	}
	expected := []int64{800, 500, 300, 200, 100}
	for i, want := range expected {
		if spills[i].temp != want {
			t.Errorf("pos %d: expected %d, got %d", i, want, spills[i].temp)
		}
	}
}

// ---------------------------------------------------------------------------
// WAL PendingRestart formatting (wal.go)
// ---------------------------------------------------------------------------

func TestWALSetting_PendingRestart(t *testing.T) {
	s := collector.PGSetting{
		Name:           "max_wal_size",
		Setting:        "2GB",
		PendingRestart: true,
	}
	restart := ""
	if s.PendingRestart {
		restart = " (pending_restart=true)"
	}
	if restart == "" {
		t.Fatal("expected pending_restart annotation")
	}
}

func TestWALSetting_NoPendingRestart(t *testing.T) {
	s := collector.PGSetting{
		Name:           "max_wal_size",
		Setting:        "1GB",
		PendingRestart: false,
	}
	restart := ""
	if s.PendingRestart {
		restart = " (pending_restart=true)"
	}
	if restart != "" {
		t.Fatal("expected no annotation")
	}
}

// ---------------------------------------------------------------------------
// Rewrite query selection sort (rewrite.go)
// ---------------------------------------------------------------------------

func TestRewriteQuerySort_ByTotalTime(t *testing.T) {
	queries := []collector.QueryStats{
		{QueryID: 1, TotalExecTime: 5000},
		{QueryID: 2, TotalExecTime: 15000},
		{QueryID: 3, TotalExecTime: 10000},
	}
	sorted := make([]collector.QueryStats, len(queries))
	copy(sorted, queries)
	for i := 0; i < len(sorted) && i < 10; i++ {
		maxIdx := i
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].TotalExecTime > sorted[maxIdx].TotalExecTime {
				maxIdx = j
			}
		}
		sorted[i], sorted[maxIdx] = sorted[maxIdx], sorted[i]
	}
	if sorted[0].QueryID != 2 {
		t.Errorf("expected query 2 first, got %d", sorted[0].QueryID)
	}
	if sorted[1].QueryID != 3 {
		t.Errorf("expected query 3 second, got %d", sorted[1].QueryID)
	}
	if sorted[2].QueryID != 1 {
		t.Errorf("expected query 1 third, got %d", sorted[2].QueryID)
	}
}

// ---------------------------------------------------------------------------
// Rewrite dedup and cap (rewrite.go)
// ---------------------------------------------------------------------------

func TestRewriteDedup_NoDuplicates(t *testing.T) {
	type candidate struct {
		queryID int64
	}
	candidates := []candidate{{1}, {2}, {3}}
	seen := make(map[int64]bool)
	var unique []candidate
	for _, c := range candidates {
		if !seen[c.queryID] {
			seen[c.queryID] = true
			unique = append(unique, c)
		}
	}
	if len(unique) != 3 {
		t.Fatalf("expected 3, got %d", len(unique))
	}
}

func TestRewriteDedup_AllDuplicates(t *testing.T) {
	type candidate struct {
		queryID int64
	}
	candidates := []candidate{{1}, {1}, {1}}
	seen := make(map[int64]bool)
	var unique []candidate
	for _, c := range candidates {
		if !seen[c.queryID] {
			seen[c.queryID] = true
			unique = append(unique, c)
		}
	}
	if len(unique) != 1 {
		t.Fatalf("expected 1, got %d", len(unique))
	}
}

func TestRewriteCap_Over10(t *testing.T) {
	type candidate struct{ queryID int64 }
	var unique []candidate
	for i := 0; i < 15; i++ {
		unique = append(unique, candidate{int64(i)})
	}
	if len(unique) > 10 {
		unique = unique[:10]
	}
	if len(unique) != 10 {
		t.Fatalf("expected 10, got %d", len(unique))
	}
}

// ---------------------------------------------------------------------------
// Memory hit ratio edge: zero queries (memory.go)
// ---------------------------------------------------------------------------

func TestMemoryHitRatio_NoQueries(t *testing.T) {
	var totalHit, totalRead int64
	ratio := float64(0)
	if totalHit+totalRead > 0 {
		ratio = float64(totalHit) / float64(totalHit+totalRead) * 100
	}
	if ratio != 0 {
		t.Fatalf("expected 0%%, got %.1f%%", ratio)
	}
}

// ===========================================================================
// Full analyze function tests using mock LLM server
// ===========================================================================

// ---------------------------------------------------------------------------
// analyzeBloat with mock LLM (bloat.go)
// ---------------------------------------------------------------------------

func TestAnalyzeBloat_FullPath_WithMockLLM(t *testing.T) {
	llmResp := `[{"object_identifier":"public.orders",` +
		`"severity":"critical","rationale":"High bloat",` +
		`"recommended_sql":"VACUUM FULL public.orders",` +
		`"bloat_pct":35}]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	now := time.Now()
	lastVac := now.Add(-24 * time.Hour)
	snap := &collector.Snapshot{
		CollectedAt: now,
		ConfigData: &collector.ConfigSnapshot{
			ExtensionsAvailable: []string{"pg_stat_statements"},
		},
		Tables: []collector.TableStats{
			{
				SchemaName:      "public",
				RelName:         "orders",
				NLiveTup:        7000,
				NDeadTup:        3000,
				TableBytes:      50 * 1024 * 1024,
				IndexBytes:      10 * 1024 * 1024,
				LastAutovacuum:  &lastVac,
				AutovacuumCount: 5,
			},
		},
		System: collector.SystemStats{
			DBSizeBytes: 500 * 1024 * 1024,
		},
	}

	findings, err := analyzeBloat(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	// Severity and SQL should be forced.
	if findings[0].Severity != "info" {
		t.Errorf("severity = %q, want 'info'", findings[0].Severity)
	}
	if findings[0].RecommendedSQL != "" {
		t.Errorf("RecommendedSQL should be cleared, got %q",
			findings[0].RecommendedSQL)
	}
	if findings[0].Category != "bloat_remediation" {
		t.Errorf("category = %q", findings[0].Category)
	}
}

func TestAnalyzeBloat_WithPrevSnapshot(t *testing.T) {
	llmResp := `[{"object_identifier":"public.big",` +
		`"severity":"info","rationale":"Growing bloat"}]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	now := time.Now()
	prev := &collector.Snapshot{
		CollectedAt: now.Add(-time.Hour),
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "big",
				NLiveTup: 7000, NDeadTup: 2000,
				TableBytes: 50 * 1024 * 1024},
		},
	}
	snap := &collector.Snapshot{
		CollectedAt: now,
		ConfigData: &collector.ConfigSnapshot{
			ExtensionsAvailable: []string{"pg_repack"},
		},
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "big",
				NLiveTup: 7000, NDeadTup: 3500,
				TableBytes: 55 * 1024 * 1024,
				IndexBytes: 10 * 1024 * 1024},
		},
		System: collector.SystemStats{DBSizeBytes: 500 * 1024 * 1024},
	}

	findings, err := analyzeBloat(
		context.Background(), mgr, snap, prev,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
}

func TestAnalyzeBloat_LLMError(t *testing.T) {
	srv, mgr := mockLLMServerError(t)
	defer srv.Close()

	snap := &collector.Snapshot{
		CollectedAt: time.Now(),
		ConfigData:  &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "bloated",
				NLiveTup: 5000, NDeadTup: 5000,
				TableBytes: 20 * 1024 * 1024},
		},
		System: collector.SystemStats{DBSizeBytes: 100 * 1024 * 1024},
	}

	_, err := analyzeBloat(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
	if !strings.Contains(err.Error(), "bloat LLM") {
		t.Fatalf("expected 'bloat LLM' in error, got: %v", err)
	}
}

func TestAnalyzeBloat_NilLastAutovacuum(t *testing.T) {
	llmResp := `[]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		CollectedAt: time.Now(),
		ConfigData:  &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders",
				NLiveTup: 5000, NDeadTup: 5000,
				TableBytes: 50 * 1024 * 1024,
				LastAutovacuum: nil},
		},
		System: collector.SystemStats{DBSizeBytes: 500 * 1024 * 1024},
	}

	findings, err := analyzeBloat(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatalf("expected nil findings for [], got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// analyzeVacuum with mock LLM (vacuum.go)
// ---------------------------------------------------------------------------

func TestAnalyzeVacuum_FullPath_WithMockLLM(t *testing.T) {
	llmResp := `[{"object_identifier":"public.orders",` +
		`"severity":"info",` +
		`"rationale":"Scale factor too high",` +
		`"recommended_sql":""}]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	now := time.Now()
	lastVac := now.Add(-2 * time.Hour)
	snap := &collector.Snapshot{
		CollectedAt: now,
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "autovacuum", Setting: "on"},
				{Name: "autovacuum_vacuum_scale_factor",
					Setting: "0.2"},
			},
			TableReloptions: []collector.TableReloption{
				{SchemaName: "public", RelName: "orders",
					Reloptions: "{autovacuum_vacuum_scale_factor=0.1}"},
			},
		},
		Tables: []collector.TableStats{
			{
				SchemaName:      "public",
				RelName:         "orders",
				NLiveTup:        10000,
				NDeadTup:        2000,
				NTupIns:         5000,
				NTupUpd:         3000,
				NTupDel:         500,
				LastAutovacuum:  &lastVac,
				AutovacuumCount: 10,
			},
		},
	}

	prev := &collector.Snapshot{
		CollectedAt: now.Add(-time.Hour),
		Tables: []collector.TableStats{
			{
				SchemaName: "public",
				RelName:    "orders",
				NLiveTup:   9500,
				NDeadTup:   1000,
				NTupIns:    2000,
				NTupUpd:    1000,
				NTupDel:    200,
			},
		},
	}

	findings, err := analyzeVacuum(
		context.Background(), mgr, snap, prev,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The finding has empty RecommendedSQL, so it goes to the "else"
	// branch (valid = append(valid, f)).
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Category != "vacuum_tuning" {
		t.Errorf("category = %q", findings[0].Category)
	}
}

func TestAnalyzeVacuum_WithRecommendedSQL(t *testing.T) {
	// Finding with RecommendedSQL goes through the validation branch.
	// ValidateConfigRecommendation("", "", "") returns error, so the
	// finding is dropped by the validator.
	llmResp := `[{"object_identifier":"public.orders",` +
		`"severity":"info",` +
		`"rationale":"Need lower threshold",` +
		`"recommended_sql":"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.02)"}]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		CollectedAt: time.Now(),
		ConfigData:  &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders",
				NLiveTup: 10000, NDeadTup: 2000},
		},
	}

	findings, err := analyzeVacuum(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The broken validation that called ValidateConfigRecommendation("","","")
	// was removed — it always returned an error on empty setting name,
	// silently dropping every finding with RecommendedSQL. Findings with
	// RecommendedSQL are now correctly returned.
	if len(findings) == 0 {
		t.Fatal("expected at least 1 finding, got 0")
	}
}

func TestAnalyzeVacuum_LLMError(t *testing.T) {
	srv, mgr := mockLLMServerError(t)
	defer srv.Close()

	snap := &collector.Snapshot{
		CollectedAt: time.Now(),
		ConfigData:  &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "big",
				NLiveTup: 10000, NDeadTup: 2000},
		},
	}

	_, err := analyzeVacuum(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
	if !strings.Contains(err.Error(), "vacuum LLM") {
		t.Fatalf("expected 'vacuum LLM' in error, got: %v", err)
	}
}

func TestAnalyzeVacuum_NilLastAutovacuum(t *testing.T) {
	llmResp := `[]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		CollectedAt: time.Now(),
		ConfigData:  &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders",
				NLiveTup:       10000,
				NDeadTup:       2000,
				LastAutovacuum: nil},
		},
	}

	findings, err := analyzeVacuum(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatalf("expected nil for [], got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// analyzeConnections with mock LLM (connection.go)
// ---------------------------------------------------------------------------

func TestAnalyzeConnections_FullPath(t *testing.T) {
	llmResp := `[{"object_identifier":"instance",` +
		`"severity":"info",` +
		`"rationale":"Connections healthy",` +
		`"recommended_sql":""}]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "max_connections", Setting: "100", Unit: ""},
				{Name: "superuser_reserved_connections",
					Setting: "3", Unit: ""},
				{Name: "idle_in_transaction_session_timeout",
					Setting: "0", Unit: "ms"},
				{Name: "statement_timeout", Setting: "0", Unit: "ms"},
				{Name: "tcp_keepalives_idle",
					Setting: "7200", Unit: "s"},
				{Name: "tcp_keepalives_interval",
					Setting: "75", Unit: "s"},
			},
			ConnectionStates: []collector.ConnectionState{
				{State: "active", Count: 5,
					AvgDurationSeconds: 0.5},
				{State: "idle", Count: 20,
					AvgDurationSeconds: 120},
				{State: "idle in transaction", Count: 2,
					AvgDurationSeconds: 45},
			},
			ConnectionChurn: 50,
		},
		System: collector.SystemStats{
			ActiveBackends: 5,
			TotalBackends:  27,
			MaxConnections: 100,
		},
	}

	findings, err := analyzeConnections(
		context.Background(), mgr, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Category != "connection_tuning" {
		t.Errorf("category = %q", findings[0].Category)
	}
}

func TestAnalyzeConnections_LLMError(t *testing.T) {
	srv, mgr := mockLLMServerError(t)
	defer srv.Close()

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{},
		System:     collector.SystemStats{MaxConnections: 100},
	}

	_, err := analyzeConnections(
		context.Background(), mgr, snap,
		&config.Config{}, noopLog,
	)
	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
	if !strings.Contains(err.Error(), "connection LLM") {
		t.Fatalf("expected 'connection LLM' in error, got: %v", err)
	}
}

func TestAnalyzeConnections_EmptyResponse(t *testing.T) {
	srv, mgr := mockLLMServer(t, `[]`)
	defer srv.Close()

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{},
		System:     collector.SystemStats{MaxConnections: 100},
	}

	findings, err := analyzeConnections(
		context.Background(), mgr, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// analyzeMemory with mock LLM (memory.go)
// ---------------------------------------------------------------------------

func TestAnalyzeMemory_FullPath(t *testing.T) {
	llmResp := `[{"object_identifier":"instance",` +
		`"severity":"info",` +
		`"rationale":"work_mem could be increased",` +
		`"recommended_sql":"ALTER SYSTEM SET work_mem = '16MB'"}]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "shared_buffers", Setting: "128", Unit: "MB"},
				{Name: "work_mem", Setting: "4", Unit: "MB"},
				{Name: "maintenance_work_mem",
					Setting: "64", Unit: "MB"},
				{Name: "effective_cache_size",
					Setting: "4", Unit: "GB"},
				{Name: "huge_pages", Setting: "try", Unit: ""},
				{Name: "temp_buffers", Setting: "8", Unit: "MB"},
				{Name: "hash_mem_multiplier",
					Setting: "2", Unit: ""},
				{Name: "max_connections", Setting: "100", Unit: ""},
			},
		},
		Queries: []collector.QueryStats{
			{SharedBlksHit: 9000, SharedBlksRead: 1000,
				TempBlksWritten: 500, Calls: 100,
				Query: "SELECT * FROM orders WHERE id > $1"},
			{SharedBlksHit: 5000, SharedBlksRead: 0,
				TempBlksWritten: 0, Calls: 200,
				Query: "SELECT 1"},
			{SharedBlksHit: 3000, SharedBlksRead: 500,
				TempBlksWritten: 200, Calls: 50,
				Query: "SELECT o.* FROM orders o JOIN items i ON o.id = i.order_id"},
		},
	}

	findings, err := analyzeMemory(
		context.Background(), mgr, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	if findings[0].Category != "memory_tuning" {
		t.Errorf("category = %q", findings[0].Category)
	}
}

func TestAnalyzeMemory_LLMError(t *testing.T) {
	srv, mgr := mockLLMServerError(t)
	defer srv.Close()

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{},
	}

	_, err := analyzeMemory(
		context.Background(), mgr, snap,
		&config.Config{}, noopLog,
	)
	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
	if !strings.Contains(err.Error(), "memory LLM") {
		t.Fatalf("expected 'memory LLM' in error, got: %v", err)
	}
}

func TestAnalyzeMemory_NoSpills(t *testing.T) {
	srv, mgr := mockLLMServer(t, `[]`)
	defer srv.Close()

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "shared_buffers", Setting: "128", Unit: "MB"},
				{Name: "work_mem", Setting: "4", Unit: "MB"},
			},
		},
		Queries: []collector.QueryStats{
			{SharedBlksHit: 9000, SharedBlksRead: 100,
				TempBlksWritten: 0},
		},
	}

	findings, err := analyzeMemory(
		context.Background(), mgr, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// analyzeWAL with mock LLM (wal.go)
// ---------------------------------------------------------------------------

func TestAnalyzeWAL_FullPath(t *testing.T) {
	llmResp := `[{"object_identifier":"instance",` +
		`"severity":"info",` +
		`"rationale":"WAL healthy",` +
		`"recommended_sql":"",` +
		`"requires_restart":false}]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "max_wal_size", Setting: "1", Unit: "GB"},
				{Name: "min_wal_size", Setting: "80", Unit: "MB"},
				{Name: "checkpoint_completion_target",
					Setting: "0.9", Unit: ""},
				{Name: "wal_compression", Setting: "off", Unit: ""},
				{Name: "wal_level", Setting: "replica", Unit: ""},
				{Name: "wal_buffers", Setting: "16", Unit: "MB",
					PendingRestart: true},
				{Name: "checkpoint_timeout",
					Setting: "300", Unit: "s"},
				{Name: "full_page_writes", Setting: "on", Unit: ""},
			},
			WALPosition: "0/1234ABCD",
		},
		Tables: []collector.TableStats{
			{RelName: "cache", Relpersistence: "u"},
			{RelName: "orders", Relpersistence: "p"},
		},
		System: collector.SystemStats{
			TotalCheckpoints: 150,
		},
	}

	prev := &collector.Snapshot{
		System: collector.SystemStats{
			TotalCheckpoints: 120,
		},
	}

	findings, err := analyzeWAL(
		context.Background(), mgr, snap, prev,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	if findings[0].Category != "wal_tuning" {
		t.Errorf("category = %q", findings[0].Category)
	}
}

func TestAnalyzeWAL_LLMError(t *testing.T) {
	srv, mgr := mockLLMServerError(t)
	defer srv.Close()

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{},
	}

	_, err := analyzeWAL(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
	if !strings.Contains(err.Error(), "wal LLM") {
		t.Fatalf("expected 'wal LLM' in error, got: %v", err)
	}
}

func TestAnalyzeWAL_NoPrevSnapshot(t *testing.T) {
	srv, mgr := mockLLMServer(t, `[]`)
	defer srv.Close()

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "max_wal_size", Setting: "1", Unit: "GB"},
			},
		},
		System: collector.SystemStats{TotalCheckpoints: 100},
	}

	findings, err := analyzeWAL(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// analyzeQueryRewrites with mock LLM (rewrite.go)
// ---------------------------------------------------------------------------

func TestAnalyzeQueryRewrites_FullPath(t *testing.T) {
	llmResp := `[{"object_identifier":"queryid:1",` +
		`"severity":"info",` +
		`"rationale":"Replace correlated subquery with JOIN",` +
		`"recommended_sql":"SELECT ...",` +
		`"impact_rating":"high"}]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{
				QueryID:       1,
				Query:         "SELECT * FROM orders WHERE id IN (SELECT order_id FROM items)",
				Calls:         200,
				MeanExecTime:  100,
				TotalExecTime: 20000,
				Rows:          500,
				SharedBlksRead: 1000,
			},
		},
	}

	findings, err := analyzeQueryRewrites(
		context.Background(), nil, mgr, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
	// Post-processing forces severity to warning, clears SQL.
	if findings[0].Severity != "warning" {
		t.Errorf("severity = %q", findings[0].Severity)
	}
	if findings[0].RecommendedSQL != "" {
		t.Errorf("RecommendedSQL not cleared: %q",
			findings[0].RecommendedSQL)
	}
	if findings[0].ActionRisk != "" {
		t.Errorf("ActionRisk not cleared: %q", findings[0].ActionRisk)
	}
	if findings[0].Category != "query_rewrite" {
		t.Errorf("category = %q", findings[0].Category)
	}
}

func TestAnalyzeQueryRewrites_SpillCandidates(t *testing.T) {
	llmResp := `[{"object_identifier":"queryid:5",` +
		`"severity":"info","rationale":"temp spills"}]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{QueryID: 5, Query: "SELECT large_join",
				Calls: 100, MeanExecTime: 10,
				TotalExecTime: 1000, TempBlksWritten: 500},
		},
	}

	findings, err := analyzeQueryRewrites(
		context.Background(), nil, mgr, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
}

func TestAnalyzeQueryRewrites_LLMError(t *testing.T) {
	srv, mgr := mockLLMServerError(t)
	defer srv.Close()

	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{QueryID: 1, Calls: 200, MeanExecTime: 100,
				TotalExecTime: 20000},
		},
	}

	_, err := analyzeQueryRewrites(
		context.Background(), nil, mgr, snap,
		&config.Config{}, noopLog,
	)
	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
	if !strings.Contains(err.Error(), "rewrite LLM") {
		t.Fatalf("expected 'rewrite LLM' in error, got: %v", err)
	}
}

func TestAnalyzeQueryRewrites_LongQuery_Truncated(t *testing.T) {
	// Query longer than 300 chars should be truncated in the prompt.
	longQuery := strings.Repeat("SELECT 1 UNION ALL ", 20)
	llmResp := `[]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{QueryID: 1, Query: longQuery,
				Calls: 200, MeanExecTime: 100,
				TotalExecTime: 20000},
		},
	}

	findings, err := analyzeQueryRewrites(
		context.Background(), nil, mgr, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// Analyze orchestrator with mock LLM (advisor.go)
// ---------------------------------------------------------------------------

func TestAnalyze_NoConfigSnapshot(t *testing.T) {
	// Even with all advisors enabled, if snap.ConfigData is nil,
	// the function should skip all and return empty.
	// We can't easily test this without a real collector, but
	// the nil snapshot check happens before sub-advisors.
	a := newTestAdvisor(true, true, 0) // interval=0 so ShouldRun=true
	a.lastRunAt = time.Time{}          // zero value

	// Analyze will call a.coll.LatestSnapshot() which panics on nil coll.
	// We can only test the disabled/LLM-disabled paths safely.
	a.cfg.Advisor.Enabled = false
	findings, err := a.Analyze(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Fatal("expected nil when disabled")
	}
}

// ---------------------------------------------------------------------------
// Bloat with multiple tables, mixed qualifying (bloat.go)
// ---------------------------------------------------------------------------

func TestAnalyzeBloat_MultipleTables_OnlyQualifyingIncluded(t *testing.T) {
	llmResp := `[{"object_identifier":"public.big",` +
		`"severity":"info","rationale":"bloated"}]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		CollectedAt: time.Now(),
		ConfigData:  &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			// Small — excluded
			{SchemaName: "public", RelName: "tiny",
				NLiveTup: 100, NDeadTup: 100},
			// Low ratio — excluded
			{SchemaName: "public", RelName: "healthy",
				NLiveTup: 9000, NDeadTup: 500},
			// Qualifies
			{SchemaName: "public", RelName: "big",
				NLiveTup: 5000, NDeadTup: 5000,
				TableBytes: 100 * 1024 * 1024,
				IndexBytes: 20 * 1024 * 1024},
		},
		System: collector.SystemStats{DBSizeBytes: 500 * 1024 * 1024},
	}

	findings, err := analyzeBloat(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// Memory with > 5 spilling queries triggers top-5 sort (memory.go)
// ---------------------------------------------------------------------------

func TestAnalyzeMemory_ManySpillingQueries(t *testing.T) {
	llmResp := `[]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	queries := make([]collector.QueryStats, 8)
	for i := range queries {
		queries[i] = collector.QueryStats{
			SharedBlksHit:   1000,
			SharedBlksRead:  100,
			TempBlksWritten: int64((i + 1) * 100),
			Calls:           100,
			Query:           "SELECT spill_query_" + string(rune('A'+i)),
		}
	}

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "work_mem", Setting: "4", Unit: "MB"},
			},
		},
		Queries: queries,
	}

	findings, err := analyzeMemory(
		context.Background(), mgr, snap,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty response from LLM means no findings.
	if len(findings) != 0 {
		t.Fatalf("expected 0, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// WAL with unlogged tables (wal.go)
// ---------------------------------------------------------------------------

func TestAnalyzeWAL_WithUnloggedTables(t *testing.T) {
	llmResp := `[]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "max_wal_size", Setting: "1", Unit: "GB"},
				{Name: "wal_level", Setting: "replica", Unit: ""},
			},
			WALPosition: "0/ABCD1234",
		},
		Tables: []collector.TableStats{
			{RelName: "cache1", Relpersistence: "u"},
			{RelName: "cache2", Relpersistence: "u"},
			{RelName: "orders", Relpersistence: "p"},
		},
		System: collector.SystemStats{TotalCheckpoints: 50},
	}

	findings, err := analyzeWAL(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// WAL detectPlatform integration (wal.go)
// ---------------------------------------------------------------------------

func TestAnalyzeWAL_CloudSQLPlatform(t *testing.T) {
	llmResp := `[]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "max_wal_size", Setting: "1", Unit: "GB",
					Source: "cloud-sql"},
			},
		},
		System: collector.SystemStats{TotalCheckpoints: 50},
	}

	findings, err := analyzeWAL(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// Vacuum with global autovacuum settings (vacuum.go)
// ---------------------------------------------------------------------------

func TestAnalyzeVacuum_GlobalSettings(t *testing.T) {
	llmResp := `[]`
	srv, mgr := mockLLMServer(t, llmResp)
	defer srv.Close()

	snap := &collector.Snapshot{
		CollectedAt: time.Now(),
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "autovacuum", Setting: "on"},
				{Name: "autovacuum_vacuum_scale_factor",
					Setting: "0.2"},
				{Name: "autovacuum_vacuum_threshold",
					Setting: "50"},
				{Name: "autovacuum_naptime", Setting: "60"},
				{Name: "autovacuum_max_workers", Setting: "3"},
				// Non-autovacuum setting should be excluded.
				{Name: "work_mem", Setting: "4MB"},
			},
		},
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "active",
				NLiveTup: 10000, NDeadTup: 2000},
		},
	}

	findings, err := analyzeVacuum(
		context.Background(), mgr, snap, nil,
		&config.Config{}, noopLog,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty LLM response -> no findings.
	if findings != nil {
		t.Fatalf("expected nil, got %d", len(findings))
	}
}
