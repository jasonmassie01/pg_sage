package rca

import (
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// runLogDecisionTrees — all 18 signal IDs produce an incident
// ---------------------------------------------------------------------------

func TestRunLogDecisionTrees_AllSignals(t *testing.T) {
	eng := newTestEngine()

	type signalCase struct {
		id              string
		metrics         map[string]any
		wantSeverity    string
		wantRootContain string
		wantSource      string
	}

	cases := []signalCase{
		// P0 critical signals
		{
			id:              "log_deadlock_detected",
			metrics:         map[string]any{"database": "mydb", "message": "deadlock detected"},
			wantSeverity:    "critical",
			wantRootContain: "Deadlock",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_connection_refused",
			metrics:         map[string]any{"message": "too many clients"},
			wantSeverity:    "critical",
			wantRootContain: "Connection refused",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_out_of_memory",
			metrics:         map[string]any{"database": "proddb", "message": "oom"},
			wantSeverity:    "critical",
			wantRootContain: "Out of memory",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_disk_full",
			metrics:         map[string]any{"message": "no space left"},
			wantSeverity:    "critical",
			wantRootContain: "Disk full",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_panic_server_crash",
			metrics:         map[string]any{"message": "server process was terminated by signal 11"},
			wantSeverity:    "critical",
			wantRootContain: "server crash",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_data_corruption",
			metrics:         map[string]any{"message": "invalid page in block"},
			wantSeverity:    "critical",
			wantRootContain: "Data corruption",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_txid_wraparound_warning",
			metrics:         map[string]any{"message": "database is not accepting commands to avoid wraparound data loss"},
			wantSeverity:    "critical",
			wantRootContain: "wraparound",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_archive_failed",
			metrics:         map[string]any{"message": "archive command failed"},
			wantSeverity:    "critical",
			wantRootContain: "WAL archive command failed",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_temp_file_created",
			metrics:         map[string]any{"temp_file_bytes": 104857600},
			wantSeverity:    "warning",
			wantRootContain: "temporary file",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_checkpoint_too_frequent",
			metrics:         map[string]any{"message": "checkpoints are occurring too frequently"},
			wantSeverity:    "warning",
			wantRootContain: "Checkpoints",
			wantSource:      "log_deterministic",
		},
		// P1 warning signals
		{
			id:              "log_lock_timeout",
			metrics:         map[string]any{"message": "canceling statement due to lock timeout"},
			wantSeverity:    "warning",
			wantRootContain: "Lock wait timeout",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_statement_timeout",
			metrics:         map[string]any{"message": "canceling statement due to statement timeout"},
			wantSeverity:    "warning",
			wantRootContain: "Statement timeout",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_replication_conflict",
			metrics:         map[string]any{"message": "canceling statement due to conflict with recovery"},
			wantSeverity:    "warning",
			wantRootContain: "Replication conflict",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_wal_segment_removed",
			metrics:         map[string]any{"message": "requested WAL segment has already been removed"},
			wantSeverity:    "critical",
			wantRootContain: "WAL segment removed",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_autovacuum_cancel",
			metrics:         map[string]any{"message": "canceling autovacuum task"},
			wantSeverity:    "warning",
			wantRootContain: "Autovacuum task cancelled",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_replication_slot_inactive",
			metrics:         map[string]any{"message": "replication slot is inactive"},
			wantSeverity:    "warning",
			wantRootContain: "Inactive replication slot",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_authentication_failure",
			metrics:         map[string]any{"message": "password authentication failed for user"},
			wantSeverity:    "warning",
			wantRootContain: "Authentication failure",
			wantSource:      "log_deterministic",
		},
		{
			id:              "log_slow_query",
			metrics:         map[string]any{"duration_ms": float64(5200), "query": "SELECT * FROM large_table"},
			wantSeverity:    "info",
			wantRootContain: "Slow query",
			wantSource:      "log_deterministic",
		},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			sig := &Signal{
				ID:       tc.id,
				FiredAt:  time.Now(),
				Severity: tc.wantSeverity,
				Metrics:  tc.metrics,
			}
			incidents := eng.runLogDecisionTrees([]*Signal{sig})
			if len(incidents) == 0 {
				t.Fatalf("expected at least 1 incident for signal %s, got 0", tc.id)
			}
			inc := incidents[0]
			if inc.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", inc.Source, tc.wantSource)
			}
			if inc.Severity != tc.wantSeverity {
				t.Errorf("Severity = %q, want %q", inc.Severity, tc.wantSeverity)
			}
			if !strings.Contains(inc.RootCause, tc.wantRootContain) {
				t.Errorf("RootCause = %q, want substring %q",
					inc.RootCause, tc.wantRootContain)
			}
			if len(inc.SignalIDs) != 1 || inc.SignalIDs[0] != tc.id {
				t.Errorf("SignalIDs = %v, want [%s]", inc.SignalIDs, tc.id)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// logTreeDeadlock specifics
// ---------------------------------------------------------------------------

func TestLogTreeDeadlock_NormalDeadlock(t *testing.T) {
	sig := &Signal{
		ID:       "log_deadlock_detected",
		FiredAt:  time.Now(),
		Severity: "critical",
		Metrics: map[string]any{
			"database": "mydb",
			"message":  "deadlock detected",
		},
	}
	inc := logTreeDeadlock(sig)

	if !strings.Contains(inc.RootCause, "Deadlock") {
		t.Errorf("RootCause = %q, want substring 'Deadlock'", inc.RootCause)
	}
	if inc.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", inc.Severity)
	}
	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	if !strings.Contains(evidence, "database=mydb") {
		t.Errorf("Evidence = %q, want substring 'database=mydb'", evidence)
	}
	// Normal deadlock: self-inflicted tag should NOT be present.
	if strings.Contains(evidence, "[self-inflicted deadlock]") {
		t.Errorf("Evidence should not contain self-inflicted tag for normal deadlock")
	}
}

func TestLogTreeDeadlock_SelfInflicted(t *testing.T) {
	sig := &Signal{
		ID:       "log_deadlock_detected",
		FiredAt:  time.Now(),
		Severity: "critical",
		Metrics: map[string]any{
			"database":       "mydb",
			"message":        "deadlock detected",
			"self_inflicted": true,
		},
	}
	inc := logTreeDeadlock(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	if !strings.Contains(evidence, "[self-inflicted deadlock]") {
		t.Errorf("Evidence = %q, want substring '[self-inflicted deadlock]'",
			evidence)
	}
}

func TestLogTreeDeadlock_WithPIDs(t *testing.T) {
	sig := &Signal{
		ID:       "log_deadlock_detected",
		FiredAt:  time.Now(),
		Severity: "critical",
		Metrics: map[string]any{
			"database": "mydb",
			"pids":     "1234,5678",
		},
	}
	inc := logTreeDeadlock(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	if !strings.Contains(evidence, "pids=1234,5678") {
		t.Errorf("Evidence = %q, want substring 'pids=1234,5678'", evidence)
	}
}

func TestLogTreeDeadlock_SelfInflictedFalse(t *testing.T) {
	sig := &Signal{
		ID:       "log_deadlock_detected",
		FiredAt:  time.Now(),
		Severity: "critical",
		Metrics: map[string]any{
			"database":       "mydb",
			"self_inflicted": false,
		},
	}
	inc := logTreeDeadlock(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	if strings.Contains(evidence, "[self-inflicted deadlock]") {
		t.Errorf("Evidence should not contain self-inflicted tag when false")
	}
}

// ---------------------------------------------------------------------------
// logTreeOOM — database in evidence
// ---------------------------------------------------------------------------

func TestLogTreeOOM_WithDatabase(t *testing.T) {
	sig := &Signal{
		ID:       "log_out_of_memory",
		FiredAt:  time.Now(),
		Severity: "critical",
		Metrics:  map[string]any{"database": "proddb"},
	}
	inc := logTreeOOM(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	if !strings.Contains(evidence, "database=proddb") {
		t.Errorf("Evidence = %q, want substring 'database=proddb'", evidence)
	}
	if len(inc.AffectedObjects) != 1 || inc.AffectedObjects[0] != "proddb" {
		t.Errorf("AffectedObjects = %v, want [proddb]", inc.AffectedObjects)
	}
}

func TestLogTreeOOM_WithoutDatabase(t *testing.T) {
	sig := &Signal{
		ID:       "log_out_of_memory",
		FiredAt:  time.Now(),
		Severity: "critical",
		Metrics:  map[string]any{"message": "out of memory"},
	}
	inc := logTreeOOM(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	// Without database, evidence should just be the base message.
	if strings.Contains(evidence, "database=") {
		t.Errorf("Evidence should not contain database= when absent, got %q",
			evidence)
	}
	if inc.AffectedObjects != nil {
		t.Errorf("AffectedObjects = %v, want nil", inc.AffectedObjects)
	}
}

// ---------------------------------------------------------------------------
// logTreeTxidWraparound — recommended SQL contains pg_database
// ---------------------------------------------------------------------------

func TestLogTreeTxidWraparound_RecommendedSQL(t *testing.T) {
	sig := &Signal{
		ID:       "log_txid_wraparound_warning",
		FiredAt:  time.Now(),
		Severity: "critical",
		Metrics:  map[string]any{"message": "wraparound imminent"},
	}
	inc := logTreeTxidWraparound(sig)

	if !strings.Contains(inc.RecommendedSQL, "pg_database") {
		t.Errorf("RecommendedSQL = %q, want substring 'pg_database'",
			inc.RecommendedSQL)
	}
	if !strings.Contains(inc.RootCause, "wraparound") {
		t.Errorf("RootCause = %q, want substring 'wraparound'",
			inc.RootCause)
	}
	if inc.ActionRisk != "high_risk" {
		t.Errorf("ActionRisk = %q, want high_risk", inc.ActionRisk)
	}
}

// ---------------------------------------------------------------------------
// logTreeTempFile — temp_file_bytes extracted into evidence
// ---------------------------------------------------------------------------

func TestLogTreeTempFile_WithBytes(t *testing.T) {
	sig := &Signal{
		ID:       "log_temp_file_created",
		FiredAt:  time.Now(),
		Severity: "warning",
		Metrics:  map[string]any{"temp_file_bytes": 104857600},
	}
	inc := logTreeTempFile(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	if !strings.Contains(evidence, "temp_file_bytes=104857600") {
		t.Errorf("Evidence = %q, want substring 'temp_file_bytes=104857600'",
			evidence)
	}
	if inc.Severity != "warning" {
		t.Errorf("Severity = %q, want warning", inc.Severity)
	}
}

func TestLogTreeTempFile_WithoutBytes(t *testing.T) {
	sig := &Signal{
		ID:       "log_temp_file_created",
		FiredAt:  time.Now(),
		Severity: "warning",
		Metrics:  map[string]any{},
	}
	inc := logTreeTempFile(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	// Without bytes, evidence falls back to the root cause description.
	if !strings.Contains(evidence, "temporary file") {
		t.Errorf("Evidence = %q, want substring 'temporary file'", evidence)
	}
}

func TestLogTreeTempFile_ZeroBytes(t *testing.T) {
	sig := &Signal{
		ID:       "log_temp_file_created",
		FiredAt:  time.Now(),
		Severity: "warning",
		Metrics:  map[string]any{"temp_file_bytes": 0},
	}
	inc := logTreeTempFile(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	// intMetric returns 0 for zero value, and 0 > 0 is false,
	// so evidence should fall back to root cause.
	if strings.Contains(evidence, "temp_file_bytes=0") {
		t.Errorf("Evidence should not contain zero bytes, got %q", evidence)
	}
}

// ---------------------------------------------------------------------------
// logTreeSlowQuery — duration and query in evidence
// ---------------------------------------------------------------------------

func TestLogTreeSlowQuery_WithDurationAndQuery(t *testing.T) {
	sig := &Signal{
		ID:       "log_slow_query",
		FiredAt:  time.Now(),
		Severity: "info",
		Metrics: map[string]any{
			"duration_ms": float64(5200),
			"query":       "SELECT * FROM large_table WHERE id > 0",
		},
	}
	inc := logTreeSlowQuery(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	if !strings.Contains(evidence, "duration=5200ms") {
		t.Errorf("Evidence = %q, want substring 'duration=5200ms'", evidence)
	}
	if !strings.Contains(evidence, "query=SELECT") {
		t.Errorf("Evidence = %q, want substring 'query=SELECT'", evidence)
	}
	if inc.Severity != "info" {
		t.Errorf("Severity = %q, want info", inc.Severity)
	}
}

func TestLogTreeSlowQuery_WithDurationOnly(t *testing.T) {
	sig := &Signal{
		ID:       "log_slow_query",
		FiredAt:  time.Now(),
		Severity: "info",
		Metrics:  map[string]any{"duration_ms": float64(1500)},
	}
	inc := logTreeSlowQuery(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	if !strings.Contains(evidence, "duration=1500ms") {
		t.Errorf("Evidence = %q, want substring 'duration=1500ms'", evidence)
	}
	// No query metric: should not contain "query=".
	if strings.Contains(evidence, "query=") {
		t.Errorf("Evidence should not contain 'query=' when absent, got %q",
			evidence)
	}
}

func TestLogTreeSlowQuery_WithoutDuration(t *testing.T) {
	sig := &Signal{
		ID:       "log_slow_query",
		FiredAt:  time.Now(),
		Severity: "info",
		Metrics:  map[string]any{},
	}
	inc := logTreeSlowQuery(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	// Falls back to root cause text.
	if !strings.Contains(evidence, "Slow query") {
		t.Errorf("Evidence = %q, want substring 'Slow query'", evidence)
	}
}

func TestLogTreeSlowQuery_LongQueryTruncated(t *testing.T) {
	longQuery := strings.Repeat("X", 200)
	sig := &Signal{
		ID:       "log_slow_query",
		FiredAt:  time.Now(),
		Severity: "info",
		Metrics: map[string]any{
			"duration_ms": float64(9000),
			"query":       longQuery,
		},
	}
	inc := logTreeSlowQuery(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	// Query should be truncated to 120 chars + "..."
	if !strings.HasSuffix(evidence, "...") {
		t.Errorf("Evidence should end with '...' for long query, got %q",
			evidence)
	}
	// Full 200-char query should NOT appear.
	if strings.Contains(evidence, longQuery) {
		t.Errorf("Evidence should not contain the full 200-char query")
	}
}

// ---------------------------------------------------------------------------
// logTreeReplicationSlot — recommended SQL contains pg_replication_slots
// ---------------------------------------------------------------------------

func TestLogTreeReplicationSlot_RecommendedSQL(t *testing.T) {
	sig := &Signal{
		ID:       "log_replication_slot_inactive",
		FiredAt:  time.Now(),
		Severity: "warning",
		Metrics:  map[string]any{"message": "slot is inactive"},
	}
	inc := logTreeReplicationSlot(sig)

	if !strings.Contains(inc.RecommendedSQL, "pg_replication_slots") {
		t.Errorf("RecommendedSQL = %q, want substring 'pg_replication_slots'",
			inc.RecommendedSQL)
	}
	if !strings.Contains(inc.RootCause, "Inactive replication slot") {
		t.Errorf("RootCause = %q, want substring 'Inactive replication slot'",
			inc.RootCause)
	}
}

func TestLogTreeReplicationSlot_NoMessage(t *testing.T) {
	sig := &Signal{
		ID:       "log_replication_slot_inactive",
		FiredAt:  time.Now(),
		Severity: "warning",
		Metrics:  map[string]any{},
	}
	inc := logTreeReplicationSlot(sig)

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	// When message is absent, evidence falls back to root cause.
	evidence := inc.CausalChain[0].Evidence
	if !strings.Contains(evidence, "Inactive replication slot") {
		t.Errorf("Evidence = %q, want fallback to root cause", evidence)
	}
}

// ---------------------------------------------------------------------------
// No matching signal -> empty incidents
// ---------------------------------------------------------------------------

func TestRunLogDecisionTrees_UnrecognizedSignal(t *testing.T) {
	eng := newTestEngine()

	sig := &Signal{
		ID:       "totally_unknown_signal",
		FiredAt:  time.Now(),
		Severity: "warning",
		Metrics:  map[string]any{},
	}
	incidents := eng.runLogDecisionTrees([]*Signal{sig})
	if len(incidents) != 0 {
		t.Errorf("expected 0 incidents for unrecognized signal, got %d",
			len(incidents))
	}
}

func TestRunLogDecisionTrees_EmptySignals(t *testing.T) {
	eng := newTestEngine()

	incidents := eng.runLogDecisionTrees(nil)
	if len(incidents) != 0 {
		t.Errorf("expected 0 incidents for nil signals, got %d",
			len(incidents))
	}
}

// ---------------------------------------------------------------------------
// dbAffected
// ---------------------------------------------------------------------------

func TestDbAffected_Present(t *testing.T) {
	sig := &Signal{
		Metrics: map[string]any{"database": "mydb"},
	}
	got := dbAffected(sig)
	if len(got) != 1 || got[0] != "mydb" {
		t.Errorf("dbAffected = %v, want [mydb]", got)
	}
}

func TestDbAffected_Absent(t *testing.T) {
	sig := &Signal{
		Metrics: map[string]any{},
	}
	got := dbAffected(sig)
	if got != nil {
		t.Errorf("dbAffected = %v, want nil", got)
	}
}

func TestDbAffected_EmptyString(t *testing.T) {
	sig := &Signal{
		Metrics: map[string]any{"database": ""},
	}
	got := dbAffected(sig)
	if got != nil {
		t.Errorf("dbAffected = %v, want nil for empty string", got)
	}
}

func TestDbAffected_WrongType(t *testing.T) {
	sig := &Signal{
		Metrics: map[string]any{"database": 42},
	}
	got := dbAffected(sig)
	if got != nil {
		t.Errorf("dbAffected = %v, want nil for non-string type", got)
	}
}

// ---------------------------------------------------------------------------
// buildLogIncident — source verification
// ---------------------------------------------------------------------------

func TestBuildLogIncident_Source(t *testing.T) {
	now := time.Now()
	inc := buildLogIncident(
		now, "warning",
		[]string{"test_sig"},
		"root cause",
		[]ChainLink{{Order: 1, Signal: "test_sig",
			Description: "d", Evidence: "e"}},
		nil, "", "safe",
	)
	if inc.Source != "log_deterministic" {
		t.Errorf("Source = %q, want 'log_deterministic'", inc.Source)
	}
	// Verify it's NOT "deterministic" (the base buildIncident default).
	if inc.Source == "deterministic" {
		t.Errorf("Source should be overridden from 'deterministic'")
	}
	if inc.Confidence != 0.85 {
		t.Errorf("Confidence = %f, want 0.85", inc.Confidence)
	}
}

// ---------------------------------------------------------------------------
// stringMetric and boolMetric
// ---------------------------------------------------------------------------

func TestStringMetric_Present(t *testing.T) {
	sig := &Signal{Metrics: map[string]any{"key": "value"}}
	got := stringMetric(sig, "key")
	if got != "value" {
		t.Errorf("stringMetric = %q, want 'value'", got)
	}
}

func TestStringMetric_Absent(t *testing.T) {
	sig := &Signal{Metrics: map[string]any{}}
	got := stringMetric(sig, "missing")
	if got != "" {
		t.Errorf("stringMetric = %q, want empty", got)
	}
}

func TestStringMetric_WrongType(t *testing.T) {
	sig := &Signal{Metrics: map[string]any{"key": 123}}
	got := stringMetric(sig, "key")
	if got != "" {
		t.Errorf("stringMetric = %q, want empty for int value", got)
	}
}

func TestBoolMetric_Present(t *testing.T) {
	sig := &Signal{Metrics: map[string]any{"flag": true}}
	got := boolMetric(sig, "flag")
	if !got {
		t.Errorf("boolMetric = false, want true")
	}
}

func TestBoolMetric_PresentFalse(t *testing.T) {
	sig := &Signal{Metrics: map[string]any{"flag": false}}
	got := boolMetric(sig, "flag")
	if got {
		t.Errorf("boolMetric = true, want false")
	}
}

func TestBoolMetric_Absent(t *testing.T) {
	sig := &Signal{Metrics: map[string]any{}}
	got := boolMetric(sig, "missing")
	if got {
		t.Errorf("boolMetric = true, want false for missing key")
	}
}

func TestBoolMetric_WrongType(t *testing.T) {
	sig := &Signal{Metrics: map[string]any{"flag": "true"}}
	got := boolMetric(sig, "flag")
	if got {
		t.Errorf("boolMetric = true, want false for string value")
	}
}

// ---------------------------------------------------------------------------
// logTreeSimple — evidence truncation and message fallback
// ---------------------------------------------------------------------------

func TestLogTreeSimple_MessageTruncation(t *testing.T) {
	longMsg := strings.Repeat("A", 300)
	sig := &Signal{
		ID:       "log_connection_refused",
		FiredAt:  time.Now(),
		Severity: "critical",
		Metrics:  map[string]any{"message": longMsg},
	}
	inc := logTreeSimple(sig, "critical",
		"Connection refused: too many clients already", "high_risk")

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	// Evidence should be truncated to 200 chars + "..."
	if len(evidence) != 203 {
		t.Errorf("Evidence length = %d, want 203 (200 + '...')",
			len(evidence))
	}
	if !strings.HasSuffix(evidence, "...") {
		t.Errorf("Evidence should end with '...'")
	}
}

func TestLogTreeSimple_EmptyMessageFallback(t *testing.T) {
	sig := &Signal{
		ID:       "log_connection_refused",
		FiredAt:  time.Now(),
		Severity: "critical",
		Metrics:  map[string]any{},
	}
	inc := logTreeSimple(sig, "critical",
		"Connection refused: too many clients already", "high_risk")

	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain is empty")
	}
	evidence := inc.CausalChain[0].Evidence
	// When message is empty, evidence falls back to rootCause.
	if evidence != "Connection refused: too many clients already" {
		t.Errorf("Evidence = %q, want root cause as fallback", evidence)
	}
}

// ---------------------------------------------------------------------------
// Multiple signals in a single batch
// ---------------------------------------------------------------------------

func TestRunLogDecisionTrees_MultipleSignals(t *testing.T) {
	eng := newTestEngine()

	signals := []*Signal{
		{
			ID:       "log_deadlock_detected",
			FiredAt:  time.Now(),
			Severity: "critical",
			Metrics:  map[string]any{"database": "db1"},
		},
		{
			ID:       "log_slow_query",
			FiredAt:  time.Now(),
			Severity: "info",
			Metrics:  map[string]any{"duration_ms": float64(3000)},
		},
		{
			ID:       "log_disk_full",
			FiredAt:  time.Now(),
			Severity: "critical",
			Metrics:  map[string]any{"message": "no space left"},
		},
	}
	incidents := eng.runLogDecisionTrees(signals)
	if len(incidents) != 3 {
		t.Errorf("expected 3 incidents, got %d", len(incidents))
	}

	// Verify each incident has the correct source.
	for _, inc := range incidents {
		if inc.Source != "log_deterministic" {
			t.Errorf("incident %q has Source = %q, want log_deterministic",
				inc.SignalIDs[0], inc.Source)
		}
	}
}

// ---------------------------------------------------------------------------
// Duplicate signal IDs — last one wins in sigMap
// ---------------------------------------------------------------------------

func TestRunLogDecisionTrees_DuplicateSignalID(t *testing.T) {
	eng := newTestEngine()

	// Two signals with same ID — map overwrites, so only one incident.
	signals := []*Signal{
		{
			ID:       "log_disk_full",
			FiredAt:  time.Now(),
			Severity: "critical",
			Metrics:  map[string]any{"message": "first"},
		},
		{
			ID:       "log_disk_full",
			FiredAt:  time.Now(),
			Severity: "critical",
			Metrics:  map[string]any{"message": "second"},
		},
	}
	incidents := eng.runLogDecisionTrees(signals)
	if len(incidents) != 1 {
		t.Errorf("expected 1 incident (deduped by sigMap), got %d",
			len(incidents))
	}
}
