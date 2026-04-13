package rca

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// ---------------------------------------------------------------------------
// severityForPct
// ---------------------------------------------------------------------------

func TestSeverityForPct(t *testing.T) {
	tests := []struct {
		name          string
		pct           int
		critThreshold int
		want          string
	}{
		{"below threshold", 70, 90, "warning"},
		{"at threshold", 90, 90, "critical"},
		{"above threshold", 95, 90, "critical"},
		{"one below threshold", 89, 90, "warning"},
		{"zero pct", 0, 90, "warning"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := severityForPct(tt.pct, tt.critThreshold)
			if got != tt.want {
				t.Errorf("severityForPct(%d, %d) = %q, want %q",
					tt.pct, tt.critThreshold, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// severityForFloat
// ---------------------------------------------------------------------------

func TestSeverityForFloat(t *testing.T) {
	tests := []struct {
		name          string
		val           float64
		critThreshold float64
		want          string
	}{
		{"below threshold", 29.9, 60.0, "warning"},
		{"at threshold", 60.0, 60.0, "critical"},
		{"above threshold", 75.5, 60.0, "critical"},
		{"just below", 59.999, 60.0, "warning"},
		{"zero value", 0.0, 60.0, "warning"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := severityForFloat(tt.val, tt.critThreshold)
			if got != tt.want {
				t.Errorf("severityForFloat(%f, %f) = %q, want %q",
					tt.val, tt.critThreshold, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// severityRank
// ---------------------------------------------------------------------------

func TestSeverityRank(t *testing.T) {
	tests := []struct {
		sev  string
		want int
	}{
		{"critical", 3},
		{"warning", 2},
		{"info", 1},
		{"unknown", 0},
		{"", 0},
		{"CRITICAL", 0}, // case-sensitive
	}
	for _, tt := range tests {
		t.Run(tt.sev, func(t *testing.T) {
			got := severityRank(tt.sev)
			if got != tt.want {
				t.Errorf("severityRank(%q) = %d, want %d",
					tt.sev, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// intMetric
// ---------------------------------------------------------------------------

func TestIntMetric(t *testing.T) {
	sig := &Signal{
		Metrics: map[string]any{
			"int_val":     42,
			"int64_val":   int64(100),
			"float64_val": float64(99.7),
			"string_val":  "nope",
		},
	}
	tests := []struct {
		key  string
		want int
	}{
		{"int_val", 42},
		{"int64_val", 100},
		{"float64_val", 99}, // truncated
		{"string_val", 0},   // unsupported type
		{"missing", 0},      // key absent
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := intMetric(sig, tt.key)
			if got != tt.want {
				t.Errorf("intMetric(sig, %q) = %d, want %d",
					tt.key, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// floatMetric
// ---------------------------------------------------------------------------

func TestFloatMetric(t *testing.T) {
	sig := &Signal{
		Metrics: map[string]any{
			"float64_val": float64(3.14),
			"int_val":     7,
			"int64_val":   int64(42),
			"string_val":  "nope",
		},
	}
	tests := []struct {
		key  string
		want float64
	}{
		{"float64_val", 3.14},
		{"int_val", 7.0},
		{"int64_val", 42.0},
		{"string_val", 0.0},
		{"missing", 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := floatMetric(sig, tt.key)
			if got != tt.want {
				t.Errorf("floatMetric(sig, %q) = %f, want %f",
					tt.key, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// sortedCopy
// ---------------------------------------------------------------------------

func TestSortedCopy(t *testing.T) {
	t.Run("sorts and does not mutate original", func(t *testing.T) {
		orig := []string{"cherry", "apple", "banana"}
		got := sortedCopy(orig)

		// Original unchanged.
		if orig[0] != "cherry" || orig[1] != "apple" || orig[2] != "banana" {
			t.Errorf("original was mutated: %v", orig)
		}
		// Sorted copy.
		want := []string{"apple", "banana", "cherry"}
		if !stringsEqual(got, want) {
			t.Errorf("sortedCopy = %v, want %v", got, want)
		}
	})

	t.Run("empty slice", func(t *testing.T) {
		got := sortedCopy([]string{})
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})

	t.Run("single element", func(t *testing.T) {
		got := sortedCopy([]string{"only"})
		if len(got) != 1 || got[0] != "only" {
			t.Errorf("expected [only], got %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// stringsEqual
// ---------------------------------------------------------------------------

func TestStringsEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want bool
	}{
		{"equal", []string{"a", "b"}, []string{"a", "b"}, true},
		{"different length", []string{"a"}, []string{"a", "b"}, false},
		{"different values", []string{"a", "b"}, []string{"a", "c"}, false},
		{"both empty", []string{}, []string{}, true},
		{"both nil", nil, nil, true},
		{"one nil one empty", nil, []string{}, true}, // len(nil)==0
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stringsEqual(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("stringsEqual(%v, %v) = %v, want %v",
					tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseIntervalSeconds
// ---------------------------------------------------------------------------

func TestParseIntervalSeconds(t *testing.T) {
	s := func(v string) *string { return &v }

	tests := []struct {
		name string
		in   *string
		want float64
	}{
		{"valid HH:MM:SS.micro", s("00:00:05.123456"), 5.123456},
		{"one hour five seconds", s("01:00:05.000000"), 3605.0},
		{"ten minutes", s("00:10:00.000000"), 600.0},
		{"nil", nil, 0},
		{"empty string", s(""), 0},
		{"malformed no colons", s("12345"), 0},
		{"malformed two parts", s("00:05"), 0},
		{"non-numeric hours", s("xx:00:05.000"), 0},
		{"non-numeric minutes", s("00:xx:05.000"), 0},
		{"non-numeric seconds", s("00:00:abc"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIntervalSeconds(tt.in)
			// Use a tolerance for float comparisons.
			diff := got - tt.want
			if diff < 0 {
				diff = -diff
			}
			if diff > 0.001 {
				t.Errorf("parseIntervalSeconds(%v) = %f, want %f",
					tt.in, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// marshalChain
// ---------------------------------------------------------------------------

func TestMarshalChain(t *testing.T) {
	t.Run("empty chain", func(t *testing.T) {
		got := marshalChain(nil)
		if got != "[]" {
			t.Errorf("marshalChain(nil) = %q, want %q", got, "[]")
		}
	})

	t.Run("single link", func(t *testing.T) {
		chain := []ChainLink{
			{Order: 1, Signal: "sig1", Description: "desc", Evidence: "ev"},
		}
		got := marshalChain(chain)
		if !strings.Contains(got, `"order":1`) {
			t.Errorf("missing order: %s", got)
		}
		if !strings.Contains(got, `"signal":"sig1"`) {
			t.Errorf("missing signal: %s", got)
		}
		if !strings.HasPrefix(got, "[") || !strings.HasSuffix(got, "]") {
			t.Errorf("not a JSON array: %s", got)
		}
	})

	t.Run("multiple links", func(t *testing.T) {
		chain := []ChainLink{
			{Order: 1, Signal: "a", Description: "d1", Evidence: "e1"},
			{Order: 2, Signal: "b", Description: "d2", Evidence: "e2"},
		}
		got := marshalChain(chain)
		// Should contain a comma separating two objects.
		if strings.Count(got, "},{") != 1 {
			t.Errorf("expected 2 objects separated by comma: %s", got)
		}
	})

	t.Run("special characters", func(t *testing.T) {
		chain := []ChainLink{
			{
				Order:       1,
				Signal:      `sig"quote`,
				Description: "line\nnewline",
				Evidence:    `back\slash`,
			},
		}
		got := marshalChain(chain)
		// %q verb in fmt.Fprintf should escape quotes and newlines.
		if !strings.Contains(got, `\"`) || !strings.Contains(got, `\n`) {
			t.Errorf("special chars not escaped: %s", got)
		}
	})
}

// ---------------------------------------------------------------------------
// buildIncident
// ---------------------------------------------------------------------------

func TestBuildIncident(t *testing.T) {
	now := time.Now()
	chain := []ChainLink{
		{Order: 1, Signal: "test_signal", Description: "d", Evidence: "e"},
	}
	inc := buildIncident(
		now, "warning",
		[]string{"test_signal"},
		"root cause text",
		chain,
		[]string{"public.users"},
		"SELECT 1;",
		"moderate",
	)

	if inc.DetectedAt != now {
		t.Errorf("DetectedAt = %v, want %v", inc.DetectedAt, now)
	}
	if inc.Severity != "warning" {
		t.Errorf("Severity = %q, want %q", inc.Severity, "warning")
	}
	if inc.RootCause != "root cause text" {
		t.Errorf("RootCause = %q", inc.RootCause)
	}
	if len(inc.CausalChain) != 1 {
		t.Fatalf("CausalChain len = %d, want 1", len(inc.CausalChain))
	}
	if inc.CausalChain[0].Signal != "test_signal" {
		t.Errorf("CausalChain[0].Signal = %q", inc.CausalChain[0].Signal)
	}
	if len(inc.AffectedObjects) != 1 || inc.AffectedObjects[0] != "public.users" {
		t.Errorf("AffectedObjects = %v", inc.AffectedObjects)
	}
	if len(inc.SignalIDs) != 1 || inc.SignalIDs[0] != "test_signal" {
		t.Errorf("SignalIDs = %v", inc.SignalIDs)
	}
	if inc.RecommendedSQL != "SELECT 1;" {
		t.Errorf("RecommendedSQL = %q", inc.RecommendedSQL)
	}
	if inc.ActionRisk != "moderate" {
		t.Errorf("ActionRisk = %q", inc.ActionRisk)
	}
	// Source should always be "deterministic" for Tier 1 incidents.
	if inc.Source != "deterministic" {
		t.Errorf("Source = %q, want %q", inc.Source, "deterministic")
	}
	if inc.Confidence != 0.85 {
		t.Errorf("Confidence = %f, want 0.85", inc.Confidence)
	}
	// ID and OccurrenceCount are set by dedup, not buildIncident.
	if inc.ID != "" {
		t.Errorf("ID should be empty before dedup, got %q", inc.ID)
	}
}

func TestBuildIncident_EmptySignalIDs(t *testing.T) {
	inc := buildIncident(
		time.Now(), "info",
		[]string{},
		"root",
		nil, nil, "", "safe",
	)
	// Source should always be "deterministic" for Tier 1 incidents.
	if inc.Source != "deterministic" {
		t.Errorf("Source = %q, want %q", inc.Source, "deterministic")
	}
}

// ---------------------------------------------------------------------------
// Signal detectors — tested via Engine methods
// ---------------------------------------------------------------------------

func newTestEngine() *Engine {
	rcaCfg := &config.RCAConfig{
		Enabled:                  true,
		DedupWindowMinutes:       30,
		EscalationCycles:         5,
		ResolutionCycles:         2,
		ConnectionSaturationPct:  80,
		ReplicationLagThresholdS: 30,
		WALSpikeMultiplier:       2.0,
	}
	return NewEngine(rcaCfg, func(string, string, ...any) {})
}

func defaultCfg() *config.Config {
	return &config.Config{
		RCA: config.RCAConfig{
			Enabled:                  true,
			DedupWindowMinutes:       30,
			EscalationCycles:         5,
			ResolutionCycles:         2,
			ConnectionSaturationPct:  80,
			ReplicationLagThresholdS: 30,
			WALSpikeMultiplier:       2.0,
		},
		Analyzer: config.AnalyzerConfig{
			IdleInTxTimeoutMinutes: 5,
			CacheHitRatioWarning:   0.95,
			TableBloatDeadTuplePct: 10,
		},
	}
}

// ---------------------------------------------------------------------------
// detectConnectionsHigh
// ---------------------------------------------------------------------------

func TestDetectConnectionsHigh(t *testing.T) {
	eng := newTestEngine()
	cfg := defaultCfg()

	t.Run("below threshold returns nil", func(t *testing.T) {
		snap := &collector.Snapshot{
			System: collector.SystemStats{
				TotalBackends:  50,
				MaxConnections: 100,
			},
		}
		sig := eng.detectConnectionsHigh(snap, nil, cfg)
		if sig != nil {
			t.Errorf("expected nil for 50%%, got %+v", sig)
		}
	})

	t.Run("at threshold returns warning", func(t *testing.T) {
		snap := &collector.Snapshot{
			System: collector.SystemStats{
				TotalBackends:  80,
				MaxConnections: 100,
			},
		}
		sig := eng.detectConnectionsHigh(snap, nil, cfg)
		if sig == nil {
			t.Fatal("expected signal at 80%%, got nil")
		}
		if sig.ID != "connections_high" {
			t.Errorf("ID = %q", sig.ID)
		}
		if sig.Severity != "warning" {
			t.Errorf("Severity = %q, want warning", sig.Severity)
		}
		pct := intMetric(sig, "pct")
		if pct != 80 {
			t.Errorf("pct metric = %d, want 80", pct)
		}
	})

	t.Run("above 90pct returns critical", func(t *testing.T) {
		snap := &collector.Snapshot{
			System: collector.SystemStats{
				TotalBackends:  95,
				MaxConnections: 100,
			},
		}
		sig := eng.detectConnectionsHigh(snap, nil, cfg)
		if sig == nil {
			t.Fatal("expected signal at 95%%, got nil")
		}
		if sig.Severity != "critical" {
			t.Errorf("Severity = %q, want critical", sig.Severity)
		}
	})

	t.Run("zero max_connections returns nil", func(t *testing.T) {
		snap := &collector.Snapshot{
			System: collector.SystemStats{
				TotalBackends:  50,
				MaxConnections: 0,
			},
		}
		sig := eng.detectConnectionsHigh(snap, nil, cfg)
		if sig != nil {
			t.Errorf("expected nil for zero max_connections, got %+v", sig)
		}
	})
}

// ---------------------------------------------------------------------------
// detectIdleInTxElevated
// ---------------------------------------------------------------------------

func TestDetectIdleInTxElevated(t *testing.T) {
	eng := newTestEngine()
	cfg := defaultCfg()

	t.Run("no idle sessions returns nil", func(t *testing.T) {
		snap := &collector.Snapshot{
			ConfigData: &collector.ConfigSnapshot{
				ConnectionStates: []collector.ConnectionState{
					{State: "active", Count: 5, AvgDurationSeconds: 1.0},
				},
			},
		}
		sig := eng.detectIdleInTxElevated(snap, cfg)
		if sig != nil {
			t.Errorf("expected nil, got %+v", sig)
		}
	})

	t.Run("nil ConfigData returns nil", func(t *testing.T) {
		snap := &collector.Snapshot{}
		sig := eng.detectIdleInTxElevated(snap, cfg)
		if sig != nil {
			t.Errorf("expected nil for nil ConfigData, got %+v", sig)
		}
	})

	t.Run("below duration threshold returns nil", func(t *testing.T) {
		// Threshold = 5 minutes = 300 seconds; avg 100s should not fire.
		snap := &collector.Snapshot{
			ConfigData: &collector.ConfigSnapshot{
				ConnectionStates: []collector.ConnectionState{
					{
						State:              "idle in transaction",
						Count:              3,
						AvgDurationSeconds: 100.0,
					},
				},
			},
		}
		sig := eng.detectIdleInTxElevated(snap, cfg)
		if sig != nil {
			t.Errorf("expected nil for 100s < 300s threshold, got %+v", sig)
		}
	})

	t.Run("above duration threshold fires", func(t *testing.T) {
		// 5 minutes = 300 seconds; avg 400s should fire.
		snap := &collector.Snapshot{
			ConfigData: &collector.ConfigSnapshot{
				ConnectionStates: []collector.ConnectionState{
					{
						State:              "idle in transaction",
						Count:              3,
						AvgDurationSeconds: 400.0,
					},
				},
			},
		}
		sig := eng.detectIdleInTxElevated(snap, cfg)
		if sig == nil {
			t.Fatal("expected signal for 400s > 300s threshold, got nil")
		}
		if sig.ID != "idle_in_tx_elevated" {
			t.Errorf("ID = %q", sig.ID)
		}
		if sig.Severity != "warning" {
			t.Errorf("Severity = %q, want warning", sig.Severity)
		}
		count := intMetric(sig, "idle_in_tx_count")
		if count != 3 {
			t.Errorf("idle_in_tx_count = %d, want 3", count)
		}
	})
}

// ---------------------------------------------------------------------------
// detectCacheHitDrop
// ---------------------------------------------------------------------------

func TestDetectCacheHitDrop(t *testing.T) {
	eng := newTestEngine()
	cfg := defaultCfg()

	t.Run("above threshold returns nil", func(t *testing.T) {
		snap := &collector.Snapshot{
			System: collector.SystemStats{CacheHitRatio: 0.99},
		}
		sig := eng.detectCacheHitDrop(snap, nil, cfg)
		if sig != nil {
			t.Errorf("expected nil for 0.99 >= 0.95, got %+v", sig)
		}
	})

	t.Run("below threshold returns warning", func(t *testing.T) {
		snap := &collector.Snapshot{
			System: collector.SystemStats{CacheHitRatio: 0.93},
		}
		sig := eng.detectCacheHitDrop(snap, nil, cfg)
		if sig == nil {
			t.Fatal("expected signal for 0.93 < 0.95")
		}
		if sig.Severity != "warning" {
			t.Errorf("Severity = %q, want warning", sig.Severity)
		}
	})

	t.Run("well below threshold returns critical", func(t *testing.T) {
		// critical threshold = warnThreshold - 0.05 = 0.90
		snap := &collector.Snapshot{
			System: collector.SystemStats{CacheHitRatio: 0.85},
		}
		sig := eng.detectCacheHitDrop(snap, nil, cfg)
		if sig == nil {
			t.Fatal("expected signal for 0.85 < 0.90")
		}
		if sig.Severity != "critical" {
			t.Errorf("Severity = %q, want critical", sig.Severity)
		}
	})

	t.Run("exactly at threshold returns nil", func(t *testing.T) {
		snap := &collector.Snapshot{
			System: collector.SystemStats{CacheHitRatio: 0.95},
		}
		sig := eng.detectCacheHitDrop(snap, nil, cfg)
		if sig != nil {
			t.Errorf("expected nil for ratio == threshold, got %+v", sig)
		}
	})
}

// ---------------------------------------------------------------------------
// detectReplicationLag
// ---------------------------------------------------------------------------

func TestDetectReplicationLag(t *testing.T) {
	eng := newTestEngine()
	cfg := defaultCfg()
	strPtr := func(v string) *string { return &v }

	t.Run("no replicas returns nil", func(t *testing.T) {
		snap := &collector.Snapshot{Replication: nil}
		sig := eng.detectReplicationLag(snap, nil, cfg)
		if sig != nil {
			t.Errorf("expected nil for nil Replication, got %+v", sig)
		}
	})

	t.Run("empty replicas returns nil", func(t *testing.T) {
		snap := &collector.Snapshot{
			Replication: &collector.ReplicationStats{
				Replicas: []collector.ReplicaInfo{},
			},
		}
		sig := eng.detectReplicationLag(snap, nil, cfg)
		if sig != nil {
			t.Errorf("expected nil for empty replicas, got %+v", sig)
		}
	})

	t.Run("below threshold returns nil", func(t *testing.T) {
		lag := "00:00:10.000000" // 10 seconds < 30 threshold
		snap := &collector.Snapshot{
			Replication: &collector.ReplicationStats{
				Replicas: []collector.ReplicaInfo{
					{
						ClientAddr: strPtr("10.0.0.2"),
						ReplayLag:  &lag,
					},
				},
			},
		}
		sig := eng.detectReplicationLag(snap, nil, cfg)
		if sig != nil {
			t.Errorf("expected nil for 10s lag, got %+v", sig)
		}
	})

	t.Run("above threshold fires warning", func(t *testing.T) {
		lag := "00:00:45.000000" // 45 seconds > 30 threshold
		snap := &collector.Snapshot{
			CollectedAt: time.Now(),
			Replication: &collector.ReplicationStats{
				Replicas: []collector.ReplicaInfo{
					{
						ClientAddr: strPtr("10.0.0.2"),
						ReplayLag:  &lag,
					},
				},
			},
		}
		sig := eng.detectReplicationLag(snap, nil, cfg)
		if sig == nil {
			t.Fatal("expected signal for 45s lag")
		}
		if sig.ID != "replication_lag_increasing" {
			t.Errorf("ID = %q", sig.ID)
		}
		// 45 < 60 (2x threshold), so warning.
		if sig.Severity != "warning" {
			t.Errorf("Severity = %q, want warning", sig.Severity)
		}
	})

	t.Run("above 2x threshold fires critical", func(t *testing.T) {
		lag := "00:01:30.000000" // 90 seconds >= 60 (2x30)
		snap := &collector.Snapshot{
			CollectedAt: time.Now(),
			Replication: &collector.ReplicationStats{
				Replicas: []collector.ReplicaInfo{
					{
						ClientAddr: strPtr("10.0.0.3"),
						ReplayLag:  &lag,
					},
				},
			},
		}
		sig := eng.detectReplicationLag(snap, nil, cfg)
		if sig == nil {
			t.Fatal("expected signal for 90s lag")
		}
		if sig.Severity != "critical" {
			t.Errorf("Severity = %q, want critical", sig.Severity)
		}
	})
}

// ---------------------------------------------------------------------------
// detectVacuumBlocked
// ---------------------------------------------------------------------------

func TestDetectVacuumBlocked(t *testing.T) {
	eng := newTestEngine()
	cfg := defaultCfg()

	t.Run("no tables returns nil", func(t *testing.T) {
		snap := &collector.Snapshot{}
		sig := eng.detectVacuumBlocked(snap, cfg)
		if sig != nil {
			t.Errorf("expected nil for no tables, got %+v", sig)
		}
	})

	t.Run("tables below dead tuple pct returns nil", func(t *testing.T) {
		snap := &collector.Snapshot{
			Tables: []collector.TableStats{
				{
					SchemaName: "public",
					RelName:    "users",
					NLiveTup:   1000,
					NDeadTup:   10, // 10/1010 ~= 0.99%
				},
			},
		}
		sig := eng.detectVacuumBlocked(snap, cfg)
		if sig != nil {
			t.Errorf("expected nil for low dead tuples, got %+v", sig)
		}
	})

	t.Run("tables above dead tuple pct fires", func(t *testing.T) {
		snap := &collector.Snapshot{
			CollectedAt: time.Now(),
			Tables: []collector.TableStats{
				{
					SchemaName: "public",
					RelName:    "orders",
					NLiveTup:   1000,
					NDeadTup:   200, // 200/1200 ~= 16.7%
				},
			},
		}
		sig := eng.detectVacuumBlocked(snap, cfg)
		if sig == nil {
			t.Fatal("expected signal for high dead tuples")
		}
		if sig.ID != "vacuum_blocked" {
			t.Errorf("ID = %q", sig.ID)
		}
		if sig.Severity != "warning" {
			t.Errorf("Severity = %q, want warning", sig.Severity)
		}
		tables, ok := sig.Metrics["blocked_tables"].([]string)
		if !ok || len(tables) == 0 {
			t.Errorf("blocked_tables missing or empty")
		} else if tables[0] != "public.orders" {
			t.Errorf("blocked_tables[0] = %q, want public.orders", tables[0])
		}
	})

	t.Run("zero live tuples skipped", func(t *testing.T) {
		snap := &collector.Snapshot{
			Tables: []collector.TableStats{
				{
					SchemaName: "public",
					RelName:    "empty_table",
					NLiveTup:   0,
					NDeadTup:   50,
				},
			},
		}
		sig := eng.detectVacuumBlocked(snap, cfg)
		if sig != nil {
			t.Errorf("expected nil for zero live tuples, got %+v", sig)
		}
	})
}

// ---------------------------------------------------------------------------
// detectLockContention
// ---------------------------------------------------------------------------

func TestDetectLockContention(t *testing.T) {
	eng := newTestEngine()

	t.Run("empty findings returns nil", func(t *testing.T) {
		sig := eng.detectLockContention(nil)
		if sig != nil {
			t.Errorf("expected nil for nil findings, got %+v", sig)
		}
	})

	t.Run("empty slice returns nil", func(t *testing.T) {
		sig := eng.detectLockContention([]analyzer.Finding{})
		if sig != nil {
			t.Errorf("expected nil for empty findings, got %+v", sig)
		}
	})

	t.Run("lock chain findings present fires", func(t *testing.T) {
		findings := []analyzer.Finding{
			{
				Category: "lock_chain",
				Severity: "warning",
				Detail: map[string]any{
					"total_blocked": 5,
				},
			},
		}
		sig := eng.detectLockContention(findings)
		if sig == nil {
			t.Fatal("expected signal for lock chain findings")
		}
		if sig.ID != "lock_contention" {
			t.Errorf("ID = %q", sig.ID)
		}
		if sig.Severity != "warning" {
			t.Errorf("Severity = %q, want warning", sig.Severity)
		}
		blocked := intMetric(sig, "total_blocked")
		if blocked != 5 {
			t.Errorf("total_blocked = %d, want 5", blocked)
		}
	})

	t.Run("non lock_chain category with no blocked returns nil", func(t *testing.T) {
		// Category != "lock_chain" means worstSev stays "info" and
		// totalBlocked stays 0.
		findings := []analyzer.Finding{
			{
				Category: "other",
				Severity: "critical",
				Detail:   map[string]any{},
			},
		}
		sig := eng.detectLockContention(findings)
		if sig != nil {
			t.Errorf("expected nil for non-lock_chain category, got %+v", sig)
		}
	})

	t.Run("multiple findings aggregates blocked count", func(t *testing.T) {
		findings := []analyzer.Finding{
			{
				Category: "lock_chain",
				Severity: "warning",
				Detail:   map[string]any{"total_blocked": 3},
			},
			{
				Category: "lock_chain",
				Severity: "critical",
				Detail:   map[string]any{"total_blocked": 7},
			},
		}
		sig := eng.detectLockContention(findings)
		if sig == nil {
			t.Fatal("expected signal")
		}
		if sig.Severity != "critical" {
			t.Errorf("Severity = %q, want critical (worst of findings)",
				sig.Severity)
		}
		blocked := intMetric(sig, "total_blocked")
		if blocked != 10 {
			t.Errorf("total_blocked = %d, want 10", blocked)
		}
	})
}

// ---------------------------------------------------------------------------
// detectWALSpike
// ---------------------------------------------------------------------------

func TestDetectWALSpike(t *testing.T) {
	eng := newTestEngine()
	cfg := defaultCfg()

	t.Run("no previous returns nil", func(t *testing.T) {
		curr := &collector.Snapshot{
			Queries: []collector.QueryStats{
				{WALBytes: 1000},
			},
		}
		sig := eng.detectWALSpike(curr, nil, cfg)
		if sig != nil {
			t.Errorf("expected nil for nil previous, got %+v", sig)
		}
	})

	t.Run("below multiplier returns nil", func(t *testing.T) {
		prev := &collector.Snapshot{
			Queries: []collector.QueryStats{{WALBytes: 1000}},
		}
		curr := &collector.Snapshot{
			Queries: []collector.QueryStats{{WALBytes: 1500}}, // 1.5x
		}
		sig := eng.detectWALSpike(curr, prev, cfg)
		if sig != nil {
			t.Errorf("expected nil for 1.5x < 2.0x, got %+v", sig)
		}
	})

	t.Run("at multiplier fires", func(t *testing.T) {
		prev := &collector.Snapshot{
			Queries: []collector.QueryStats{{WALBytes: 1000}},
		}
		curr := &collector.Snapshot{
			CollectedAt: time.Now(),
			Queries:     []collector.QueryStats{{WALBytes: 2000}}, // 2.0x
		}
		sig := eng.detectWALSpike(curr, prev, cfg)
		if sig == nil {
			t.Fatal("expected signal for 2.0x ratio")
		}
		if sig.ID != "wal_growth_spike" {
			t.Errorf("ID = %q", sig.ID)
		}
		if sig.Severity != "warning" {
			t.Errorf("Severity = %q, want warning", sig.Severity)
		}
		ratio := floatMetric(sig, "ratio")
		if ratio != 2.0 {
			t.Errorf("ratio = %f, want 2.0", ratio)
		}
	})

	t.Run("zero previous WAL returns nil", func(t *testing.T) {
		prev := &collector.Snapshot{
			Queries: []collector.QueryStats{{WALBytes: 0}},
		}
		curr := &collector.Snapshot{
			Queries: []collector.QueryStats{{WALBytes: 5000}},
		}
		sig := eng.detectWALSpike(curr, prev, cfg)
		if sig != nil {
			t.Errorf("expected nil for zero prevWAL, got %+v", sig)
		}
	})
}
