package optimizer

import (
	"testing"
)

// --- Confidence scoring ---

// --- Prompt parsing ---

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
		{
			// GIN with an operator class — column is "payload", not
			// "payload jsonb_path_ops".
			"CREATE INDEX CONCURRENTLY ON e USING gin (payload jsonb_path_ops)",
			[]string{"payload"},
		},
		{
			// HNSW vector index — column is "embedding".
			"CREATE INDEX CONCURRENTLY ON d USING hnsw (embedding vector_l2_ops)",
			[]string{"embedding"},
		},
		{
			// opclass + sort direction together.
			"CREATE INDEX idx ON t (name text_pattern_ops DESC)",
			[]string{"name"},
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
		got := stripKeyDecorations(tt.input)
		if got != tt.want {
			t.Errorf("stripKeyDecorations(%q) = %q, want %q",
				tt.input, got, tt.want)
		}
	}
}
