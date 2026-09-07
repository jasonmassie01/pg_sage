package lint

import (
	"context"
	"testing"
	"time"
)

func TestParseLLMJsonbResponse_ValidJSON(t *testing.T) {
	raw := `[
		{"schema":"public","table":"events","column":"payload",
		 "used_in":"where","query_snippet":"payload->>'type' = 'click'"},
		{"schema":"app","table":"logs","column":"data",
		 "used_in":"both","query_snippet":"data @> '{\"level\":\"error\"}'"}
	]`
	matches, err := parseLLMJsonbResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	if matches[0].Schema != "public" || matches[0].Table != "events" {
		t.Errorf("match[0] = %+v, want public.events", matches[0])
	}
	if matches[1].UsedIn != "both" {
		t.Errorf("match[1].UsedIn = %q, want 'both'", matches[1].UsedIn)
	}
}

func TestParseLLMJsonbResponse_MarkdownWrapped(t *testing.T) {
	raw := "```json\n" +
		`[{"schema":"public","table":"orders","column":"meta",` +
		`"used_in":"join","query_snippet":"meta->>'customer_id'"}]` +
		"\n```"
	matches, err := parseLLMJsonbResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Column != "meta" {
		t.Errorf("column = %q, want 'meta'", matches[0].Column)
	}
}

func TestParseLLMJsonbResponse_EmptyArray(t *testing.T) {
	matches, err := parseLLMJsonbResponse("[]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(matches))
	}
}

func TestParseLLMJsonbResponse_EmptyArrayWithFences(t *testing.T) {
	raw := "```json\n[]\n```"
	matches, err := parseLLMJsonbResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(matches))
	}
}

func TestParseLLMJsonbResponse_InvalidJSON(t *testing.T) {
	_, err := parseLLMJsonbResponse("not json at all")
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestParseLLMJsonbResponse_LeadingText(t *testing.T) {
	raw := "Here are the results:\n" +
		`[{"schema":"public","table":"t","column":"c",` +
		`"used_in":"where","query_snippet":"c->>'k'"}]`
	matches, err := parseLLMJsonbResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
}

func TestEnhance_NilLLMClient(t *testing.T) {
	analyzer := &LLMJsonbAnalyzer{
		pool:      nil,
		llmClient: nil,
		logFn:     func(string, string, ...any) {},
	}
	now := time.Now()
	findings := []Finding{
		{
			RuleID:      "lint_jsonb_in_joins",
			Schema:      "public",
			Table:       "events",
			Column:      "payload",
			Severity:    "warning",
			Category:    "performance",
			Description: "JSONB column public.events.payload has no GIN/GiST index",
			FirstSeen:   now,
			LastSeen:    now,
		},
		{
			RuleID:      "lint_missing_fk_index",
			Schema:      "public",
			Table:       "orders",
			Severity:    "warning",
			Category:    "performance",
			Description: "Missing FK index",
			FirstSeen:   now,
			LastSeen:    now,
		},
	}

	result := analyzer.Enhance(context.Background(), findings)
	if len(result) != len(findings) {
		t.Fatalf("expected %d findings unchanged, got %d",
			len(findings), len(result))
	}
	for i := range findings {
		if result[i].Description != findings[i].Description {
			t.Errorf("finding[%d] description changed: %q -> %q",
				i, findings[i].Description, result[i].Description)
		}
	}
}

func TestExtractJsonbFindings(t *testing.T) {
	analyzer := &LLMJsonbAnalyzer{
		logFn: func(string, string, ...any) {},
	}
	now := time.Now()
	findings := []Finding{
		{
			RuleID: "lint_jsonb_in_joins", Schema: "public",
			Table: "events", Column: "payload",
			FirstSeen: now, LastSeen: now,
		},
		{
			RuleID: "lint_missing_fk_index", Schema: "public",
			Table: "orders",
			FirstSeen: now, LastSeen: now,
		},
		{
			RuleID: "lint_jsonb_in_joins", Schema: "app",
			Table: "logs", Column: "data",
			FirstSeen: now, LastSeen: now,
		},
	}

	jsonbIdx, tables := analyzer.extractJsonbFindings(findings)
	if len(jsonbIdx) != 2 {
		t.Fatalf("expected 2 jsonb findings, got %d", len(jsonbIdx))
	}
	if _, ok := jsonbIdx["public.events.payload"]; !ok {
		t.Error("missing public.events.payload in jsonbIdx")
	}
	if _, ok := jsonbIdx["app.logs.data"]; !ok {
		t.Error("missing app.logs.data in jsonbIdx")
	}
	if len(tables) != 2 {
		t.Errorf("expected 2 tables, got %d", len(tables))
	}
}

func TestApplyMatches_FilterAndUpgrade(t *testing.T) {
	analyzer := &LLMJsonbAnalyzer{
		logFn: func(string, string, ...any) {},
	}
	now := time.Now()
	findings := []Finding{
		{
			RuleID: "lint_jsonb_in_joins", Schema: "public",
			Table: "events", Column: "payload", Severity: "warning",
			Description: "original", FirstSeen: now, LastSeen: now,
		},
		{
			RuleID: "lint_missing_fk_index", Schema: "public",
			Table: "orders", Severity: "warning",
			Description: "fk finding", FirstSeen: now, LastSeen: now,
		},
		{
			RuleID: "lint_jsonb_in_joins", Schema: "app",
			Table: "logs", Column: "data", Severity: "warning",
			Description: "original2", FirstSeen: now, LastSeen: now,
		},
	}
	jsonbIdx := map[string]int{
		"public.events.payload": 0,
		"app.logs.data":         2,
	}
	// Only events.payload is confirmed by LLM; logs.data is filtered.
	matches := []llmJsonbMatch{
		{
			Schema: "public", Table: "events", Column: "payload",
			UsedIn: "where", QuerySnippet: "payload->>'type'",
		},
	}

	result := analyzer.applyMatches(findings, jsonbIdx, matches)

	// Expect: upgraded events.payload + unchanged FK finding = 2 results.
	// logs.data should be filtered out.
	if len(result) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(result))
	}

	// First should be the upgraded JSONB finding.
	if result[0].RuleID != "lint_jsonb_in_joins" {
		t.Errorf("result[0].RuleID = %q, want lint_jsonb_in_joins",
			result[0].RuleID)
	}
	if result[0].Description == "original" {
		t.Error("expected description to be upgraded, still 'original'")
	}

	// Second should be the unchanged FK finding.
	if result[1].RuleID != "lint_missing_fk_index" {
		t.Errorf("result[1].RuleID = %q, want lint_missing_fk_index",
			result[1].RuleID)
	}
	if result[1].Description != "fk finding" {
		t.Errorf("result[1].Description = %q, want 'fk finding'",
			result[1].Description)
	}
}

func TestStripJsonbToJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain array",
			in:   `[{"a":1}]`,
			want: `[{"a":1}]`,
		},
		{
			name: "markdown fenced",
			in:   "```json\n[{\"a\":1}]\n```",
			want: `[{"a":1}]`,
		},
		{
			name: "leading text before array",
			in:   "Here you go:\n[{\"a\":1}]",
			want: `[{"a":1}]`,
		},
		{
			name: "empty array",
			in:   "[]",
			want: "[]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripJsonbToJSON(tt.in)
			if got != tt.want {
				t.Errorf("stripJsonbToJSON(%q) = %q, want %q",
					tt.in, got, tt.want)
			}
		})
	}
}
