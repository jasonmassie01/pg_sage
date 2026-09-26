package rca

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Regression tests for R05 (database identity), substrate-B11 (self-action
// family mismatch), G1-B11 (fleet log fanout database filter), G1-B25
// (startup log replay), substrate-B7 (unbounded memory) and the cache-hit
// ratio unit change (percent vs fraction).

func TestSelfAction_MapsExecutorActionFamilies(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		family   string
		sql      string
		signal   string
		wantSelf bool
	}{
		{"alter system work_mem causes OOM", "alter",
			"ALTER SYSTEM SET work_mem = '512MB'", "log_out_of_memory", true},
		{"alter role work_mem causes OOM", "alter",
			"ALTER ROLE app SET work_mem TO '1GB'", "log_out_of_memory", true},
		{"alter shared_buffers is not work_mem", "alter",
			"ALTER SYSTEM SET shared_buffers = '4GB'",
			"log_out_of_memory", false},
		{"vacuum full causes lock timeout", "vacuum",
			"VACUUM FULL public.orders", "log_lock_timeout", true},
		{"vacuum (full, analyze) causes lock timeout", "vacuum",
			"VACUUM (FULL, ANALYZE) public.orders", "log_lock_timeout", true},
		{"plain vacuum is not vacuum_full", "vacuum",
			"VACUUM (ANALYZE) public.orders", "log_lock_timeout", false},
		{"create index fills disk", "create_index",
			"CREATE INDEX CONCURRENTLY i ON t (a)", "log_disk_full", true},
		{"drop index slows queries", "drop_index",
			"DROP INDEX CONCURRENTLY i", "log_slow_query", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewSelfActionCorrelator(noopLog)
			inc := Incident{
				DetectedAt: now, Severity: "warning",
				SignalIDs: []string{tt.signal}, DatabaseName: "orders",
			}
			acts := []SageAction{{
				ID: "7", Family: tt.family, Description: tt.sql,
				ExecutedAt: now.Add(-5 * time.Minute), Database: "orders",
			}}
			_, self, review := c.Correlate([]Incident{inc}, acts, nil)
			gotSelf := len(self) == 1
			if gotSelf != tt.wantSelf {
				t.Fatalf("self-caused = %v (n=%d), want %v; review=%d",
					gotSelf, len(self), tt.wantSelf, len(review))
			}
			if gotSelf && self[0].DatabaseName != "orders" {
				t.Errorf("DatabaseName = %q, want orders",
					self[0].DatabaseName)
			}
		})
	}
}

func TestSelfAction_RollbackCountUsesMappedFamily(t *testing.T) {
	now := time.Now()
	c := NewSelfActionCorrelator(noopLog)
	inc := Incident{
		DetectedAt: now, Severity: "critical", DatabaseName: "orders",
		SignalIDs: []string{"log_out_of_memory"},
	}
	recent := []SageAction{{
		ID: "9", Family: "alter", Database: "orders",
		Description: "ALTER SYSTEM SET work_mem = '256MB'",
		ExecutedAt:  now.Add(-time.Minute),
	}}
	workMemRollbacks := []SageAction{
		{ID: "1", Family: "alter", RolledBack: true,
			Description: "ALTER SYSTEM SET work_mem = '128MB'"},
		{ID: "2", Family: "alter", RolledBack: true,
			Description: "ALTER SYSTEM SET work_mem = '64MB'"},
	}
	_, self, review := c.Correlate([]Incident{inc}, recent, workMemRollbacks)
	if len(review) != 1 || len(self) != 0 {
		t.Fatalf("work_mem rollbacks: review=%d self=%d, want 1/0",
			len(review), len(self))
	}

	otherRollbacks := []SageAction{
		{ID: "3", Family: "alter", RolledBack: true,
			Description: "ALTER SYSTEM SET shared_buffers = '1GB'"},
		{ID: "4", Family: "alter", RolledBack: true,
			Description: "ALTER SYSTEM SET max_wal_size = '4GB'"},
	}
	_, self, review = c.Correlate([]Incident{inc}, recent, otherRollbacks)
	if len(review) != 0 || len(self) != 1 {
		t.Fatalf("unrelated alter rollbacks: review=%d self=%d, want 0/1",
			len(review), len(self))
	}
}

func TestSelfAction_EmptyDatabaseNeverMatches(t *testing.T) {
	now := time.Now()
	c := NewSelfActionCorrelator(noopLog)
	acts := []SageAction{{
		ID: "7", Family: "create_index", Database: "orders",
		Description: "CREATE INDEX i ON t (a)",
		ExecutedAt:  now.Add(-time.Minute),
	}}
	inc := Incident{
		DetectedAt: now, Severity: "critical",
		SignalIDs: []string{"log_disk_full"},
	}
	ann, self, _ := c.Correlate([]Incident{inc}, acts, nil)
	if len(self) != 0 {
		t.Errorf("incident without database matched action on orders")
	}
	if len(ann) != 1 || len(ann[0].SageActions) != 0 {
		t.Errorf("annotation actions = %v, want none", ann)
	}
}

func TestAnalyze_StampsDatabaseNameOnEveryIncident(t *testing.T) {
	eng := testEngine()
	eng.WithDatabaseName("orders")
	store := &mockActionStore{recentActions: []SageAction{{
		ID: "11", Family: "create_index",
		Description: "CREATE INDEX i ON t (a)",
		ExecutedAt:  time.Now().Add(-time.Minute),
	}}}
	eng.WithActionStore(store)
	eng.SetLogSource(&mockLogSource{signals: []*Signal{{
		ID: "log_disk_full", FiredAt: time.Now(), Severity: "critical",
		Metrics: map[string]any{"message": "no space left on device"},
	}}})

	incs := eng.Analyze(hotSnapshotForRegress(), nil, testConfig(), nil)
	sources := map[string]bool{}
	for _, inc := range incs {
		sources[inc.Source] = true
		if inc.DatabaseName != "orders" {
			t.Errorf("%s incident DatabaseName = %q, want orders",
				inc.Source, inc.DatabaseName)
		}
	}
	for _, want := range []string{
		"deterministic", "log_deterministic", "self_action",
	} {
		if !sources[want] {
			t.Errorf("missing %s incident; got sources %v", want, sources)
		}
	}
}

func TestAnalyze_WithoutDatabaseNameSkipsSelfActionMatch(t *testing.T) {
	eng := testEngine()
	eng.WithActionStore(&mockActionStore{recentActions: []SageAction{{
		ID: "11", Family: "create_index",
		Description: "CREATE INDEX i ON t (a)",
		ExecutedAt:  time.Now().Add(-time.Minute),
	}}})
	eng.SetLogSource(&mockLogSource{signals: []*Signal{{
		ID: "log_disk_full", FiredAt: time.Now(), Severity: "critical",
		Metrics: map[string]any{"message": "no space left on device"},
	}}})
	for _, inc := range eng.Analyze(
		quietSnapshot(), nil, testConfig(), nil) {
		if inc.Source == "self_action" {
			t.Fatalf("self-action matched with empty identity: %+v", inc)
		}
	}
}

func TestLogSignals_FilteredByLogDatabase(t *testing.T) {
	eng := testEngine()
	eng.WithLogDatabase("orders")
	now := time.Now()
	eng.SetLogSource(&mockLogSource{signals: []*Signal{
		{ID: "log_deadlock_detected", FiredAt: now, Severity: "critical",
			Metrics: map[string]any{"database": "billing"}},
		{ID: "log_lock_timeout", FiredAt: now, Severity: "warning",
			Metrics: map[string]any{"database": "orders"}},
		{ID: "log_disk_full", FiredAt: now, Severity: "critical",
			Metrics: map[string]any{"database": ""}},
	}})
	incs := eng.Analyze(quietSnapshot(), nil, testConfig(), nil)
	got := map[string]bool{}
	for _, inc := range incs {
		for _, s := range inc.SignalIDs {
			got[s] = true
		}
	}
	if got["log_deadlock_detected"] {
		t.Error("billing deadlock routed to the orders engine")
	}
	if !got["log_lock_timeout"] {
		t.Error("orders lock timeout dropped")
	}
	if !got["log_disk_full"] {
		t.Error("cluster-wide (no database) disk-full signal dropped")
	}
}

func TestLogSignals_IgnoreReplayOlderThanStart(t *testing.T) {
	eng := testEngine()
	old := time.Now().Add(-2 * time.Hour)
	eng.SetLogSource(&mockLogSource{signals: []*Signal{
		{ID: "log_disk_full", FiredAt: old, Severity: "critical",
			Metrics: map[string]any{"message": "replayed"}},
	}})
	if incs := eng.Analyze(quietSnapshot(), nil, testConfig(), nil); len(incs) != 0 {
		t.Fatalf("replayed pre-start log line produced %d incidents: %+v",
			len(incs), incs)
	}

	eng2 := testEngine()
	eng2.WithLogReplayCutoff(time.Time{}) // disabled
	eng2.SetLogSource(&mockLogSource{signals: []*Signal{
		{ID: "log_disk_full", FiredAt: old, Severity: "critical",
			Metrics: map[string]any{"message": "replayed"}},
	}})
	if incs := eng2.Analyze(quietSnapshot(), nil, testConfig(), nil); len(incs) != 1 {
		t.Fatalf("cutoff disabled: incidents = %d, want 1", len(incs))
	}
}

func TestDetectCacheHitDrop_AcceptsPercentAndFraction(t *testing.T) {
	tests := []struct {
		name      string
		ratio     float64
		wantFire  bool
		wantRatio float64
	}{
		{"percent below threshold", 90.0, true, 0.90},
		{"percent above threshold", 99.5, false, 0},
		{"fraction below threshold", 0.90, true, 0.90},
		{"fraction above threshold", 0.995, false, 0},
		{"zero means no data", 0, false, 0},
		{"exactly 100 percent", 100.0, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eng := testEngine()
			snap := quietSnapshot()
			snap.System.CacheHitRatio = tt.ratio
			sig := eng.detectCacheHitDrop(snap, nil, testConfig())
			if (sig != nil) != tt.wantFire {
				t.Fatalf("fired = %v, want %v", sig != nil, tt.wantFire)
			}
			if sig == nil {
				return
			}
			got := floatMetric(sig, "cache_hit_ratio")
			if got < tt.wantRatio-1e-9 || got > tt.wantRatio+1e-9 {
				t.Errorf("cache_hit_ratio metric = %v, want %v",
					got, tt.wantRatio)
			}
		})
	}
}

func TestDedup_InMemoryIncidentsBounded(t *testing.T) {
	eng := testEngine()
	now := time.Now()
	for i := 0; i < 1000; i++ {
		inc := buildIncident(now, "warning", []string{"vacuum_blocked"},
			"bloat", nil, []string{fmt.Sprintf("public.t%d", i)}, "", "safe")
		eng.dedup(&inc)
	}
	if n := len(eng.incidents); n > 500 {
		t.Fatalf("tracked incidents = %d, want <= 500", n)
	}
}

func hotSnapshotForRegress() *collector.Snapshot {
	return &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends: 85, MaxConnections: 100, CacheHitRatio: 0.999,
		},
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
