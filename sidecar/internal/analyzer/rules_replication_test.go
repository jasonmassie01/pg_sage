package analyzer

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// humanBytes
// ---------------------------------------------------------------------------

func TestHumanBytes_Zero(t *testing.T) {
	assert.Equal(t, "0 B", humanBytes(0))
}

func TestHumanBytes_SubKB(t *testing.T) {
	assert.Equal(t, "1023 B", humanBytes(1023))
}

func TestHumanBytes_ExactlyOneKB(t *testing.T) {
	assert.Equal(t, "1.0 KB", humanBytes(1024))
}

func TestHumanBytes_FractionalKB(t *testing.T) {
	assert.Equal(t, "1.5 KB", humanBytes(1536))
}

func TestHumanBytes_ExactlyOneMB(t *testing.T) {
	assert.Equal(t, "1.0 MB", humanBytes(1<<20))
}

func TestHumanBytes_ExactlyOneGB(t *testing.T) {
	assert.Equal(t, "1.0 GB", humanBytes(1<<30))
}

func TestHumanBytes_ExactlyOneTB(t *testing.T) {
	assert.Equal(t, "1.0 TB", humanBytes(1<<40))
}

// ---------------------------------------------------------------------------
// ruleInactiveSlots — slow active replication slot detection
// ---------------------------------------------------------------------------

func slotSnapshot(slots []collector.SlotInfo) *collector.Snapshot {
	return &collector.Snapshot{
		Replication: &collector.ReplicationStats{
			Slots: slots,
		},
	}
}

func TestSlowSlot_ActiveBelowThreshold_NoFinding(t *testing.T) {
	snap := slotSnapshot([]collector.SlotInfo{
		{SlotName: "sub1", Active: true, SlotType: "logical",
			RetainedBytes: 500 * (1 << 20)}, // 500 MB, below 1 GB default
	})

	findings := ruleInactiveSlots(snap, nil, nil, nil)

	assert.Empty(t, findings,
		"active slot below 1 GB threshold should produce no findings")
}

func TestSlowSlot_ActiveAtExactlyOneGB_Warning(t *testing.T) {
	oneGB := int64(1 << 30)
	snap := slotSnapshot([]collector.SlotInfo{
		{SlotName: "debezium", Active: true, SlotType: "logical",
			RetainedBytes: oneGB},
	})

	findings := ruleInactiveSlots(snap, nil, nil, nil)

	if assert.Len(t, findings, 1, "exactly 1 GB should trigger one finding") {
		f := findings[0]
		assert.Equal(t, "slow_replication_slot", f.Category)
		assert.Equal(t, "warning", f.Severity)
		assert.Equal(t, "slot:debezium", f.ObjectIdentifier)
		assert.Equal(t, "replication_slot", f.ObjectType)
		assert.Equal(t, "moderate", f.ActionRisk)
		assert.Contains(t, f.Title, "debezium")
		assert.Contains(t, f.Title, "1.0 GB")
		assert.Contains(t, f.Recommendation, "slow consumer")

		detail := f.Detail
		assert.Equal(t, "debezium", detail["slot_name"])
		assert.Equal(t, "logical", detail["slot_type"])
		assert.Equal(t, oneGB, detail["retained_bytes"])
		assert.Equal(t, true, detail["active"])
	}
}

func TestSlowSlot_ActiveAtFiveGB_Critical(t *testing.T) {
	fiveGB := int64(5 * (1 << 30))
	snap := slotSnapshot([]collector.SlotInfo{
		{SlotName: "fivetran", Active: true, SlotType: "logical",
			RetainedBytes: fiveGB},
	})

	findings := ruleInactiveSlots(snap, nil, nil, nil)

	if assert.Len(t, findings, 1) {
		assert.Equal(t, "slow_replication_slot", findings[0].Category)
		assert.Equal(t, "critical", findings[0].Severity,
			"5 GB (5x default 1 GB) should be critical")
		assert.Equal(t, fiveGB, findings[0].Detail["retained_bytes"])
	}
}

func TestSlowSlot_CustomThreshold_Critical(t *testing.T) {
	// Custom threshold: 500 MB. Critical at 5 * 500 MB = 2.5 GB.
	// 2 GB is below critical but above warning.
	halfGB := int64(500 * (1 << 20))
	twoGB := int64(2 * (1 << 30))
	cfg := &config.Config{
		Analyzer: config.AnalyzerConfig{
			SlowSlotRetainedBytes: halfGB,
		},
	}

	snap := slotSnapshot([]collector.SlotInfo{
		{SlotName: "cdc", Active: true, SlotType: "logical",
			RetainedBytes: twoGB},
	})

	findings := ruleInactiveSlots(snap, nil, cfg, nil)

	if assert.Len(t, findings, 1) {
		assert.Equal(t, "slow_replication_slot", findings[0].Category)
		// 2 GB < 5 * 500 MB = 2.5 GB, so still warning
		assert.Equal(t, "warning", findings[0].Severity,
			"2 GB < 5 * 500 MB, should be warning not critical")
	}
}

func TestSlowSlot_CustomThreshold_ExactCriticalBoundary(t *testing.T) {
	// Custom threshold: 500 MB. Critical at >= 5 * 500 MB = 2.5 GB.
	halfGB := int64(500 * (1 << 20))
	criticalBytes := halfGB * 5 // exactly 2.5 GB
	cfg := &config.Config{
		Analyzer: config.AnalyzerConfig{
			SlowSlotRetainedBytes: halfGB,
		},
	}

	snap := slotSnapshot([]collector.SlotInfo{
		{SlotName: "cdc", Active: true, SlotType: "logical",
			RetainedBytes: criticalBytes},
	})

	findings := ruleInactiveSlots(snap, nil, cfg, nil)

	if assert.Len(t, findings, 1) {
		assert.Equal(t, "critical", findings[0].Severity,
			"exactly 5x threshold should be critical")
	}
}

func TestSlowSlot_NilConfig_UsesOneGBDefault(t *testing.T) {
	oneGB := int64(1 << 30)
	snap := slotSnapshot([]collector.SlotInfo{
		{SlotName: "replica_slot", Active: true, SlotType: "physical",
			RetainedBytes: oneGB},
	})

	// Pass nil config — function should default to 1 GB threshold.
	findings := ruleInactiveSlots(snap, nil, nil, nil)

	if assert.Len(t, findings, 1,
		"nil config should use 1 GB default; 1 GB retained should trigger") {
		assert.Equal(t, "slow_replication_slot", findings[0].Category)
		assert.Equal(t, "warning", findings[0].Severity)
		assert.Equal(t, "physical", findings[0].Detail["slot_type"])
	}
}

func TestSlowSlot_InactiveSlot_StillProducesInactiveCategory(t *testing.T) {
	// Regression check: inactive slots should still produce
	// "inactive_slot" category, not "slow_replication_slot".
	snap := slotSnapshot([]collector.SlotInfo{
		{SlotName: "orphan", Active: false, SlotType: "logical",
			RetainedBytes: 10 * (1 << 30)}, // 10 GB but inactive
	})

	findings := ruleInactiveSlots(snap, nil, nil, nil)

	if assert.Len(t, findings, 1) {
		f := findings[0]
		assert.Equal(t, "inactive_slot", f.Category,
			"inactive slots must still use 'inactive_slot' category")
		assert.Equal(t, "warning", f.Severity)
		assert.Equal(t, "safe", f.ActionRisk)
		assert.Contains(t, f.RecommendedSQL, "pg_drop_replication_slot")
	}
}

func TestSlowSlot_MixedActiveAndInactive(t *testing.T) {
	oneGB := int64(1 << 30)
	snap := slotSnapshot([]collector.SlotInfo{
		{SlotName: "healthy", Active: true, SlotType: "logical",
			RetainedBytes: 100}, // well below threshold
		{SlotName: "slow_cdc", Active: true, SlotType: "logical",
			RetainedBytes: 2 * oneGB}, // above threshold
		{SlotName: "dead", Active: false, SlotType: "physical",
			RetainedBytes: 0},
	})

	findings := ruleInactiveSlots(snap, nil, nil, nil)

	assert.Len(t, findings, 2, "one slow + one inactive = 2 findings")

	categories := map[string]string{}
	for _, f := range findings {
		categories[f.ObjectIdentifier] = f.Category
	}
	assert.Equal(t, "slow_replication_slot", categories["slot:slow_cdc"])
	assert.Equal(t, "inactive_slot", categories["slot:dead"])
}

// ---------------------------------------------------------------------------
// ruleXIDWraparound — updated recommendation and SQL assertions
// ---------------------------------------------------------------------------

func TestXIDWraparound_RecommendedSQL_ContainsPgStatActivity(t *testing.T) {
	cfg := &config.Config{
		Analyzer: config.AnalyzerConfig{
			XIDWraparoundWarning:  500000000,
			XIDWraparoundCritical: 1000000000,
		},
	}

	findings := ruleXIDWraparound(700000000, cfg)

	if assert.Len(t, findings, 1) {
		f := findings[0]
		assert.Contains(t, f.RecommendedSQL, "pg_stat_activity",
			"RecommendedSQL should query pg_stat_activity for xmin holders")
		assert.Contains(t, f.RecommendedSQL, "backend_xmin",
			"RecommendedSQL should reference backend_xmin column")
	}
}

func TestXIDWraparound_ActionRisk_IsModerate(t *testing.T) {
	cfg := &config.Config{
		Analyzer: config.AnalyzerConfig{
			XIDWraparoundWarning:  500000000,
			XIDWraparoundCritical: 1000000000,
		},
	}

	findings := ruleXIDWraparound(700000000, cfg)

	if assert.Len(t, findings, 1) {
		assert.Equal(t, "moderate", findings[0].ActionRisk,
			"XID wraparound finding should have moderate action risk")
	}
}

func TestXIDWraparound_Recommendation_MentionsXmin(t *testing.T) {
	cfg := &config.Config{
		Analyzer: config.AnalyzerConfig{
			XIDWraparoundWarning:  500000000,
			XIDWraparoundCritical: 1000000000,
		},
	}

	findings := ruleXIDWraparound(700000000, cfg)

	if assert.Len(t, findings, 1) {
		assert.True(t,
			strings.Contains(findings[0].Recommendation, "xmin"),
			"recommendation should mention 'xmin' as part of the diagnostic guidance")
	}
}
