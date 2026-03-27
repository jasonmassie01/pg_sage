package optimizer

import (
	"strings"
	"testing"
)

func TestParseRecommendations_InvalidJSON(t *testing.T) {
	_, err := parseRecommendations("not json at all")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseRecommendations_JSONObject(t *testing.T) {
	_, err := parseRecommendations(`{}`)
	if err == nil {
		t.Fatal("expected error for JSON object instead of array")
	}
}

func TestParseRecommendations_MultipleRecs(t *testing.T) {
	input := `[
		{"table":"t1","ddl":"CREATE INDEX ...","rationale":"r1","severity":"high","index_type":"btree","category":"missing","estimated_improvement_pct":20},
		{"table":"t2","ddl":"CREATE INDEX ...","rationale":"r2","severity":"low","index_type":"hash","category":"unused","estimated_improvement_pct":5}
	]`
	recs, err := parseRecommendations(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 recs, got %d", len(recs))
	}
	if recs[0].Table != "t1" {
		t.Errorf("expected table t1, got %s", recs[0].Table)
	}
	if recs[1].Table != "t2" {
		t.Errorf("expected table t2, got %s", recs[1].Table)
	}
}

func TestParseRecommendations_OptionalFieldsMissing(t *testing.T) {
	input := `[{"table":"t","ddl":"d","rationale":"r","severity":"s"}]`
	recs, err := parseRecommendations(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
	if recs[0].DropDDL != "" {
		t.Errorf("expected empty DropDDL, got %s", recs[0].DropDDL)
	}
}

func TestParseRecommendations_PlainMarkdownFence(t *testing.T) {
	input := "```\n[]\n```"
	recs, err := parseRecommendations(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recs != nil {
		t.Fatalf("expected nil for empty array, got %v", recs)
	}
}

func TestParseRecommendations_NestedJSONFence(t *testing.T) {
	input := "```json\n" +
		`[{"table":"t","ddl":"d","rationale":"r","severity":"s","index_type":"btree","category":"c","estimated_improvement_pct":10}]` +
		"\n```"
	recs, err := parseRecommendations(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
}

func TestParseRecommendations_WhitespaceOnly(t *testing.T) {
	recs, err := parseRecommendations("   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recs != nil {
		t.Fatalf("expected nil for whitespace-only, got %v", recs)
	}
}

func TestParseRecommendations_LongResponse(t *testing.T) {
	longRationale := strings.Repeat("a", 200)
	input := `[{"table":"t","ddl":"CREATE INDEX idx ON t(col)","rationale":"` +
		longRationale +
		`","severity":"medium","index_type":"btree","category":"missing","estimated_improvement_pct":15}]`
	recs, err := parseRecommendations(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 rec, got %d", len(recs))
	}
}

func TestStripMarkdownFences_NoFences(t *testing.T) {
	input := `[{"a":1}]`
	got := stripMarkdownFences(input)
	if got != input {
		t.Errorf("expected %q, got %q", input, got)
	}
}

func TestStripMarkdownFences_JSONFence(t *testing.T) {
	input := "```json\n[]\n```"
	got := stripMarkdownFences(input)
	if got != "[]" {
		t.Errorf("expected %q, got %q", "[]", got)
	}
}

func TestStripMarkdownFences_PlainFence(t *testing.T) {
	input := "```\n[]\n```"
	got := stripMarkdownFences(input)
	if got != "[]" {
		t.Errorf("expected %q, got %q", "[]", got)
	}
}

func TestStripMarkdownFences_OpeningOnly(t *testing.T) {
	input := "```json\n[]"
	got := stripMarkdownFences(input)
	if got != "[]" {
		t.Errorf("expected %q, got %q", "[]", got)
	}
}

func TestFormatPrompt_Header(t *testing.T) {
	tc := TableContext{
		Schema: "public",
		Table:  "orders",
	}
	out := FormatPrompt(tc)
	if !strings.Contains(out, "## Table: public.orders") {
		t.Errorf("expected header with schema.table, got:\n%s", out)
	}
}

func TestFormatPrompt_ColStats(t *testing.T) {
	tc := TableContext{
		Schema: "public",
		Table:  "orders",
		ColStats: []ColStat{
			{
				Column:    "status",
				NDistinct: 5,
			},
		},
	}
	out := FormatPrompt(tc)
	if !strings.Contains(out, "### Column Statistics") {
		t.Errorf("expected Column Statistics section, got:\n%s", out)
	}
}

func TestFormatPrompt_Plans(t *testing.T) {
	tc := TableContext{
		Schema: "public",
		Table:  "orders",
		Plans: []PlanSummary{
			{
				QueryID:  1,
				Summary:  "Seq Scan on orders",
				ScanType: "Seq Scan",
			},
		},
	}
	out := FormatPrompt(tc)
	if !strings.Contains(out, "### Execution Plans") {
		t.Errorf("expected Execution Plans section, got:\n%s", out)
	}
}

func TestFormatPrompt_Collation(t *testing.T) {
	tc := TableContext{
		Schema:    "public",
		Table:     "orders",
		Collation: "en_US.UTF-8",
	}
	out := FormatPrompt(tc)
	if !strings.Contains(out, "en_US.UTF-8") {
		t.Errorf("expected collation in output, got:\n%s", out)
	}
}

func TestHumanBytes_Zero(t *testing.T) {
	got := humanBytes(0)
	if got != "0 B" {
		t.Errorf("expected %q, got %q", "0 B", got)
	}
}

func TestHumanBytes_Negative(t *testing.T) {
	got := humanBytes(-5)
	if got != "-5 B" {
		t.Errorf("expected %q, got %q", "-5 B", got)
	}
}

func TestSystemPrompt_ContainsAllRules(t *testing.T) {
	prompt := SystemPrompt()
	rules := []string{
		"CONCURRENTLY",
		"partial indexes",
		"INCLUDE",
		"GIN",
		"already exists",
		"write-heavy",
		"correlation",
		"collation",
		"INCLUDE column count",
		"Maximum 10 indexes",
		"work_mem",
		"materialized view",
		"non-leading position",
	}
	for _, rule := range rules {
		if !strings.Contains(prompt, rule) {
			t.Errorf("SystemPrompt missing rule keyword: %q", rule)
		}
	}
}

func TestStripToJSON_ThinkingPrefix(t *testing.T) {
	input := "Let me think about this...\n\n" + `[{"table":"t","ddl":"d","rationale":"r","severity":"s"}]`
	got := stripToJSON(input)
	want := `[{"table":"t","ddl":"d","rationale":"r","severity":"s"}]`
	if got != want {
		t.Errorf("stripToJSON thinking prefix:\ngot:  %s\nwant: %s", got, want)
	}
}

func TestStripToJSON_MarkdownFencedJSON(t *testing.T) {
	input := "```json\n[{\"ddl\":\"d\"}]\n```"
	got := stripToJSON(input)
	want := "[{\"ddl\":\"d\"}]"
	if got != want {
		t.Errorf("stripToJSON fenced:\ngot:  %s\nwant: %s", got, want)
	}
}

func TestStripToJSON_CleanJSON(t *testing.T) {
	input := `[{"ddl":"d"}]`
	got := stripToJSON(input)
	if got != input {
		t.Errorf("stripToJSON clean:\ngot:  %s\nwant: %s", got, input)
	}
}

func TestStripToJSON_TruncatedJSON(t *testing.T) {
	input := `[{"ddl":"CREATE INDEX`
	got := stripToJSON(input)
	// Should still extract from [ to end, even without closing ]
	// Actually there's no ], so it falls through to stripMarkdownFences
	// which returns the trimmed input
	if got != input {
		t.Errorf("stripToJSON truncated:\ngot:  %s\nwant: %s", got, input)
	}
}

func TestFormatPrompt_ResponseDirective(t *testing.T) {
	tc := TableContext{
		Schema:  "public",
		Table:   "orders",
		Queries: []QueryInfo{{QueryID: 1, Text: "SELECT 1", Calls: 1}},
	}
	out := FormatPrompt(tc)
	if !strings.Contains(out, "RESPOND NOW") {
		t.Error("FormatPrompt should contain RESPOND NOW directive")
	}
}

func TestSystemPrompt_AntiThinking(t *testing.T) {
	prompt := SystemPrompt()
	if !strings.Contains(prompt, "No thinking, no reasoning") {
		t.Error("SystemPrompt should contain anti-thinking directive")
	}
	// Anti-thinking should come before rules
	thinkIdx := strings.Index(prompt, "No thinking")
	rulesIdx := strings.Index(prompt, "Rules:")
	if thinkIdx > rulesIdx {
		t.Error("Anti-thinking directive should come before rules")
	}
}

func TestFormatPrompt_UnloggedTable(t *testing.T) {
	tc := TableContext{
		Schema:         "public",
		Table:          "cache_data",
		Relpersistence: "u",
		Queries:        []QueryInfo{{QueryID: 1, Text: "SELECT 1", Calls: 1}},
	}
	prompt := FormatPrompt(tc)
	if !strings.Contains(prompt, "UNLOGGED TABLE") {
		t.Error("prompt should contain UNLOGGED TABLE note for relpersistence=u")
	}
	if !strings.Contains(prompt, "crash-unsafe") {
		t.Error("prompt should mention crash-unsafe for unlogged table")
	}
}

func TestFormatPrompt_PermanentTableNoUnloggedNote(t *testing.T) {
	tc := TableContext{
		Schema:         "public",
		Table:          "orders",
		Relpersistence: "p",
		Queries:        []QueryInfo{{QueryID: 1, Text: "SELECT 1", Calls: 1}},
	}
	prompt := FormatPrompt(tc)
	if strings.Contains(prompt, "UNLOGGED") {
		t.Error("prompt should not contain UNLOGGED for permanent table")
	}
}

func TestFormatPrompt_CollationConditional(t *testing.T) {
	// Non-C collation should include warning
	tc := TableContext{
		Schema: "public", Table: "t",
		Collation: "en_US.UTF-8",
		Queries:   []QueryInfo{{QueryID: 1, Text: "SELECT 1", Calls: 1}},
	}
	prompt := FormatPrompt(tc)
	if !strings.Contains(prompt, "non-C") {
		t.Error("non-C collation should include pattern_ops warning")
	}

	// C collation should NOT include warning
	tc.Collation = "C"
	prompt = FormatPrompt(tc)
	if strings.Contains(prompt, "non-C") {
		t.Error("C collation should not include pattern_ops warning")
	}

	// POSIX collation should NOT include warning
	tc.Collation = "POSIX"
	prompt = FormatPrompt(tc)
	if strings.Contains(prompt, "non-C") {
		t.Error("POSIX collation should not include pattern_ops warning")
	}
}

func TestSystemPrompt_CompositeIndexColumnOrdering(t *testing.T) {
	prompt := SystemPrompt()

	required := []string{
		"Composite index column order matters",
		"B-tree on (a, b) only helps queries that filter on",
		"non-leading position",
		"recommend a new single-column index",
		"Do not assume an index on (a, b) covers WHERE b = ?",
	}
	for _, phrase := range required {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("SystemPrompt missing composite index guidance: %q", phrase)
		}
	}
}

func TestFormatPrompt_IndexDefinitionsIncluded(t *testing.T) {
	tc := TableContext{
		Schema: "public",
		Table:  "orders",
		Indexes: []IndexInfo{
			{
				Name:       "idx_orders_user_status",
				Definition: "CREATE INDEX idx_orders_user_status ON public.orders USING btree (user_id, status)",
				Scans:      100,
			},
		},
		Queries: []QueryInfo{{QueryID: 1, Text: "SELECT * FROM orders WHERE status = 'active'", Calls: 50}},
	}
	prompt := FormatPrompt(tc)

	if !strings.Contains(prompt, "### Existing Indexes") {
		t.Error("prompt should include Existing Indexes section")
	}
	if !strings.Contains(prompt, "idx_orders_user_status") {
		t.Error("prompt should include index name")
	}
	if !strings.Contains(prompt, "(user_id, status)") {
		t.Error("prompt should include index column order from definition")
	}
}
