package rca

import (
	"strings"
	"testing"
	"time"
)

// No concurrent-access tests: the log trees are pure functions of one
// signal and touch no engine state.

func logTreeFor(t *testing.T, id string, metrics map[string]any) Incident {
	t.Helper()
	incs := newTestEngine().runLogDecisionTrees([]*Signal{{
		ID: id, FiredAt: time.Now(), Severity: "critical", Metrics: metrics,
	}})
	if len(incs) != 1 {
		t.Fatalf("%s: got %d incidents, want 1", id, len(incs))
	}
	return incs[0]
}

func TestLogTree_InternalErrorIsWarningNotCorruption(t *testing.T) {
	inc := logTreeFor(t, "log_internal_error", map[string]any{
		"message": "cache lookup failed for operator class 0", "sql_state": "XX000",
	})
	if inc.Severity != "warning" {
		t.Errorf("Severity = %s, want warning", inc.Severity)
	}
	if strings.Contains(strings.ToLower(inc.RootCause), "corruption") {
		t.Errorf("RootCause %q calls an XX000 error corruption", inc.RootCause)
	}
	if !strings.Contains(inc.RootCause, "XX000") {
		t.Errorf("RootCause %q does not name XX000", inc.RootCause)
	}
	if inc.ActionRisk == "high_risk" {
		t.Errorf("ActionRisk = high_risk for an internal error")
	}
	if inc.CausalChain[0].Evidence != "cache lookup failed for operator class 0" {
		t.Errorf("Evidence = %q, want the log message", inc.CausalChain[0].Evidence)
	}
}

func TestLogTree_WraparoundOutage(t *testing.T) {
	msg := `database is not accepting commands that assign new transaction IDs ` +
		`to avoid wraparound data loss in database "appdb"`
	inc := logTreeFor(t, "log_wraparound_outage", map[string]any{
		"message": msg, "database": "appdb", "wraparound_database": "appdb",
	})
	if inc.Severity != "critical" || inc.ActionRisk != "high_risk" {
		t.Errorf("Severity/ActionRisk = %s/%s, want critical/high_risk",
			inc.Severity, inc.ActionRisk)
	}
	if !strings.Contains(inc.RootCause, "refus") {
		t.Errorf("RootCause %q does not say the database refuses writes", inc.RootCause)
	}
	for _, want := range []string{"pg_stat_activity", "pg_replication_slots",
		"pg_prepared_xacts"} {
		if !strings.Contains(inc.RecommendedSQL, want) {
			t.Errorf("RecommendedSQL missing %s", want)
		}
	}
	if !strings.Contains(inc.CausalChain[0].Evidence, `"appdb"`) {
		t.Errorf("Evidence %q does not carry the log message", inc.CausalChain[0].Evidence)
	}
}

func TestLogTree_MultixactLimits(t *testing.T) {
	for _, id := range []string{"log_multixact_wraparound_warning",
		"log_multixact_wraparound_outage"} {
		inc := logTreeFor(t, id, map[string]any{"message": "multixact limit"})
		if inc.Severity != "critical" {
			t.Errorf("%s: Severity = %s, want critical", id, inc.Severity)
		}
		if !strings.Contains(inc.RootCause, "MultiXact") {
			t.Errorf("%s: RootCause %q does not name MultiXact", id, inc.RootCause)
		}
		if !strings.Contains(inc.RecommendedSQL, "mxid_age") {
			t.Errorf("%s: RecommendedSQL %q lacks mxid_age", id, inc.RecommendedSQL)
		}
	}
}

// Only max_wal_size changes how often WAL volume forces a checkpoint;
// checkpoint_completion_target only spreads the writes.
func TestLogTree_CheckpointAdviceNamesMaxWALSizeOnly(t *testing.T) {
	inc := logTreeFor(t, "log_checkpoint_too_frequent", map[string]any{
		"message": "checkpoints are occurring too frequently (5 seconds apart)",
	})
	if !strings.Contains(inc.RootCause, "max_wal_size") {
		t.Errorf("RootCause %q does not name max_wal_size", inc.RootCause)
	}
	if strings.Contains(inc.RootCause, "checkpoint_completion_target") {
		t.Errorf("RootCause %q still cites checkpoint_completion_target", inc.RootCause)
	}
}

func TestLogTree_CorruptionStaysCritical(t *testing.T) {
	inc := logTreeFor(t, "log_data_corruption", map[string]any{
		"message": "invalid page in block 0 of relation base/5/16434",
	})
	if inc.Severity != "critical" || inc.ActionRisk != "high_risk" {
		t.Errorf("Severity/ActionRisk = %s/%s, want critical/high_risk",
			inc.Severity, inc.ActionRisk)
	}
}

// The slot rule fires on invalidation, so the incident must say the slot
// was invalidated and name it, not merely that it retains WAL.
func TestLogTree_SlotInvalidationNamesTheSlot(t *testing.T) {
	inc := logTreeFor(t, "log_replication_slot_inactive", map[string]any{
		"message":   `invalidating obsolete replication slot "corpus_slot"`,
		"slot_name": "corpus_slot",
	})
	if !strings.Contains(inc.RootCause, `invalidated slot "corpus_slot"`) {
		t.Errorf("RootCause %q does not name the invalidated slot", inc.RootCause)
	}
	bare := logTreeFor(t, "log_replication_slot_inactive", map[string]any{})
	if !strings.Contains(bare.RootCause, "invalidated a slot") {
		t.Errorf("RootCause without slot_name = %q", bare.RootCause)
	}
}
