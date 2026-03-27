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
