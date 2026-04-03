package optimizer

import (
	"testing"
)

// --- Confidence scoring ---

func TestComputeConfidence_AllSignals(t *testing.T) {
	input := ConfidenceInput{
		QueryVolume:      1.0,
		PlanClarity:      1.0,
		WriteRateKnown:   1.0,
		HypoPGValidated:  1.0,
		SelectivityKnown: 1.0,
		TableCallVolume:  1.0,
	}
	score := ComputeConfidence(input)
	if score < 0.99 || score > 1.01 {
		t.Errorf("score = %.2f, want 1.0", score)
	}
}

func TestComputeConfidence_NoSignals(t *testing.T) {
	input := ConfidenceInput{}
	score := ComputeConfidence(input)
	if score != 0 {
		t.Errorf("score = %.2f, want 0", score)
	}
}

func TestActionLevel(t *testing.T) {
	tests := []struct {
		confidence float64
		want       string
	}{
		{0.9, "safe"},
		{0.7, "safe"},
		{0.6, "moderate"},
		{0.4, "moderate"},
		{0.3, "high_risk"},
		{0.0, "high_risk"},
	}
	for _, tt := range tests {
		got := ActionLevel(tt.confidence)
		if got != tt.want {
			t.Errorf("ActionLevel(%.1f) = %q, want %q",
				tt.confidence, got, tt.want)
		}
	}
}

// --- Prompt parsing ---

func TestParseRecommendations_Valid(t *testing.T) {
	input := `[{"table":"public.orders","ddl":"CREATE INDEX CONCURRENTLY idx ON public.orders (status)","rationale":"test","severity":"info","index_type":"btree","category":"missing_index","estimated_improvement_pct":20}]`
	recs, err := parseRecommendations(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("recs = %d, want 1", len(recs))
	}
	if recs[0].Table != "public.orders" {
		t.Errorf("table = %q, want public.orders", recs[0].Table)
	}
}

func TestParseRecommendations_MarkdownFences(t *testing.T) {
	input := "```json\n[]\n```"
	recs, err := parseRecommendations(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("recs = %d, want 0", len(recs))
	}
}

func TestParseRecommendations_Empty(t *testing.T) {
	recs, err := parseRecommendations("[]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("recs = %d, want 0", len(recs))
	}
}

// --- Validation helpers ---

func TestExtractColumnsFromDDL(t *testing.T) {
	tests := []struct {
		ddl  string
		want []string
	}{
		{
			"CREATE INDEX CONCURRENTLY idx ON t (a, b)",
			[]string{"a", "b"},
		},
		{
			"CREATE INDEX idx ON t (a DESC, b ASC NULLS FIRST)",
			[]string{"a", "b"},
		},
		{
			"CREATE INDEX idx ON t (a) INCLUDE (b, c)",
			[]string{"a"},
		},
	}
	for _, tt := range tests {
		got := extractColumnsFromDDL(tt.ddl)
		if len(got) != len(tt.want) {
			t.Errorf("extractColumnsFromDDL(%q) = %v, want %v",
				tt.ddl, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("col[%d] = %q, want %q", i, got[i], tt.want[i])
			}
		}
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{500, "500 B"},
		{10240, "10.0 KB"},
		{10485760, "10.0 MB"},
		{1073741824, "1.0 GB"},
	}
	for _, tt := range tests {
		got := humanBytes(tt.input)
		if got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatPrompt_BasicOutput(t *testing.T) {
	tc := TableContext{
		Schema:     "public",
		Table:      "orders",
		LiveTuples: 50000,
		DeadTuples: 100,
		TableBytes: 10485760,
		IndexBytes: 2097152,
		WriteRate:  15.5,
		Workload:   "oltp_read",
		IndexCount: 2,
		Collation:  "en_US.UTF-8",
		Columns: []ColumnInfo{
			{Name: "id", Type: "integer", IsNullable: false},
		},
		Queries: []QueryInfo{
			{QueryID: 1, Text: "SELECT * FROM orders", Calls: 100,
				MeanTimeMs: 5.0, TotalTimeMs: 500.0},
		},
	}
	prompt := FormatPrompt(tc)
	if prompt == "" {
		t.Fatal("empty prompt")
	}
	if len(prompt) < 100 {
		t.Errorf("prompt too short: %d chars", len(prompt))
	}
}

// --- extractIndexName (in executor, tested via exported function) ---

func TestStripSortDirection(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"col1 DESC", "col1"},
		{"col1 ASC NULLS FIRST", "col1"},
		{"col1 NULLS LAST", "col1"},
		{"col1", "col1"},
	}
	for _, tt := range tests {
		got := stripSortDirection(tt.input)
		if got != tt.want {
			t.Errorf("stripSortDirection(%q) = %q, want %q",
				tt.input, got, tt.want)
		}
	}
}

func TestMaxNewPerTable_Default(t *testing.T) {
	// The constant should be 3 per the spec
	if defaultMaxNewPerTable != 3 {
		t.Errorf("defaultMaxNewPerTable = %d, want 3", defaultMaxNewPerTable)
	}
}

func TestTotalQueryCalls(t *testing.T) {
	queries := []QueryInfo{
		{Calls: 100},
		{Calls: 200},
		{Calls: 300},
	}
	got := totalQueryCalls(queries)
	if got != 600 {
		t.Errorf("totalQueryCalls = %d, want 600", got)
	}
}

func TestTotalQueryCalls_Empty(t *testing.T) {
	got := totalQueryCalls(nil)
	if got != 0 {
		t.Errorf("totalQueryCalls(nil) = %d, want 0", got)
	}
}
