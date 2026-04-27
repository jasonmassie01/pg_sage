package migration

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComputeRiskScore_TableDriven(t *testing.T) {
	tests := []struct {
		name     string
		risk     DDLRisk
		wantMin  float64
		wantMax  float64
	}{
		{
			name: "ACCESS EXCLUSIVE rewrite on large table",
			risk: DDLRisk{
				RuleID:          "ddl_alter_type_rewrite",
				LockLevel:       "ACCESS EXCLUSIVE",
				RequiresRewrite: true,
				EstimatedRows:   10_000_000, // 10M rows => log10=7 => 0.7
				ActiveQueries:   50,         // 50/100 = 0.5
				PendingLocks:    5,          // 5/10 = 0.5
				ReplicationLag:  15.0,       // 15/30 = 0.5
			},
			// base = 1.0 * 1.0 = 1.0
			// combined = 0.4*0.7 + 0.3*0.5 + 0.2*0.5 + 0.1*0.5
			//          = 0.28 + 0.15 + 0.10 + 0.05 = 0.58
			// risk = 1.0 * max(0.1, 0.58) = 0.58
			wantMin: 0.57,
			wantMax: 0.59,
		},
		{
			name: "SHARE lock index build on small table",
			risk: DDLRisk{
				RuleID:          "ddl_index_not_concurrent",
				LockLevel:       "SHARE",
				RequiresRewrite: false,
				EstimatedRows:   100, // log10=2 => 0.2
				ActiveQueries:   0,
				PendingLocks:    0,
				ReplicationLag:  0,
			},
			// base = 0.5 * 0.6 = 0.3
			// combined = 0.4*0.2 + 0 + 0 + 0 = 0.08 => max(0.1, 0.08) = 0.1
			// risk = 0.3 * 0.1 = 0.03
			wantMin: 0.02,
			wantMax: 0.04,
		},
		{
			name: "metadata-only drop column on empty table",
			risk: DDLRisk{
				RuleID:          "ddl_drop_column",
				LockLevel:       "ACCESS EXCLUSIVE",
				RequiresRewrite: false,
				EstimatedRows:   0,
				ActiveQueries:   0,
				PendingLocks:    0,
				ReplicationLag:  0,
			},
			// base = 1.0 * 0.2 = 0.2
			// combined = 0 => max(0.1, 0) = 0.1
			// risk = 0.2 * 0.1 = 0.02
			wantMin: 0.01,
			wantMax: 0.03,
		},
		{
			name: "high activity no table size",
			risk: DDLRisk{
				RuleID:          "ddl_set_not_null",
				LockLevel:       "ACCESS EXCLUSIVE",
				RequiresRewrite: false,
				EstimatedRows:   0,
				ActiveQueries:   200, // capped at 1.0
				PendingLocks:    20,  // capped at 1.0
				ReplicationLag:  60,  // capped at 1.0
			},
			// base = 1.0 * 0.6 = 0.6
			// combined = 0.4*0 + 0.3*1.0 + 0.2*1.0 + 0.1*1.0 = 0.6
			// risk = 0.6 * 0.6 = 0.36
			wantMin: 0.35,
			wantMax: 0.37,
		},
		{
			name: "zero lock level (lock_timeout rule)",
			risk: DDLRisk{
				RuleID:          "ddl_missing_lock_timeout",
				LockLevel:       "",
				RequiresRewrite: false,
				EstimatedRows:   1_000_000,
			},
			// base = 0.0 * anything = 0.0
			wantMin: 0.0,
			wantMax: 0.001,
		},
		{
			name: "billion-row table rewrite",
			risk: DDLRisk{
				RuleID:          "ddl_alter_type_rewrite",
				LockLevel:       "ACCESS EXCLUSIVE",
				RequiresRewrite: true,
				EstimatedRows:   1_000_000_000, // log10=9 => 0.9
				ActiveQueries:   100,            // 1.0
				PendingLocks:    10,             // 1.0
				ReplicationLag:  30,             // 1.0
			},
			// base = 1.0 * 1.0 = 1.0
			// combined = 0.4*0.9 + 0.3*1.0 + 0.2*1.0 + 0.1*1.0
			//          = 0.36 + 0.3 + 0.2 + 0.1 = 0.96
			// risk = 1.0 * 0.96 = 0.96
			wantMin: 0.95,
			wantMax: 0.97,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := computeRiskScore(&tt.risk)
			assert.GreaterOrEqual(t, score, tt.wantMin,
				"score %.4f below expected minimum %.4f", score, tt.wantMin)
			assert.LessOrEqual(t, score, tt.wantMax,
				"score %.4f above expected maximum %.4f", score, tt.wantMax)
		})
	}
}

func TestLockLevelWeight(t *testing.T) {
	assert.Equal(t, 1.0, lockLevelWeight("ACCESS EXCLUSIVE"))
	assert.Equal(t, 0.7, lockLevelWeight("SHARE ROW EXCLUSIVE"))
	assert.Equal(t, 0.5, lockLevelWeight("SHARE"))
	assert.Equal(t, 0.3, lockLevelWeight("SHARE UPDATE EXCLUSIVE"))
	assert.Equal(t, 0.0, lockLevelWeight(""))
	assert.Equal(t, 0.0, lockLevelWeight("UNKNOWN"))
}

func TestRewriteWeight(t *testing.T) {
	assert.Equal(t, 1.0, rewriteWeight(&DDLRisk{RequiresRewrite: true}))
	assert.Equal(t, 0.2, rewriteWeight(&DDLRisk{
		RuleID: "ddl_drop_column", RequiresRewrite: false}))
	assert.Equal(t, 0.6, rewriteWeight(&DDLRisk{
		RuleID: "ddl_set_not_null", RequiresRewrite: false}))
}

func TestEstimateLockDuration(t *testing.T) {
	t.Run("rewrite with data", func(t *testing.T) {
		r := &DDLRisk{
			RequiresRewrite: true,
			TableSizeBytes:  500 * 1024 * 1024, // 500MB
		}
		ms := estimateLockDuration(r)
		assert.Greater(t, ms, int64(5000), "500MB should take >5s")
	})

	t.Run("metadata only", func(t *testing.T) {
		r := &DDLRisk{
			LockLevel: "ACCESS EXCLUSIVE",
		}
		ms := estimateLockDuration(r)
		assert.Equal(t, int64(100), ms)
	})

	t.Run("share lock", func(t *testing.T) {
		r := &DDLRisk{LockLevel: "SHARE"}
		ms := estimateLockDuration(r)
		assert.Equal(t, int64(50), ms)
	})
}

func TestComputeRiskScore_BoundaryOneRow(t *testing.T) {
	risk := &DDLRisk{
		RuleID:          "ddl_alter_type_rewrite",
		LockLevel:       "ACCESS EXCLUSIVE",
		RequiresRewrite: true,
		EstimatedRows:   1, // log10(1) = 0
	}
	score := computeRiskScore(risk)
	// base=1.0, tableFactor=0, all others 0 => combined=0 => max(0.1,0)=0.1
	// score = 1.0 * 0.1 = 0.1
	assert.InDelta(t, 0.1, score, 0.01)
}

func TestIsVolatileDefault(t *testing.T) {
	assert.True(t, isVolatileDefault("my_func()"))
	assert.True(t, isVolatileDefault("random()"))
	assert.False(t, isVolatileDefault("gen_random_uuid()"))
	assert.False(t, isVolatileDefault("now()"))
	assert.False(t, isVolatileDefault("'literal_string'"))
	assert.False(t, isVolatileDefault("42"))
}

// Verify score is always in [0,1].
func TestComputeRiskScore_AlwaysBounded(t *testing.T) {
	extremes := []DDLRisk{
		{LockLevel: "ACCESS EXCLUSIVE", RequiresRewrite: true,
			EstimatedRows: math.MaxInt64, ActiveQueries: 10000,
			PendingLocks: 10000, ReplicationLag: 10000},
		{LockLevel: "", RequiresRewrite: false},
	}
	for _, r := range extremes {
		score := computeRiskScore(&r)
		assert.GreaterOrEqual(t, score, 0.0)
		assert.LessOrEqual(t, score, 1.0)
	}
}
