package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestExtractIndexNameFromSQL(t *testing.T) {
	tests := []struct {
		sql  string
		want string
	}{
		{"CREATE INDEX idx_foo ON public.t (a)", "idx_foo"},
		{"CREATE INDEX CONCURRENTLY idx_bar ON t (a, b)", "idx_bar"},
		{"CREATE UNIQUE INDEX idx_uniq ON s.t (a)", "idx_uniq"},
		{
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_safe ON t (x)",
			"idx_safe",
		},
		{
			"CREATE UNIQUE INDEX CONCURRENTLY idx_uc ON t (a)",
			"idx_uc",
		},
		{"CREATE INDEX CONCURRENTLY ON t (a)", ""},
		{"CREATE INDEX public.idx_schema ON t (a)", "idx_schema"},
		{"DROP INDEX CONCURRENTLY idx_drop", ""},
		{"VACUUM FULL t", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			got := extractIndexNameFromSQL(tt.sql)
			if got != tt.want {
				t.Errorf(
					"extractIndexNameFromSQL(%q) = %q, want %q",
					tt.sql, got, tt.want,
				)
			}
		})
	}
}

func TestRuleUnusedIndexes_SkipsOnlyFKSupportingIndex(t *testing.T) {
	old := time.Now().Add(-10 * 24 * time.Hour)
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{{
			SchemaName: "public",
			RelName:    "orders",
		}},
		ForeignKeys: []collector.ForeignKey{{
			TableName:       "orders",
			FKColumn:        "customer_id",
			ReferencedTable: "customers",
			ConstraintName:  "orders_customer_id_fkey",
		}},
		Indexes: []collector.IndexStats{{
			SchemaName:   "public",
			RelName:      "orders",
			IndexRelName: "orders_customer_id_idx",
			IsValid:      true,
			IdxScan:      0,
			IndexDef: "CREATE INDEX orders_customer_id_idx " +
				"ON public.orders USING btree (customer_id)",
		}},
	}
	cfg := &config.Config{}
	cfg.Analyzer.UnusedIndexWindowDays = 7
	extras := &RuleExtras{
		FirstSeen: map[string]time.Time{
			"public.orders_customer_id_idx": old,
		},
		RecentlyCreated: map[string]time.Time{},
	}

	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 0 {
		t.Fatalf("expected no unused-index finding, got %+v", findings)
	}
}

func TestRuleUnusedIndexes_DropsFKIndexWhenAlternateExists(t *testing.T) {
	old := time.Now().Add(-10 * 24 * time.Hour)
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{{
			SchemaName: "public",
			RelName:    "orders",
		}},
		ForeignKeys: []collector.ForeignKey{{
			TableName:       "orders",
			FKColumn:        "customer_id",
			ReferencedTable: "customers",
			ConstraintName:  "orders_customer_id_fkey",
		}},
		Indexes: []collector.IndexStats{
			{
				SchemaName:   "public",
				RelName:      "orders",
				IndexRelName: "orders_customer_id_idx",
				IsValid:      true,
				IdxScan:      0,
				IndexDef: "CREATE INDEX orders_customer_id_idx " +
					"ON public.orders USING btree (customer_id)",
			},
			{
				SchemaName:   "public",
				RelName:      "orders",
				IndexRelName: "orders_customer_id_created_idx",
				IsValid:      true,
				IdxScan:      10,
				IndexDef: "CREATE INDEX orders_customer_id_created_idx " +
					"ON public.orders USING btree (customer_id, created_at)",
			},
		},
	}
	cfg := &config.Config{}
	cfg.Analyzer.UnusedIndexWindowDays = 7
	extras := &RuleExtras{
		FirstSeen: map[string]time.Time{
			"public.orders_customer_id_idx": old,
		},
		RecentlyCreated: map[string]time.Time{},
	}

	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 1 {
		t.Fatalf("expected one unused-index finding, got %+v", findings)
	}
	if findings[0].ObjectIdentifier != "public.orders_customer_id_idx" {
		t.Fatalf("wrong finding: %+v", findings[0])
	}
}

func TestRuleDuplicateIndexes_SchemaScoped(t *testing.T) {
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName:   "tenant_a",
				RelName:      "orders",
				IndexRelName: "orders_pkey",
				IsValid:      true,
				IndexDef: "CREATE UNIQUE INDEX orders_pkey " +
					"ON tenant_a.orders USING btree (id)",
				IsPrimary: true,
				IsUnique:  true,
			},
			{
				SchemaName:   "tenant_b",
				RelName:      "orders",
				IndexRelName: "orders_pkey",
				IsValid:      true,
				IndexDef: "CREATE UNIQUE INDEX orders_pkey " +
					"ON tenant_b.orders USING btree (id)",
				IsPrimary: true,
				IsUnique:  true,
			},
		},
	}

	findings := ruleDuplicateIndexes(snap, nil, &config.Config{}, nil)
	if len(findings) != 0 {
		t.Fatalf("expected no cross-schema duplicate finding, got %d",
			len(findings))
	}
}

func TestRuleDuplicateIndexes_DoesNotDropConstraintBacked(t *testing.T) {
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName:   "public",
				RelName:      "orders",
				IndexRelName: "orders_pkey",
				IsValid:      true,
				IndexDef: "CREATE UNIQUE INDEX orders_pkey " +
					"ON public.orders USING btree (id)",
				IsPrimary: true,
				IsUnique:  true,
			},
			{
				SchemaName:   "public",
				RelName:      "orders",
				IndexRelName: "idx_orders_id",
				IsValid:      true,
				IndexDef: "CREATE INDEX idx_orders_id " +
					"ON public.orders USING btree (id)",
			},
		},
	}

	findings := ruleDuplicateIndexes(snap, nil, &config.Config{}, nil)
	if len(findings) != 1 {
		t.Fatalf("expected one safe duplicate finding, got %d",
			len(findings))
	}
	if findings[0].ObjectIdentifier != "public.idx_orders_id" {
		t.Fatalf("drop target = %q, want public.idx_orders_id",
			findings[0].ObjectIdentifier)
	}
}

func TestRuleDuplicateIndexes_SkipsUniqueSubset(t *testing.T) {
	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName:   "public",
				RelName:      "orders",
				IndexRelName: "orders_external_id_key",
				IsValid:      true,
				IndexDef: "CREATE UNIQUE INDEX orders_external_id_key " +
					"ON public.orders USING btree (external_id)",
				IsUnique: true,
			},
			{
				SchemaName:   "public",
				RelName:      "orders",
				IndexRelName: "idx_orders_external_created",
				IsValid:      true,
				IndexDef: "CREATE INDEX idx_orders_external_created " +
					"ON public.orders USING btree " +
					"(external_id, created_at)",
			},
		},
	}

	findings := ruleDuplicateIndexes(snap, nil, &config.Config{}, nil)
	if len(findings) != 0 {
		t.Fatalf("expected unique subset to be preserved, got %d",
			len(findings))
	}
}

func TestRuleUnusedIndexes_SkipsRecentlyCreated(t *testing.T) {
	cfg := &config.Config{}
	cfg.Analyzer.UnusedIndexWindowDays = 7

	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName:   "public",
				IndexRelName: "idx_recently_created",
				RelName:      "orders",
				IdxScan:      0,
				IsValid:      true,
				IndexDef: "CREATE INDEX idx_recently_created " +
					"ON public.orders (customer_id)",
			},
		},
	}

	extras := &RuleExtras{
		FirstSeen: map[string]time.Time{
			"public.idx_recently_created": time.Now().Add(
				-30 * 24 * time.Hour,
			),
		},
		RecentlyCreated: map[string]time.Time{
			"idx_recently_created": time.Now().Add(-1 * time.Hour),
		},
	}

	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 0 {
		t.Errorf(
			"expected 0 findings for recently created index, got %d",
			len(findings),
		)
	}
}

func TestRuleUnusedIndexes_FlagsOldUnused(t *testing.T) {
	cfg := &config.Config{}
	cfg.Analyzer.UnusedIndexWindowDays = 7

	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName:   "public",
				IndexRelName: "idx_old_unused",
				RelName:      "orders",
				IdxScan:      0,
				IsValid:      true,
				IndexDef: "CREATE INDEX idx_old_unused " +
					"ON public.orders (old_col)",
			},
		},
	}

	extras := &RuleExtras{
		FirstSeen: map[string]time.Time{
			"public.idx_old_unused": time.Now().Add(
				-30 * 24 * time.Hour,
			),
		},
		RecentlyCreated: make(map[string]time.Time),
	}

	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 1 {
		t.Errorf(
			"expected 1 finding for old unused index, got %d",
			len(findings),
		)
	}
	if len(findings) > 0 && findings[0].Category != "unused_index" {
		t.Errorf(
			"expected category unused_index, got %s",
			findings[0].Category,
		)
	}
}

func TestRuleUnusedIndexes_WindowNotElapsed(t *testing.T) {
	cfg := &config.Config{}
	cfg.Analyzer.UnusedIndexWindowDays = 7

	snap := &collector.Snapshot{
		Indexes: []collector.IndexStats{
			{
				SchemaName:   "public",
				IndexRelName: "idx_new",
				RelName:      "orders",
				IdxScan:      0,
				IsValid:      true,
				IndexDef: "CREATE INDEX idx_new " +
					"ON public.orders (col)",
			},
		},
	}

	extras := &RuleExtras{
		FirstSeen: map[string]time.Time{
			"public.idx_new": time.Now().Add(-2 * 24 * time.Hour),
		},
		RecentlyCreated: make(map[string]time.Time),
	}

	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 0 {
		t.Errorf(
			"expected 0 findings (window not elapsed), got %d",
			len(findings),
		)
	}
}

func TestRuleUnusedIndexes_UnloggedDowngrade(t *testing.T) {
	cfg := &config.Config{}
	cfg.Analyzer.UnusedIndexWindowDays = 7

	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName:     "public",
				RelName:        "staging",
				Relpersistence: "u",
			},
		},
		Indexes: []collector.IndexStats{
			{
				SchemaName:   "public",
				IndexRelName: "idx_staging_col",
				RelName:      "staging",
				IdxScan:      0,
				IsValid:      true,
				IndexDef: "CREATE INDEX idx_staging_col " +
					"ON public.staging (col)",
			},
		},
	}

	extras := &RuleExtras{
		FirstSeen: map[string]time.Time{
			"public.idx_staging_col": time.Now().Add(
				-30 * 24 * time.Hour,
			),
		},
		RecentlyCreated: make(map[string]time.Time),
	}

	findings := ruleUnusedIndexes(snap, nil, cfg, extras)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "info" {
		t.Errorf(
			"severity = %q, want info (unlogged downgrade)",
			findings[0].Severity,
		)
	}
	ul, ok := findings[0].Detail["unlogged"].(bool)
	if !ok || !ul {
		t.Errorf(
			"detail[unlogged] = %v, want true",
			findings[0].Detail["unlogged"],
		)
	}
}

func TestRuleMissingFK_UnloggedDowngrade(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName:     "public",
				RelName:        "tmp_orders",
				Relpersistence: "u",
			},
		},
		ForeignKeys: []collector.ForeignKey{
			{
				TableName:       "tmp_orders",
				ReferencedTable: "customers",
				FKColumn:        "customer_id",
				ConstraintName:  "fk_customer",
			},
		},
	}
	cfg := &config.Config{}

	findings := ruleMissingFKIndexes(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "info" {
		t.Errorf(
			"severity = %q, want info (unlogged downgrade)",
			findings[0].Severity,
		)
	}
	ul, ok := findings[0].Detail["unlogged"].(bool)
	if !ok || !ul {
		t.Errorf(
			"detail[unlogged] = %v, want true",
			findings[0].Detail["unlogged"],
		)
	}
}
