package migration

import (
	"context"
	"strings"
	"testing"
)

func TestExtractSQLFromResponse_PlainSQL(t *testing.T) {
	input := `-- Step 1: Add new column
ALTER TABLE orders ADD COLUMN new_status text;
-- Step 2: Backfill
UPDATE orders SET new_status = status::text;`

	got := extractSQLFromResponse(input)
	if got != input {
		t.Errorf("expected plain SQL returned as-is, got %q", got)
	}
}

func TestExtractSQLFromResponse_MarkdownFenced(t *testing.T) {
	input := "Here is the migration:\n```sql\nBEGIN;\nALTER TABLE foo ADD COLUMN bar int;\nCOMMIT;\n```\nDone."

	got := extractSQLFromResponse(input)
	want := "BEGIN;\nALTER TABLE foo ADD COLUMN bar int;\nCOMMIT;"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestExtractSQLFromResponse_MarkdownNoLang(t *testing.T) {
	input := "```\nSELECT 1;\n```"

	got := extractSQLFromResponse(input)
	if got != "SELECT 1;" {
		t.Errorf("expected 'SELECT 1;', got %q", got)
	}
}

func TestExtractSQLFromResponse_Empty(t *testing.T) {
	got := extractSQLFromResponse("")
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestExtractSQLFromResponse_WhitespaceOnly(t *testing.T) {
	got := extractSQLFromResponse("   \n\t  ")
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestExtractSQLFromResponse_MixedTextAndSQL(t *testing.T) {
	input := "I suggest the following approach:\n```pgsql\nSET lock_timeout = '5s';\nALTER TABLE t ADD COLUMN c int;\n```\nThis is safe because..."

	got := extractSQLFromResponse(input)
	want := "SET lock_timeout = '5s';\nALTER TABLE t ADD COLUMN c int;"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestExtractSQLFromResponse_NoClosingFence(t *testing.T) {
	// LLM response truncated mid-fence
	input := "```sql\nBEGIN;\nALTER TABLE x ADD COLUMN y text;"

	got := extractSQLFromResponse(input)
	want := "BEGIN;\nALTER TABLE x ADD COLUMN y text;"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestExtractFromFences_MultipleFences(t *testing.T) {
	input := "Step 1:\n```sql\nCREATE INDEX CONCURRENTLY idx ON t(c);\n```\nStep 2:\n```sql\nALTER TABLE t ADD CONSTRAINT ...\n```"

	// extractFromFences only returns the first fence block
	got := extractFromFences(input)
	want := "CREATE INDEX CONCURRENTLY idx ON t(c);"
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestLooksLikeSQL(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"BEGIN; ALTER TABLE foo ADD COLUMN bar int; COMMIT;", true},
		{"SELECT 1;", true},
		{"CREATE INDEX CONCURRENTLY idx ON t(c);", true},
		{"This is just a plain text explanation.", false},
		{"SET lock_timeout = '5s';", true},
		{"DROP INDEX foo;", true},
		{"Hello world", false},
		{"ROLLBACK;", true},
	}

	for _, tc := range tests {
		got := looksLikeSQL(tc.input)
		if got != tc.want {
			t.Errorf("looksLikeSQL(%q) = %v, want %v",
				tc.input, got, tc.want)
		}
	}
}

func TestGenerate_NilLLMClient(t *testing.T) {
	gen := NewScriptGenerator(nil, nil, 16, noopLog)

	risk := &DDLRisk{
		Statement:       "ALTER TABLE orders ALTER COLUMN status TYPE text",
		RuleID:          "alter_column_type",
		SafeAlternative: "New column + trigger + backfill + swap",
	}

	got, err := gen.Generate(context.Background(), risk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != risk.SafeAlternative {
		t.Errorf("expected SafeAlternative %q, got %q",
			risk.SafeAlternative, got)
	}
}

func TestGenerate_NilLLMClient_EmptyAlternative(t *testing.T) {
	gen := NewScriptGenerator(nil, nil, 16, noopLog)

	risk := &DDLRisk{
		Statement:       "ALTER TABLE orders DROP COLUMN foo",
		RuleID:          "drop_column",
		SafeAlternative: "",
	}

	got, err := gen.Generate(context.Background(), risk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestScriptSystemPrompt_NotEmpty(t *testing.T) {
	prompt := scriptSystemPrompt()
	if prompt == "" {
		t.Error("system prompt should not be empty")
	}
	// Verify key requirements are mentioned
	keywords := []string{
		"lock_timeout",
		"CONCURRENTLY",
		"NOT VALID",
		"VALIDATE CONSTRAINT",
		"transaction",
	}
	for _, kw := range keywords {
		if !strings.Contains(prompt, kw) {
			t.Errorf("system prompt missing keyword %q", kw)
		}
	}
}

func TestScriptUserPrompt_Content(t *testing.T) {
	risk := &DDLRisk{
		Statement:       "ALTER TABLE orders ALTER COLUMN status TYPE text",
		RuleID:          "alter_column_type",
		Description:     "Column type change requires table rewrite",
		LockLevel:       "ACCESS EXCLUSIVE",
		SchemaName:      "public",
		TableName:       "orders",
		EstimatedRows:   1000000,
		TableSizeBytes:  536870912,
		SafeAlternative: "New column + trigger + backfill + swap",
	}

	prompt := scriptUserPrompt(risk, "-- Table: public.orders\n", 16)

	required := []string{
		"PostgreSQL version: 16",
		"ALTER TABLE orders ALTER COLUMN status TYPE text",
		"alter_column_type",
		"ACCESS EXCLUSIVE",
		"public.orders",
		"1000000 rows",
		"536870912 bytes",
		"Safe alternative hint:",
	}
	for _, r := range required {
		if !strings.Contains(prompt, r) {
			t.Errorf("user prompt missing %q", r)
		}
	}
}

func TestScriptUserPrompt_DefaultSchema(t *testing.T) {
	risk := &DDLRisk{
		Statement:  "ALTER TABLE foo ADD COLUMN bar int",
		RuleID:     "add_column",
		TableName:  "foo",
		SchemaName: "", // empty should default to public
	}

	prompt := scriptUserPrompt(risk, "", 15)
	if !strings.Contains(prompt, "public.foo") {
		t.Error("expected default schema 'public' in prompt")
	}
}

func noopLog(_ string, _ string, _ ...any) {}
