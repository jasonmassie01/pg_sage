package rca

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// mockLogSource implements LogSource for E2E tests.
type mockLogSource struct{ signals []*Signal }

func (m *mockLogSource) Start(_ context.Context) error { return nil }
func (m *mockLogSource) Stop()                         {}
func (m *mockLogSource) Drain() []*Signal {
	out := m.signals
	m.signals = nil
	return out
}

func analyzeWithLogSignal(t *testing.T, sig *Signal) []Incident {
	t.Helper()
	eng := testEngine()
	eng.SetLogSource(&mockLogSource{signals: []*Signal{sig}})
	return eng.Analyze(quietSnapshot(), quietSnapshot(), testConfig(), nil)
}

func requireOne(t *testing.T, incidents []Incident) Incident {
	t.Helper()
	if len(incidents) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(incidents))
	}
	return incidents[0]
}

func assertIncident(
	t *testing.T, inc Incident,
	wantSigID, wantSeverity, wantRootSub string,
) {
	t.Helper()
	if inc.Source != "log_deterministic" {
		t.Errorf("Source = %q, want log_deterministic", inc.Source)
	}
	if inc.Severity != wantSeverity {
		t.Errorf("Severity = %q, want %q", inc.Severity, wantSeverity)
	}
	if !strings.Contains(inc.RootCause, wantRootSub) {
		t.Errorf("RootCause = %q, want substring %q",
			inc.RootCause, wantRootSub)
	}
	hasSig := false
	for _, sid := range inc.SignalIDs {
		if sid == wantSigID {
			hasSig = true
		}
	}
	if !hasSig {
		t.Errorf("SignalIDs = %v, want %q present", inc.SignalIDs, wantSigID)
	}
	if inc.Confidence == 0 {
		t.Error("Confidence should be non-zero")
	}
}

// TestAnalyzeE2E_AllLogSignals exercises every log signal through Analyze().
func TestAnalyzeE2E_AllLogSignals(t *testing.T) {
	type tc struct {
		id, sev, root string
		m             map[string]any
	}
	cases := []tc{
		{"log_deadlock_detected", "critical", "Deadlock",
			map[string]any{"database": "db1", "pids": "10,20"}},
		{"log_connection_refused", "critical", "Connection refused",
			map[string]any{"message": "too many clients"}},
		{"log_out_of_memory", "critical", "Out of memory",
			map[string]any{"database": "proddb"}},
		{"log_disk_full", "critical", "Disk full",
			map[string]any{"message": "no space left"}},
		{"log_panic_server_crash", "critical", "server crash",
			map[string]any{"message": "signal 11"}},
		{"log_data_corruption", "critical", "Data corruption",
			map[string]any{"message": "invalid page"}},
		{"log_txid_wraparound_warning", "critical", "wraparound",
			map[string]any{"message": "wraparound"}},
		{"log_archive_failed", "critical", "WAL archive command failed",
			map[string]any{"message": "archive failed"}},
		{"log_temp_file_created", "warning", "temporary file",
			map[string]any{"temp_file_bytes": 104857600}},
		{"log_checkpoint_too_frequent", "warning", "Checkpoints",
			map[string]any{"message": "too frequently"}},
		{"log_lock_timeout", "warning", "Lock wait timeout",
			map[string]any{"message": "lock timeout"}},
		{"log_statement_timeout", "warning", "Statement timeout",
			map[string]any{"message": "statement timeout"}},
		{"log_replication_conflict", "warning", "Replication conflict",
			map[string]any{"message": "conflict"}},
		{"log_wal_segment_removed", "critical", "WAL segment removed",
			map[string]any{"message": "WAL removed"}},
		{"log_autovacuum_cancel", "warning", "Autovacuum task cancelled",
			map[string]any{"message": "cancelled"}},
		{"log_replication_slot_inactive", "warning", "Inactive replication slot",
			map[string]any{"message": "slot inactive"}},
		{"log_authentication_failure", "warning", "Authentication failure",
			map[string]any{"message": "auth failed"}},
		{"log_slow_query", "info", "Slow query",
			map[string]any{"duration_ms": float64(5200), "query": "SELECT * FROM big"}},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			sig := &Signal{ID: c.id, FiredAt: time.Now(), Severity: c.sev, Metrics: c.m}
			inc := requireOne(t, analyzeWithLogSignal(t, sig))
			assertIncident(t, inc, c.id, c.sev, c.root)
		})
	}
}

// TestAnalyzeE2E_DeadlockEvidence checks pids, database, self-inflicted.
func TestAnalyzeE2E_DeadlockEvidence(t *testing.T) {
	sig := &Signal{
		ID: "log_deadlock_detected", FiredAt: time.Now(), Severity: "critical",
		Metrics: map[string]any{
			"database": "mydb", "pids": "1234,5678", "self_inflicted": true,
		},
	}
	inc := requireOne(t, analyzeWithLogSignal(t, sig))
	assertIncident(t, inc, "log_deadlock_detected", "critical", "Deadlock")
	if len(inc.CausalChain) == 0 {
		t.Fatal("CausalChain empty")
	}
	ev := inc.CausalChain[0].Evidence
	for _, want := range []string{"pids=1234,5678", "database=mydb", "[self-inflicted"} {
		if !strings.Contains(ev, want) {
			t.Errorf("Evidence = %q, want %q", ev, want)
		}
	}
}

// TestAnalyzeE2E_OOMEvidence checks database in evidence and AffectedObjects.
func TestAnalyzeE2E_OOMEvidence(t *testing.T) {
	sig := &Signal{
		ID: "log_out_of_memory", FiredAt: time.Now(), Severity: "critical",
		Metrics: map[string]any{"database": "proddb"},
	}
	inc := requireOne(t, analyzeWithLogSignal(t, sig))
	assertIncident(t, inc, "log_out_of_memory", "critical", "Out of memory")
	if len(inc.AffectedObjects) != 1 || inc.AffectedObjects[0] != "proddb" {
		t.Errorf("AffectedObjects = %v, want [proddb]", inc.AffectedObjects)
	}
	if !strings.Contains(inc.CausalChain[0].Evidence, "database=proddb") {
		t.Errorf("Evidence missing database=proddb")
	}
}

// TestAnalyzeE2E_TxidWraparoundSQL checks RecommendedSQL and ActionRisk.
func TestAnalyzeE2E_TxidWraparoundSQL(t *testing.T) {
	sig := &Signal{
		ID: "log_txid_wraparound_warning", FiredAt: time.Now(), Severity: "critical",
		Metrics: map[string]any{"message": "wraparound imminent"},
	}
	inc := requireOne(t, analyzeWithLogSignal(t, sig))
	assertIncident(t, inc, "log_txid_wraparound_warning", "critical", "wraparound")
	if !strings.Contains(inc.RecommendedSQL, "pg_database") {
		t.Errorf("RecommendedSQL = %q, want pg_database", inc.RecommendedSQL)
	}
	if inc.ActionRisk != "high_risk" {
		t.Errorf("ActionRisk = %q, want high_risk", inc.ActionRisk)
	}
}

// TestAnalyzeE2E_TempFileEvidence checks temp_file_bytes in evidence.
func TestAnalyzeE2E_TempFileEvidence(t *testing.T) {
	sig := &Signal{
		ID: "log_temp_file_created", FiredAt: time.Now(), Severity: "warning",
		Metrics: map[string]any{"temp_file_bytes": 104857600},
	}
	inc := requireOne(t, analyzeWithLogSignal(t, sig))
	assertIncident(t, inc, "log_temp_file_created", "warning", "temporary file")
	if !strings.Contains(inc.CausalChain[0].Evidence, "temp_file_bytes=104857600") {
		t.Errorf("Evidence missing temp_file_bytes value")
	}
}

// TestAnalyzeE2E_SlowQueryEvidence checks duration_ms and query in evidence.
func TestAnalyzeE2E_SlowQueryEvidence(t *testing.T) {
	sig := &Signal{
		ID: "log_slow_query", FiredAt: time.Now(), Severity: "info",
		Metrics: map[string]any{
			"duration_ms": float64(9500), "query": "SELECT * FROM orders WHERE id > 0",
		},
	}
	inc := requireOne(t, analyzeWithLogSignal(t, sig))
	assertIncident(t, inc, "log_slow_query", "info", "Slow query")
	ev := inc.CausalChain[0].Evidence
	if !strings.Contains(ev, "duration=9500ms") {
		t.Errorf("Evidence = %q, want duration=9500ms", ev)
	}
	if !strings.Contains(ev, "query=SELECT") {
		t.Errorf("Evidence = %q, want query=SELECT", ev)
	}
}

// TestAnalyzeE2E_ReplicationSlotSQL checks RecommendedSQL.
func TestAnalyzeE2E_ReplicationSlotSQL(t *testing.T) {
	sig := &Signal{
		ID: "log_replication_slot_inactive", FiredAt: time.Now(), Severity: "warning",
		Metrics: map[string]any{"message": "slot inactive"},
	}
	inc := requireOne(t, analyzeWithLogSignal(t, sig))
	assertIncident(t, inc, "log_replication_slot_inactive", "warning", "Inactive replication slot")
	if !strings.Contains(inc.RecommendedSQL, "pg_replication_slots") {
		t.Errorf("RecommendedSQL = %q, want pg_replication_slots", inc.RecommendedSQL)
	}
}

// TestAnalyzeE2E_MultipleLogSignals: 3 signals in one cycle -> 3 incidents.
func TestAnalyzeE2E_MultipleLogSignals(t *testing.T) {
	eng := testEngine()
	now := time.Now()
	eng.SetLogSource(&mockLogSource{signals: []*Signal{
		{ID: "log_disk_full", FiredAt: now, Severity: "critical",
			Metrics: map[string]any{"message": "no space"}},
		{ID: "log_slow_query", FiredAt: now, Severity: "info",
			Metrics: map[string]any{"duration_ms": float64(3000)}},
		{ID: "log_lock_timeout", FiredAt: now, Severity: "warning",
			Metrics: map[string]any{"message": "lock timeout"}},
	}})
	incidents := eng.Analyze(quietSnapshot(), quietSnapshot(), testConfig(), nil)
	if len(incidents) != 3 {
		t.Fatalf("expected 3 incidents, got %d", len(incidents))
	}
	sigs := make(map[string]bool)
	for _, inc := range incidents {
		for _, sid := range inc.SignalIDs {
			sigs[sid] = true
		}
		if inc.Source != "log_deterministic" {
			t.Errorf("Source = %q, want log_deterministic", inc.Source)
		}
	}
	for _, want := range []string{"log_disk_full", "log_slow_query", "log_lock_timeout"} {
		if !sigs[want] {
			t.Errorf("missing signal %q in incidents", want)
		}
	}
}

// TestAnalyzeE2E_LogSignalDedup: same signal twice -> occurrence_count bumps.
func TestAnalyzeE2E_LogSignalDedup(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()
	now := time.Now()

	eng.SetLogSource(&mockLogSource{signals: []*Signal{
		{ID: "log_disk_full", FiredAt: now, Severity: "critical",
			Metrics: map[string]any{"message": "no space"}},
	}})
	inc1 := eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil)
	if len(inc1) != 1 {
		t.Fatalf("cycle 1: expected 1, got %d", len(inc1))
	}
	firstID := inc1[0].ID

	eng.SetLogSource(&mockLogSource{signals: []*Signal{
		{ID: "log_disk_full", FiredAt: now.Add(time.Minute), Severity: "critical",
			Metrics: map[string]any{"message": "no space"}},
	}})
	inc2 := eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil)
	if len(inc2) != 1 {
		t.Fatalf("cycle 2: expected 1, got %d", len(inc2))
	}
	if inc2[0].ID != firstID {
		t.Errorf("expected same ID %q, got %q", firstID, inc2[0].ID)
	}
	if inc2[0].OccurrenceCount < 2 {
		t.Errorf("OccurrenceCount = %d, want >= 2", inc2[0].OccurrenceCount)
	}
}

// TestAnalyzeE2E_LogAndMetricSignalsCombined: both sources in one cycle.
func TestAnalyzeE2E_LogAndMetricSignalsCombined(t *testing.T) {
	eng := testEngine()
	eng.SetLogSource(&mockLogSource{signals: []*Signal{
		{ID: "log_disk_full", FiredAt: time.Now(), Severity: "critical",
			Metrics: map[string]any{"message": "no space"}},
	}})
	hot := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends: 85, MaxConnections: 100, CacheHitRatio: 0.999,
		},
	}
	incidents := eng.Analyze(hot, nil, testConfig(), nil)
	if len(incidents) < 2 {
		t.Fatalf("expected >= 2 incidents (metric+log), got %d", len(incidents))
	}
	sources := make(map[string]bool)
	for _, inc := range incidents {
		sources[inc.Source] = true
	}
	if !sources["deterministic"] {
		t.Error("missing metric incident with Source=deterministic")
	}
	if !sources["log_deterministic"] {
		t.Error("missing log incident with Source=log_deterministic")
	}
}

// TestAnalyzeE2E_SelfActionWithLogSignal: log signal + self-action correlation.
func TestAnalyzeE2E_SelfActionWithLogSignal(t *testing.T) {
	eng := testEngine()
	now := time.Now()
	store := &mockActionStore{
		recentActions: []SageAction{
			{ID: "42", Family: "drop_index", ExecutedAt: now.Add(-10 * time.Minute),
				Description: "DROP INDEX idx_users_email"},
		},
		rollbackHistory: []SageAction{},
	}
	eng.WithActionStore(store)
	eng.SetLogSource(&mockLogSource{signals: []*Signal{
		{ID: "log_slow_query", FiredAt: now, Severity: "info",
			Metrics: map[string]any{"duration_ms": float64(8000), "query": "SELECT * FROM users"}},
	}})
	incidents := eng.Analyze(quietSnapshot(), quietSnapshot(), testConfig(), nil)

	var logInc, selfInc *Incident
	for i := range incidents {
		switch incidents[i].Source {
		case "log_deterministic":
			logInc = &incidents[i]
		case "self_action":
			selfInc = &incidents[i]
		}
	}
	if logInc == nil {
		t.Error("expected log_deterministic incident")
	}
	if selfInc == nil {
		t.Error("expected self_action incident from drop_index -> log_slow_query")
	}
	if selfInc != nil && !strings.Contains(selfInc.RootCause, "Dropping index") {
		t.Errorf("self-action RootCause = %q, want causal reason", selfInc.RootCause)
	}
}

// TestAnalyzeE2E_LogSourceDrainsOnce: second cycle has no new log signals.
func TestAnalyzeE2E_LogSourceDrainsOnce(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()
	eng.SetLogSource(&mockLogSource{signals: []*Signal{
		{ID: "log_disk_full", FiredAt: time.Now(), Severity: "critical",
			Metrics: map[string]any{"message": "no space"}},
	}})
	inc1 := eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil)
	if len(inc1) != 1 {
		t.Fatalf("cycle 1: expected 1, got %d", len(inc1))
	}
	inc2 := eng.Analyze(quietSnapshot(), quietSnapshot(), cfg, nil)
	if len(inc2) != 1 {
		t.Fatalf("cycle 2: expected 1 active, got %d", len(inc2))
	}
	eng.mu.Lock()
	total := len(eng.incidents)
	eng.mu.Unlock()
	if total != 1 {
		t.Errorf("expected 1 total incident, got %d", total)
	}
}

// TestAnalyzeE2E_NilLogSource: Analyze works without SetLogSource.
func TestAnalyzeE2E_NilLogSource(t *testing.T) {
	eng := testEngine()
	incidents := eng.Analyze(quietSnapshot(), quietSnapshot(), testConfig(), nil)
	if len(incidents) != 0 {
		t.Errorf("expected 0 incidents with nil logSource, got %d", len(incidents))
	}
}
