package executor

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestShouldExecute_AllCombinations(t *testing.T) {
	now := time.Now()
	currentWindowCron := fmt.Sprintf(
		"%d %d * * *", now.Minute(), now.Hour(),
	)

	tests := []struct {
		name          string
		trustLevel    string
		actionRisk    string
		rampStart     time.Time
		tier3Safe     bool
		tier3Moderate bool
		maintWindow   string
		emergencyStop bool
		isReplica     bool
		want          bool
	}{
		{
			name:       "observation level + safe risk",
			trustLevel: "observation",
			actionRisk: "safe",
			rampStart:  now.Add(-30 * 24 * time.Hour),
			tier3Safe:  true,
			want:       false,
		},
		{
			name:       "advisory level + moderate risk",
			trustLevel: "advisory",
			actionRisk: "moderate",
			rampStart:  now.Add(-60 * 24 * time.Hour),
			tier3Safe:  true,
			want:       false,
		},
		{
			name:       "autonomous + safe + ramp < 8d",
			trustLevel: "autonomous",
			actionRisk: "safe",
			rampStart:  now.Add(-5 * 24 * time.Hour),
			tier3Safe:  true,
			want:       false,
		},
		{
			name:       "autonomous + safe + ramp > 8d + Tier3Safe=true",
			trustLevel: "autonomous",
			actionRisk: "safe",
			rampStart:  now.Add(-10 * 24 * time.Hour),
			tier3Safe:  true,
			want:       true,
		},
		{
			name:       "autonomous + safe + ramp > 8d + Tier3Safe=false",
			trustLevel: "autonomous",
			actionRisk: "safe",
			rampStart:  now.Add(-10 * 24 * time.Hour),
			tier3Safe:  false,
			want:       false,
		},
		{
			name:          "autonomous + moderate + ramp < 31d",
			trustLevel:    "autonomous",
			actionRisk:    "moderate",
			rampStart:     now.Add(-20 * 24 * time.Hour),
			tier3Moderate: true,
			maintWindow:   currentWindowCron,
			want:          false,
		},
		{
			name:          "autonomous + moderate + ramp > 31d + Tier3Moderate + in window",
			trustLevel:    "autonomous",
			actionRisk:    "moderate",
			rampStart:     now.Add(-40 * 24 * time.Hour),
			tier3Moderate: true,
			maintWindow:   currentWindowCron,
			want:          true,
		},
		{
			name:          "autonomous + moderate + ramp > 31d + Tier3Moderate + no window",
			trustLevel:    "autonomous",
			actionRisk:    "moderate",
			rampStart:     now.Add(-40 * 24 * time.Hour),
			tier3Moderate: true,
			maintWindow:   "",
			want:          false,
		},
		{
			name:          "autonomous + moderate + ramp > 31d + Tier3Moderate=false",
			trustLevel:    "autonomous",
			actionRisk:    "moderate",
			rampStart:     now.Add(-40 * 24 * time.Hour),
			tier3Moderate: false,
			maintWindow:   currentWindowCron,
			want:          false,
		},
		{
			name:       "autonomous + high_risk + any",
			trustLevel: "autonomous",
			actionRisk: "high_risk",
			rampStart:  now.Add(-365 * 24 * time.Hour),
			tier3Safe:  true,
			want:       false,
		},
		{
			name:          "autonomous + safe + ramp > 8d + emergencyStop",
			trustLevel:    "autonomous",
			actionRisk:    "safe",
			rampStart:     now.Add(-10 * 24 * time.Hour),
			tier3Safe:     true,
			emergencyStop: true,
			want:          false,
		},
		{
			name:       "autonomous + safe + ramp > 8d + isReplica",
			trustLevel: "autonomous",
			actionRisk: "safe",
			rampStart:  now.Add(-10 * 24 * time.Hour),
			tier3Safe:  true,
			isReplica:  true,
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Trust: config.TrustConfig{
					Level:         tc.trustLevel,
					Tier3Safe:     tc.tier3Safe,
					Tier3Moderate: tc.tier3Moderate,
					MaintenanceWindow: tc.maintWindow,
				},
			}
			f := analyzer.Finding{
				ActionRisk: tc.actionRisk,
			}
			got := ShouldExecute(
				f, cfg, tc.rampStart, tc.isReplica, tc.emergencyStop,
			)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNeedsConcurrently(t *testing.T) {
	tests := []struct {
		sql  string
		want bool
	}{
		{"CREATE INDEX CONCURRENTLY idx ON t (c)", true},
		{"create index concurrently idx on t (c)", true},
		{"CREATE INDEX idx ON t (c)", false},
		{"DROP INDEX CONCURRENTLY idx", true},
		{"VACUUM FULL t", false},
	}

	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			got := NeedsConcurrently(tc.sql)
			if got != tc.want {
				t.Errorf("NeedsConcurrently(%q) = %v, want %v",
					tc.sql, got, tc.want)
			}
		})
	}
}

func TestNeedsTopLevel(t *testing.T) {
	tests := []struct {
		sql  string
		want bool
	}{
		{"VACUUM t", true},
		{"VACUUM FULL t", true},
		{"VACUUM (VERBOSE) public.large_table", true},
		{"vacuum analyze t", true},
		{"  VACUUM t", true},
		{"CREATE INDEX idx ON t (c)", false},
		{"ANALYZE t", false},
		{"SELECT pg_terminate_backend(123)", false},
		{"DROP INDEX idx", false},
	}

	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			got := NeedsTopLevel(tc.sql)
			if got != tc.want {
				t.Errorf("NeedsTopLevel(%q) = %v, want %v",
					tc.sql, got, tc.want)
			}
		})
	}
}

func TestCategorizeAction(t *testing.T) {
	tests := []struct {
		sql  string
		want string
	}{
		{"CREATE INDEX CONCURRENTLY idx ON t (c)", "create_index"},
		{"DROP INDEX idx", "drop_index"},
		{"REINDEX INDEX idx", "reindex"},
		{"VACUUM t", "vacuum"},
		{"ANALYZE t", "analyze"},
		{"SELECT pg_terminate_backend(123)", "terminate_backend"},
		{"ALTER TABLE t ADD COLUMN c int", "alter"},
		{"SOMETHING ELSE", "ddl"},
	}

	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			got := categorizeAction(tc.sql)
			if got != tc.want {
				t.Errorf("categorizeAction(%q) = %q, want %q",
					tc.sql, got, tc.want)
			}
		})
	}
}

func TestNilIfEmpty(t *testing.T) {
	t.Run("empty string returns nil", func(t *testing.T) {
		got := nilIfEmpty("")
		if got != nil {
			t.Errorf("nilIfEmpty(\"\") = %v, want nil", got)
		}
	})

	t.Run("non-empty string returns pointer", func(t *testing.T) {
		input := "DROP INDEX foo"
		got := nilIfEmpty(input)
		if got == nil {
			t.Fatal("nilIfEmpty(\"DROP INDEX foo\") = nil, want non-nil")
		}
		if *got != input {
			t.Errorf("*nilIfEmpty(%q) = %q, want %q",
				input, *got, input)
		}
	})
}

func TestCascadeGuard_BlocksRecentAction(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{
				CascadeCooldownCycles: 3,
			},
			Collector: config.CollectorConfig{
				IntervalSeconds: 60,
			},
		},
		recentActions: map[string]time.Time{
			"public.orders": time.Now().Add(-30 * time.Second),
		},
		logFn: func(string, string, ...any) {},
	}

	if !e.isCascadeCooldown("public.orders") {
		t.Error("expected cascade guard to block recent action")
	}
}

func TestCascadeGuard_AllowsAfterCooldown(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{
				CascadeCooldownCycles: 3,
			},
			Collector: config.CollectorConfig{
				IntervalSeconds: 60,
			},
		},
		recentActions: map[string]time.Time{
			"public.orders": time.Now().Add(-5 * time.Minute),
		},
		logFn: func(string, string, ...any) {},
	}

	if e.isCascadeCooldown("public.orders") {
		t.Error("expected cascade guard to allow after cooldown")
	}
}

func TestPruneRecentActions(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{
				CascadeCooldownCycles: 3,
			},
			Collector: config.CollectorConfig{
				IntervalSeconds: 60,
			},
		},
		recentActions: map[string]time.Time{
			"public.old":    time.Now().Add(-10 * time.Minute),
			"public.recent": time.Now().Add(-30 * time.Second),
		},
		logFn: func(string, string, ...any) {},
	}

	e.pruneRecentActions()

	if _, ok := e.recentActions["public.old"]; ok {
		t.Error("expected old entry to be pruned")
	}
	if _, ok := e.recentActions["public.recent"]; !ok {
		t.Error("expected recent entry to be kept")
	}
}

func TestWithLockTimeout(t *testing.T) {
	opt := WithLockTimeout(30000)
	var o ddlOpts
	opt(&o)
	if o.lockTimeoutMs != 30000 {
		t.Errorf("lockTimeoutMs = %d, want 30000", o.lockTimeoutMs)
	}
}

func TestApplyDDLOpts_NoOpts(t *testing.T) {
	o := applyDDLOpts(nil)
	if o.lockTimeoutMs != 0 {
		t.Errorf("lockTimeoutMs = %d, want 0", o.lockTimeoutMs)
	}
}
