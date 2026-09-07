package rca

import (
	"strings"
	"testing"
	"time"
)

// nopLog is a no-op logger for test correlators.
func nopLog(string, string, ...any) {}

// ---------------------------------------------------------------------------
// No actions, no correlation
// ---------------------------------------------------------------------------

func TestCorrelate_NoActions(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   time.Now(),
			Severity:     "warning",
			SignalIDs:    []string{"log_slow_query"},
			DatabaseName: "mydb",
		},
	}

	annotations, selfCaused, manualReview := c.Correlate(
		incidents, nil, nil)

	if len(annotations) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(annotations))
	}
	if len(annotations[0].SageActions) != 0 {
		t.Errorf("SageActions = %v, want empty", annotations[0].SageActions)
	}
	if annotations[0].CausalAction != nil {
		t.Errorf("CausalAction should be nil")
	}
	if len(selfCaused) != 0 {
		t.Errorf("selfCaused = %d, want 0", len(selfCaused))
	}
	if len(manualReview) != 0 {
		t.Errorf("manualReview = %d, want 0", len(manualReview))
	}
}

// ---------------------------------------------------------------------------
// Informational layer: action within 30min window + matching database
// ---------------------------------------------------------------------------

func TestCorrelate_InformationalLayer(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "warning",
			SignalIDs:    []string{"log_lock_timeout"},
			DatabaseName: "mydb",
		},
	}
	// Action within window, matching database, but no causal path match.
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "analyze_table",
			ExecutedAt:  now.Add(-10 * time.Minute),
			Database:    "mydb",
			Description: "ANALYZE public.users",
		},
	}

	annotations, selfCaused, manualReview := c.Correlate(
		incidents, actions, nil)

	if len(annotations) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(annotations))
	}
	if len(annotations[0].SageActions) != 1 {
		t.Fatalf("SageActions count = %d, want 1",
			len(annotations[0].SageActions))
	}
	if annotations[0].SageActions[0].ID != "act-1" {
		t.Errorf("SageActions[0].ID = %q, want act-1",
			annotations[0].SageActions[0].ID)
	}
	// No causal path matched: "analyze_table:log_lock_timeout" is not in
	// causalPaths.
	if annotations[0].CausalAction != nil {
		t.Errorf("CausalAction should be nil (no causal path)")
	}
	if len(selfCaused) != 0 {
		t.Errorf("selfCaused = %d, want 0", len(selfCaused))
	}
	if len(manualReview) != 0 {
		t.Errorf("manualReview = %d, want 0", len(manualReview))
	}
}

// ---------------------------------------------------------------------------
// Causal path: set_work_mem -> log_out_of_memory
// ---------------------------------------------------------------------------

func TestCorrelate_CausalPath_SetWorkMem_OOM(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "critical",
			SignalIDs:    []string{"log_out_of_memory"},
			DatabaseName: "mydb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "set_work_mem",
			ExecutedAt:  now.Add(-5 * time.Minute),
			Database:    "mydb",
			Description: "SET work_mem = '1GB'",
		},
	}

	annotations, selfCaused, manualReview := c.Correlate(
		incidents, actions, nil)

	if len(annotations) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(annotations))
	}
	if annotations[0].CausalAction == nil {
		t.Fatal("CausalAction should not be nil")
	}
	if annotations[0].CausalAction.ID != "act-1" {
		t.Errorf("CausalAction.ID = %q, want act-1",
			annotations[0].CausalAction.ID)
	}
	if annotations[0].CausalReason == "" {
		t.Errorf("CausalReason should not be empty")
	}
	if len(selfCaused) != 1 {
		t.Fatalf("selfCaused = %d, want 1", len(selfCaused))
	}
	if selfCaused[0].Source != "self_action" {
		t.Errorf("selfCaused[0].Source = %q, want 'self_action'",
			selfCaused[0].Source)
	}
	if selfCaused[0].Confidence != 0.9 {
		t.Errorf("selfCaused[0].Confidence = %f, want 0.9",
			selfCaused[0].Confidence)
	}
	if !strings.Contains(selfCaused[0].RootCause, "Self-caused") {
		t.Errorf("RootCause = %q, want substring 'Self-caused'",
			selfCaused[0].RootCause)
	}
	if len(manualReview) != 0 {
		t.Errorf("manualReview = %d, want 0", len(manualReview))
	}
}

// ---------------------------------------------------------------------------
// Causal path: create_index -> log_disk_full
// ---------------------------------------------------------------------------

func TestCorrelate_CausalPath_CreateIndex_DiskFull(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "critical",
			SignalIDs:    []string{"log_disk_full"},
			DatabaseName: "proddb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-2",
			Family:      "create_index",
			ExecutedAt:  now.Add(-15 * time.Minute),
			Database:    "proddb",
			Description: "CREATE INDEX CONCURRENTLY ...",
		},
	}

	_, selfCaused, manualReview := c.Correlate(incidents, actions, nil)

	if len(selfCaused) != 1 {
		t.Fatalf("selfCaused = %d, want 1", len(selfCaused))
	}
	if selfCaused[0].Source != "self_action" {
		t.Errorf("Source = %q, want 'self_action'", selfCaused[0].Source)
	}
	if !strings.Contains(selfCaused[0].RootCause, "disk space") {
		t.Errorf("RootCause = %q, want substring 'disk space'",
			selfCaused[0].RootCause)
	}
	if len(manualReview) != 0 {
		t.Errorf("manualReview = %d, want 0", len(manualReview))
	}
}

// ---------------------------------------------------------------------------
// Causal path: vacuum_full -> log_lock_timeout
// ---------------------------------------------------------------------------

func TestCorrelate_CausalPath_VacuumFull_LockTimeout(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "warning",
			SignalIDs:    []string{"log_lock_timeout"},
			DatabaseName: "mydb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-3",
			Family:      "vacuum_full",
			ExecutedAt:  now.Add(-2 * time.Minute),
			Database:    "mydb",
			Description: "VACUUM FULL public.orders",
		},
	}

	_, selfCaused, _ := c.Correlate(incidents, actions, nil)

	if len(selfCaused) != 1 {
		t.Fatalf("selfCaused = %d, want 1", len(selfCaused))
	}
	if selfCaused[0].Source != "self_action" {
		t.Errorf("Source = %q, want 'self_action'", selfCaused[0].Source)
	}
	if !strings.Contains(selfCaused[0].RootCause, "VACUUM FULL") {
		t.Errorf("RootCause = %q, want substring 'VACUUM FULL'",
			selfCaused[0].RootCause)
	}
	// Original was "warning", self-caused should escalate to "critical".
	if selfCaused[0].Severity != "critical" {
		t.Errorf("Severity = %q, want 'critical' (escalated from warning)",
			selfCaused[0].Severity)
	}
}

// ---------------------------------------------------------------------------
// Causal path: create_index -> log_temp_file_created
// ---------------------------------------------------------------------------

func TestCorrelate_CausalPath_CreateIndex_TempFile(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "warning",
			SignalIDs:    []string{"log_temp_file_created"},
			DatabaseName: "mydb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-4",
			Family:      "create_index",
			ExecutedAt:  now.Add(-20 * time.Minute),
			Database:    "mydb",
			Description: "CREATE INDEX ...",
		},
	}

	_, selfCaused, _ := c.Correlate(incidents, actions, nil)

	if len(selfCaused) != 1 {
		t.Fatalf("selfCaused = %d, want 1", len(selfCaused))
	}
	if selfCaused[0].Source != "self_action" {
		t.Errorf("Source = %q, want 'self_action'", selfCaused[0].Source)
	}
	if !strings.Contains(selfCaused[0].RootCause, "spilled to disk") {
		t.Errorf("RootCause = %q, want substring 'spilled to disk'",
			selfCaused[0].RootCause)
	}
}

// ---------------------------------------------------------------------------
// Causal path: drop_index -> log_slow_query
// ---------------------------------------------------------------------------

func TestCorrelate_CausalPath_DropIndex_SlowQuery(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "info",
			SignalIDs:    []string{"log_slow_query"},
			DatabaseName: "mydb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-5",
			Family:      "drop_index",
			ExecutedAt:  now.Add(-25 * time.Minute),
			Database:    "mydb",
			Description: "DROP INDEX public.idx_users_email",
		},
	}

	_, selfCaused, _ := c.Correlate(incidents, actions, nil)

	if len(selfCaused) != 1 {
		t.Fatalf("selfCaused = %d, want 1", len(selfCaused))
	}
	if selfCaused[0].Source != "self_action" {
		t.Errorf("Source = %q, want 'self_action'", selfCaused[0].Source)
	}
	if !strings.Contains(selfCaused[0].RootCause, "performance regression") {
		t.Errorf("RootCause = %q, want substring 'performance regression'",
			selfCaused[0].RootCause)
	}
	// Original was "info", self-caused should escalate to "warning".
	if selfCaused[0].Severity != "warning" {
		t.Errorf("Severity = %q, want 'warning' (escalated from info)",
			selfCaused[0].Severity)
	}
}

// ---------------------------------------------------------------------------
// Anti-oscillation: rollback threshold exceeded -> manualReview
// ---------------------------------------------------------------------------

func TestCorrelate_AntiOscillation(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "critical",
			SignalIDs:    []string{"log_out_of_memory"},
			DatabaseName: "mydb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "set_work_mem",
			ExecutedAt:  now.Add(-5 * time.Minute),
			Database:    "mydb",
			Description: "SET work_mem = '1GB'",
		},
	}
	// Rollback history: set_work_mem rolled back 2+ times.
	rollbackHistory := []SageAction{
		{
			ID:         "hist-1",
			Family:     "set_work_mem",
			RolledBack: true,
		},
		{
			ID:         "hist-2",
			Family:     "set_work_mem",
			RolledBack: true,
		},
	}

	_, selfCaused, manualReview := c.Correlate(
		incidents, actions, rollbackHistory)

	if len(selfCaused) != 0 {
		t.Errorf("selfCaused = %d, want 0 (anti-oscillation triggered)",
			len(selfCaused))
	}
	if len(manualReview) != 1 {
		t.Fatalf("manualReview = %d, want 1", len(manualReview))
	}
	if manualReview[0].Source != "manual_review_required" {
		t.Errorf("Source = %q, want 'manual_review_required'",
			manualReview[0].Source)
	}
	if manualReview[0].Severity != "critical" {
		t.Errorf("Severity = %q, want 'critical'",
			manualReview[0].Severity)
	}
	if manualReview[0].Confidence != 1.0 {
		t.Errorf("Confidence = %f, want 1.0", manualReview[0].Confidence)
	}
	if !strings.Contains(manualReview[0].RootCause, "Manual review required") {
		t.Errorf("RootCause = %q, want substring 'Manual review required'",
			manualReview[0].RootCause)
	}
	if !strings.Contains(manualReview[0].RootCause, "rolled back") {
		t.Errorf("RootCause = %q, want substring 'rolled back'",
			manualReview[0].RootCause)
	}
}

func TestCorrelate_AntiOscillation_BelowThreshold(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "critical",
			SignalIDs:    []string{"log_out_of_memory"},
			DatabaseName: "mydb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "set_work_mem",
			ExecutedAt:  now.Add(-5 * time.Minute),
			Database:    "mydb",
			Description: "SET work_mem = '1GB'",
		},
	}
	// Only 1 rollback -- below threshold of 2.
	rollbackHistory := []SageAction{
		{
			ID:         "hist-1",
			Family:     "set_work_mem",
			RolledBack: true,
		},
	}

	_, selfCaused, manualReview := c.Correlate(
		incidents, actions, rollbackHistory)

	// Below threshold: selfCaused, not manualReview.
	if len(selfCaused) != 1 {
		t.Errorf("selfCaused = %d, want 1", len(selfCaused))
	}
	if len(manualReview) != 0 {
		t.Errorf("manualReview = %d, want 0", len(manualReview))
	}
}

// ---------------------------------------------------------------------------
// Database mismatch: action.Database != incident.DatabaseName
// ---------------------------------------------------------------------------

func TestCorrelate_DatabaseMismatch(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "critical",
			SignalIDs:    []string{"log_out_of_memory"},
			DatabaseName: "proddb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "set_work_mem",
			ExecutedAt:  now.Add(-5 * time.Minute),
			Database:    "otherdb", // Different database.
			Description: "SET work_mem = '1GB'",
		},
	}

	annotations, selfCaused, manualReview := c.Correlate(
		incidents, actions, nil)

	if len(annotations) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(annotations))
	}
	// Action should NOT appear in SageActions due to DB mismatch.
	if len(annotations[0].SageActions) != 0 {
		t.Errorf("SageActions = %d, want 0 (database mismatch)",
			len(annotations[0].SageActions))
	}
	if len(selfCaused) != 0 {
		t.Errorf("selfCaused = %d, want 0", len(selfCaused))
	}
	if len(manualReview) != 0 {
		t.Errorf("manualReview = %d, want 0", len(manualReview))
	}
}

// ---------------------------------------------------------------------------
// Empty incident database matches any action
// ---------------------------------------------------------------------------

func TestCorrelate_EmptyIncidentDatabase(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "critical",
			SignalIDs:    []string{"log_out_of_memory"},
			DatabaseName: "", // Empty: should match any action.
		},
	}
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "set_work_mem",
			ExecutedAt:  now.Add(-5 * time.Minute),
			Database:    "anydb",
			Description: "SET work_mem = '1GB'",
		},
	}

	annotations, selfCaused, _ := c.Correlate(incidents, actions, nil)

	if len(annotations) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(annotations))
	}
	if len(annotations[0].SageActions) != 1 {
		t.Errorf("SageActions = %d, want 1 (empty DB matches any)",
			len(annotations[0].SageActions))
	}
	if len(selfCaused) != 1 {
		t.Errorf("selfCaused = %d, want 1", len(selfCaused))
	}
}

// ---------------------------------------------------------------------------
// Action outside lookback window: >30 minutes before incident
// ---------------------------------------------------------------------------

func TestCorrelate_ActionOutsideWindow(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "critical",
			SignalIDs:    []string{"log_out_of_memory"},
			DatabaseName: "mydb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "set_work_mem",
			ExecutedAt:  now.Add(-31 * time.Minute), // Outside window.
			Database:    "mydb",
			Description: "SET work_mem = '1GB'",
		},
	}

	annotations, selfCaused, _ := c.Correlate(incidents, actions, nil)

	if len(annotations[0].SageActions) != 0 {
		t.Errorf("SageActions = %d, want 0 (outside 30min window)",
			len(annotations[0].SageActions))
	}
	if len(selfCaused) != 0 {
		t.Errorf("selfCaused = %d, want 0", len(selfCaused))
	}
}

// ---------------------------------------------------------------------------
// Action AFTER incident: action.ExecutedAt > incident.DetectedAt
// ---------------------------------------------------------------------------

func TestCorrelate_ActionAfterIncident(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "critical",
			SignalIDs:    []string{"log_out_of_memory"},
			DatabaseName: "mydb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "set_work_mem",
			ExecutedAt:  now.Add(5 * time.Minute), // After the incident.
			Database:    "mydb",
			Description: "SET work_mem = '1GB'",
		},
	}

	annotations, selfCaused, _ := c.Correlate(incidents, actions, nil)

	if len(annotations[0].SageActions) != 0 {
		t.Errorf("SageActions = %d, want 0 (action is after incident)",
			len(annotations[0].SageActions))
	}
	if len(selfCaused) != 0 {
		t.Errorf("selfCaused = %d, want 0", len(selfCaused))
	}
}

// ---------------------------------------------------------------------------
// Action exactly at window boundary
// ---------------------------------------------------------------------------

func TestCorrelate_ActionAtExactBoundary(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "critical",
			SignalIDs:    []string{"log_out_of_memory"},
			DatabaseName: "mydb",
		},
	}
	// Exactly 30 minutes before = cutoff. actionsInWindow uses
	// Before(cutoff), so an action exactly at cutoff is NOT before it
	// and should be included.
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "set_work_mem",
			ExecutedAt:  now.Add(-30 * time.Minute),
			Database:    "mydb",
			Description: "SET work_mem = '1GB'",
		},
	}

	annotations, _, _ := c.Correlate(incidents, actions, nil)

	// Exactly at cutoff: Before(cutoff) returns false, so action is included.
	if len(annotations[0].SageActions) != 1 {
		t.Errorf("SageActions = %d, want 1 (at exact boundary)",
			len(annotations[0].SageActions))
	}
}

// ---------------------------------------------------------------------------
// Action exactly at incident time
// ---------------------------------------------------------------------------

func TestCorrelate_ActionAtIncidentTime(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "critical",
			SignalIDs:    []string{"log_out_of_memory"},
			DatabaseName: "mydb",
		},
	}
	// Action exactly at incident time: After(inc.DetectedAt) is false
	// for equal time, so should be included.
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "set_work_mem",
			ExecutedAt:  now,
			Database:    "mydb",
			Description: "SET work_mem = '1GB'",
		},
	}

	annotations, _, _ := c.Correlate(incidents, actions, nil)

	if len(annotations[0].SageActions) != 1 {
		t.Errorf("SageActions = %d, want 1 (action at incident time)",
			len(annotations[0].SageActions))
	}
}

// ---------------------------------------------------------------------------
// escalateSeverity
// ---------------------------------------------------------------------------

func TestEscalateSeverity(t *testing.T) {
	tests := []struct {
		original string
		want     string
	}{
		{"info", "warning"},
		{"warning", "critical"},
		{"critical", "critical"},
		{"unknown", "critical"}, // default case
		{"", "critical"},        // empty string hits default
	}
	for _, tt := range tests {
		t.Run(tt.original, func(t *testing.T) {
			got := escalateSeverity(tt.original)
			if got != tt.want {
				t.Errorf("escalateSeverity(%q) = %q, want %q",
					tt.original, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// countRollbacksByFamily
// ---------------------------------------------------------------------------

func TestCountRollbacksByFamily(t *testing.T) {
	t.Run("empty history", func(t *testing.T) {
		got := countRollbacksByFamily(nil)
		if len(got) != 0 {
			t.Errorf("expected empty map, got %v", got)
		}
	})

	t.Run("only counts RolledBack=true", func(t *testing.T) {
		history := []SageAction{
			{Family: "set_work_mem", RolledBack: true},
			{Family: "set_work_mem", RolledBack: false},
			{Family: "set_work_mem", RolledBack: true},
			{Family: "create_index", RolledBack: true},
			{Family: "create_index", RolledBack: false},
		}
		got := countRollbacksByFamily(history)

		if got["set_work_mem"] != 2 {
			t.Errorf("set_work_mem = %d, want 2", got["set_work_mem"])
		}
		if got["create_index"] != 1 {
			t.Errorf("create_index = %d, want 1", got["create_index"])
		}
	})

	t.Run("no rollbacks", func(t *testing.T) {
		history := []SageAction{
			{Family: "set_work_mem", RolledBack: false},
			{Family: "create_index", RolledBack: false},
		}
		got := countRollbacksByFamily(history)

		if got["set_work_mem"] != 0 {
			t.Errorf("set_work_mem = %d, want 0", got["set_work_mem"])
		}
		if got["create_index"] != 0 {
			t.Errorf("create_index = %d, want 0", got["create_index"])
		}
	})
}

// ---------------------------------------------------------------------------
// databaseMatches
// ---------------------------------------------------------------------------

func TestDatabaseMatches(t *testing.T) {
	tests := []struct {
		name       string
		incidentDB string
		actionDB   string
		want       bool
	}{
		{"same database", "mydb", "mydb", true},
		{"different database", "mydb", "otherdb", false},
		{"empty incident matches any", "", "anydb", true},
		{"empty incident matches empty action", "", "", true},
		{"non-empty incident vs empty action", "mydb", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := databaseMatches(tt.incidentDB, tt.actionDB)
			if got != tt.want {
				t.Errorf("databaseMatches(%q, %q) = %v, want %v",
					tt.incidentDB, tt.actionDB, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// buildSelfCausedIncident — field correctness
// ---------------------------------------------------------------------------

func TestBuildSelfCausedIncident_Fields(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	original := &Incident{
		DetectedAt:      now,
		Severity:        "warning",
		SignalIDs:       []string{"log_lock_timeout"},
		AffectedObjects: []string{"public.orders"},
		RollbackSQL:     "ROLLBACK;",
		DatabaseName:    "mydb",
	}
	action := &SageAction{
		ID:          "act-1",
		Family:      "vacuum_full",
		ExecutedAt:  now.Add(-2 * time.Minute),
		Description: "VACUUM FULL public.orders",
	}
	match := &causalMatch{
		action: action,
		reason: "VACUUM FULL holds AccessExclusive lock, causing lock timeouts",
	}

	inc := c.buildSelfCausedIncident(original, match)

	if inc.Source != "self_action" {
		t.Errorf("Source = %q, want 'self_action'", inc.Source)
	}
	if inc.Confidence != 0.9 {
		t.Errorf("Confidence = %f, want 0.9", inc.Confidence)
	}
	if inc.DatabaseName != "mydb" {
		t.Errorf("DatabaseName = %q, want 'mydb'", inc.DatabaseName)
	}
	if inc.Severity != "critical" {
		t.Errorf("Severity = %q, want 'critical' (escalated from warning)",
			inc.Severity)
	}
	if inc.ActionRisk != "high_risk" {
		t.Errorf("ActionRisk = %q, want 'high_risk'", inc.ActionRisk)
	}
	if len(inc.CausalChain) != 2 {
		t.Fatalf("CausalChain len = %d, want 2", len(inc.CausalChain))
	}
	if inc.CausalChain[0].Order != 1 || inc.CausalChain[1].Order != 2 {
		t.Errorf("CausalChain order: [%d, %d], want [1, 2]",
			inc.CausalChain[0].Order, inc.CausalChain[1].Order)
	}
	if !strings.Contains(inc.RootCause, "Self-caused") {
		t.Errorf("RootCause = %q, want substring 'Self-caused'",
			inc.RootCause)
	}
	if !strings.Contains(inc.RootCause, "act-1") {
		t.Errorf("RootCause = %q, want substring 'act-1'", inc.RootCause)
	}
}

// ---------------------------------------------------------------------------
// buildManualReviewIncident — field correctness
// ---------------------------------------------------------------------------

func TestBuildManualReviewIncident_Fields(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	original := &Incident{
		DetectedAt:      now,
		Severity:        "critical",
		SignalIDs:       []string{"log_out_of_memory"},
		AffectedObjects: []string{"public.big_table"},
		DatabaseName:    "proddb",
	}
	action := &SageAction{
		ID:     "act-1",
		Family: "set_work_mem",
	}
	match := &causalMatch{
		action: action,
		reason: "Increasing work_mem may have caused OOM",
	}

	inc := c.buildManualReviewIncident(original, match)

	if inc.Source != "manual_review_required" {
		t.Errorf("Source = %q, want 'manual_review_required'", inc.Source)
	}
	if inc.Confidence != 1.0 {
		t.Errorf("Confidence = %f, want 1.0", inc.Confidence)
	}
	if inc.Severity != "critical" {
		t.Errorf("Severity = %q, want 'critical'", inc.Severity)
	}
	if inc.DatabaseName != "proddb" {
		t.Errorf("DatabaseName = %q, want 'proddb'", inc.DatabaseName)
	}
	if inc.RecommendedSQL != "" {
		t.Errorf("RecommendedSQL = %q, want empty (human must decide)",
			inc.RecommendedSQL)
	}
	if !strings.Contains(inc.RootCause, "Manual review required") {
		t.Errorf("RootCause = %q, want substring 'Manual review required'",
			inc.RootCause)
	}
	if len(inc.CausalChain) != 1 {
		t.Fatalf("CausalChain len = %d, want 1", len(inc.CausalChain))
	}
	if !strings.Contains(inc.CausalChain[0].Description,
		"anti-oscillation") {
		t.Errorf("CausalChain[0].Description = %q, want 'anti-oscillation'",
			inc.CausalChain[0].Description)
	}
}

// ---------------------------------------------------------------------------
// Multiple incidents with mixed causal and non-causal
// ---------------------------------------------------------------------------

func TestCorrelate_MultipleIncidents(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:           "inc-1",
			DetectedAt:   now,
			Severity:     "critical",
			SignalIDs:    []string{"log_out_of_memory"},
			DatabaseName: "mydb",
		},
		{
			ID:           "inc-2",
			DetectedAt:   now,
			Severity:     "warning",
			SignalIDs:    []string{"log_lock_timeout"},
			DatabaseName: "mydb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "set_work_mem",
			ExecutedAt:  now.Add(-5 * time.Minute),
			Database:    "mydb",
			Description: "SET work_mem = '1GB'",
		},
	}

	annotations, selfCaused, _ := c.Correlate(incidents, actions, nil)

	if len(annotations) != 2 {
		t.Fatalf("expected 2 annotations, got %d", len(annotations))
	}
	// inc-1 (log_out_of_memory) matches set_work_mem causal path.
	if annotations[0].CausalAction == nil {
		t.Errorf("inc-1 should have a causal action")
	}
	// inc-2 (log_lock_timeout) does NOT match set_work_mem.
	if annotations[1].CausalAction != nil {
		t.Errorf("inc-2 should NOT have a causal action")
	}
	// Only 1 self-caused incident (for OOM).
	if len(selfCaused) != 1 {
		t.Errorf("selfCaused = %d, want 1", len(selfCaused))
	}
}

// ---------------------------------------------------------------------------
// NewSelfActionCorrelator defaults
// ---------------------------------------------------------------------------

func TestNewSelfActionCorrelator_Defaults(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)

	if c.lookbackWindow != 30*time.Minute {
		t.Errorf("lookbackWindow = %v, want 30m", c.lookbackWindow)
	}
	if c.rollbackLookback != 30*24*time.Hour {
		t.Errorf("rollbackLookback = %v, want 30d", c.rollbackLookback)
	}
	if c.rollbackThreshold != 2 {
		t.Errorf("rollbackThreshold = %d, want 2", c.rollbackThreshold)
	}
	if c.logFn == nil {
		t.Errorf("logFn should not be nil")
	}
}

// ---------------------------------------------------------------------------
// Empty incidents list
// ---------------------------------------------------------------------------

func TestCorrelate_EmptyIncidents(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)

	actions := []SageAction{
		{
			ID:         "act-1",
			Family:     "set_work_mem",
			ExecutedAt: time.Now(),
			Database:   "mydb",
		},
	}

	annotations, selfCaused, manualReview := c.Correlate(
		nil, actions, nil)

	if len(annotations) != 0 {
		t.Errorf("annotations = %d, want 0", len(annotations))
	}
	if len(selfCaused) != 0 {
		t.Errorf("selfCaused = %d, want 0", len(selfCaused))
	}
	if len(manualReview) != 0 {
		t.Errorf("manualReview = %d, want 0", len(manualReview))
	}
}

// ---------------------------------------------------------------------------
// Incident with multiple signal IDs — any match triggers causal
// ---------------------------------------------------------------------------

func TestCorrelate_MultipleSignalIDs(t *testing.T) {
	c := NewSelfActionCorrelator(nopLog)
	now := time.Now()

	incidents := []Incident{
		{
			ID:         "inc-1",
			DetectedAt: now,
			Severity:   "critical",
			SignalIDs: []string{
				"log_connection_refused",
				"log_out_of_memory",
			},
			DatabaseName: "mydb",
		},
	}
	actions := []SageAction{
		{
			ID:          "act-1",
			Family:      "set_work_mem",
			ExecutedAt:  now.Add(-5 * time.Minute),
			Database:    "mydb",
			Description: "SET work_mem = '1GB'",
		},
	}

	_, selfCaused, _ := c.Correlate(incidents, actions, nil)

	// "set_work_mem:log_out_of_memory" is a causal path match.
	if len(selfCaused) != 1 {
		t.Fatalf("selfCaused = %d, want 1", len(selfCaused))
	}
}
