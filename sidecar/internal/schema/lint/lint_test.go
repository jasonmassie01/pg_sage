package lint

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestDefaultRulesCount(t *testing.T) {
	// Expected count lowered from 21 to 16 on the schema/analyzer
	// consolidation: five runtime-stats-driven rules (missing-FK-index,
	// unused-index, duplicate-index, bloated-table, invalid-index) were
	// unregistered because internal/analyzer owns them on a faster
	// cadence. See the long comment above defaultRules() in linter.go.
	rules := defaultRules()
	assert.Len(t, rules, 16, "expected exactly 16 default rules")
}

func TestRuleMetadataConsistency(t *testing.T) {
	validSeverities := map[string]bool{
		"info": true, "warning": true, "critical": true,
	}
	validCategories := map[string]bool{
		"indexing": true, "data_integrity": true,
		"schema_design": true, "maintenance": true,
		"performance": true, "correctness": true,
		"convention": true,
	}

	rules := defaultRules()
	for _, r := range rules {
		t.Run(r.ID(), func(t *testing.T) {
			assert.NotEmpty(t, r.ID(), "ID must not be empty")
			assert.NotEmpty(t, r.Name(), "Name must not be empty")
			assert.NotEmpty(t, r.Severity(), "Severity must not be empty")
			assert.NotEmpty(t, r.Category(), "Category must not be empty")

			assert.Truef(t, len(r.ID()) > 5 && r.ID()[:5] == "lint_",
				"ID %q must start with 'lint_'", r.ID())
			assert.Truef(t, validSeverities[r.Severity()],
				"Severity %q is not valid", r.Severity())
			assert.Truef(t, validCategories[r.Category()],
				"Category %q is not valid", r.Category())
		})
	}
}

func TestNoDuplicateRuleIDs(t *testing.T) {
	rules := defaultRules()
	seen := make(map[string]bool, len(rules))
	for _, r := range rules {
		id := r.ID()
		assert.Falsef(t, seen[id], "duplicate rule ID: %s", id)
		seen[id] = true
	}
}

// --- Helper function tests (no DB required) ---

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1572864, "1.5 MB"},
		{1073741824, "1.0 GB"},
		{2684354560, "2.5 GB"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			got := humanSize(tc.bytes)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFormatAge(t *testing.T) {
	tests := []struct {
		age  int64
		want string
	}{
		{0, "0"},
		{999999, "999999"},
		{1_000_000, "1M"},
		{500_000_000, "500M"},
		{999_999_999, "999M"},
		{1_000_000_000, "1.0B"},
		{1_500_000_000, "1.5B"},
		{2_000_000_000, "2.0B"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, formatAge(tc.age))
		})
	}
}

func TestFormatMxidAge(t *testing.T) {
	tests := []struct {
		age  int64
		want string
	}{
		{0, "0"},
		{500_000, "500000"},
		{1_000_000, "1M"},
		{750_000_000, "750M"},
		{1_000_000_000, "1.0B"},
		{1_900_000_000, "1.9B"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, formatMxidAge(tc.age))
		})
	}
}

func TestSuggestSequenceFix(t *testing.T) {
	tests := []struct {
		dataType string
		contains string
	}{
		{"integer", "AS bigint"},
		{"smallint", "AS bigint"},
		{"bigint", "Monitor"},
	}
	for _, tc := range tests {
		t.Run(tc.dataType, func(t *testing.T) {
			result := suggestSequenceFix("public", "my_seq", tc.dataType)
			assert.Contains(t, result, tc.contains)
		})
	}
}

// --- schemaExcludeSQL tests ---

func TestSchemaExcludeSQL_DefaultSchemas(t *testing.T) {
	result := schemaExcludeSQL(nil)
	assert.Contains(t, result, "'pg_catalog'")
	assert.Contains(t, result, "'information_schema'")
	assert.Contains(t, result, "'pg_toast'")
}

func TestSchemaExcludeSQL_ExtraSchemas(t *testing.T) {
	result := schemaExcludeSQL([]string{"myschema", "test_data"})
	assert.Contains(t, result, "'myschema'")
	assert.Contains(t, result, "'test_data'")
}

func TestSchemaExcludeSQL_RejectsUnsafe(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"sql_injection", "'; DROP TABLE users;--"},
		{"uppercase", "MySchema"},
		{"spaces", "my schema"},
		{"dash", "my-schema"},
		{"dot", "my.schema"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := schemaExcludeSQL([]string{tc.input})
			assert.NotContains(t, result, "'"+tc.input+"'",
				"unsafe input %q should be rejected", tc.input)
		})
	}
}

func TestSchemaExcludeSQL_RejectsEmptyString(t *testing.T) {
	baseline := schemaExcludeSQL(nil)
	withEmpty := schemaExcludeSQL([]string{""})
	assert.Equal(t, baseline, withEmpty,
		"empty string should not add an extra schema entry")
}

func TestSchemaExcludeSQL_AcceptsValidIdentifiers(t *testing.T) {
	tests := []string{
		"public", "my_schema", "schema123", "a", "test_01",
	}
	for _, id := range tests {
		t.Run(id, func(t *testing.T) {
			result := schemaExcludeSQL([]string{id})
			assert.Contains(t, result, "'"+id+"'")
		})
	}
}

// --- Linter.Scan disabled rules test ---

// fakeRule is a Rule that records whether Check was called.
type fakeRule struct {
	id     string
	called bool
}

func (f *fakeRule) ID() string       { return f.id }
func (f *fakeRule) Name() string     { return "Fake Rule" }
func (f *fakeRule) Severity() string { return "info" }
func (f *fakeRule) Category() string { return "convention" }

func (f *fakeRule) Check(
	_ context.Context, _ *pgxpool.Pool, _ RuleOpts,
) ([]Finding, error) {
	f.called = true
	return nil, nil
}

func TestLinterScan_DisabledRules(t *testing.T) {
	enabledRule := &fakeRule{id: "lint_test_enabled"}
	disabledRule := &fakeRule{id: "lint_test_disabled"}

	cfg := &config.SchemaLintConfig{
		DisabledRules: []string{"lint_test_disabled"},
		MinTableRows:  100,
	}
	linter := &Linter{
		pool:  nil, // not used by fakeRule
		cfg:   cfg,
		pgVer: 160000,
		logFn: func(string, string, ...any) {},
		rules: []Rule{enabledRule, disabledRule},
	}

	_, err := linter.Scan(context.Background())
	require.NoError(t, err)
	assert.True(t, enabledRule.called, "enabled rule should be called")
	assert.False(t, disabledRule.called, "disabled rule should NOT be called")
}

func TestLinterScan_MinTableRowsDefault(t *testing.T) {
	// When MinTableRows is 0, Scan should default it to 1000.
	var capturedOpts RuleOpts
	spy := &optsCapture{id: "lint_test_spy", opts: &capturedOpts}

	cfg := &config.SchemaLintConfig{MinTableRows: 0}
	linter := &Linter{
		pool:  nil,
		cfg:   cfg,
		pgVer: 160000,
		logFn: func(string, string, ...any) {},
		rules: []Rule{spy},
	}

	_, err := linter.Scan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1000, capturedOpts.MinTableRows,
		"MinTableRows should default to 1000 when cfg is 0")
}

// optsCapture captures the RuleOpts passed to Check.
type optsCapture struct {
	id   string
	opts *RuleOpts
}

func (o *optsCapture) ID() string       { return o.id }
func (o *optsCapture) Name() string     { return "Spy" }
func (o *optsCapture) Severity() string { return "info" }
func (o *optsCapture) Category() string { return "convention" }

func (o *optsCapture) Check(
	_ context.Context, _ *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	*o.opts = opts
	return nil, nil
}

