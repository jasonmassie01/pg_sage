package optimizer

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func noopLog(string, string, ...any) {}

func newTestValidator(cfg *config.OptimizerConfig) *Validator {
	if cfg == nil {
		cfg = &config.OptimizerConfig{}
	}
	return NewValidator(nil, cfg, noopLog)
}

// --- checkConcurrently ---

func TestCheckConcurrently_Present(t *testing.T) {
	v := newTestValidator(nil)
	ok, reason := v.checkConcurrently(Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY idx_foo ON bar (col)",
	})
	if !ok {
		t.Fatalf("expected accepted, got rejected: %s", reason)
	}
}

func TestCheckConcurrently_Missing(t *testing.T) {
	v := newTestValidator(nil)
	ok, reason := v.checkConcurrently(Recommendation{
		DDL: "CREATE INDEX idx_foo ON bar (col)",
	})
	if ok {
		t.Fatal("expected rejected for missing CONCURRENTLY")
	}
	if reason != "DDL missing CONCURRENTLY keyword" {
		t.Fatalf("unexpected reason: %s", reason)
	}
}

func TestCheckConcurrently_Lowercase(t *testing.T) {
	v := newTestValidator(nil)
	ok, reason := v.checkConcurrently(Recommendation{
		DDL: "CREATE INDEX concurrently idx_foo ON bar (col)",
	})
	if !ok {
		t.Fatalf("expected accepted for lowercase concurrently, got: %s", reason)
	}
}

func TestCheckConcurrently_EmptyDDL(t *testing.T) {
	v := newTestValidator(nil)
	ok, _ := v.checkConcurrently(Recommendation{DDL: ""})
	if ok {
		t.Fatal("expected rejected for empty DDL")
	}
}

func TestCheckConcurrently_InMiddle(t *testing.T) {
	v := newTestValidator(nil)
	ok, reason := v.checkConcurrently(Recommendation{
		DDL: "CREATE INDEX IF NOT EXISTS CONCURRENTLY idx_foo ON bar (col)",
	})
	if !ok {
		t.Fatalf("expected accepted for CONCURRENTLY in middle, got: %s", reason)
	}
}

// --- checkColumnExistence ---

func TestCheckColumnExistence_AllExist(t *testing.T) {
	v := newTestValidator(nil)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY idx ON t (name, age)"}
	tc := TableContext{
		Columns: []ColumnInfo{
			{Name: "name", Type: "text"},
			{Name: "age", Type: "int"},
		},
	}
	ok, reason := v.checkColumnExistence(rec, tc)
	if !ok {
		t.Fatalf("expected accepted, got: %s", reason)
	}
}

func TestCheckColumnExistence_HallucinatedColumn(t *testing.T) {
	v := newTestValidator(nil)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY idx ON t (fake_col)"}
	tc := TableContext{
		Columns: []ColumnInfo{
			{Name: "name", Type: "text"},
		},
	}
	ok, reason := v.checkColumnExistence(rec, tc)
	if ok {
		t.Fatal("expected rejected for hallucinated column")
	}
	if reason == "" {
		t.Fatal("expected reason mentioning the column")
	}
}

func TestCheckColumnExistence_CaseInsensitive(t *testing.T) {
	v := newTestValidator(nil)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY idx ON t (Status)"}
	tc := TableContext{
		Columns: []ColumnInfo{
			{Name: "status", Type: "text"},
		},
	}
	ok, reason := v.checkColumnExistence(rec, tc)
	if !ok {
		t.Fatalf("expected accepted for case-insensitive match, got: %s", reason)
	}
}

func TestCheckColumnExistence_NoExtractableColumns(t *testing.T) {
	v := newTestValidator(nil)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY idx ON t USING gin"}
	tc := TableContext{
		Columns: []ColumnInfo{
			{Name: "name", Type: "text"},
		},
	}
	ok, reason := v.checkColumnExistence(rec, tc)
	if !ok {
		t.Fatalf("expected accepted when no cols extractable, got: %s", reason)
	}
}

func TestCheckColumnExistence_EmptyTableColumns(t *testing.T) {
	v := newTestValidator(nil)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY idx ON t (name)"}
	tc := TableContext{Columns: []ColumnInfo{}}
	ok, _ := v.checkColumnExistence(rec, tc)
	if ok {
		t.Fatal("expected rejected when tc has no columns")
	}
}

func TestCheckColumnExistence_MultiColumnAllExist(t *testing.T) {
	v := newTestValidator(nil)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY idx ON t (a, b, c)"}
	tc := TableContext{
		Columns: []ColumnInfo{
			{Name: "a", Type: "int"},
			{Name: "b", Type: "int"},
			{Name: "c", Type: "int"},
			{Name: "d", Type: "int"},
		},
	}
	ok, reason := v.checkColumnExistence(rec, tc)
	if !ok {
		t.Fatalf("expected accepted for 3 existing columns, got: %s", reason)
	}
}

// --- checkDuplicate ---

func TestCheckDuplicate_ExactMatch(t *testing.T) {
	v := newTestValidator(nil)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY idx ON t (a, b)"}
	tc := TableContext{
		Indexes: []IndexInfo{
			{Name: "idx_existing", IsValid: true,
				Definition: "CREATE INDEX idx_existing ON t (a, b)"},
		},
	}
	ok, reason := v.checkDuplicate(rec, tc)
	if ok {
		t.Fatal("expected rejected for duplicate index")
	}
	if reason == "" {
		t.Fatal("expected reason about duplicate")
	}
}

func TestCheckDuplicate_DifferentOrder(t *testing.T) {
	v := newTestValidator(nil)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY idx ON t (a, b)"}
	tc := TableContext{
		Indexes: []IndexInfo{
			{Name: "idx_existing", Definition: "CREATE INDEX idx_existing ON t (b, a)"},
		},
	}
	ok, reason := v.checkDuplicate(rec, tc)
	if !ok {
		t.Fatalf("expected accepted for different column order, got: %s", reason)
	}
}

func TestCheckDuplicate_NoIndexes(t *testing.T) {
	v := newTestValidator(nil)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY idx ON t (a)"}
	tc := TableContext{Indexes: []IndexInfo{}}
	ok, reason := v.checkDuplicate(rec, tc)
	if !ok {
		t.Fatalf("expected accepted with no indexes, got: %s", reason)
	}
}

func TestCheckDuplicate_EmptyDDL(t *testing.T) {
	v := newTestValidator(nil)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY idx ON t USING gin"}
	tc := TableContext{
		Indexes: []IndexInfo{
			{Name: "idx_existing", Definition: "CREATE INDEX idx_existing ON t (a)"},
		},
	}
	ok, reason := v.checkDuplicate(rec, tc)
	if !ok {
		t.Fatalf("expected accepted when no cols extractable, got: %s", reason)
	}
}

func TestCheckDuplicate_CaseInsensitiveMatch(t *testing.T) {
	v := newTestValidator(nil)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY idx ON t (Status)"}
	tc := TableContext{
		Indexes: []IndexInfo{
			{Name: "idx_existing", IsValid: true,
				Definition: "CREATE INDEX idx_existing ON t (status)"},
		},
	}
	ok, _ := v.checkDuplicate(rec, tc)
	if ok {
		t.Fatal("expected rejected for case-insensitive duplicate")
	}
}

// --- checkWriteImpact ---

func TestCheckWriteImpact_HighWriteLowImprovement(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{
		WriteHeavyRatioPct:   70,
		WriteImpactThreshPct: 15,
	})
	rec := Recommendation{EstimatedImprovementPct: 5}
	tc := TableContext{WriteRate: 80}
	ok, _ := v.checkWriteImpact(rec, tc)
	if ok {
		t.Fatal("expected rejected for high write rate with low improvement")
	}
}

func TestCheckWriteImpact_HighWriteHighImprovement(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{
		WriteHeavyRatioPct:   70,
		WriteImpactThreshPct: 15,
	})
	rec := Recommendation{EstimatedImprovementPct: 30}
	tc := TableContext{WriteRate: 80}
	ok, reason := v.checkWriteImpact(rec, tc)
	if !ok {
		t.Fatalf("expected accepted for high improvement, got: %s", reason)
	}
}

func TestCheckWriteImpact_LowWriteRate(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{
		WriteHeavyRatioPct:   70,
		WriteImpactThreshPct: 15,
	})
	rec := Recommendation{EstimatedImprovementPct: 1}
	tc := TableContext{WriteRate: 20}
	ok, reason := v.checkWriteImpact(rec, tc)
	if !ok {
		t.Fatalf("expected accepted for low write rate, got: %s", reason)
	}
}

func TestCheckWriteImpact_ZeroThresholdDefaults(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{
		WriteHeavyRatioPct:   70,
		WriteImpactThreshPct: 0,
	})
	rec := Recommendation{EstimatedImprovementPct: 10}
	tc := TableContext{WriteRate: 80}
	ok, _ := v.checkWriteImpact(rec, tc)
	if ok {
		t.Fatal("expected rejected: threshold should default to 15, improvement 10 < 15")
	}
}

func TestCheckWriteImpact_BoundaryWriteRate(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{
		WriteHeavyRatioPct:   70,
		WriteImpactThreshPct: 15,
	})
	rec := Recommendation{EstimatedImprovementPct: 5}
	tc := TableContext{WriteRate: 70}
	ok, reason := v.checkWriteImpact(rec, tc)
	if !ok {
		t.Fatalf("expected accepted at boundary (needs >), got: %s", reason)
	}
}

// --- checkMaxIndexes ---

func TestCheckMaxIndexes_AtLimit(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{MaxIndexesPerTable: 10})
	tc := TableContext{IndexCount: 10}
	ok, _ := v.checkMaxIndexes(tc)
	if ok {
		t.Fatal("expected rejected when IndexCount >= max")
	}
}

func TestCheckMaxIndexes_BelowLimit(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{MaxIndexesPerTable: 10})
	tc := TableContext{IndexCount: 5}
	ok, reason := v.checkMaxIndexes(tc)
	if !ok {
		t.Fatalf("expected accepted below limit, got: %s", reason)
	}
}

func TestCheckMaxIndexes_AboveLimit(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{MaxIndexesPerTable: 10})
	tc := TableContext{IndexCount: 15}
	ok, _ := v.checkMaxIndexes(tc)
	if ok {
		t.Fatal("expected rejected when IndexCount > max")
	}
}

func TestCheckMaxIndexes_ZeroConfigDefaults(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{MaxIndexesPerTable: 0})
	tc := TableContext{IndexCount: 10}
	ok, _ := v.checkMaxIndexes(tc)
	if ok {
		t.Fatal("expected rejected: max defaults to 10, IndexCount=10 >= 10")
	}
}

// --- Validate integration ---

func TestValidate_AllPass(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{
		WriteHeavyRatioPct:   70,
		WriteImpactThreshPct: 15,
		MaxIndexesPerTable:   10,
	})
	rec := Recommendation{
		DDL:                     "CREATE INDEX CONCURRENTLY idx ON t (name)",
		EstimatedImprovementPct: 50,
	}
	tc := TableContext{
		Columns:    []ColumnInfo{{Name: "name", Type: "text"}},
		Indexes:    []IndexInfo{},
		WriteRate:  10,
		IndexCount: 3,
	}
	ok, reason := v.Validate(context.Background(), rec, tc)
	if !ok {
		t.Fatalf("expected all checks to pass, got: %s", reason)
	}
}

func TestValidate_FailsOnConcurrently(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{MaxIndexesPerTable: 10})
	rec := Recommendation{
		DDL: "CREATE INDEX idx ON t (name)",
	}
	tc := TableContext{
		Columns:    []ColumnInfo{{Name: "name", Type: "text"}},
		IndexCount: 3,
	}
	ok, reason := v.Validate(context.Background(), rec, tc)
	if ok {
		t.Fatal("expected failure on CONCURRENTLY check")
	}
	if reason != "DDL missing CONCURRENTLY keyword" {
		t.Fatalf("unexpected reason: %s", reason)
	}
}

func TestValidate_FailsOnDuplicate(t *testing.T) {
	v := newTestValidator(&config.OptimizerConfig{
		WriteHeavyRatioPct:   70,
		WriteImpactThreshPct: 15,
		MaxIndexesPerTable:   10,
	})
	rec := Recommendation{
		DDL:                     "CREATE INDEX CONCURRENTLY idx ON t (name)",
		EstimatedImprovementPct: 50,
	}
	tc := TableContext{
		Columns: []ColumnInfo{{Name: "name", Type: "text"}},
		Indexes: []IndexInfo{
			{Name: "idx_existing", IsValid: true,
				Definition: "CREATE INDEX idx_existing ON t (name)"},
		},
		WriteRate:  10,
		IndexCount: 3,
	}
	ok, reason := v.Validate(context.Background(), rec, tc)
	if ok {
		t.Fatal("expected failure on duplicate check")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason for duplicate")
	}
}
